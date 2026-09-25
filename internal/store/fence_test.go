package store_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store"
	"github.com/steading-ai/mill/internal/testsupport"
)

// lockRunRow takes the runs row lock on a connection of its own and holds
// it until the returned function is called, which commits. It is the lock
// half of the fence, run by hand, so a spec can put a transaction in the
// middle of the race the fence exists for.
func lockRunRow(ctx context.Context, l *ledger, runID string) (release func()) {
	GinkgoHelper()
	conn, err := l.db.Acquire(ctx)
	Expect(err).NotTo(HaveOccurred())
	tx, err := conn.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	var locked string
	Expect(tx.QueryRow(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&locked)).To(Succeed())
	return func() {
		Expect(tx.Commit(ctx)).To(Succeed())
		conn.Release()
	}
}

var _ = Describe("Lease fencing", Label(testsupport.DBLabel), func() {
	It("A stale worker's write is fenced out", func(ctx SpecContext) {
		// Given run R holds lease L1
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		// And connection B calls SetLease(R, L2) and commits
		_, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
		Expect(err).NotTo(HaveOccurred())

		// When connection A, still presenting L1, calls CloseStepRun
		_, _, err = l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(),
			Evidence:  commitEvidence("stale"),
		})

		// Then the call fails with ErrFenced and step_runs is unchanged
		Expect(err).To(MatchError(store.ErrFenced))
		Expect(l.statusOf(ctx, open.GetId())).To(Equal("running"))
		var evidence int
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE step_run_id = $1`, open.GetId()).
			Scan(&evidence)).To(Succeed())
		Expect(evidence).To(BeZero(), "a fenced-out write writes nothing")
	}, SpecTimeout(time.Minute))

	It("The re-read after the lock sees the concurrent claim", func(ctx SpecContext) {
		// Given connection A has taken the runs row lock for R presenting L1
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")
		commitA := lockRunRow(ctx, l, run.GetId())

		// And connection B's SetLease(R, L2) is waiting on that lock
		claimed := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
			claimed <- err
		}()
		Consistently(claimed, 300*time.Millisecond).ShouldNot(Receive(),
			"SetLease must wait on the row lock A holds")

		// When A commits without writing and B commits
		commitA()
		Eventually(claimed).Should(Receive(BeNil()))

		// And A begins a new transaction presenting L1
		_, _, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(),
			Evidence:  commitEvidence("stale"),
		})

		// Then A's write fails with ErrFenced
		Expect(err).To(MatchError(store.ErrFenced))
		Expect(l.statusOf(ctx, open.GetId())).To(Equal("running"))
	}, SpecTimeout(time.Minute))

	It("rejects a write whose lease a concurrent SetLease replaced while it waited for the lock", func(ctx SpecContext) {
		// The trap the fence's separate re-read exists for: A's transaction
		// begins before B's claim commits, so A's write is already in
		// flight when the run changes hands. A third connection holds the
		// row lock so that both queue behind it, B first.
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")
		release := lockRunRow(ctx, l, run.GetId())

		claimed := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := l.store.SetLease(ctx, run.GetId(), "L2", "worker-2")
			claimed <- err
		}()
		Consistently(claimed, 300*time.Millisecond).ShouldNot(Receive())

		closed := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, _, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
				StepRunID: open.GetId(),
				Evidence:  commitEvidence("stale"),
			})
			closed <- err
		}()
		Consistently(closed, 300*time.Millisecond).ShouldNot(Receive())

		release()
		Eventually(claimed).Should(Receive(BeNil()))
		Eventually(closed).Should(Receive(MatchError(store.ErrFenced)))

		Expect(l.statusOf(ctx, open.GetId())).To(Equal("running"))
		var evidence int
		Expect(l.db.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE step_run_id = $1`, open.GetId()).
			Scan(&evidence)).To(Succeed())
		Expect(evidence).To(BeZero())
	}, SpecTimeout(time.Minute))

	It("admits the holder's write and rejects a write presenting no lease at all", func(ctx SpecContext) {
		l := newLedger(ctx)
		run, _ := l.runWithWorkflow(ctx, "mill#8")
		open := l.start(ctx, run.GetId(), "implement")

		_, err := l.store.StartStepRun(ctx, "", store.StartStepRunParams{
			RunID: run.GetId(), StepName: "simplify", Kind: millv1.StepKind_STEP_KIND_SKILL,
		})
		Expect(err).To(MatchError(store.ErrFenced))

		closed, _, err := l.store.CloseStepRun(ctx, l.lease, store.CloseStepRunParams{
			StepRunID: open.GetId(),
			Evidence:  commitEvidence("sha"),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(closed.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	}, SpecTimeout(time.Minute))

	It("reports a run it has never heard of as not found rather than fenced", func(ctx SpecContext) {
		l := newLedger(ctx)
		_, err := l.store.StartStepRun(ctx, l.lease, store.StartStepRunParams{
			RunID: "01J0000000000000000000000X", StepName: "implement",
			Kind: millv1.StepKind_STEP_KIND_SKILL,
		})
		Expect(err).To(MatchError(store.ErrNotFound))
	}, SpecTimeout(time.Minute))
})
