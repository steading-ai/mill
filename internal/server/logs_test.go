package server_test

import (
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// seq sets a fixture record's seq: AppendActivity writes exactly what it is
// given, unlike the real writer (internal/run/activity.go's append), which
// assigns seq itself, so a fixture that means to exercise seq order has to
// assign it by hand.
func seq(a *millv1.Activity, n int64) *millv1.Activity {
	a.Seq = n
	return a
}

// at sets a fixture record's at, for the specs that exercise since and the
// every-run merge order — the real writer stamps it on every append, but
// AppendActivity does not.
func at(a *millv1.Activity, t time.Time) *millv1.Activity {
	a.At = timestamppb.New(t)
	return a
}

// drain reads a Logs stream to the end, failing the spec on any transport
// error or a RunEnd frame — a single-run, unfollowed read never sends one
// (docs/activity.md §The RPC: RunEnd is what ends a followed stream).
func drain(stream *connect.ServerStreamForClient[millv1.LogsResponse]) []*millv1.Activity {
	GinkgoHelper()
	var out []*millv1.Activity
	for stream.Receive() {
		frame := stream.Msg()
		Expect(frame.GetRun()).To(BeNil(), "an unfollowed read over one run never ends with RunEnd")
		out = append(out, frame.GetActivity())
	}
	Expect(stream.Err()).NotTo(HaveOccurred())
	return out
}

var _ = Describe("Logs", Label(testsupport.DBLabel), func() {
	It("returns two attempts' records in seq order, across attempts, with boundaries in-band", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#101")

		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "implement", Attempt: first.GetAttempt()}}}, 1),
			seq(testsupport.TextActivity("first attempt"), 2),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_FAILED}}}, 3),
		)
		// A running attempt blocks a second one at the same step
		// (step_runs_one_running_idx), so the first has to fail before the
		// retry that is this spec's second attempt can start.
		_, err := f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId: lease, StepRunId: first.GetId(), Exit: &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())

		second := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		Expect(second.GetAttempt()).To(BeNumerically(">", first.GetAttempt()), "a second attempt at the same step")
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", second.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "implement", Attempt: second.GetAttempt()}}}, 1),
			seq(testsupport.TextActivity("second attempt"), 2),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement"}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(stream)

		Expect(activities).To(HaveLen(5))
		for _, a := range activities[:3] {
			Expect(a.GetStepRunId()).To(Equal(first.GetId()))
		}
		for _, a := range activities[3:] {
			Expect(a.GetStepRunId()).To(Equal(second.GetId()))
		}
		Expect(activities[0].GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptStarted{}), "the first attempt's boundary opens the fold")
		Expect(activities[2].GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptEnded{}), "the first attempt's boundary closes it")
		Expect(activities[3].GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptStarted{}), "the second attempt opens its own fold")
		Expect(activities).To(HaveEach(WithTransform(func(a *millv1.Activity) int64 { return a.GetSeq() }, BeNumerically(">", 0))))
	})

	It("narrows to one attempt with step and attempt", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#102")

		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(testsupport.TextActivity("first attempt"), 1))
		_, err := f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId: lease, StepRunId: first.GetId(), Exit: &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())

		second := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", second.GetAttempt()),
			seq(testsupport.TextActivity("second attempt"), 1))

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{
			Id: f.run.GetId(), Step: "implement", Attempt: first.GetAttempt(),
		}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(stream)

		Expect(activities).To(HaveLen(1))
		Expect(activities[0].GetStepRunId()).To(Equal(first.GetId()))
		Expect(activities[0].GetText().GetText()).To(Equal("first attempt"))
	})

	It("tail returns the last N records", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#103")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()),
			seq(testsupport.TextActivity("one"), 1),
			seq(testsupport.TextActivity("two"), 2),
			seq(testsupport.TextActivity("three"), 3),
			seq(testsupport.TextActivity("four"), 4),
			seq(testsupport.TextActivity("five"), 5),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement", Tail: 2}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(stream)

		Expect(activities).To(HaveLen(2))
		Expect(activities[0].GetText().GetText()).To(Equal("four"))
		Expect(activities[1].GetText().GetText()).To(Equal("five"))
	})

	It("after skips through a cursor", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#104")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()),
			seq(testsupport.TextActivity("one"), 1),
			seq(testsupport.TextActivity("two"), 2),
			seq(testsupport.TextActivity("three"), 3),
		)

		full, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement"}))
		Expect(err).NotTo(HaveOccurred())
		seen := drain(full)
		Expect(seen).To(HaveLen(3))
		cursor := &millv1.Cursor{StepRunId: seen[0].GetStepRunId(), Seq: seen[0].GetSeq()}

		resumed, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement", After: cursor}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(resumed)

		Expect(activities).To(HaveLen(2))
		Expect(activities[0].GetText().GetText()).To(Equal("two"))
		Expect(activities[1].GetText().GetText()).To(Equal("three"))
	})

	It("before with limit pages backward from a cursor in seq order", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#105")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		var records []*millv1.Activity
		for i := int64(1); i <= 25; i++ {
			records = append(records, seq(testsupport.TextActivity(fmt.Sprintf("record %d", i)), i))
		}
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()), records...)

		full, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement"}))
		Expect(err).NotTo(HaveOccurred())
		seen := drain(full)
		Expect(seen).To(HaveLen(25))
		cursor := &millv1.Cursor{StepRunId: seen[24].GetStepRunId(), Seq: seen[24].GetSeq()}

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{
			Id: f.run.GetId(), Step: "implement", Before: cursor, Limit: 20,
		}))
		Expect(err).NotTo(HaveOccurred())
		page := drain(stream)

		Expect(page).To(HaveLen(20))
		Expect(page[0].GetText().GetText()).To(Equal("record 5"))
		Expect(page[19].GetText().GetText()).To(Equal("record 24"))
	})

	It("before is refused together with follow", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#109")
		_ = f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{
			Id: f.run.GetId(), Step: "implement", Follow: true, Before: &millv1.Cursor{StepRunId: "x", Seq: 1},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeFalse())
		Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeInvalidArgument))
	})

	It("after with limit 1 fetches a cut body whole, since an unfollowed read never truncates", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#106")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		big := strings.Repeat("x", 70*1024)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()),
			seq(testsupport.TextActivity("before"), 1),
			seq(testsupport.TextActivity(big), 2),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{
			Id: f.run.GetId(), Step: "implement",
			After: &millv1.Cursor{StepRunId: sr.GetId(), Seq: 1}, Limit: 1,
		}))
		Expect(err).NotTo(HaveOccurred())
		page := drain(stream)

		Expect(page).To(HaveLen(1))
		Expect(page[0].GetTruncated()).To(BeFalse())
		Expect(page[0].GetText().GetText()).To(Equal(big))
	})

	It("every run merges records across runs, since-ordered", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#107")
		g := f.anotherRun(ctx, "mill#108")

		srF := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		srG := g.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		base := time.Now().UTC()
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", srF.GetAttempt()),
			at(seq(testsupport.TextActivity("f-one"), 1), base),
			at(seq(testsupport.TextActivity("f-three"), 2), base.Add(2*time.Second)),
		)
		testsupport.AppendActivity(run.ActivityPath(g.logDir, g.run.GetId(), "implement", srG.GetAttempt()),
			at(seq(testsupport.TextActivity("g-two"), 1), base.Add(1*time.Second)),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Since: timestamppb.New(base)}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(stream)

		Expect(activities).To(HaveLen(3))
		Expect(activities[0].GetText().GetText()).To(Equal("f-one"))
		Expect(activities[1].GetText().GetText()).To(Equal("g-two"))
		Expect(activities[2].GetText().GetText()).To(Equal("f-three"))
	})

	It("returns byte-identical records for a closed attempt whether read live or from its sealed frame", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#118")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()),
			seq(testsupport.TextActivity("one"), 1),
			seq(&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Turn: 1,
				Body: &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "read"}}}, 2),
			seq(testsupport.TextActivity("two"), 3),
		)

		live, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement"}))
		Expect(err).NotTo(HaveOccurred())
		beforeSeal := drain(live)
		Expect(beforeSeal).To(HaveLen(3))

		Expect(run.SealAttempt(ctx, f.logDir, f.run.GetId(), "implement", sr.GetAttempt(), sr.GetId(), "", f.client(), nil)).To(Succeed())

		sealed, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement"}))
		Expect(err).NotTo(HaveOccurred())
		afterSeal := drain(sealed)

		Expect(afterSeal).To(HaveLen(3))
		for i := range beforeSeal {
			Expect(proto.Equal(afterSeal[i], beforeSeal[i])).To(BeTrue(), "record %d: sealed read must match the live read it replaces", i)
		}
	})

	It("every run interleaves a sealed run's records with a still-live run's, by at", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#119")
		g := f.anotherRun(ctx, "mill#120")

		srF := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		srG := g.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		base := time.Now().UTC()
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", srF.GetAttempt()),
			at(seq(testsupport.TextActivity("f-one"), 1), base),
		)
		testsupport.AppendActivity(run.ActivityPath(g.logDir, g.run.GetId(), "implement", srG.GetAttempt()),
			at(seq(testsupport.TextActivity("g-two"), 1), base.Add(time.Second)),
		)

		// f's attempt seals — its record now comes from the index — while
		// g's stays live, so the merge has to draw from both sources.
		Expect(run.SealAttempt(ctx, f.logDir, f.run.GetId(), "implement", srF.GetAttempt(), srF.GetId(), "", f.client(), nil)).To(Succeed())

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Since: timestamppb.New(base)}))
		Expect(err).NotTo(HaveOccurred())
		activities := drain(stream)

		Expect(activities).To(HaveLen(2))
		Expect(activities[0].GetText().GetText()).To(Equal("f-one"))
		Expect(activities[1].GetText().GetText()).To(Equal("g-two"))
	})

	It("seals a frame under a fifth of its live file's size", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#122")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		path := run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt())

		args, err := structpb.NewStruct(map[string]any{
			"path":    "internal/server/logs.go",
			"pattern": "readEveryRunActivities",
		})
		Expect(err).NotTo(HaveOccurred())
		records := make([]*millv1.Activity, 0, 300)
		for i := int64(1); i <= 300; i++ {
			records = append(records, seq(&millv1.Activity{
				Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Turn: 1,
				Body: &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: fmt.Sprintf("call-%d", i), Name: "grep", Arguments: args}},
			}, i))
		}
		testsupport.AppendActivity(path, records...)

		liveInfo, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())

		Expect(run.SealAttempt(ctx, f.logDir, f.run.GetId(), "implement", sr.GetAttempt(), sr.GetId(), "", f.client(), nil)).To(Succeed())

		frameInfo, err := os.Stat(run.FramePath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()))
		Expect(err).NotTo(HaveOccurred())
		Expect(frameInfo.Size()).To(BeNumerically("<", liveInfo.Size()/5))
	})

	It("limit is refused together with follow", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#117")
		_ = f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{
			Id: f.run.GetId(), Step: "implement", Follow: true, Limit: 1,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeFalse())
		Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeInvalidArgument))
	})

	It("follows a run across a step boundary, in-band, without ending", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#110", time.Second)
		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "implement", Attempt: first.GetAttempt()}}}, 1),
			seq(testsupport.TextActivity("working"), 2),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Follow: true}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })

		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetActivity().GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptStarted{}))
		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetActivity().GetText().GetText()).To(Equal("working"))

		// Close the attempt: its own boundary is appended to the file first,
		// then CloseStepRun — the ledger write that signals the observer —
		// so the wake that follows sees the boundary already on record.
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED}}}, 3),
		)
		_, err = f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: first.GetId(), Evidence: commitEvidence(), WorkflowId: f.workflow.GetId(), Tier: millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetActivity().GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptEnded{}), "implement's own boundary closes its fold")

		// Start the next step. Its own attempt_started is written before
		// StartStepRun, the write that signals: this run's follow (step
		// unset) must keep going past the step boundary rather than ending
		// merely because implement's attempt left running.
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "review", 1),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "review", Attempt: 1}}}, 1),
		)
		second := f.start(ctx, "review", millv1.StepKind_STEP_KIND_SKILL)
		Expect(second.GetAttempt()).To(Equal(int64(1)))

		Expect(stream.Receive()).To(BeTrue())
		next := stream.Msg().GetActivity()
		Expect(next.GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptStarted{}))
		Expect(next.GetStepRunId()).To(Equal(second.GetId()), "the stream kept going past the step boundary")
	})

	It("step with follow ends on that step's own attempt_ended, not an earlier attempt's", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#113", time.Second)
		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(testsupport.TextActivity("first attempt"), 1),
		)
		_, err := f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId: lease, StepRunId: first.GetId(), Exit: &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_FAILED}}}, 2),
		)

		second := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", second.GetAttempt()),
			seq(testsupport.TextActivity("second attempt"), 1),
		)

		// Everything above is on record before the follow opens, so the
		// catch-up batch alone carries the first attempt's attempt_ended —
		// the naive reading of "step with follow ends on attempt_ended"
		// would stop right there. It must not: the second attempt is still
		// running.
		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement", Follow: true}))
		Expect(err).NotTo(HaveOccurred())

		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", second.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED}}}, 2),
		)
		_, err = f.client().CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: second.GetId(), Evidence: commitEvidence(), WorkflowId: f.workflow.GetId(), Tier: millv1.Tier_TIER_FEATURE,
		}))
		Expect(err).NotTo(HaveOccurred())

		var bodies []*millv1.Activity
		var ended bool
		for stream.Receive() {
			frame := stream.Msg()
			if frame.GetRun() != nil {
				ended = true
				continue
			}
			bodies = append(bodies, frame.GetActivity())
		}
		Expect(stream.Err()).NotTo(HaveOccurred())
		Expect(ended).To(BeFalse(), "step-scoped follow ends silently, with no RunEnd")

		Expect(bodies).To(HaveLen(4))
		Expect(bodies[0].GetStepRunId()).To(Equal(first.GetId()))
		Expect(bodies[1].GetStepRunId()).To(Equal(first.GetId()))
		Expect(bodies[1].GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptEnded{}))
		Expect(bodies[2].GetStepRunId()).To(Equal(second.GetId()))
		Expect(bodies[3].GetStepRunId()).To(Equal(second.GetId()))
		Expect(bodies[3].GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptEnded{}), "the second, still-running attempt's own boundary ends the stream")
	})

	It("ends a followed run with RunEnd carrying the gate it waits on", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#111", time.Second)
		gate := f.start(ctx, "approve", millv1.StepKind_STEP_KIND_GATE)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "approve", gate.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_GateFiled{GateFiled: &millv1.GateFiled{
				Kind: "merge", NodeId: "gate-node-1", NodeRef: "mill#111/approve", Title: "Approve mill#111",
			}}}, 1),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Follow: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeTrue())
		Expect(stream.Msg().GetActivity().GetGateFiled()).NotTo(BeNil())

		_, err = f.client().MarkWaiting(ctx, connect.NewRequest(&millv1.MarkWaitingRequest{
			LeaseId: lease, RunId: f.run.GetId(), GateNodeId: "gate-node-1", StepRunId: gate.GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		end := stream.Msg().GetRun()
		Expect(end).NotTo(BeNil())
		Expect(end.GetRunId()).To(Equal(f.run.GetId()))
		Expect(end.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
		Expect(end.GetGate().GetNodeId()).To(Equal("gate-node-1"))
		Expect(end.GetGate().GetNodeRef()).To(Equal("mill#111/approve"))
		Expect(stream.Receive()).To(BeFalse(), "RunEnd is the last frame of a followed stream over one run")
	})

	It("ends a followed run with RunEnd carrying the gate it parks on", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#112", time.Second)
		gate := f.start(ctx, "approve", millv1.StepKind_STEP_KIND_GATE)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "approve", gate.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_GateFiled{GateFiled: &millv1.GateFiled{
				Kind: "park", NodeId: "gate-node-2", NodeRef: "mill#112/approve",
			}}}, 1),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Follow: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeTrue())

		_, err = f.client().Park(ctx, connect.NewRequest(&millv1.ParkRequest{
			LeaseId: lease, RunId: f.run.GetId(), GateNodeId: "gate-node-2", Reason: millv1.ParkedReason_PARKED_REASON_RETRIES,
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		end := stream.Msg().GetRun()
		Expect(end).NotTo(BeNil())
		Expect(end.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(end.GetGate().GetNodeId()).To(Equal("gate-node-2"))
	})

	It("wakes on signal well before the fallback elapses", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#114", 2*time.Second)
		first := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(testsupport.TextActivity("working"), 1),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement", Follow: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Receive()).To(BeTrue())

		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", first.GetAttempt()),
			seq(&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_FAILED}}}, 2),
		)
		woke := time.Now()
		_, err = f.client().FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
			LeaseId: lease, StepRunId: first.GetId(), Exit: &harnessv1.ProcessExit{Status: 1},
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(stream.Receive()).To(BeTrue())
		Expect(time.Since(woke)).To(BeNumerically("<", time.Second), "the signal, not the 2s fallback, woke the stream")
		Expect(stream.Msg().GetActivity().GetBody()).To(BeAssignableToTypeOf(&millv1.Activity_AttemptEnded{}))
	})

	It("cuts a body over 64 KiB while following, with truncated set", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#115")
		sr := f.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		big := strings.Repeat("x", 70*1024)
		testsupport.AppendActivity(run.ActivityPath(f.logDir, f.run.GetId(), "implement", sr.GetAttempt()),
			seq(testsupport.TextActivity(big), 1),
		)

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Id: f.run.GetId(), Step: "implement", Follow: true}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })

		Expect(stream.Receive()).To(BeTrue())
		frame := stream.Msg().GetActivity()
		Expect(frame.GetTruncated()).To(BeTrue())
		Expect(frame.GetText().GetText()).To(BeEmpty())
	})

	It("every run follow streams RunEnd for a run that finishes, without ending the stream", func(ctx SpecContext) {
		f := newFixtureFallback(ctx, "mill#116", 300*time.Millisecond)

		// Nothing is on record at or after since, so the stream's very
		// first byte to the client is the RunEnd frame Done below causes —
		// the client call below does not return until then (a
		// server-streaming response's headers go out on its first write).
		// Done is issued from its own goroutine so it can race that first
		// write instead of waiting behind it.
		go func() {
			defer GinkgoRecover()
			_, err := f.client().Done(ctx, connect.NewRequest(&millv1.DoneRequest{LeaseId: lease, RunId: f.run.GetId()}))
			Expect(err).NotTo(HaveOccurred())
		}()

		stream, err := f.client().Logs(ctx, connect.NewRequest(&millv1.LogsRequest{Follow: true, Since: timestamppb.New(time.Now().UTC())}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stream.Close() })

		Expect(stream.Receive()).To(BeTrue())
		end := stream.Msg().GetRun()
		Expect(end).NotTo(BeNil())
		Expect(end.GetRunId()).To(Equal(f.run.GetId()))
		Expect(end.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
	})
})
