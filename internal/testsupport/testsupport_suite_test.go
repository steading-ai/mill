package testsupport_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

func TestTestsupport(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "testsupport")
}

// pg is the server every db-labelled spec in this suite clones from.
var pg *testsupport.Postgres

// The stand-in schema the specs exercise the harness with. The real
// migrations arrive with the store node and plug in the same way.
func migrate(ctx context.Context, databaseURL string) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	_, err = conn.Exec(ctx, `CREATE TABLE runs (node_ref text PRIMARY KEY)`)
	return err
}

// The first process starts the container and shares its URL; every process,
// including the first, attaches to it. Ginkgo skips suite setup entirely
// when the label filter leaves no spec to run, which is how `make test-unit`
// never touches podman.
var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	started, err := testsupport.Start(ctx, migrate)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	var err error
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})
