package cli_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

// `mill serve` applies the migrations itself, so the template this suite
// clones from is left empty: what a spec hands the service is a database
// with no schema, and the schema afterwards is the service's own doing.
var needsDatabase bool

func TestCLI(t *testing.T) {
	RegisterFailHandler(Fail)
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "cli")
}

// pg is the server every spec clones its database from.
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
	// A workspace hands every session and check MILL_SOCKET and SKIFT_SOCKET
	// pointing at dead paths, on purpose (internal/workspace/exec.go's
	// DeadSockets), so a session's own CLI dials nothing rather than the
	// production service. resolveSockets lets a set variable win over the
	// configuration file, so a spec asserting what the file or the default
	// resolves to fails under that environment unless the suite clears the
	// variable first, here, once, before any spec runs.
	//
	// This has to be the suite's one setup node (Ginkgo allows only one
	// BeforeSuite/SynchronizedBeforeSuite), not a top-level BeforeEach:
	// previewNeedsDatabase below calls PreviewSpecs, which dry-runs a *clone*
	// of the suite (global.PushClone/PopClone) before the real run. Clone
	// copies topLevelContainers and suiteNodes — so a Describe's own
	// BeforeEach, and this node, survive the clone — but it does not copy the
	// root tree's children, so a bare top-level BeforeEach (one declared
	// outside any Describe) is silently dropped before the real run starts
	// and never executes for any spec. Clearing the variable once, for the
	// whole process, is what every spec needs: sockets_internal_test.go's
	// specs that set the variable themselves (lines 50-62, 106) still get
	// Ginkgo's per-spec restore, back to the "" set here, not to the
	// developer's shell.
	GinkgoT().Setenv("MILL_SOCKET", "")
	GinkgoT().Setenv("SKIFT_SOCKET", "")

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
	for _, spec := range PreviewSpecs("cli").SpecReports {
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
