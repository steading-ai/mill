package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// defaultCache is one row of docs/sandbox.md §Caching's defaults table.
type defaultCache struct {
	// tool is the mise tool that brings the row in; empty means every
	// checkout has it.
	tool  string
	cache Cache
}

// defaults is §Caching's table in its order. A lineage's name is stable so
// mill.toml can override or disable it.
var defaults = []defaultCache{
	{"", Cache{Name: "mise", Path: Home + "/.local/share/mise", Scope: Global, Enabled: true}},
	{"go", Cache{Name: "go-mod", Path: Home + "/go/pkg/mod", Scope: Global, Enabled: true}},
	{"go", Cache{Name: "go-build", Path: Home + "/.cache/go-build", Scope: Repo, Enabled: true}},
	{"rust", Cache{Name: "cargo-registry", Path: Home + "/.cargo/registry", Scope: Global, Enabled: true}},
	{"rust", Cache{Name: "target", Path: Work + "/target", Scope: Repo, Enabled: true}},
	{"node", Cache{Name: "npm-cache", Path: Home + "/.npm", Scope: Global, Enabled: true}},
	{"bun", Cache{Name: "bun-cache", Path: Home + "/.bun/install/cache", Scope: Global, Enabled: true}},
	{"node", Cache{Name: "node_modules", Path: Work + "/node_modules", Scope: Repo, Enabled: true}},
	{"bun", Cache{Name: "node_modules", Path: Work + "/node_modules", Scope: Repo, Enabled: true}},
	{"ruby", Cache{Name: "gem-cache", Path: Home + "/.gem", Scope: Global, Enabled: true}},
	{"ruby", Cache{Name: "bundle", Path: Work + "/vendor/bundle", Scope: Repo, Enabled: true}},
}

// Defaults is the lineages a checkout declaring these mise tools gets
// before mill.toml has its say: §Caching's table, filtered to the tools
// named. `mise` itself is always present.
func Defaults(tools []string) []Cache {
	var caches []Cache
	seen := map[string]bool{}
	for _, row := range defaults {
		if row.tool != "" && !slices.Contains(tools, row.tool) {
			continue
		}
		if seen[row.cache.Name] {
			continue
		}
		seen[row.cache.Name] = true
		caches = append(caches, row.cache)
	}
	return caches
}

// readTools is the keys of mise.toml's `[tools]` table, whatever their
// values are: a version string, a list of them, or a table. A tool named
// through mise's `core:` backend counts as the bare name. An absent file is
// no tools; an unreadable one is a finding, so a broken mise.toml costs the
// ecosystem defaults and nothing else.
func readTools(name string) ([]string, []Finding, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	var mise struct {
		Tools map[string]any `toml:"tools"`
	}
	if _, err := toml.Decode(string(data), &mise); err != nil {
		return nil, []Finding{{
			File:    MiseFile,
			Key:     "tools",
			Message: fmt.Sprintf("the file is not valid TOML, so no ecosystem defaults were derived from it: %v", err),
		}}, nil
	}
	tools := make([]string, 0, len(mise.Tools))
	for tool := range mise.Tools {
		tools = append(tools, strings.TrimPrefix(tool, "core:"))
	}
	return tools, nil, nil
}
