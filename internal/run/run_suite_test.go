package run_test

import (
	"context"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/testsupport"
)

// The run loop is specified against a real dagger and, where it writes them,
// a real ledger. Fencing is the whole contract on both sides and a fake
// would prove nothing about it, so the container holds both databases.
// `make test-unit` filters the db label out, so it must not be started
// unconditionally: the preview below asks Ginkgo which specs this run will
// execute before any of them runs.
var needsDatabase bool

func TestRun(t *testing.T) {
	RegisterFailHandler(Fail)
	// The commit specs assert whose commit the branch head is, and git hands
	// GIT_AUTHOR_* and GIT_COMMITTER_* to the hooks it runs. Left in place,
	// they would displace the identity the workspace and the fixture commit
	// under, and whether the suite passed would depend on who ran it.
	for _, name := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
	} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	needsDatabase = previewNeedsDatabase()
	RunSpecs(t, "run")
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
	for _, spec := range PreviewSpecs("run").SpecReports {
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
