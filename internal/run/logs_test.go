package run_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/claude"
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

	// Scenario: A crashed sidecar explains itself
	//   Given a sidecar entrypoint that throws before reading its request
	//   When a skill step runs
	//   Then the step run is failed
	//   And its log file contains the thrown error's stack
	//
	// A sidecar that dies before its terminal record leaves nothing on the
	// wire, so its standard error is the only account of what happened. The
	// last line of a Node crash is always `Node.js v24.x`, which is why the
	// whole of it is kept and written here.
	It("A crashed sidecar explains itself", func() {
		By("given a sidecar entrypoint that throws before reading its request")
		f.node(ctx, "a step whose sidecar crashes", "This one never gets a session.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		adapter := claude.New(
			claude.WithEntrypoint("/bin/sh", crashingSidecar()),
			claude.WithGrace(time.Second))

		By("when a skill step runs")
		Expect(f.runner(withHarness(adapter), func(o *run.Options) { o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run is failed")
		attempts := stepRuns(f.inspect(ctx), "gather-context")
		Expect(attempts).NotTo(BeEmpty())
		for _, attempt := range attempts {
			Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		}

		By("and its log file contains the thrown error's stack")
		raw, err := os.ReadFile(run.StepLogPath(logDir, f.runOf(ctx).GetId(), "gather-context", 1))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("Error: the sidecar could not start"))
		Expect(string(raw)).To(ContainSubstring("at Object.<anonymous>"))
		Expect(string(raw)).To(ContainSubstring("at Module._compile"))

		// And what the failure said for itself is the same stack, not the
		// one line that follows it.
		body := f.theGate(ctx).GetBody()
		Expect(body).To(ContainSubstring("Error: the sidecar could not start"))
		Expect(body).To(ContainSubstring("at Object.<anonymous>"))
	})

	// A check has no session and so no events, and before this it wrote no
	// log at all: `mill logs <run> --step clean-tree` answered "no log" for
	// a step that had certainly run and certainly failed.
	It("writes a failed check's stderr to the attempt's log", func() {
		f.node(ctx, "a failing check", "This one leaves the tree dirty.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		workflow := f.variant(ctx, "logged-check-fix", func(definition *millv1.Workflow) {
			check := stepOf(definition, "clean-tree").GetCheck()
			check.Command = []string{
				"sh", "-c", "echo 'the tree is dirty: README.md' >&2; echo 'M README.md'; exit 1",
			}
			// Both streams reach the log, so the contract keeps stdout: a
			// `none` contract drops it before the runner sees it.
			check.Stdout = millv1.StdoutShape_STDOUT_SHAPE_TEXT
		})
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed again", nil, usage(5)), Do: f.commits},
		)

		Expect(f.runner(withHarness(f.harness),
			func(o *run.Options) { o.Workflow = workflow; o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		raw, err := os.ReadFile(run.StepLogPath(logDir, f.runOf(ctx).GetId(), "clean-tree", 1))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("the tree is dirty: README.md"))
		Expect(string(raw)).To(ContainSubstring("M README.md"))
		Expect(string(raw)).To(ContainSubstring("check clean-tree exit status=1"))
	})
})

// crashingSidecar is a sidecar entrypoint that throws before it reads its
// request: a shell script writing what a Node crash writes — the stack, then
// the version line that is all a one-line diagnostic would have kept — and
// exiting non-zero. A real node is not needed to specify what the adapter
// does with a process that dies talking to nobody.
func crashingSidecar() string {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "crash.sh")
	Expect(os.WriteFile(path, []byte(`#!/bin/sh
cat >&2 <<'EOT'
/opt/mill/sidecar/dist/main.js:42
    throw new Error('the sidecar could not start');
    ^

Error: the sidecar could not start
    at Object.<anonymous> (/opt/mill/sidecar/dist/main.js:42:11)
    at Module._compile (node:internal/modules/cjs/loader:1234:14)
    at Module._load (node:internal/modules/cjs/loader:1023:12)

Node.js v24.4.1
EOT
exit 1
`), 0o700)).To(Succeed())
	return path
}

// toolResult is one tool's answer on the wire. The fake harness builds the
// other event shapes; this one it does not.
func toolResult(body string) *harnessv1.Event {
	return &harnessv1.Event{Body: &harnessv1.Event_ToolResult{ToolResult: &harnessv1.ToolResult{
		CallId: "call-1", Content: body,
	}}}
}
