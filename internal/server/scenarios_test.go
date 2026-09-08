package server_test

import (
	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
)

// The scenarios of the node this package was built for. Each is the Gherkin
// it came from, and asserts what its Then clause says and nothing else.
var _ = Describe("The rules the store does not own", Label(testsupport.DBLabel), func() {
	It("A transition not in the diagram is refused", func(ctx SpecContext) {
		// Given run R is done
		f := newFixture(ctx, "mill#22")
		done, err := f.client().Done(ctx, connect.NewRequest(&millv1.DoneRequest{
			LeaseId: lease, RunId: f.run.GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(done.Msg.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))

		// When MarkWaiting(R) is called
		_, err = f.client().MarkWaiting(ctx, connect.NewRequest(&millv1.MarkWaitingRequest{
			LeaseId:    lease,
			RunId:      f.run.GetId(),
			GateNodeId: "gate-node",
		}))

		// Then the response is failed_precondition and names done and waiting
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("done"))
		Expect(err.Error()).To(ContainSubstring("waiting"))
	})

	It("Evidence is validated by kind at the boundary", func(ctx SpecContext) {
		// Given a step run whose step names evidence kind commit
		f := newFixture(ctx, "mill#23")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		Expect(steps[0].GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_COMMIT))
		before := f.evidenceRows(ctx)

		// When CloseStepRun carries an exit-code value
		_, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:   lease,
			StepRunId: stepRun.GetId(),
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
				Value:      &millv1.Evidence_Exit{Exit: &harnessv1.ProcessExit{Status: 0}},
				VerifiedBy: "the check that should not have closed this",
			},
		}))

		// Then the request is rejected and the store has no evidence row
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(f.evidenceRows(ctx)).To(Equal(before), "nothing was written")

		detail, err := f.client().Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		Expect(detail.Msg.GetSteps()[1].GetStepRun().GetStatus()).
			To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING), "the attempt is still open")
		Expect(detail.Msg.GetSteps()[1].GetEvidence()).To(BeNil())
	})

	It("evidence of a kind the step does not name closes nothing", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#24")
		stepRun := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		before := f.evidenceRows(ctx)

		_, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:   lease,
			StepRunId: stepRun.GetId(),
			Evidence: &millv1.Evidence{
				Kind:  millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
				Value: &millv1.Evidence_Exit{Exit: &harnessv1.ProcessExit{Status: 0}},
			},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("commit"), "the refusal names the kind the step is closed by")
		Expect(f.evidenceRows(ctx)).To(Equal(before))
	})
})
