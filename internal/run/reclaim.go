package run

import (
	"context"
	"sort"
	"time"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// Resumable is one of this worker's own runs left with an aborted attempt: a
// candidate to reclaim before the next ClaimNext (docs/architecture.md
// §"Failure and stop", "Stop.", mill#44).
type Resumable struct {
	// RunID is the run's id, for Inspect.
	RunID string
	// NodeRef is the run's node: what a reclaim claims by name.
	NodeRef string
	// AbortedAt is when the latest attempt was aborted: the order a reclaim
	// claims candidates in.
	AbortedAt time.Time
}

// Resumable lists this worker's own live runs whose latest attempt is
// aborted, oldest abort first.
//
// A run left `running` under this worker_ref by a crash, rather than
// `aborted` by a graceful stop, is not a candidate here: that case is
// deferred to the ordinary claim path exactly as it is today
// (docs/ledger.md §Resume's `run.status in (running, selecting)` branch),
// because nothing yet tells a live crash apart from a dead one without the
// sweep's own TTL guard.
func (r *Runner) Resumable(ctx context.Context) ([]Resumable, error) {
	res, err := call(ctx, r.options.Store.Status, &millv1.StatusRequest{
		Statuses: []millv1.RunStatus{millv1.RunStatus_RUN_STATUS_RUNNING, millv1.RunStatus_RUN_STATUS_SELECTING},
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
		if summary.GetLastStepStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED {
			continue
		}
		detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: run.GetId()})
		if err != nil {
			return nil, err
		}
		sr := latestStepRun(detail, summary.GetLastStep())
		if sr == nil {
			continue
		}
		candidates = append(candidates, Resumable{
			RunID:     run.GetId(),
			NodeRef:   run.GetNodeRef(),
			AbortedAt: sr.GetEndedAt().AsTime(),
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].AbortedAt.Before(candidates[j].AbortedAt) })
	return candidates, nil
}
