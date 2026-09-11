package cache

import (
	"github.com/ivy/mill/internal/workspace/machine/config"
)

// Lineages is what a run restores, from the caches its configuration
// declares: a global cache is the global lineage of its name, a repo cache
// is the repository's, and a cache with key files carries their hash from
// the checkout (docs/sandbox.md §Caching).
func Lineages(caches []config.Cache, repo Scope, checkout string) ([]Lineage, error) {
	out := make([]Lineage, 0, len(caches))
	for _, cache := range caches {
		lineage := Lineage{Scope: repo, Name: cache.Name}
		if cache.Scope == config.Global {
			lineage.Scope = Global
		}
		hash, err := HashKey(checkout, cache.Key)
		if err != nil {
			return nil, err
		}
		lineage.KeyHash = hash
		out = append(out, lineage)
	}
	return out, nil
}
