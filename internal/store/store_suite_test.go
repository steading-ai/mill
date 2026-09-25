package store_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/store/migrations"
	"github.com/steading-ai/mill/internal/testsupport"
)

// Every spec in this suite runs against a real PostgreSQL, because the
// invariants under test are constraints, triggers and lock behaviour and a
// fake would prove nothing about any of them. `make test-unit` filters the
// db label out, so the container must not be started unconditionally: the
// preview below asks Ginkgo which specs this run will execute before any of
// them runs.
var needsDatabase bool

func TestStore(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "store")
}

// pg is the server every spec clones its database from.
var pg *testsupport.Postgres

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !needsDatabase {
		return nil
	}
	started, err := testsupport.Start(ctx, migrations.Migrate)
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
	for _, spec := range PreviewSpecs("store").SpecReports {
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
