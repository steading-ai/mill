package run_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace/redact"
)

var _ = Describe("The step log", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	It("writes one file per attempt, and never a value registered with redact during the step", func() {
		const secret = "s3cret-forge-token-value"
		f.node(ctx, "a logged step", "This one talks.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")

		// Do runs as the turn begins: the value is registered while the step
		// is in flight, through the same Workspace.Redact the socket's
		// `redact` verb dispatches to, and only then does the session say
		// anything.
		f.script(fake.Script{
			Do: func(harness.Session) { f.workspace.Redact(secret) },
			Events: []*harnessv1.Event{
				fake.Text("turn-1", "using "+secret+" to reach the forge"),
				toolResult("the answer mentions " + secret + " again"),
			},
			Terminal: fake.Terminal("context gathered", nil, usage(10)),
		})

		done := make(chan error, 1)
		go func() {
			done <- f.runner(func(o *run.Options) { o.LogDir = logDir }).Execute(ctx, f.nodeRef)
		}()
		Eventually(done, 90*time.Second).Should(Receive(BeNil()))

		path := run.StepLogPath(logDir, f.runOf(ctx).GetId(), "gather-context", 1)
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("using " + redact.Mask))
		Expect(string(raw)).To(ContainSubstring("result call=call-1"))
		Expect(string(raw)).NotTo(ContainSubstring(secret))
	})
})

// toolResult is one tool's answer on the wire. The fake harness builds the
// other event shapes; this one it does not.
func toolResult(body string) *harnessv1.Event {
	return &harnessv1.Event{Body: &harnessv1.Event_ToolResult{ToolResult: &harnessv1.ToolResult{
		CallId: "call-1", Content: body,
	}}}
}
