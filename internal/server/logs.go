package server

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/run"
)

// truncatedLimit is how large a followed record's body may be before Logs
// cuts it. docs/activity.md: "A body over 64 KiB is cut in a followed
// stream with truncated set; unfollowed reads get bodies whole."
const truncatedLimit = 64 * 1024

// liveRunStatus is every status a run can hold while one of its step runs
// may still be running and therefore not yet sealed. A run that has gone
// done, failed or abandoned has no attempt still writing: whatever it
// closed either sealed already (and is in the index) or is the same
// accepted seal-failure gap SealOrphans (internal/run/recover.go) exists to
// close, not one this read grows a live-file fallback for.
var liveRunStatus = []millv1.RunStatus{
	millv1.RunStatus_RUN_STATUS_SELECTING,
	millv1.RunStatus_RUN_STATUS_RUNNING,
	millv1.RunStatus_RUN_STATUS_WAITING,
	millv1.RunStatus_RUN_STATUS_PARKED,
}

// Logs streams a run's activity, oldest first. With id or node_ref: every
// attempt of the narrowed step (or every step) in start order. With
// neither: every run's records at or after since, merged in (at,
// step_run_id, seq) order. Either way the result is then filtered by after
// and before/limit, and capped by tail. docs/activity.md §The RPC is the
// specification this implements.
//
// follow keeps the stream open past what is on record: a single run ends it
// with RunEnd once the run (or, with step set, that step's latest attempt)
// leaves running; every run never ends it on its own. before and limit page
// a closed read and are refused together with follow.
func (s *StoreService) Logs(ctx context.Context, req *connect.Request[millv1.LogsRequest], stream *connect.ServerStream[millv1.LogsResponse]) error {
	msg := req.Msg
	if msg.GetFollow() && (msg.GetBefore() != nil || msg.GetLimit() != 0) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("Logs: before and limit are refused together with follow"))
	}
	if msg.GetFollow() {
		if msg.GetId() == "" && msg.GetNodeRef() == "" {
			return s.logsFollowEveryRun(ctx, msg, stream)
		}
		return s.logsFollowOneRun(ctx, msg, stream)
	}

	var activities []*millv1.Activity
	if msg.GetId() == "" && msg.GetNodeRef() == "" {
		var err error
		activities, err = s.readEveryRunActivities(ctx, msg.GetSince())
		if err != nil {
			return connectError(err)
		}
	} else {
		detail, err := s.svc.Inspect(ctx, domain.InspectParams{ID: msg.GetId(), NodeRef: msg.GetNodeRef()})
		if err != nil {
			return connectError(err)
		}
		activities, err = s.readRunActivities(detail, msg.GetStep(), msg.GetAttempt())
		if err != nil {
			return connectError(err)
		}
	}

	activities = afterCursor(activities, msg.GetAfter())
	if msg.GetBefore() != nil {
		activities = beforeCursor(activities, msg.GetBefore(), msg.GetLimit())
	} else if limit := msg.GetLimit(); limit > 0 && int64(len(activities)) > limit {
		activities = activities[:limit]
	}
	if tail := msg.GetTail(); tail > 0 && int64(len(activities)) > tail {
		activities = activities[int64(len(activities))-tail:]
	}

	for _, a := range activities {
		if err := stream.Send(&millv1.LogsResponse{Frame: &millv1.LogsResponse_Activity{Activity: a}}); err != nil {
			return err
		}
	}
	return nil
}

// readEveryRunActivities reads every run's activity, unnarrowed by step or
// attempt — a global stream has no one run to narrow within — and merges it
// in the order docs/activity.md's every-run semantics specify: (at,
// step_run_id, seq). since, when set, drops everything before it; zero
// means every record on file.
//
// Closed attempts never change once sealed, so their records come from the
// index (readIndexedActivities), bounded by since at the database — not by
// re-reading every attempt file of every run on every wake. Only runs that
// may still have a running (unsealed) attempt are walked and read from disk
// directly.
func (s *StoreService) readEveryRunActivities(ctx context.Context, since *timestamppb.Timestamp) ([]*millv1.Activity, error) {
	var sinceTime *time.Time
	if since != nil {
		t := since.AsTime()
		sinceTime = &t
	}
	indexed, err := s.svc.ActivitySince(ctx, sinceTime)
	if err != nil {
		return nil, err
	}
	out, err := s.readIndexedActivities(ctx, indexed)
	if err != nil {
		return nil, err
	}

	runs, _, err := s.svc.Status(ctx, liveRunStatus, "")
	if err != nil {
		return nil, err
	}
	for _, summary := range runs {
		detail, err := s.svc.Inspect(ctx, domain.InspectParams{ID: summary.GetRun().GetId()})
		if err != nil {
			return nil, err
		}
		runID := detail.Summary.GetRun().GetId()
		for _, stepRunDetail := range detail.Steps {
			stepRun := stepRunDetail.GetStepRun()
			if stepRun.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING {
				// Not running: either already sealed (and read above, from
				// the index) or the accepted seal-failure gap liveRunStatus's
				// comment names.
				continue
			}
			activities, err := run.ReadActivities(s.logDir, runID, stepRun.GetStepName(), stepRun.GetAttempt())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, a := range activities {
				a.StepRunId = stepRun.GetId()
			}
			out = append(out, activities...)
		}
	}

	out = sinceFilter(out, since)
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		ti, tj := ai.GetAt().AsTime(), aj.GetAt().AsTime()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if ai.GetStepRunId() != aj.GetStepRunId() {
			return ai.GetStepRunId() < aj.GetStepRunId()
		}
		return ai.GetSeq() < aj.GetSeq()
	})
	return out, nil
}

// readIndexedActivities reads the full sealed frame of every attempt named
// in indexed, once per attempt no matter how many of its rows are in
// indexed — the index only says an attempt has records since the read's
// lower bound, not which ones, and sinceFilter (applied by the caller,
// exactly as it already is for a live read) trims the rest.
func (s *StoreService) readIndexedActivities(ctx context.Context, indexed []*millv1.ActivityIndexRow) ([]*millv1.Activity, error) {
	seen := map[string]bool{}
	var out []*millv1.Activity
	for _, row := range indexed {
		stepRunID := row.GetStepRunId()
		if seen[stepRunID] {
			continue
		}
		seen[stepRunID] = true
		stepRun, err := s.svc.GetStepRun(ctx, stepRunID)
		if err != nil {
			return nil, err
		}
		activities, err := run.ReadFrame(run.FramePath(s.logDir, stepRun.GetRunId(), stepRun.GetStepName(), stepRun.GetAttempt()))
		if err != nil {
			return nil, err
		}
		for _, a := range activities {
			a.StepRunId = stepRunID
		}
		out = append(out, activities...)
	}
	return out, nil
}

// sinceFilter drops every activity recorded before since. A nil since
// leaves activities untouched.
func sinceFilter(activities []*millv1.Activity, since *timestamppb.Timestamp) []*millv1.Activity {
	if since == nil {
		return activities
	}
	out := activities[:0]
	for _, a := range activities {
		if a.GetAt().AsTime().Before(since.AsTime()) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// readRunActivities reads every activity record of detail's run, narrowed to
// step and attempt (either or both may be unset), attempts in the start
// order Inspect already returns them in. A missing attempt file — one whose
// writer has not opened it yet — contributes nothing rather than failing the
// read: the ledger row and the file are written by different actors and the
// gap between them is not an error here.
//
// Reads through [run.ReadSealedActivities]: a still-running attempt's live
// file, or a closed one's sealed frame once SealAttempt has replaced it —
// the two read byte-identical records for the same attempt (docs/
// activity.md), so a caller narrowed to one run never needs to know which.
//
// The writer never stamps step_run_id on a record — a live file is already
// scoped to one attempt by its path, so it has no need to — but the record's
// documented primary key (docs/activity.md's index row) is (step_run_id,
// seq), and Cursor names a record by it. This is where that gap is closed:
// the attempt this file belongs to is known here, so it is stamped on every
// record read from it before the record leaves this process.
func (s *StoreService) readRunActivities(detail *domain.RunDetail, step string, attempt int64) ([]*millv1.Activity, error) {
	runID := detail.Summary.GetRun().GetId()
	var out []*millv1.Activity
	for _, stepRunDetail := range detail.Steps {
		stepRun := stepRunDetail.GetStepRun()
		if step != "" && stepRun.GetStepName() != step {
			continue
		}
		if attempt != 0 && stepRun.GetAttempt() != attempt {
			continue
		}
		activities, err := run.ReadSealedActivities(s.logDir, runID, stepRun.GetStepName(), stepRun.GetAttempt())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, a := range activities {
			a.StepRunId = stepRun.GetId()
		}
		out = append(out, activities...)
	}
	return out, nil
}

// afterCursor drops every activity at or before cursor, identified by the
// (step_run_id, seq) it names, a record's own primary key. A cursor naming
// nothing in activities — the empty cursor, or one from outside the
// narrowed set — leaves activities untouched.
func afterCursor(activities []*millv1.Activity, cursor *millv1.Cursor) []*millv1.Activity {
	if cursor.GetStepRunId() == "" {
		return activities
	}
	for i, a := range activities {
		if a.GetStepRunId() == cursor.GetStepRunId() && a.GetSeq() == cursor.GetSeq() {
			return activities[i+1:]
		}
	}
	return activities
}

// beforeCursor returns the limit records immediately before cursor, in seq
// order — a page that opened on tail scrolling up. limit 0 returns every
// record before cursor. A cursor naming nothing in activities leaves
// activities untouched, as afterCursor does for the same case.
func beforeCursor(activities []*millv1.Activity, cursor *millv1.Cursor, limit int64) []*millv1.Activity {
	if cursor.GetStepRunId() == "" {
		return activities
	}
	for i, a := range activities {
		if a.GetStepRunId() == cursor.GetStepRunId() && a.GetSeq() == cursor.GetSeq() {
			before := activities[:i]
			if limit > 0 && int64(len(before)) > limit {
				before = before[int64(len(before))-limit:]
			}
			return before
		}
	}
	return activities
}

// runEnds reports whether status is one a run stops being acted on in — the
// five docs/activity.md names as ending a followed stream over one run.
func runEnds(status millv1.RunStatus) bool {
	switch status {
	case millv1.RunStatus_RUN_STATUS_DONE,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
		millv1.RunStatus_RUN_STATUS_WAITING,
		millv1.RunStatus_RUN_STATUS_PARKED:
		return true
	default:
		return false
	}
}

// latestStepRun returns detail's step run for step, narrowed to attempt
// when it is nonzero, or the highest-numbered attempt otherwise — the one a
// step-scoped follow watches, since it is the only one still eligible to
// leave running. nil when step names nothing detail has started yet.
func latestStepRun(detail *domain.RunDetail, step string, attempt int64) *millv1.StepRun {
	var latest *millv1.StepRun
	for _, stepRunDetail := range detail.Steps {
		stepRun := stepRunDetail.GetStepRun()
		if stepRun.GetStepName() != step {
			continue
		}
		if attempt != 0 && stepRun.GetAttempt() != attempt {
			continue
		}
		if latest == nil || stepRun.GetAttempt() > latest.GetAttempt() {
			latest = stepRun
		}
	}
	return latest
}

// gateRef reads a's gate_filed body, when it has one, into the shape
// RunEnd.gate carries. docs/activity.md's Decisions: RunEnd's gate is built
// from the gate_filed record already in the stream being followed, not a
// fresh call to dagger.
func gateRef(a *millv1.Activity) *millv1.GateRef {
	filed := a.GetGateFiled()
	if filed == nil {
		return nil
	}
	return &millv1.GateRef{NodeId: filed.GetNodeId(), NodeRef: filed.GetNodeRef()}
}

// lastGateRef is gateRef over the last gate_filed record in runID's own
// activity, for the every-run follow: unlike a single-run follow, which
// already reads every record of the one run it is scoped to as it streams,
// an every-run follow does not otherwise hold a run's full history once it
// has streamed past it, so a run that turns out to end waiting or parked is
// read once more here, at that moment, rather than kept in memory against
// the chance it needs this.
func (s *StoreService) lastGateRef(ctx context.Context, runID string) (*millv1.GateRef, error) {
	detail, err := s.svc.Inspect(ctx, domain.InspectParams{ID: runID})
	if err != nil {
		return nil, err
	}
	activities, err := s.readRunActivities(detail, "", 0)
	if err != nil {
		return nil, err
	}
	var gate *millv1.GateRef
	for _, a := range activities {
		if g := gateRef(a); g != nil {
			gate = g
		}
	}
	return gate, nil
}

// logsFollowOneRun answers Logs for a single run with follow set: it sends
// what readRunActivities already returns, then re-reads and re-checks after
// every Observer wake, until the run (msg.step unset) or that step's latest
// attempt (msg.step set) leaves running. See docs/activity.md's follow
// bullets in §The RPC.
func (s *StoreService) logsFollowOneRun(ctx context.Context, msg *millv1.LogsRequest, stream *connect.ServerStream[millv1.LogsResponse]) error {
	last := msg.GetAfter()
	var gate *millv1.GateRef
	first := true

	for {
		detail, err := s.svc.Inspect(ctx, domain.InspectParams{ID: msg.GetId(), NodeRef: msg.GetNodeRef()})
		if err != nil {
			return connectError(err)
		}
		runID := detail.Summary.GetRun().GetId()

		// Subscribed before this wake's read, so a signal landing between
		// the read below and the Wait at the bottom of the loop is not
		// missed (Observer.Subscribe's own contract).
		woken := s.observer.Subscribe(runID)

		activities, err := s.readRunActivities(detail, msg.GetStep(), msg.GetAttempt())
		if err != nil {
			return connectError(err)
		}
		activities = afterCursor(activities, last)
		if first && msg.GetTail() > 0 && int64(len(activities)) > msg.GetTail() {
			activities = activities[int64(len(activities))-msg.GetTail():]
		}
		first = false

		for _, a := range activities {
			if g := gateRef(a); g != nil {
				gate = g
			}
			if err := stream.Send(&millv1.LogsResponse{Frame: &millv1.LogsResponse_Activity{Activity: truncate(a)}}); err != nil {
				return err
			}
			last = &millv1.Cursor{StepRunId: a.GetStepRunId(), Seq: a.GetSeq()}
		}

		if msg.GetStep() != "" {
			if sr := latestStepRun(detail, msg.GetStep(), msg.GetAttempt()); sr != nil && sr.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING {
				return nil
			}
		} else if status := detail.Summary.GetRun().GetStatus(); runEnds(status) {
			return stream.Send(&millv1.LogsResponse{Frame: &millv1.LogsResponse_Run{Run: &millv1.RunEnd{
				RunId: runID, Status: status, Gate: gate,
			}}})
		}

		s.observer.Wait(ctx, woken)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// logsFollowEveryRun answers Logs with follow set and no run named: it
// never ends on its own (docs/activity.md: "Over every run it does not
// end"), streaming new activity across every run as it lands and a
// LogsResponse_Run frame — without closing the stream — for each run that
// newly reaches an ending status.
func (s *StoreService) logsFollowEveryRun(ctx context.Context, msg *millv1.LogsRequest, stream *connect.ServerStream[millv1.LogsResponse]) error {
	last := msg.GetAfter()
	since := msg.GetSince()
	ended := map[string]bool{}
	first := true

	for {
		// The empty key is what a stream over every run watches: CreateRun
		// is the only write that signals it, since it is the only write
		// that changes what such a stream cares about and touches no
		// existing run's own key (Observer's doc comment).
		woken := s.observer.Subscribe("")

		activities, err := s.readEveryRunActivities(ctx, since)
		if err != nil {
			return connectError(err)
		}
		activities = afterCursor(activities, last)
		if first && msg.GetTail() > 0 && int64(len(activities)) > msg.GetTail() {
			activities = activities[int64(len(activities))-msg.GetTail():]
		}
		first = false

		for _, a := range activities {
			if err := stream.Send(&millv1.LogsResponse{Frame: &millv1.LogsResponse_Activity{Activity: truncate(a)}}); err != nil {
				return err
			}
			last = &millv1.Cursor{StepRunId: a.GetStepRunId(), Seq: a.GetSeq()}
		}

		runs, _, err := s.svc.Status(ctx, domain.EveryRunStatus, "")
		if err != nil {
			return connectError(err)
		}
		for _, summary := range runs {
			run := summary.GetRun()
			if ended[run.GetId()] || !runEnds(run.GetStatus()) {
				continue
			}
			ended[run.GetId()] = true
			gate, err := s.lastGateRef(ctx, run.GetId())
			if err != nil {
				return connectError(err)
			}
			if err := stream.Send(&millv1.LogsResponse{Frame: &millv1.LogsResponse_Run{Run: &millv1.RunEnd{
				RunId: run.GetId(), Status: run.GetStatus(), Gate: gate,
			}}}); err != nil {
				return err
			}
		}

		s.observer.Wait(ctx, woken)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// bodyOneof is Activity's "body" oneof descriptor, resolved once. It
// mirrors internal/run/activity.go's own activityBodyOneof rather than
// reusing it — that one is unexported, and the two ceilings apply at
// different moments (write time, permanent, on disk; send time, only while
// following, over the wire) — but the field it names is the same field.
var bodyOneof = (&millv1.Activity{}).ProtoReflect().Descriptor().Oneofs().ByName("body")

// bodySize is a's body, marshaled alone, found by proto reflection over the
// oneof rather than trusted from a's own bytes field: a fixture built by a
// test may leave bytes unset, and truncate must still cut a body that is
// actually oversized.
func bodySize(a *millv1.Activity) int64 {
	if bodyOneof == nil {
		return 0
	}
	m := a.ProtoReflect()
	field := m.WhichOneof(bodyOneof)
	if field == nil || field.Kind() != protoreflect.MessageKind {
		return 0
	}
	return int64(proto.Size(m.Get(field).Message().Interface()))
}

// truncate returns a unchanged if its body is at or under truncatedLimit,
// and otherwise a clone with every string and bytes field of the body
// cleared and truncated set. It clones rather than mutating a in place: a
// followed stream's cut must not reach the file a later, unfollowed read
// (or another followed stream) sees, only the frame this one call is about
// to send.
func truncate(a *millv1.Activity) *millv1.Activity {
	if bodyOneof == nil || bodySize(a) <= truncatedLimit {
		return a
	}
	clone, ok := proto.Clone(a).(*millv1.Activity)
	if !ok {
		return a
	}
	field := clone.ProtoReflect().WhichOneof(bodyOneof)
	if field == nil || field.Kind() != protoreflect.MessageKind {
		return clone
	}
	body := clone.ProtoReflect().Get(field).Message()
	var cut []protoreflect.FieldDescriptor
	body.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.Kind() == protoreflect.StringKind || fd.Kind() == protoreflect.BytesKind {
			cut = append(cut, fd)
		}
		return true
	})
	for _, fd := range cut {
		body.Clear(fd)
	}
	clone.Truncated = true
	return clone
}
