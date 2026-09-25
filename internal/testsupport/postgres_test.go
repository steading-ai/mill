package testsupport_test

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("PostgreSQL harness", Label(testsupport.DBLabel), func() {
	// The same node reference written from two specs at once. Each It below
	// is a separate spec, so under -p they land on different processes and
	// share nothing but the container: if the clone per spec were not real,
	// the second insert would collide on the primary key.
	const nodeRef = "mill#2"

	countRuns := func(ctx context.Context, db *pgxpool.Pool) int {
		GinkgoHelper()
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n)).To(Succeed())
		return n
	}

	insertRun := func(ctx context.Context, db *pgxpool.Pool) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO runs (node_ref) VALUES ($1)`, nodeRef)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("Database specs are isolated", func() {
		It("the first spec starts empty and writes a run", func(ctx SpecContext) {
			db := pg.Database(ctx)
			Expect(countRuns(ctx, db)).To(BeZero(), "the template must hold no rows")
			insertRun(ctx, db)
			Expect(countRuns(ctx, db)).To(Equal(1))
		})

		It("the second spec starts empty and writes the same run", func(ctx SpecContext) {
			db := pg.Database(ctx)
			Expect(countRuns(ctx, db)).To(BeZero(), "no other spec's rows are visible")
			insertRun(ctx, db)
			Expect(countRuns(ctx, db)).To(Equal(1))
		})
	})

	It("The template is migrated once and every clone carries the schema", func(ctx SpecContext) {
		var exists bool
		Expect(pg.Database(ctx).QueryRow(ctx, `SELECT to_regclass('runs') IS NOT NULL`).Scan(&exists)).To(Succeed())
		Expect(exists).To(BeTrue())
	})

	It("A spec database is dropped when the spec ends", func(ctx SpecContext) {
		var name string
		// Cleanups run in reverse order of registration, so one registered
		// before Database() observes the server after the harness dropped
		// the spec's database.
		DeferCleanup(func(ctx context.Context) {
			admin, err := pgx.Connect(ctx, pg.URL())
			Expect(err).NotTo(HaveOccurred())
			defer admin.Close(ctx) //nolint:errcheck // single-statement session
			var exists bool
			Expect(admin.QueryRow(ctx, `SELECT exists(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists)).To(Succeed())
			Expect(exists).To(BeFalse(), "the spec database should be gone")
		})
		Expect(pg.Database(ctx).QueryRow(ctx, `SELECT current_database()`).Scan(&name)).To(Succeed())
		Expect(name).To(HavePrefix("spec_"))
	})
})
