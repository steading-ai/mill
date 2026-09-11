package domain

import (
	"context"
	"fmt"
	"time"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
)

// CreateRunParams opens a run on a claimed node.
type CreateRunParams struct {
	NodeID     string
	NodeRef    string
	ContextID  string
	Generation int64
	WorkerRef  string
	Branch     string
}

// InspectParams names the run to inspect: its id, or the node's display key
// for a human who has that instead.
type InspectParams struct {
	ID      string
	NodeRef string
}

// RunDetail is one run in full: the run and what the ledger derives about
// it, the workflow version it follows, and every attempt in order.
type RunDetail struct {
	Summary  *millv1.RunSummary
	Workflow *millv1.Workflow
	Steps    []*millv1.StepRunDetail
}

// GetRunByNode returns the live run for a dagger node, or nil when the node
// has none. The first call a worker makes after a claim.
func (s *Service) GetRunByNode(ctx context.Context, nodeID string) (*millv1.Run, error) {
	return s.store.GetRunByNode(ctx, nodeID)
}

// CreateRun opens a run in status selecting, holding the lease of the claim
// that created it. A run has no prior status, so there is no transition to
// judge; at most one live run per node is the store's Invariant 1.
func (s *Service) CreateRun(ctx context.Context, leaseID string, p CreateRunParams) (*millv1.Run, error) {
	return s.store.CreateRun(ctx, leaseID, store.CreateRunParams{
		NodeID:     p.NodeID,
		NodeRef:    p.NodeRef,
		ContextID:  p.ContextID,
		Generation: p.Generation,
		WorkerRef:  p.WorkerRef,
		Branch:     p.Branch,
	})
}

// SetLease makes a claim's lease the run's current one. It is not judged
// against [runTransitions]: a takeover has its own admission rule, from
// docs/ledger.md Invariant 9 — running with an expired lease, waiting,
// parked, or done with the node still open — which the store applies inside
// the transaction that takes the run's row lock. Applying the diagram here
// as well would refuse the resume the rule exists to allow.
func (s *Service) SetLease(ctx context.Context, runID, leaseID, workerRef string) (*millv1.Run, error) {
	return s.store.SetLease(ctx, runID, leaseID, workerRef)
}

// MarkWaiting records the gate a run is blocked on and marks it waiting.
func (s *Service) MarkWaiting(ctx context.Context, leaseID, runID, gateNodeID, stepRunID string) (*millv1.Run, error) {
	if err := s.checkTransition(ctx, runID, millv1.RunStatus_RUN_STATUS_WAITING); err != nil {
		return nil, err
	}
	return s.store.MarkWaiting(ctx, leaseID, runID, gateNodeID, stepRunID)
}

// Park trips the circuit breaker: the run stops, a gate asks a human what to
// do, and the reason says what stopped it.
func (s *Service) Park(ctx context.Context, leaseID, runID, gateNodeID string, reason millv1.ParkedReason) (*millv1.Run, error) {
	if err := s.checkTransition(ctx, runID, millv1.RunStatus_RUN_STATUS_PARKED); err != nil {
		return nil, err
	}
	return s.store.Park(ctx, leaseID, runID, gateNodeID, reason)
}

// RecordNotice appends what carrying a filed gate to a person did. There is
// no transition to check: it records an errand, not a change to the run.
func (s *Service) RecordNotice(ctx context.Context, leaseID, runID, stepRunID, kind, gateRef string, exitCode int32, detail string) error {
	return s.store.RecordNotice(ctx, leaseID, runID, stepRunID, kind, gateRef, exitCode, detail)
}

// ReconciliationFinding is one line of a reconciliation pass, and
// Reconciliation one recorded pass, as the store keeps them; named here so
// the service layer, which may not reach the store, can hand them over.
type (
	ReconciliationFinding = store.ReconciliationFinding
	ReconciliationPass    = store.ReconciliationPass
)

// RecordReconciliation appends what a worker's reconciliation pass found and
// did at start. There is no run to transition and no lease to fence: it is
// a record of the host.
func (s *Service) RecordReconciliation(ctx context.Context, workerRef string, startedAt time.Time, findings []ReconciliationFinding) error {
	return s.store.RecordReconciliation(ctx, workerRef, startedAt, findings)
}

// Reconciliations is the newest recorded passes, newest first.
func (s *Service) Reconciliations(ctx context.Context, limit int) ([]ReconciliationPass, error) {
	return s.store.Reconciliations(ctx, limit)
}

// RecordHeartbeat appends one heartbeat the harness sent a quiet session.
// There is no transition to check: it records what the wait cost, not a
// change to the run.
func (s *Service) RecordHeartbeat(ctx context.Context, leaseID, runID, stepRunID string, strike, strikes int32, waitingSeconds int64, tasks []string) error {
	return s.store.RecordHeartbeat(ctx, leaseID, runID, stepRunID, strike, strikes, waitingSeconds, tasks)
}

// SetCompleteRequestID mints the completing patch's idempotency key, or
// returns the one the run already carries. There is no transition to check:
// it records a key against a run that is still running, before the patch
// that ends it.
func (s *Service) SetCompleteRequestID(ctx context.Context, leaseID, runID, requestID string) (*millv1.Run, error) {
	return s.store.SetCompleteRequestID(ctx, leaseID, runID, requestID)
}

// Done marks a run done, after dagger has confirmed the completing patch.
func (s *Service) Done(ctx context.Context, leaseID, runID string) (*millv1.Run, error) {
	if err := s.checkTransition(ctx, runID, millv1.RunStatus_RUN_STATUS_DONE); err != nil {
		return nil, err
	}
	return s.store.Done(ctx, leaseID, runID)
}

// Fail ends a run a gate denied. The node's own result already carries the
// gate's deny_result: this is the ledger catching up with what dagger was
// told, in the order docs/ledger.md fixes.
func (s *Service) Fail(ctx context.Context, leaseID, runID, reason string) (*millv1.Run, error) {
	if err := s.checkTransition(ctx, runID, millv1.RunStatus_RUN_STATUS_FAILED); err != nil {
		return nil, err
	}
	return s.store.Failed(ctx, leaseID, runID, reason)
}

// Resume clears the gate an answered run stopped on, so the run carries no
// gate and the next gate step files its own. There is no transition to
// judge: the claim that resumed the run has already made it running, and
// this is the write that catches the gate up with that.
func (s *Service) Resume(ctx context.Context, leaseID, runID string) (*millv1.Run, error) {
	return s.store.ClearGate(ctx, leaseID, runID)
}

// Abandon marks a run abandoned so a new generation can be created: the
// restart path.
func (s *Service) Abandon(ctx context.Context, leaseID, runID, reason string) (*millv1.Run, error) {
	if err := s.checkTransition(ctx, runID, millv1.RunStatus_RUN_STATUS_ABANDONED); err != nil {
		return nil, err
	}
	return s.store.Abandon(ctx, leaseID, runID, reason)
}

// Cancel asks a run's worker to stop at its next heartbeat. It is a request,
// not a transition — the flag is set and the status is left alone — so the
// diagram has nothing to say about it; what it does need is a run that is
// still being worked, since nothing will ever read the flag on one that has
// ended.
func (s *Service) Cancel(ctx context.Context, runID, requestedBy, reason string) (*millv1.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if terminal(run.GetStatus()) {
		return nil, fmt.Errorf("run %s: %w", runID, &RunEndedError{Status: run.GetStatus()})
	}
	return s.store.RequestCancel(ctx, runID, requestedBy, reason)
}

// Rerun supersedes every step from fromStep through the run's current one,
// so the next claim resumes at fromStep. What it promises is that the run is
// resumable afterwards; a `failed` run is therefore reopened by the same
// transaction, because a rerun is a person overruling the ending, and a span
// superseded on a run no claim can take over would strand the node.
//
// `abandoned` is the one status a rerun refuses. The run has been replaced
// by a later generation, so reviving it would put two live runs on one node
// (Invariant 1), and what the operator wants on such a node is the newer
// generation or a fresh `mill restart`.
func (s *Service) Rerun(ctx context.Context, runID, fromStep, requestedBy, reason string) (*store.RerunResult, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.GetStatus() == millv1.RunStatus_RUN_STATUS_ABANDONED {
		return nil, fmt.Errorf("run %s was replaced by a later generation, so it cannot be reopened; `mill restart` opens a new one: %w",
			runID, &RunEndedError{Status: run.GetStatus()})
	}
	return s.store.Rerun(ctx, runID, fromStep, requestedBy, reason)
}

// Status lists runs by state with what the ledger derives about each.
func (s *Service) Status(ctx context.Context, statuses []millv1.RunStatus, project string) ([]*millv1.RunSummary, error) {
	return s.store.Status(ctx, statuses, project)
}

// Inspect returns one run's every attempt in order with its evidence, exit,
// session, and usage.
func (s *Service) Inspect(ctx context.Context, p InspectParams) (*RunDetail, error) {
	detail, err := s.store.Inspect(ctx, store.InspectParams{ID: p.ID, NodeRef: p.NodeRef})
	if err != nil {
		return nil, err
	}
	return &RunDetail{Summary: detail.Summary, Workflow: detail.Workflow, Steps: detail.Steps}, nil
}

// History returns a run's audit trail, oldest first.
func (s *Service) History(ctx context.Context, p InspectParams) ([]*millv1.Event, error) {
	return s.store.History(ctx, store.InspectParams{ID: p.ID, NodeRef: p.NodeRef})
}

// CostParams is the window `mill cost` asks about.
type CostParams struct {
	// Since bounds the window below. Zero means every row ever recorded.
	Since time.Time
	// Project narrows to one dagger project. Empty means every project.
	Project string
}

// Cost rolls a window of usage up by run, workflow, step and model.
func (s *Service) Cost(ctx context.Context, p CostParams) (*store.Cost, error) {
	return s.store.Cost(ctx, store.CostParams{Since: p.Since, Project: p.Project})
}

// Health is what the service can say about itself: whether the database
// answers and whether its schema is the one this binary carries.
func (s *Service) Health(ctx context.Context) *store.Health {
	return s.store.Health(ctx)
}

// checkTransition reads the run's status and judges the move to want
// against the diagram.
//
// The read is not inside the writing transaction, and does not need to be:
// the write that follows fences the run under its row lock, so a status that
// changed in between belongs to a worker that has taken the lease, and the
// write is refused as fenced rather than applied to a state it never saw.
func (s *Service) checkTransition(ctx context.Context, runID string, want millv1.RunStatus) error {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := CheckRunTransition(run.GetStatus(), want); err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	return nil
}

// terminal reports whether a run has ended: done, failed or abandoned, the
// three states docs/domain-model.md §"Run states" draws no edge out of.
func terminal(status millv1.RunStatus) bool {
	switch status {
	case millv1.RunStatus_RUN_STATUS_DONE,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED:
		return true
	default:
		return false
	}
}
