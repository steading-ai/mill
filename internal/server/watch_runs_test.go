package server_test

import (
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/server"
	"github.com/steading-ai/mill/internal/testsupport"
)

// followFallback is short enough that an every-run WatchRuns stream, whose
// wake between events relies on the observer's fallback timer rather than a
// signal (only CreateRun signals the empty key; see Observer), notices what
// a spec just wrote well inside a spec's own timeout.
const followFallback = 300 * time.Millisecond

var _ = Describe("WatchRuns", Label(testsupport.DBLabel), func() {
	It("a fresh subscription's first message carries the opening position alone", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#210")

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })

		Expect(stream.Receive()).To(BeTrue())
		opening := stream.Msg()
		Expect(opening.GetCursor()).NotTo(BeEmpty())
		Expect(opening.GetEvent()).To(BeNil())
	})

	It("an event written during a subscription arrives once", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#211", followFallback)
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })
		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetEvent()).To(BeNil())

		closed, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:    lease,
			StepRunId:  sr.GetId(),
			Evidence:   commitEvidence(),
			WorkflowId: f.workflow.GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		event := stream.Msg().GetEvent()
		Expect(event.GetRunId()).To(Equal(f.run.GetId()))
		Expect(event.GetNodeRef()).To(Equal("mill#211"))
		Expect(event.GetStepRunId()).To(Equal(closed.Msg.GetStepRun().GetId()))
		Expect(event.GetOp()).To(Equal("close_step_run"))
		Expect(event.GetOccurredAt().AsTime()).NotTo(BeZero())
	})

	It("a subscriber reconnecting from its last cursor sees no gap and no duplicate", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#212", followFallback)

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeTrue())

		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		_, err = f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:    lease,
			StepRunId:  first.GetId(),
			Evidence:   commitEvidence(),
			WorkflowId: f.workflow.GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetEvent().GetOp()).To(Equal("start_step_run"))

		Expect(stream.Receive()).To(BeTrue())
		closedEvent := stream.Msg()
		Expect(closedEvent.GetEvent().GetStepRunId()).To(Equal(first.GetId()))
		Expect(closedEvent.GetEvent().GetOp()).To(Equal("close_step_run"))
		lastCursor := closedEvent.GetCursor()
		Expect(stream.Close()).To(Succeed())

		second := f.start(ctx, "review", millv1.StepKind_STEP_KIND_SKILL)
		_, err = f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId:   lease,
			StepRunId: second.GetId(),
			Exit:      &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())

		resumed, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{Cursor: lastCursor}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = resumed.Close() })

		Expect(resumed.Receive()).To(BeTrue())
		started := resumed.Msg().GetEvent()
		Expect(started).NotTo(BeNil(), "a presented cursor opens straight into events, no opening-position frame")
		Expect(started.GetStepRunId()).To(Equal(second.GetId()), "resumes after the last cursor: no repeat of the first event")
		Expect(started.GetOp()).To(Equal("start_step_run"))

		Expect(resumed.Receive()).To(BeTrue())
		failed := resumed.Msg().GetEvent()
		Expect(failed.GetStepRunId()).To(Equal(second.GetId()))
		Expect(failed.GetOp()).To(Equal("fail_step_run"))
	})

	It("refuses a cursor older than the configured window with FailedPrecondition naming the window", func(ctx SpecContext) {
		window := 50 * time.Millisecond
		svc := newServiceWith(ctx, server.DefaultObserverFallback, server.WithWatchRunsWindow(window))
		f := newFixtureOn(ctx, svc, "mill#213")

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeTrue())
		cursor := stream.Msg().GetCursor()
		Expect(stream.Close()).To(Succeed())

		time.Sleep(3 * window)

		stale, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{Cursor: cursor}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stale.Receive()).To(BeFalse())
		Expect(connect.CodeOf(stale.Err())).To(Equal(connect.CodeFailedPrecondition))
		Expect(stale.Err().Error()).To(ContainSubstring(window.String()))
	})

	It("filters to the runs of the given project", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "millA#1", followFallback)
		g := f.anotherRun(ctx, "millB#1")

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{Project: "millA"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })
		Expect(stream.Receive()).To(BeTrue())

		gStep := g.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		_, err = g.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:    lease,
			StepRunId:  gStep.GetId(),
			Evidence:   commitEvidence(),
			WorkflowId: g.workflow.GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		fStep := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		_, err = f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:    lease,
			StepRunId:  fStep.GetId(),
			Evidence:   commitEvidence(),
			WorkflowId: f.workflow.GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		started := stream.Msg().GetEvent()
		Expect(started.GetStepRunId()).To(Equal(fStep.GetId()), "millB's events are filtered out; millA's are the first ones seen")
		Expect(started.GetOp()).To(Equal("start_step_run"))

		Expect(stream.Receive()).To(BeTrue())
		closedEvent := stream.Msg().GetEvent()
		Expect(closedEvent.GetStepRunId()).To(Equal(fStep.GetId()))
		Expect(closedEvent.GetOp()).To(Equal("close_step_run"))
	})

	It("sends a cursor-only keepalive when the stream is otherwise idle", func(ctx SpecContext) {
		keepalive := 200 * time.Millisecond
		svc := newServiceWith(ctx, 30*time.Millisecond, server.WithWatchRunsKeepalive(keepalive))
		f := newFixtureOn(ctx, svc, "mill#214")

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })
		Expect(stream.Receive()).To(BeTrue())
		opened := time.Now()

		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetEvent()).To(BeNil(), "a keepalive carries no event")
		Expect(time.Since(opened)).To(BeNumerically(">=", keepalive))
	})

	// Scenario: No gap between the opening position and the first fetch
	//   Given a subscriber has the opening cursor from a fresh WatchRuns call
	//   When a step closes before the subscriber calls Status
	//   Then the close arrives as an event after the opening cursor
	It("leaves no gap between the opening position and the first fetch", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#215", followFallback)
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		stream, err := f.client().WatchRuns(ctx, connect.NewRequest(&millv1.WatchRunsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })
		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetCursor()).NotTo(BeEmpty())

		closed, err := f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:    lease,
			StepRunId:  sr.GetId(),
			Evidence:   commitEvidence(),
			WorkflowId: f.workflow.GetId(),
			Tier:       millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.client().Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		event := stream.Msg().GetEvent()
		Expect(event.GetStepRunId()).To(Equal(closed.Msg.GetStepRun().GetId()), "the step's close arrives as an event after the opening cursor")
		Expect(event.GetOp()).To(Equal("close_step_run"))
	})
})
