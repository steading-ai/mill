package run_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
)

// The cache-candidate hook: the loop asks a workspace that carries lineages
// for a candidate exactly when a step closes on evidence, and never
// otherwise (docs/sandbox.md §"Step boundary"). The workspace here is the
// local one with the hook recorded over it; what a candidate is on a
// machine is internal/workspace/machine/cache's.

// candidates is a workspace that records the steps it was asked for a
// candidate at.
type candidates struct {
	run.Workspace
	mu    sync.Mutex
	steps []string
}

var _ workspace.CacheCandidate = (*candidates)(nil)

func (c *candidates) Candidate(_ context.Context, step string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, step)
	return nil
}

func (c *candidates) taken() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.steps...)
}

var _ = Describe("The cache candidate hook", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context
	var ws *candidates

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.wrapWorkspace = func(inner run.Workspace) run.Workspace {
			ws = &candidates{Workspace: inner}
			return ws
		}
	})

	It("A step that closes on evidence leaves a promotion candidate", func() {
		By("given a run whose every step closes on evidence")
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)

		By("when the run finishes")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then a candidate was taken at each evidence-closed step that ran in the workspace, in the order they closed, and at no other")
		// `select` closes before the workspace is opened: there is no copy
		// to snapshot yet, and nothing asks for one.
		closed := closedSteps(f.inspect(ctx))
		delete(closed, "select")
		Expect(ws.taken()).To(HaveLen(len(closed)))
		for _, step := range ws.taken() {
			Expect(closed).To(HaveKey(step))
		}
		// The toolchain step is first: what readying the machine put in the
		// caches — a warm mise — is exactly what is worth promoting.
		Expect(ws.taken()[0]).To(Equal("toolchain"))
		Expect(ws.taken()[len(ws.taken())-1]).To(Equal("merge"))
	})

	It("A step that is cancelled leaves no candidate", func() {
		By("given a step that is cancelled")
		f.node(ctx, "a long step", "This one would run for ten minutes.")
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})
		runner := f.runner(func(o *run.Options) {
			o.TTL = 3 * time.Second
			o.Interval = time.Second
			o.Grace = 5 * time.Second
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0), "the step is in flight")
		stepRunID := f.openStepRun(ctx).GetId()

		By("when the step is cancelled")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))

		By("then no candidate was taken for it; the toolchain step before it, closed on evidence, left the only one")
		Expect(f.stepRunStatus(ctx, stepRunID)).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		Expect(ws.taken()).To(Equal([]string{"toolchain"}))
	})
})
