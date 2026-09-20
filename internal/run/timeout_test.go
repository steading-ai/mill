package run_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// A skill step has a wall clock, and a session that outlives it is stopped.
// The attempt ran — the session worked for the whole budget — so its exit
// must say `timeout`, not `refused`: a refusal is a session that never
// started, and the two are different findings for whoever reads the ledger.

var _ = Describe("A session over its step's budget", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("is stopped, and the attempt's exit says timeout", func() {
		By("given an implement step whose budget is a second and a session that never ends on its own")
		workflow := f.variant(ctx, "short-implement", func(definition *millv1.Workflow) {
			implement := stepOf(definition, "implement")
			implement.Timeout = durationpb.New(time.Second)
			implement.Retries = 1
		})
		scripts := f.quickFixScripts()
		f.script(append([]fake.Script{scripts[0], {Hold: true}}, scripts[1:]...)...)

		By("when the run reaches it")
		started := time.Now()
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(time.Since(started)).To(BeNumerically("<", 30*time.Second), "the session was stopped, not waited for")

		By("then the first attempt failed at its budget, and the retry closed the step")
		attempts := stepRuns(f.inspect(ctx), "implement")
		Expect(attempts).To(HaveLen(2))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[0].GetExit().GetStatus()).To(Equal(int32(-1)))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_TIMEOUT))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	})
})
