// Package config reads what a repository declares about the machine it runs
// in: `mill.toml` at the checkout root, with defaults derived from the tools
// `mise.toml` names (docs/sandbox.md §mill.toml, §Caching).
//
// The repo declares what — packages, caches, whether its tests boot machines
// — and the worker owns policy. Nothing here touches a machine, and nothing
// a repo writes can fail its run: an invalid file yields the defaults and a
// [Finding] the first session receives. The package is pure so that every
// rule in §mill.toml is a spec with no host under it.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The files read from the checkout root.
const (
	// MillFile is the repo's declaration.
	MillFile = "mill.toml"
	// MiseFile is where the ecosystem defaults are derived from.
	MiseFile = "mise.toml"
)

// The paths a cache may live under inside the machine (docs/sandbox.md
// §Filesystem).
const (
	// Home is the session's home; `~` in a declared path expands to it.
	Home = "/home/agent"
	// Work is the checkout's mount.
	Work = "/work"
	// RunDir is the run's socket directory, never a cache.
	RunDir = "/run/mill"
)

// Scope is who shares a lineage (docs/sandbox.md §"Kinds and scope").
type Scope string

const (
	// Global is shared by every run on the worker: its contents are
	// verified by the tool that fills it.
	Global Scope = "global"
	// Repo is trusted only within the repository that produced it.
	Repo Scope = "repo"
)

// Config is a repository's declaration after defaults and validation: what
// a run of it is provisioned with.
type Config struct {
	// Image is what the derived image carries beyond the base.
	Image Image
	// Caches are the lineages the run restores, in a stable order:
	// defaults as §Caching's table lists them, then declared caches by
	// name. A disabled cache is absent.
	Caches []Cache
	// Machine is what the repo asks of the machine itself.
	Machine Machine
}

// Image is the `[image]` table.
type Image struct {
	// Packages are distro package names, sorted and deduplicated.
	Packages []string
}

// Cache is one `[cache.<name>]` table, or a default.
type Cache struct {
	// Name is the lineage's name; it identifies the lineage on the host.
	Name string
	// Path is where the machine sees it: absolute, with `~` expanded to
	// [Home], and under [Home] or [Work].
	Path string
	// Scope is the lineage's scope: a default's from the table, [Repo]
	// for a cache the repo introduces.
	Scope Scope
	// Key is the optional list of checkout files whose hash gates a
	// restore. Empty means the lineage always restores.
	Key []string
	// Enabled is whether the run restores the lineage. Load never returns
	// a disabled cache; the field is what a declaration sets to opt out.
	Enabled bool
}

// Machine is the `[machine]` table.
type Machine struct {
	// Nested requests that this repo's machine may boot machines of its
	// own. It is a request; worker configuration grants it.
	Nested bool
}

// Finding is one thing wrong with a declaration, said so the agent that
// wrote it can fix it. It is never an error: the run proceeds with the
// rest of the declaration, or with defaults when nothing could be read.
type Finding struct {
	// File is the file at fault, `mill.toml` or `mise.toml`.
	File string
	// Key is the TOML key at fault in dotted form, such as
	// `cache.go-build.path`; empty when the file as a whole is.
	Key string
	// Message is one sentence an agent can act on.
	Message string
}

func (f Finding) String() string {
	if f.Key == "" {
		return fmt.Sprintf("%s: %s", f.File, f.Message)
	}
	return fmt.Sprintf("%s: %s: %s", f.File, f.Key, f.Message)
}

// packageName is what a distro package name may be.
var packageName = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// lineageName is what a cache name may be: it becomes a path component on
// the host, so it is one word with no separator in it.
var lineageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// file is `mill.toml` as written.
type file struct {
	Image struct {
		Packages []string `toml:"packages"`
	} `toml:"image"`
	Cache   map[string]cacheTable `toml:"cache"`
	Machine struct {
		Nested bool `toml:"nested"`
	} `toml:"machine"`
}

// cacheTable is one `[cache.<name>]` as written. Enabled is a pointer so an
// absent key reads as true.
type cacheTable struct {
	Path    string   `toml:"path"`
	Key     []string `toml:"key"`
	Enabled *bool    `toml:"enabled"`
}

// Load reads `mill.toml` and `mise.toml` from the checkout root. The
// findings are what the first session is told; the error is only an I/O
// failure that is not the file's absence, which is the run's to handle.
func Load(checkout string) (Config, []Finding, error) {
	var findings []Finding

	tools, miseFindings, err := readTools(filepath.Join(checkout, MiseFile))
	if err != nil {
		return Config{}, nil, err
	}
	findings = append(findings, miseFindings...)
	config := Config{Caches: Defaults(tools)}

	data, err := os.ReadFile(filepath.Join(checkout, MillFile))
	if errors.Is(err, fs.ErrNotExist) {
		return config, findings, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("config: %w", err)
	}

	var declared file
	meta, err := toml.Decode(string(data), &declared)
	if err != nil {
		findings = append(findings, Finding{
			File:    MillFile,
			Message: fmt.Sprintf("the file is not valid TOML and the run proceeds with defaults: %v", err),
		})
		return config, findings, nil
	}
	for _, key := range undecoded(meta) {
		findings = append(findings, Finding{
			File:    MillFile,
			Key:     key,
			Message: "the key is not one mill.toml defines and is ignored; the tables are [image], [cache.<name>] and [machine]",
		})
	}

	config.Image.Packages, findings = packages(declared.Image.Packages, findings)
	config.Caches, findings = caches(config.Caches, declared.Cache, findings)
	config.Machine.Nested = declared.Machine.Nested
	return config, findings, nil
}

// packages sorts and deduplicates the declared list, dropping any name that
// is not a package name.
func packages(declared []string, findings []Finding) ([]string, []Finding) {
	var names []string
	for i, name := range declared {
		if !packageName.MatchString(name) {
			findings = append(findings, Finding{
				File:    MillFile,
				Key:     fmt.Sprintf("image.packages[%d]", i),
				Message: fmt.Sprintf("%q is not a package name; a name matches %s", name, packageName),
			})
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return slices.Compact(names), findings
}

// caches applies the declared tables over the defaults: a name a default
// has overrides it, `enabled = false` removes it, and a new name is a
// repo-scoped lineage of the repo's own.
func caches(defaults []Cache, declared map[string]cacheTable, findings []Finding) ([]Cache, []Finding) {
	byName := make(map[string]int, len(defaults))
	for i, cache := range defaults {
		byName[cache.Name] = i
	}
	result := slices.Clone(defaults)

	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		table := declared[name]
		key := "cache." + name
		if !lineageName.MatchString(name) {
			findings = append(findings, Finding{
				File:    MillFile,
				Key:     key,
				Message: fmt.Sprintf("%q is not a lineage name; a name matches %s", name, lineageName),
			})
			continue
		}
		index, isDefault := byName[name]

		if table.Enabled != nil && !*table.Enabled {
			if isDefault {
				result[index].Enabled = false
			}
			continue
		}

		cache := Cache{Name: name, Scope: Repo, Enabled: true}
		if isDefault {
			cache = result[index]
		}
		if table.Path != "" {
			resolved, finding := resolvePath(key+".path", table.Path)
			if finding != nil {
				findings = append(findings, *finding)
				continue
			}
			cache.Path = resolved
		} else if !isDefault {
			findings = append(findings, Finding{
				File:    MillFile,
				Key:     key + ".path",
				Message: fmt.Sprintf("%q is not a default lineage, so it needs a path under %s or %s", name, Home, Work),
			})
			continue
		}
		keyFiles, keyFindings := keyFiles(key+".key", table.Key)
		findings = append(findings, keyFindings...)
		if len(keyFindings) > 0 {
			continue
		}
		cache.Key = keyFiles

		if isDefault {
			result[index] = cache
		} else {
			result = append(result, cache)
		}
	}

	return slices.DeleteFunc(result, func(c Cache) bool { return !c.Enabled }), findings
}

// resolvePath expands `~`, cleans, and checks a declared cache path against
// §mill.toml's rules. The finding names the path as declared.
func resolvePath(key, declared string) (string, *Finding) {
	reject := func(reason string) (string, *Finding) {
		return "", &Finding{
			File:    MillFile,
			Key:     key,
			Message: fmt.Sprintf("%q %s; a cache path is a directory under %s (~) or %s", declared, reason, Home, Work),
		}
	}

	expanded := declared
	switch {
	case expanded == "~":
		expanded = Home
	case strings.HasPrefix(expanded, "~/"):
		expanded = Home + expanded[1:]
	}
	if !path.IsAbs(expanded) {
		return reject("is not an absolute path")
	}
	cleaned := path.Clean(expanded)
	if cleaned == Home || cleaned == Work {
		return reject("is a mount the machine already owns")
	}
	if cleaned == RunDir || strings.HasPrefix(cleaned, RunDir+"/") {
		return reject("is the run's socket directory")
	}
	if !strings.HasPrefix(cleaned, Home+"/") && !strings.HasPrefix(cleaned, Work+"/") {
		return reject("is outside the session's home and the checkout")
	}
	return cleaned, nil
}

// keyFiles checks that each key file names a file inside the checkout: a
// relative path that does not escape it.
func keyFiles(key string, declared []string) ([]string, []Finding) {
	var findings []Finding
	var files []string
	for i, name := range declared {
		cleaned := path.Clean(name)
		if name == "" || path.IsAbs(name) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			findings = append(findings, Finding{
				File:    MillFile,
				Key:     fmt.Sprintf("%s[%d]", key, i),
				Message: fmt.Sprintf("%q is not a file in the checkout; a key is a path relative to the checkout root", name),
			})
			continue
		}
		files = append(files, cleaned)
	}
	return files, findings
}

// undecoded is the keys the file has that the grammar does not, outermost
// only: an unknown table is one finding, not one per key in it.
func undecoded(meta toml.MetaData) []string {
	var keys []string
	for _, key := range meta.Undecoded() {
		name := key.String()
		if len(keys) > 0 && strings.HasPrefix(name, keys[len(keys)-1]+".") {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}
