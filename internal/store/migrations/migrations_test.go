package migrations_test

import (
	"context"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/store/migrations"
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

// goose refuses to prepare a set of migrations in which two files claim the
// same version, and the refusal is the service's first line at start. Two
// branches each adding the next number merge without a textual conflict, so
// the collision is only ever seen when the merged binary starts. This spec
// sees it at unit tier instead, on the embedded set the binary will carry.
var _ = Describe("The embedded migrations", func() {
	It("each carry a version no other migration claims", func() {
		entries, err := migrations.FS.ReadDir(".")
		Expect(err).NotTo(HaveOccurred())
		seen := map[string]string{}
		for _, entry := range entries {
			name := entry.Name()
			version, _, found := strings.Cut(name, "_")
			Expect(found).To(BeTrue(), "%s is not in goose's NNNNN_name.sql layout", name)
			Expect(seen).NotTo(HaveKey(version), "%s and %s both claim version %s", seen[version], name, version)
			seen[version] = name
		}
	})
})
