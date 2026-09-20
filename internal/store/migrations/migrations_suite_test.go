package migrations_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/testsupport"
)

// The concurrency spec races 00001's role creation against itself, which
// needs a real PostgreSQL. `make test-unit` filters the db label out, so the
// container must not be started unconditionally: the preview below asks
// Ginkgo which specs this run will execute before any of them runs.
var needsDatabase bool

func TestMigrations(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "migrations")
}

// pg is the server every spec takes its blank databases from. Its template
// carries no migrations: the point is to race the first creation of
// mill_app in a cluster where it does not exist yet.
var pg *testsupport.Postgres

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !needsDatabase {
		return nil
	}
	started, err := testsupport.Start(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(started.Stop)
	return []byte(started.URL())
}, func(ctx context.Context, adminURL []byte) {
	if len(adminURL) == 0 {
		return
	}
	var err error
	pg, err = testsupport.Attach(ctx, string(adminURL))
	Expect(err).NotTo(HaveOccurred())
})

// previewNeedsDatabase reports whether the current filters leave any spec
// carrying the db label to run.
func previewNeedsDatabase() bool {
	for _, spec := range PreviewSpecs("migrations").SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt || spec.State == types.SpecStateSkipped {
			continue
		}
		for _, label := range spec.Labels() {
			if label == testsupport.DBLabel {
				return true
			}
		}
	}
	return false
}
