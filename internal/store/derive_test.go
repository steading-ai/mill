package store_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store"
	"github.com/steading-ai/mill/internal/testsupport"
)

// The statuses a latest step run can carry, by the ledger's text.
var stepRunStatuses = map[string]millv1.StepRunStatus{
	"running":    millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING,
	"closed":     millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED,
	"failed":     millv1.StepRunStatus_STEP_RUN_STATUS_FAILED,
	"aborted":    millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED,
	"superseded": millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED,
}

// fiveStepWorkflow is implement → simplify → commit → pr → review.
func fiveStepWorkflow() *millv1.Workflow {
	steps := make([]*millv1.Step, 0, len(fiveSteps))
	for _, name := range fiveSteps {
		steps = append(steps, &millv1.Step{Name: name})
	}
	return &millv1.Workflow{Name: "feature", Steps: steps}
}

// stateWith builds the latest attempt at each named step, in the order the
// names are given, with attempt numbers counting from one.
func stateWith(latest map[string]string) store.RunState {
	state := store.RunState{
		Run:    &millv1.Run{Id: "run", Status: millv1.RunStatus_RUN_STATUS_RUNNING},
		Latest: map[string]*millv1.StepRun{},
	}
	for name, status := range latest {
		state.Latest[name] = &millv1.StepRun{
			StepName: name,
			Attempt:  1,
			Status:   stepRunStatuses[status],
		}
	}
	return state
}

var _ = Describe("CurrentStep", func() {
	workflow := fiveStepWorkflow()

	DescribeTable("is the first step whose latest attempt is not closed",
		func(latest map[string]string, expected string) {
			name, _ := store.CurrentStep(stateWith(latest), workflow)
			Expect(name).To(Equal(expected))
		},
		Entry("a run that has attempted nothing is at its first step",
			map[string]string{}, "implement"),
		Entry("after the first step closes, the second",
			map[string]string{"implement": "closed"}, "simplify"),
		Entry("a running attempt is still the current step",
			map[string]string{"implement": "closed", "simplify": "running"}, "simplify"),
		Entry("a failed attempt is still the current step",
			map[string]string{"implement": "closed", "simplify": "failed"}, "simplify"),
		Entry("an aborted attempt is still the current step",
			map[string]string{"implement": "closed", "simplify": "aborted"}, "simplify"),
		Entry("a superseded attempt is still the current step",
			map[string]string{"implement": "closed", "simplify": "superseded"}, "simplify"),
		Entry("a superseded step earlier than a closed one wins",
			map[string]string{
				"implement": "superseded", "simplify": "closed", "commit": "closed",
				"pr": "closed", "review": "failed",
			}, "implement"),
		Entry("every step through pr closed and review failed is review",
			map[string]string{
				"implement": "closed", "simplify": "closed", "commit": "closed",
				"pr": "closed", "review": "failed",
			}, "review"),
		Entry("a gap in the middle is the first step with no attempt",
			map[string]string{"implement": "closed", "commit": "closed"}, "simplify"),
		Entry("every step closed leaves no current step",
			map[string]string{
				"implement": "closed", "simplify": "closed", "commit": "closed",
				"pr": "closed", "review": "closed",
			}, ""),
	)

	It("reports the attempts made against the current step's retries", func() {
		state := stateWith(map[string]string{"implement": "failed"})
		state.Latest["implement"].Attempt = 3
		name, attempts := store.CurrentStep(state, workflow)
		Expect(name).To(Equal("implement"))
		Expect(attempts).To(Equal(int64(3)))
	})

	It("has no current step while the run has no workflow", func() {
		name, attempts := store.CurrentStep(stateWith(map[string]string{}), nil)
		Expect(name).To(BeEmpty())
		Expect(attempts).To(BeZero())
	})
})

var _ = Describe("LastStep", func() {
	It("has no last step for a run with no attempts", func() {
		name, status := store.LastStep(stateWith(map[string]string{}))
		Expect(name).To(BeEmpty())
		Expect(status).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_UNSPECIFIED))
	})

	It("is the step whose latest attempt started most recently", func() {
		state := stateWith(map[string]string{"implement": "closed", "simplify": "failed"})
		state.Latest["implement"].StartedAt = timestamppb.New(time.Unix(100, 0))
		state.Latest["simplify"].StartedAt = timestamppb.New(time.Unix(200, 0))

		name, status := store.LastStep(state)
		Expect(name).To(Equal("simplify"))
		Expect(status).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
	})

	It("need not be the workflow's current step: an earlier step revised after a later one was already superseded", func() {
		state := stateWith(map[string]string{"implement": "failed", "simplify": "superseded"})
		state.Latest["simplify"].StartedAt = timestamppb.New(time.Unix(100, 0))
		state.Latest["implement"].StartedAt = timestamppb.New(time.Unix(200, 0))

		name, status := store.LastStep(state)
		Expect(name).To(Equal("implement"))
		Expect(status).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
	})
})

var _ = Describe("Revise", Label(testsupport.DBLabel), func() {
	It("Revise moves the current step backward", func(ctx SpecContext) {
		// Given a workflow implement → simplify → commit → pr → review
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#8")
		Expect(stepNames(workflow)).To(Equal(fiveSteps))

		// And every step through pr is closed and review has failed
		for _, name := range []string{"implement", "simplify", "commit", "pr"} {
			l.close(ctx, run.GetId(), name)
		}
		l.fail(ctx, run.GetId(), "review")
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("review"))

		// When SupersedeSteps(implement, review) is applied
		superseded, err := l.store.SupersedeSteps(ctx, l.lease, run.GetId(), "implement", "review")
		Expect(err).NotTo(HaveOccurred())
		Expect(namesOf(superseded)).To(ConsistOf(fiveSteps))

		// Then CurrentStep is implement
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("implement"))

		// And the evidence rows for simplify, commit and pr still exist
		for _, name := range []string{"simplify", "commit", "pr"} {
			var count int
			Expect(l.db.QueryRow(ctx, `
				SELECT count(*) FROM evidence e
				JOIN step_runs sr ON sr.id = e.step_run_id
				WHERE sr.run_id = $1 AND sr.step_name = $2`, run.GetId(), name).Scan(&count)).To(Succeed())
			Expect(count).To(Equal(1), "the evidence for %s must survive the revise", name)
		}
	})

	It("leaves the steps before the revise target closed", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#9")
		for _, name := range []string{"implement", "simplify", "commit", "pr"} {
			l.close(ctx, run.GetId(), name)
		}
		l.fail(ctx, run.GetId(), "review")

		_, err := l.store.SupersedeSteps(ctx, l.lease, run.GetId(), "commit", "review")
		Expect(err).NotTo(HaveOccurred())
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("commit"))
	})

	It("refuses a revise target the workflow does not contain", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#10")
		_, err := l.store.SupersedeSteps(ctx, l.lease, run.GetId(), "deploy", "review")
		Expect(err).To(MatchError(store.ErrUnknownStep))
	})
})

func stepNames(workflow *millv1.Workflow) []string {
	names := make([]string, 0, len(workflow.GetSteps()))
	for _, step := range workflow.GetSteps() {
		names = append(names, step.GetName())
	}
	return names
}

func namesOf(stepRuns []*millv1.StepRun) []string {
	names := make([]string, 0, len(stepRuns))
	for _, stepRun := range stepRuns {
		names = append(names, stepRun.GetStepName())
	}
	return names
}
