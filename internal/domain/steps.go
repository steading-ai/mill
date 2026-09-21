package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/store"
)

// StartStepRunParams opens the next attempt at a step.
type StartStepRunParams struct {
	RunID        string
	StepName     string
	Kind         millv1.StepKind
	SessionID    string
	WorkerRef    string
	FeedbackFrom string
	BaseCommit   string
}

// CloseStepRunParams records the evidence that closes an attempt.
type CloseStepRunParams struct {
	StepRunID    string
	Evidence     *millv1.Evidence
	WorkflowID   string
	Tier         millv1.Tier
	AutoApproved bool
}

// StartStepRun opens the next attempt at a step, numbered max+1 for its
// (run, step) pair.
func (s *Service) StartStepRun(ctx context.Context, leaseID string, p StartStepRunParams) (*millv1.StepRun, error) {
	return s.store.StartStepRun(ctx, leaseID, store.StartStepRunParams{
		RunID:        p.RunID,
		StepName:     p.StepName,
		Kind:         p.Kind,
		SessionID:    p.SessionID,
		WorkerRef:    p.WorkerRef,
		FeedbackFrom: p.FeedbackFrom,
		BaseCommit:   p.BaseCommit,
	})
}

// RecordStepEnvironmentParams records what an attempt's session process was
// given of the mill's own environment.
type RecordStepEnvironmentParams struct {
	StepRunID      string
	EnvPassthrough []string
	EnvStripped    []string
}

// RecordStepEnvironment writes what the environment mode gave an attempt's
// session process: what the step asked to have passed through, and what the
// deny list removed.
func (s *Service) RecordStepEnvironment(ctx context.Context, leaseID string, p RecordStepEnvironmentParams) (*millv1.StepRun, error) {
	return s.store.RecordStepEnvironment(ctx, leaseID, store.RecordStepEnvironmentParams{
		StepRunID:      p.StepRunID,
		EnvPassthrough: p.EnvPassthrough,
		EnvStripped:    p.EnvStripped,
	})
}

// MarkSessionStarted records that the harness has produced an event in an
// attempt's session, which is what makes the session resumable.
func (s *Service) MarkSessionStarted(ctx context.Context, leaseID, stepRunID string) (*millv1.StepRun, error) {
	return s.store.MarkSessionStarted(ctx, leaseID, stepRunID)
}

// CloseStepRun records the evidence that closes an attempt and marks it
// closed, in one transaction.
//
// The evidence is judged first and entirely outside the store: its value
// must be the shape its kind defines, and its kind must be the one the
// step's definition names. A refusal therefore leaves no evidence row and no
// closed attempt — the two are inseparable, and neither is written.
func (s *Service) CloseStepRun(ctx context.Context, leaseID string, p CloseStepRunParams) (*millv1.StepRun, *millv1.Evidence, error) {
	if err := ValidateEvidence(p.Evidence); err != nil {
		return nil, nil, err
	}
	open, err := s.store.GetStepRun(ctx, p.StepRunID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.checkStepTransition(open, millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED); err != nil {
		return nil, nil, err
	}
	workflow, err := s.store.WorkflowOfRun(ctx, open.GetRunId())
	switch {
	case errors.Is(err, ErrNoWorkflow):
		// The selection step closes before the run has a workflow, so
		// there is no definition to check its kind against.
	case err != nil:
		return nil, nil, err
	default:
		if err := checkDeclaredKind(workflow, open.GetStepName(), p.Evidence); err != nil {
			return nil, nil, err
		}
	}
	return s.store.CloseStepRun(ctx, leaseID, store.CloseStepRunParams{
		StepRunID:    p.StepRunID,
		Evidence:     p.Evidence,
		WorkflowID:   p.WorkflowID,
		Tier:         p.Tier,
		AutoApproved: p.AutoApproved,
	})
}

// FailStepRun marks an attempt failed with how its process ended, so the
// step's on_failure can be applied.
func (s *Service) FailStepRun(ctx context.Context, leaseID, stepRunID string, exit *harnessv1.ProcessExit) (*millv1.StepRun, error) {
	open, err := s.store.GetStepRun(ctx, stepRunID)
	if err != nil {
		return nil, err
	}
	if err := s.checkStepTransition(open, millv1.StepRunStatus_STEP_RUN_STATUS_FAILED); err != nil {
		return nil, err
	}
	return s.store.FailStepRun(ctx, leaseID, stepRunID, exit)
}

// AbortOpenStepRuns marks any still-running attempt aborted. Called on
// resume, when a running row belongs to a dead or fenced-out worker.
func (s *Service) AbortOpenStepRuns(ctx context.Context, leaseID, runID, reason string) ([]*millv1.StepRun, error) {
	return s.store.AbortOpenStepRuns(ctx, leaseID, runID, reason)
}

// SupersedeSteps marks the latest attempts of every step from a target
// through a failing step superseded, which is what moves the run's current
// step backwards for a revise.
func (s *Service) SupersedeSteps(ctx context.Context, leaseID, runID, fromStep, throughStep string) ([]*millv1.StepRun, error) {
	return s.store.SupersedeSteps(ctx, leaseID, runID, fromStep, throughStep)
}

// IssueStepToken mints the step-scoped bearer a session presents on the
// workspace socket. The bearer is in the response and nowhere else.
func (s *Service) IssueStepToken(ctx context.Context, leaseID, stepRunID string, capabilities []millv1.Capability) (*millv1.StepToken, error) {
	return s.store.IssueStepToken(ctx, leaseID, stepRunID, capabilities)
}

// RevokeStepToken revokes a bearer.
func (s *Service) RevokeStepToken(ctx context.Context, leaseID, stepTokenID string) (*millv1.StepToken, error) {
	return s.store.RevokeStepToken(ctx, leaseID, stepTokenID)
}

// RecordUsage appends what a turn cost, per model, to the ledger.
func (s *Service) RecordUsage(ctx context.Context, leaseID, stepRunID string, usage []*millv1.Usage) ([]*millv1.Usage, error) {
	return s.store.RecordUsage(ctx, leaseID, stepRunID, usage)
}

// MarkUsagePosted marks a step run's usage rows as posted to dagger.
func (s *Service) MarkUsagePosted(ctx context.Context, leaseID, stepRunID string) (int64, error) {
	return s.store.MarkUsagePosted(ctx, leaseID, stepRunID)
}

// SealAttempt records a closed attempt's index rows, once its frame and
// twin are written.
func (s *Service) SealAttempt(ctx context.Context, stepRunID string, rows []*millv1.ActivityIndexRow) ([]*millv1.ActivityIndexRow, error) {
	return s.store.SealAttempt(ctx, stepRunID, rows)
}

// ActivityForStepRun lists a closed attempt's sealed index rows in seq
// order.
func (s *Service) ActivityForStepRun(ctx context.Context, stepRunID string) ([]*millv1.ActivityIndexRow, error) {
	return s.store.ActivityForStepRun(ctx, stepRunID)
}

// ActivitySince lists every sealed index row since a point in time, across
// every run, in log order.
func (s *Service) ActivitySince(ctx context.Context, since *time.Time) ([]*millv1.ActivityIndexRow, error) {
	return s.store.ActivitySince(ctx, since)
}

// GetStepRun looks up one attempt by id — the run, step and attempt number
// an index row's step_run_id alone does not carry, and Logs (internal/server/
// logs.go) needs to find that attempt's sealed frame on disk.
func (s *Service) GetStepRun(ctx context.Context, stepRunID string) (*millv1.StepRun, error) {
	return s.store.GetStepRun(ctx, stepRunID)
}

// checkStepTransition judges an attempt's move against the step run
// diagram. The store refuses the same move under the row lock; this is what
// names both statuses when it does not.
func (s *Service) checkStepTransition(open *millv1.StepRun, want millv1.StepRunStatus) error {
	if err := CheckStepRunTransition(open.GetStatus(), want); err != nil {
		return fmt.Errorf("step run %s: %w", open.GetId(), err)
	}
	return nil
}
