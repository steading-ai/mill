package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// knownOptions are the keys the harness in these specs has. It stands in for
// the sidecar's own schema (sidecar/src/options.ts): mill carries the
// document and the harness decides what is in it, so what a spec about the
// ledger needs is a harness that refuses the way the sidecar refuses.
var knownOptions = map[string]bool{"model": true, "effort": true, "tools": true}

// validating is a harness that answers an option it does not have the way
// the sidecar does: a session that never ran, reason `policy`, and a message
// naming the key.
type validating struct{ *fake.Harness }

func (v validating) Start(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	if refusal := refuse(session); refusal != nil {
		return refusal, nil
	}
	return v.Harness.Start(ctx, session, prompt, attachments)
}

func (v validating) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	if refusal := refuse(session); refusal != nil {
		return refusal, nil
	}
	return v.Harness.Resume(ctx, session, prompt, attachments)
}

// refuse is the harness's own validation: a turn already finished, refusing,
// when the step's options carry a key this harness has no name for.
func refuse(session harness.Session) *harness.Turn {
	for key := range session.Options {
		if knownOptions[key] {
			continue
		}
		turn := harness.NewTurn()
		turn.Finish(fake.NeverRan(harness.ReasonPolicy, key+" is not an option this harness has"), nil)
		return turn
	}
	return nil
}

var _ = Describe("A step's harness options", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	// The other half of this scenario — that the sidecar is what refuses,
	// before it loads the SDK — is the spec of the same name in
	// sidecar/sidecar_test.go. This half is what mill does with the refusal.
	It("A knob the sidecar does not know is refused by the sidecar", func() {
		By("given a step whose claude options carry the key thinking_budget")
		definition := f.variant(ctx, "unknown-option", func(w *millv1.Workflow) {
			stepOf(w, "gather-context").Harness = map[string]*millv1.HarnessSettings{
				run.DefaultHarness: {Options: options(map[string]any{
					"model":           "sonnet",
					"thinking_budget": 4096,
				})},
			}
		})
		f.script(f.quickFixScripts()...)

		By("when the step starts")
		// The refusal parks the run rather than erroring out of the loop:
		// the step's on_failure is park, so the run stops where a human can
		// answer it.
		Expect(f.runner(withHarness(validating{f.harness}), func(o *run.Options) {
			o.Workflow = definition
		}).Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

		By("then mill records a failed attempt with that message")
		attempts := stepRuns(f.inspect(ctx), "gather-context")
		Expect(attempts).To(HaveLen(1))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[0].GetExit().GetStatus()).To(BeNumerically("==", -1))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_POLICY))
		Expect(f.comments(ctx)).To(ContainElement(ContainSubstring("thinking_budget")))
	})

	It("refuses a workflow naming a harness it has no adapter for, at run start", func() {
		By("given a step whose options are for a harness mill cannot drive")
		definition := f.variant(ctx, "unknown-harness", func(w *millv1.Workflow) {
			stepOf(w, "pr").Harness = map[string]*millv1.HarnessSettings{
				"other": {Options: options(map[string]any{"model": "sonnet"})},
			}
		})
		f.script(f.quickFixScripts()...)

		By("when the run starts")
		err := f.runner(func(o *run.Options) { o.Workflow = definition }).Execute(ctx, f.nodeRef)

		By("then it is refused by name, before any step runs")
		Expect(err).To(MatchError(ContainSubstring("other")))
		Expect(stepRuns(f.inspect(ctx), "gather-context")).To(BeEmpty())
		Expect(stepRuns(f.inspect(ctx), "pr")).To(BeEmpty())
	})
})
