package server_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/testsupport"
)

// The handlers are specified against a real PostgreSQL: what they are for is
// mapping the wire to the ledger and back, and a fake ledger would prove
// nothing about either. `make test-unit` filters the db label out, so the
// container must not be started unconditionally: the preview below asks
// Ginkgo which specs this run will execute before any of them runs.
var needsDatabase bool

func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "server")
}

// pg is the server every spec clones its database from.
var pg *testsupport.Postgres

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	if !needsDatabase {
		return nil
	}
	// domain.Migrate, not the migrations package: only the domain layer may
	// reach the store, and a spec beside the handlers is held to the same
	// rule as the handlers.
	started, err := testsupport.Start(ctx, domain.Migrate)
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
	for _, spec := range PreviewSpecs("server").SpecReports {
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
