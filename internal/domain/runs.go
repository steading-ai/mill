package domain

import (
	"context"
	"fmt"

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
