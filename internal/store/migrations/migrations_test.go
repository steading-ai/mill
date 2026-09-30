package migrations_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

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

// syntheticSchema is a small, 3-file schema decoupled from the real 29
// interdependent migrations: 00001 creates a table, 00002 adds a column
// (marked out-of-order safe only when asked), 00003 adds another. It gives
// Migrator.Up's missing/out-of-order decision something legible to exercise
// instead of the production schema.
func syntheticSchema(safe bool) fstest.MapFS {
	safeComment := ""
	if safe {
		safeComment = "-- +mill out-of-order: safe\n"
	}
	return fstest.MapFS{
		"00001_widgets.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE widgets (id int);\n-- +goose Down\nDROP TABLE widgets;\n")},
		"00002_widgets_note.sql": &fstest.MapFile{Data: []byte(
			safeComment + "-- +goose Up\nALTER TABLE widgets ADD COLUMN note text;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN note;\n")},
		"00003_widgets_qty.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\nALTER TABLE widgets ADD COLUMN qty int;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN qty;\n")},
	}
}

// seedMissing applies 00001 and 00003 to databaseURL directly, skipping
// 00002, so the database ends up with a lower migration missing under a
// higher one already applied. Provider.ApplyVersion applies exactly the
// version asked, regardless of what is or isn't applied around it, so it
// builds this fixture without Migrator's own ordering decision being
// involved yet.
func seedMissing(ctx context.Context, databaseURL string, fsys fstest.MapFS) {
	GinkgoHelper()
	cfg, err := pgx.ParseConfig(databaseURL)
	Expect(err).NotTo(HaveOccurred())
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithLogger(goose.NopLogger()))
	Expect(err).NotTo(HaveOccurred())
	defer provider.Close() //nolint:errcheck // seeding connection; nothing to flush
	_, err = provider.ApplyVersion(ctx, 1, true)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.ApplyVersion(ctx, 3, true)
	Expect(err).NotTo(HaveOccurred())
}

// appliedVersions reports which versions in fsys are recorded applied in
// databaseURL, read through goose directly so a spec can check exactly what
// Migrator.Up did without a Migrator-level accessor for it.
func appliedVersions(ctx context.Context, databaseURL string, fsys fstest.MapFS) map[int64]bool {
	GinkgoHelper()
	cfg, err := pgx.ParseConfig(databaseURL)
	Expect(err).NotTo(HaveOccurred())
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithLogger(goose.NopLogger()))
	Expect(err).NotTo(HaveOccurred())
	defer provider.Close() //nolint:errcheck // read-only connection; nothing to flush
	statuses, err := provider.Status(ctx)
	Expect(err).NotTo(HaveOccurred())
	applied := make(map[int64]bool, len(statuses))
	for _, s := range statuses {
		applied[s.Source.Version] = s.State == goose.StateApplied
	}
	return applied
}

// These two specs prove Migrator.Up's response to a lower migration missing
// under a ledger already past it, on a schema small enough to read at a
// glance.
var _ = Describe("Recovering from a missing lower migration", Label("db"), func() {
	It("applies it when it is marked out-of-order safe, and continues", func(ctx context.Context) {
		url := pg.Blank(ctx)
		fsys := syntheticSchema(true)
		seedMissing(ctx, url, fsys)

		m, err := migrations.OpenFS(ctx, url, fsys)
		Expect(err).NotTo(HaveOccurred())
		defer m.Close() //nolint:errcheck // spec connection; nothing to flush

		Expect(m.Up(ctx)).To(Succeed())

		version, err := m.Version(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(BeEquivalentTo(3))
		Expect(appliedVersions(ctx, url, fsys)).To(Equal(map[int64]bool{1: true, 2: true, 3: true}))
	})

	It("refuses it when it is not marked safe, before applying anything", func(ctx context.Context) {
		url := pg.Blank(ctx)
		fsys := syntheticSchema(false)
		seedMissing(ctx, url, fsys)

		m, err := migrations.OpenFS(ctx, url, fsys)
		Expect(err).NotTo(HaveOccurred())
		defer m.Close() //nolint:errcheck // spec connection; nothing to flush

		err = m.Up(ctx)
		Expect(errors.Is(err, migrations.ErrBlocked)).To(BeTrue(), "got: %v", err)
		Expect(err.Error()).To(ContainSubstring("00002"))
		Expect(err.Error()).To(ContainSubstring("-allow-missing up"))

		version, err := m.Version(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(BeEquivalentTo(3), "the seeded version must not change on refusal")
		Expect(appliedVersions(ctx, url, fsys)).To(Equal(map[int64]bool{1: true, 2: false, 3: true}))
	})
})
