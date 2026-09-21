package store_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
	"github.com/ivy/mill/internal/testsupport"
)

// indexRow builds the row seal.go itself would, for a step run at a given
// seq, so specs read close to what a real seal actually sends.
func indexRow(stepRunID string, seq int64, at time.Time) *millv1.ActivityIndexRow {
	return &millv1.ActivityIndexRow{
		StepRunId: stepRunID,
		Seq:       seq,
		At:        timestamppb.New(at),
		Kind:      "text",
		Source:    millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
		Offset:    seq * 100,
	}
}

var _ = Describe("Activity", Label(testsupport.DBLabel), func() {
	It("SealAttempt inserts rows CountActivityForStepRun and ListActivityForStepRun both see", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		attempt := l.start(ctx, run.GetId(), "implement")
		now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)

		sealed, err := l.store.SealAttempt(ctx, attempt.GetId(), []*millv1.ActivityIndexRow{
			indexRow(attempt.GetId(), 0, now),
			indexRow(attempt.GetId(), 1, now.Add(time.Second)),
			indexRow(attempt.GetId(), 2, now.Add(2*time.Second)),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(sealed).To(HaveLen(3))

		count, err := store.New(l.db).CountActivityForStepRun(ctx, attempt.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int64(3)))

		listed, err := l.store.ActivityForStepRun(ctx, attempt.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(HaveLen(3))
		Expect(listed[0].GetSeq()).To(Equal(int64(0)))
		Expect(listed[1].GetSeq()).To(Equal(int64(1)))
		Expect(listed[2].GetSeq()).To(Equal(int64(2)), "ListActivityForStepRun orders by seq")
	}, SpecTimeout(time.Minute))

	It("SealAttempt retried after a lost response does not double-count", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		attempt := l.start(ctx, run.GetId(), "implement")
		now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
		rows := []*millv1.ActivityIndexRow{indexRow(attempt.GetId(), 0, now), indexRow(attempt.GetId(), 1, now.Add(time.Second))}

		first, err := l.store.SealAttempt(ctx, attempt.GetId(), rows)
		Expect(err).NotTo(HaveOccurred())
		Expect(first).To(HaveLen(2))

		// The worker doesn't know whether its first call landed, so it
		// retries with the same rows.
		retried, err := l.store.SealAttempt(ctx, attempt.GetId(), rows)
		Expect(err).NotTo(HaveOccurred())
		Expect(retried).To(BeEmpty(), "every seq is already recorded, so ON CONFLICT DO NOTHING skips all of them")

		count, err := store.New(l.db).CountActivityForStepRun(ctx, attempt.GetId())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int64(2)))
	}, SpecTimeout(time.Minute))

	It("ActivitySince orders two step runs' rows by at, across runs", func(ctx SpecContext) {
		l := newLedger(ctx)
		firstRun, _ := l.runWithWorkflow(ctx, "mill#8")
		secondRun, _ := l.runWithWorkflow(ctx, "mill#9")
		firstAttempt := l.start(ctx, firstRun.GetId(), "implement")
		secondAttempt := l.start(ctx, secondRun.GetId(), "implement")
		now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)

		_, err := l.store.SealAttempt(ctx, secondAttempt.GetId(), []*millv1.ActivityIndexRow{
			indexRow(secondAttempt.GetId(), 0, now.Add(2*time.Second)),
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = l.store.SealAttempt(ctx, firstAttempt.GetId(), []*millv1.ActivityIndexRow{
			indexRow(firstAttempt.GetId(), 0, now),
			indexRow(firstAttempt.GetId(), 1, now.Add(time.Second)),
		})
		Expect(err).NotTo(HaveOccurred())

		since, err := l.store.ActivitySince(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(since).To(HaveLen(3))
		Expect(since[0].GetStepRunId()).To(Equal(firstAttempt.GetId()))
		Expect(since[1].GetStepRunId()).To(Equal(firstAttempt.GetId()))
		Expect(since[2].GetStepRunId()).To(Equal(secondAttempt.GetId()), "the row with the latest at sorts last")

		cutoff := now.Add(500 * time.Millisecond)
		since, err = l.store.ActivitySince(ctx, &cutoff)
		Expect(err).NotTo(HaveOccurred())
		Expect(since).To(HaveLen(2), "the window excludes the row recorded before the cutoff")
	}, SpecTimeout(time.Minute))
})
