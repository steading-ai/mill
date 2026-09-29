package run

import (
	"context"
	"sort"
	"time"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Resumable is one of this worker's own runs left in a shape it can pick
// back up before the next ClaimNext (docs/architecture.md §"Failure and
// stop", "Stop.", mill#44): either an aborted attempt (a kill) or a run left
// `stopped` (a drain's boundary release, a cancel, or a shutdown).
type Resumable struct {
	// RunID is the run's id, for Inspect.
	RunID string
	// NodeRef is the run's node: what a reclaim claims by name.
	NodeRef string
	// UpdatedAt is when the run last changed: the order a reclaim claims
	// candidates in. Both shapes advance it on the write that leaves them
	// resumable — a kill's AbortOpenStepRuns, or a drain's ReleaseLease.
	UpdatedAt time.Time
}

// Resumable lists this worker's own live runs left in either shape a stop
// can leave one in, oldest updated first.
//
// A run left `running` under this worker_ref by a crash — not aborted, and
// still holding its lease, since nothing releases it — is not a candidate
// here: that case is deferred to the ordinary claim path exactly as it is
// today (docs/ledger.md §Resume's `run.status in (running, selecting)`
// branch), because nothing yet tells a live crash apart from a dead one
// without the sweep's own TTL guard.
func (r *Runner) Resumable(ctx context.Context) ([]Resumable, error) {
	res, err := call(ctx, r.options.Store.Status, &millv1.StatusRequest{
		Statuses: []millv1.RunStatus{
			millv1.RunStatus_RUN_STATUS_RUNNING,
			millv1.RunStatus_RUN_STATUS_SELECTING,
			millv1.RunStatus_RUN_STATUS_STOPPED,
		},
	})
	if err != nil {
		return nil, err
	}
	var candidates []Resumable
	for _, summary := range res.GetRuns() {
		run := summary.GetRun()
		if run.GetWorkerRef() != r.options.WorkerRef {
			continue
		}
		aborted := summary.GetLastStepStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED
		drained := run.GetLeaseId() == ""
		if !aborted && !drained {
			continue
		}
		candidates = append(candidates, Resumable{
			RunID:     run.GetId(),
			NodeRef:   run.GetNodeRef(),
			UpdatedAt: run.GetUpdatedAt().AsTime(),
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt) })
	return candidates, nil
}
