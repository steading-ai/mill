// Package skill materialises the mill skill where the worker mounts it.
//
// The skill and the sidecar are the worker's and change with its version;
// baked into an image they would tie a worker upgrade to a base rebuild
// (docs/adrs/018 §Rejected). Instead the worker writes the skill it was
// built with to one directory on the host — the one read-only source the
// machine helper's policy admits — and bind-mounts it into every machine
// at the harness's skill path (docs/sandbox.md §"Skill mount"). What a
// session reads under ~/.claude/skills/mill is therefore what the binary
// that started its run embeds, never a stale copy an earlier install left.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	mill "github.com/ivy/mill"
)

// What the materialised directory holds beside the plugin's own files.
// Where a machine sees the directory is the machine's business
// (internal/workspace/machine/config, SkillPath).
const (
	// VersionFile is the breadcrumb: one line, the worker's version as
	// `mill version` prints it, beside SKILL.md rather than in its
	// frontmatter so the skill's frontmatter stays the plugin's
	// (docs/core-principles.md §6).
	VersionFile = "VERSION"
)

// Materialise writes the embedded skill and the version breadcrumb into
// dir, making it if it is missing, and removes whatever else an earlier
// worker left there. Each file lands by rename, so a machine that has the
// directory bound while a newer worker rewrites it reads whole files, old
// or new, and never a truncated one. Running it twice changes nothing.
func Materialise(dir, version string) error {
	if version == "" || strings.ContainsAny(version, "\n\r") {
		return fmt.Errorf("skill: version %q is not one line", version)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // bound read-only into every machine; nothing secret
		return fmt.Errorf("skill: %w", err)
	}
	wanted := map[string]bool{}
	source, err := fs.Sub(mill.Skill, mill.SkillRoot)
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	err = fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		wanted[name] = true
		target := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // as above
		}
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		return place(target, body)
	})
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	wanted[VersionFile] = true
	if err := place(filepath.Join(dir, VersionFile), []byte(version+"\n")); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return prune(dir, wanted)
}

// Version reads the breadcrumb a materialised directory carries.
func Version(dir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(dir, VersionFile)) //nolint:gosec // the worker's own directory
	if err != nil {
		return "", fmt.Errorf("skill: %w", err)
	}
	return strings.TrimSuffix(string(body), "\n"), nil
}

// place writes body to path atomically: a sibling temporary file, then a
// rename over the target. A file already holding body is left alone, so an
// unchanged skill keeps its inode and a bound machine sees nothing move.
func place(path string, body []byte) error {
	if current, err := os.ReadFile(path); err == nil && string(current) == string(body) { //nolint:gosec // the worker's own directory
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp, body); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// write fills a temporary file, makes it world-readable, and closes it.
func write(tmp *os.File, body []byte) (err error) {
	defer func() {
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	return tmp.Chmod(0o644) //nolint:gosec // bound read-only into every machine; nothing secret
}

// prune removes what is under dir that the skill does not hold: an earlier
// version's file that was renamed away, or a placeholder. A directory the
// skill has is kept; one it does not is removed whole.
func prune(dir string, wanted map[string]bool) error {
	var stale []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." || wanted[filepath.ToSlash(rel)] {
			return nil
		}
		stale = append(stale, path)
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("skill: %w", err)
		}
	}
	return nil
}
