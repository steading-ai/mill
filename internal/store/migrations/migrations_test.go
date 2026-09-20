package migrations_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/store/migrations"
)

// 00001 creates the mill_app role with a check-then-create that is not
// atomic, because roles are cluster-wide and CREATE ROLE cannot run
// IF NOT EXISTS. Two databases in the same cluster migrating at once is
// exactly the shape ginkgo -p gives every other suite, so this races that
// case directly rather than trusting it never happens in practice.
var _ = Describe("Migrating two databases in the same cluster at once", Label("db"), func() {
	It("creates the shared mill_app role without either migration failing", func(ctx context.Context) {
		first := pg.Blank(ctx)
		second := pg.Blank(ctx)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = migrations.Migrate(ctx, first)
		}()
		go func() {
			defer wg.Done()
			errs[1] = migrations.Migrate(ctx, second)
		}()
		wg.Wait()

		Expect(errs[0]).NotTo(HaveOccurred())
		Expect(errs[1]).NotTo(HaveOccurred())
	})
})
