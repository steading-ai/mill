package domain_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/domain"
)

// drawn is the state diagram of docs/domain-model.md §"Run states", written
// out a second time and independently of the table the domain applies, so
// that a spec compares the code against the document rather than against
// itself.
var drawn = map[millv1.RunStatus][]millv1.RunStatus{
	millv1.RunStatus_RUN_STATUS_SELECTING: {
		millv1.RunStatus_RUN_STATUS_RUNNING,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
	},
	millv1.RunStatus_RUN_STATUS_RUNNING: {
		millv1.RunStatus_RUN_STATUS_DONE,
		millv1.RunStatus_RUN_STATUS_WAITING,
		millv1.RunStatus_RUN_STATUS_PARKED,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
	},
	millv1.RunStatus_RUN_STATUS_WAITING: {
		millv1.RunStatus_RUN_STATUS_RUNNING,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
	},
	millv1.RunStatus_RUN_STATUS_PARKED: {
		millv1.RunStatus_RUN_STATUS_RUNNING,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
	},
}

func isDrawn(from, to millv1.RunStatus) bool {
	for _, edge := range drawn[from] {
		if edge == to {
			return true
		}
	}
	return false
}

var _ = Describe("The run state diagram", Label("unit"), func() {
	It("allows every transition the diagram draws and refuses every one it does not", func() {
		for _, from := range domain.RunStatuses() {
			for _, to := range domain.RunStatuses() {
				err := domain.CheckRunTransition(from, to)
				if isDrawn(from, to) {
					Expect(err).NotTo(HaveOccurred(),
						"%s → %s is drawn in docs/domain-model.md and must be allowed",
						domain.RunStatusName(from), domain.RunStatusName(to))
					continue
				}
				Expect(err).To(HaveOccurred(),
					"%s → %s is not drawn in docs/domain-model.md and must be refused",
					domain.RunStatusName(from), domain.RunStatusName(to))
				Expect(err.Error()).To(ContainSubstring(domain.RunStatusName(from)), "the refusal names the state it found")
				Expect(err.Error()).To(ContainSubstring(domain.RunStatusName(to)), "the refusal names the state it was asked for")
			}
		}
	})

	It("refuses a move out of a terminal state", func() {
		for _, from := range []millv1.RunStatus{
			millv1.RunStatus_RUN_STATUS_DONE,
			millv1.RunStatus_RUN_STATUS_FAILED,
			millv1.RunStatus_RUN_STATUS_ABANDONED,
		} {
			for _, to := range domain.RunStatuses() {
				Expect(domain.CheckRunTransition(from, to)).To(HaveOccurred())
			}
		}
	})

	It("names both states in the refusal", func() {
		err := domain.CheckRunTransition(millv1.RunStatus_RUN_STATUS_DONE, millv1.RunStatus_RUN_STATUS_WAITING)
		Expect(err).To(MatchError(&domain.RunTransitionError{
			From: millv1.RunStatus_RUN_STATUS_DONE,
			To:   millv1.RunStatus_RUN_STATUS_WAITING,
		}))
		Expect(err.Error()).To(ContainSubstring("done"))
		Expect(err.Error()).To(ContainSubstring("waiting"))
	})
})

var _ = Describe("The step run state diagram", Label("unit"), func() {
	running := millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING
	closed := millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED
	failed := millv1.StepRunStatus_STEP_RUN_STATUS_FAILED
	aborted := millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED
	superseded := millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED

	DescribeTable("an attempt ends once, and a revise supersedes what it ended as",
		func(from, to millv1.StepRunStatus, allowed bool) {
			err := domain.CheckStepRunTransition(from, to)
			if allowed {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(domain.StepRunStatusName(from)))
			Expect(err.Error()).To(ContainSubstring(domain.StepRunStatusName(to)))
		},
		Entry("running closes on evidence", running, closed, true),
		Entry("running fails", running, failed, true),
		Entry("running is aborted", running, aborted, true),
		Entry("a closed attempt is superseded by a revise", closed, superseded, true),
		Entry("a failed attempt is superseded by a revise", failed, superseded, true),
		Entry("an aborted attempt is superseded by a revise", aborted, superseded, true),
		Entry("a closed attempt does not close again", closed, closed, false),
		Entry("a closed attempt does not fail", closed, failed, false),
		Entry("a failed attempt does not close", failed, closed, false),
		Entry("an aborted attempt does not close", aborted, closed, false),
		Entry("a superseded attempt moves no further", superseded, closed, false),
		Entry("a superseded attempt is not superseded twice", superseded, superseded, false),
	)
})
