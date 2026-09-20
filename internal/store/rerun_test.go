package store_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("Rerun", Label(testsupport.DBLabel), func() {
	// Scenario: A parked run is rerun from the step it is parked on
	It("reruns from the step the run is parked on, a span of one", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#2")
		l.close(ctx, run.GetId(), "implement")
		l.close(ctx, run.GetId(), "simplify")
		failed := l.fail(ctx, run.GetId(), "commit")
		_, err := l.store.Park(ctx, l.lease, run.GetId(), "gate-1", millv1.ParkedReason_PARKED_REASON_RETRIES)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("commit"))

		result, err := l.store.Rerun(ctx, run.GetId(), "commit", "ivy", "the check had no PATH")

		Expect(err).NotTo(HaveOccurred())
		Expect(result.ThroughStep).To(Equal("commit"), "the span begins and ends on the step the run is on")
		Expect(result.Superseded).To(HaveLen(1))
		Expect(result.Superseded[0].GetId()).To(Equal(failed.GetId()))
		Expect(l.statusOf(ctx, failed.GetId())).To(Equal("superseded"))
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("commit"), "the run resumes at that step")
	}, SpecTimeout(time.Minute))

	It("reruns from an earlier step through the one the run is on", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, workflow := l.runWithWorkflow(ctx, "mill#3")
		l.close(ctx, run.GetId(), "implement")
		simplified := l.close(ctx, run.GetId(), "simplify")
		failed := l.fail(ctx, run.GetId(), "commit")
		_, err := l.store.Park(ctx, l.lease, run.GetId(), "gate-1", millv1.ParkedReason_PARKED_REASON_RETRIES)
		Expect(err).NotTo(HaveOccurred())

		result, err := l.store.Rerun(ctx, run.GetId(), "simplify", "ivy", "")

		Expect(err).NotTo(HaveOccurred())
		Expect(result.ThroughStep).To(Equal("commit"))
		Expect(result.Superseded).To(HaveLen(2))
		Expect(result.Superseded[0].GetId()).To(Equal(simplified.GetId()))
		Expect(result.Superseded[1].GetId()).To(Equal(failed.GetId()))
		Expect(l.currentStep(ctx, run.GetId(), workflow)).To(Equal("simplify"))
	}, SpecTimeout(time.Minute))
})
