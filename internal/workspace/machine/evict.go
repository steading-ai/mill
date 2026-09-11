package machine

import (
	"context"

	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// Evict is docs/sandbox.md §Eviction over the host this configuration
// names, both passes: the trigger at worker start, after reconciliation,
// and what `mill cache prune` runs. Lineages go through cacheHost — the
// worker's own uid — and images through host, which reaches the
// root-owned image directory (§Privilege).
func Evict(ctx context.Context, host, cacheHost driver.Driver, c Config) (cache.Report, error) {
	c = c.withDefaults()
	if cacheHost == nil {
		cacheHost = host
	}
	caches := cache.New(cacheHost, c.Caches)
	policy := caches.Policy(image.New(host, c.Images))
	policy.ImageExpiry = c.ImageExpiry
	return caches.Evict(ctx, policy)
}
