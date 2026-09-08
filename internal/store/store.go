// Package store is the ledger: the schema in migrations, the sqlc-generated
// queries over it, and the [Store] whose methods are the store API's one
// door. docs/ledger.md is the specification — its nine tables, its twelve
// invariants, and the fencing rule every write for a run passes through.
//
// Only the domain layer imports this package (depguard enforces it), and
// every mutating method runs in one transaction that fences the run before
// it writes: the run's row lock, a re-read of the lease, a comparison
// against the lease the caller presented, and a rollback on mismatch. See
// [fence] and Invariant 9. A stale worker cannot corrupt the record however
// well it is behaving, and that lives here rather than in the worker's good
// behaviour.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// The errors a store method is refused with. [ErrFenced] is in fence.go,
// beside the mechanism that raises it.
var (
	// ErrNotFound: the row a call named does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNotClaimable: SetLease against a run whose status is not one a new
	// claim may take over. The wrapping error names the status.
	ErrNotClaimable = errors.New("run status does not permit a new claim")
	// ErrNotRunning: a call that ends an attempt found it already ended.
	// The next worker starts a new attempt rather than reopening this one.
	ErrNotRunning = errors.New("step run is not running")
	// ErrUnknownStep: a step name the run's workflow definition does not
	// contain.
	ErrUnknownStep = errors.New("workflow has no such step")
	// ErrNoWorkflow: an operation that needs the run's workflow, on a run
	// that is still selecting.
	ErrNoWorkflow = errors.New("run has no workflow yet")
)

// claimableStatuses are the run statuses a new claim may take over, from
// Invariant 9: running with an expired or absent lease, waiting, parked, or
// done with the node still open. Whether a lease has expired is dagger's
// judgement, not the ledger's — dagger issues a new lease only when the old
// one is gone — so `running` is admitted on the strength of the caller
// holding a lease dagger just granted.
//
// `selecting` is deliberately absent: a run is created with the lease of
// the claim that created it, so there is nothing for a later SetLease to
// take over.
var claimableStatuses = map[string]bool{
	"running": true,
	"waiting": true,
	"parked":  true,
	"done":    true,
}

// Store owns the connection pool and the transactions the queries run in.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to databaseURL and verifies the server answers. Close the
// store when done.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// FromPool wraps a pool the caller already owns. Closing the store closes
// the pool.
func FromPool(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Close releases every connection.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for work that must run outside a
// transaction.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// withTx runs fn in one transaction, committing when it returns nil and
// rolling back otherwise. A fenced-out write therefore writes nothing.
func (s *Store) withTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx, q *Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx, New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// event is one row of the audit trail, written by the same transaction as
// the change it records. See the events section of docs/ledger.md.
type event struct {
	runID     string
	stepRunID string
	op        string
	before    map[string]any
	after     map[string]any
	workerRef string
}

// record appends an event.
func record(ctx context.Context, q *Queries, e event) error {
	before, err := eventJSON(e.before)
	if err != nil {
		return err
	}
	after, err := eventJSON(e.after)
	if err != nil {
		return err
	}
	if _, err := q.InsertEvent(ctx, InsertEventParams{
		ID:        newULID(),
		RunID:     e.runID,
		StepRunID: nullable(e.stepRunID),
		Op:        e.op,
		Before:    before,
		After:     after,
		WorkerRef: e.workerRef,
	}); err != nil {
		return fmt.Errorf("record event %s: %w", e.op, err)
	}
	return nil
}

func eventJSON(fields map[string]any) ([]byte, error) {
	if fields == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode event fields: %w", err)
	}
	return encoded, nil
}

// GetRunByNode returns the run a claim on this node resumes, or nil when
// the node has none. It is the first call a worker makes after a claim.
func (s *Store) GetRunByNode(ctx context.Context, nodeID string) (*millv1.Run, error) {
	row, err := New(s.pool).GetRunByNode(ctx, nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run by node %s: %w", nodeID, err)
	}
	return runProto(row), nil
}

// GetRun returns one run by id.
func (s *Store) GetRun(ctx context.Context, runID string) (*millv1.Run, error) {
	row, err := New(s.pool).GetRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	return runProto(row), nil
}

// CreateRunParams opens a run on a claimed node.
type CreateRunParams struct {
	// NodeID is the dagger node's id.
	NodeID string
	// NodeRef is its display key.
	NodeRef string
	// ContextID is dagger's context id for the node, copied at creation.
	ContextID string
	// Generation is 1 for the first run on a node; restart passes the next.
	Generation int64
	// WorkerRef is where this attempt runs.
	WorkerRef string
	// Branch is the run's git branch, derived from the node ref.
	Branch string
}

// CreateRun opens a run in status selecting, holding the lease of the claim
// that created it. There is no run to fence against yet — the lease is
// recorded here so that every later write has something to compare against,
// which is why this method is the one mutation that takes a lease without
// calling [fence]. At most one live run per node: the partial unique index
// of Invariant 1 rejects a second.
func (s *Store) CreateRun(ctx context.Context, leaseID string, p CreateRunParams) (*millv1.Run, error) {
	if p.Generation == 0 {
		p.Generation = 1
	}
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, _ pgx.Tx, q *Queries) error {
		row, err := q.InsertRun(ctx, InsertRunParams{
			ID:         newULID(),
			NodeID:     p.NodeID,
			NodeRef:    p.NodeRef,
			Generation: p.Generation,
			ContextID:  p.ContextID,
			LeaseID:    nullable(leaseID),
			WorkerRef:  p.WorkerRef,
			Branch:     p.Branch,
		})
		if err != nil {
			return fmt.Errorf("create run on node %s: %w", p.NodeID, err)
		}
		run = runProto(row)
		return record(ctx, q, event{
			runID:     row.ID,
			op:        "create_run",
			after:     map[string]any{"status": row.Status, "generation": row.Generation, "lease_id": leaseID},
			workerRef: p.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// SetLease makes a claim's lease the run's current one, so every later
// write is fenced by it. It is the one write whose presented lease is not
// compared against the stored one: it is the write that changes what later
// comparisons compare against. It takes the same row lock, and it requires
// the run's status to be one a new claim may take over — see
// [claimableStatuses] — so a run that failed, was abandoned, or has never
// left selection is refused with its status named.
func (s *Store) SetLease(ctx context.Context, runID, leaseID, workerRef string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := lockRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !claimableStatuses[state.status] {
			return fmt.Errorf("run %s is %s: %w", runID, state.status, ErrNotClaimable)
		}
		row, err := q.SetRunLease(ctx, SetRunLeaseParams{ID: runID, LeaseID: nullable(leaseID), WorkerRef: workerRef})
		if err != nil {
			return fmt.Errorf("set lease on run %s: %w", runID, err)
		}
		run = runProto(row)
		return record(ctx, q, event{
			runID:     runID,
			op:        "set_lease",
			before:    map[string]any{"status": state.status, "lease_id": text(state.lease)},
			after:     map[string]any{"status": row.Status, "lease_id": leaseID},
			workerRef: workerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// MarkWaiting records the gate a run is blocked on and marks it waiting.
// The worker calls it between the patch that filed the gate and the release
// of the lease, so a crash never leaves a gate the store would file twice:
// the gate id is on the run, and Invariant 12's trigger refuses to replace
// it with another.
func (s *Store) MarkWaiting(ctx context.Context, leaseID, runID, gateNodeID, stepRunID string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := fence(ctx, tx, runID, leaseID)
		if err != nil {
			return err
		}
		row, err := q.MarkRunWaiting(ctx, MarkRunWaitingParams{ID: runID, GateNodeID: nullable(gateNodeID)})
		if err != nil {
			return fmt.Errorf("mark run %s waiting: %w", runID, err)
		}
		run = runProto(row)
		return record(ctx, q, event{
			runID:     runID,
			stepRunID: stepRunID,
			op:        "mark_waiting",
			before:    map[string]any{"status": state.status},
			after:     map[string]any{"status": row.Status, "gate_node_id": gateNodeID},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// Park trips the circuit breaker: the run stops, a gate asks a human what
// to do, and the reason says what stopped it.
func (s *Store) Park(ctx context.Context, leaseID, runID, gateNodeID string, reason millv1.ParkedReason) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := fence(ctx, tx, runID, leaseID)
		if err != nil {
			return err
		}
		row, err := q.ParkRun(ctx, ParkRunParams{
			ID:           runID,
			GateNodeID:   nullable(gateNodeID),
			ParkedReason: nullable(enumText(reason)),
		})
		if err != nil {
			return fmt.Errorf("park run %s: %w", runID, err)
		}
		run = runProto(row)
		return record(ctx, q, event{
			runID:     runID,
			op:        "park",
			before:    map[string]any{"status": state.status},
			after:     map[string]any{"status": row.Status, "gate_node_id": gateNodeID, "reason": enumText(reason)},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// Done marks a run done. It is called only after dagger has confirmed the
// completing patch: a run marked done before that would strand a node
// nothing can claim (Invariant 11).
func (s *Store) Done(ctx context.Context, leaseID, runID string) (*millv1.Run, error) {
	return s.finish(ctx, leaseID, runID, "done", "")
}

// Failed marks a run failed: the gate on its way out was denied, or nothing
// can be done with it. The lease is released by the caller.
func (s *Store) Failed(ctx context.Context, leaseID, runID, reason string) (*millv1.Run, error) {
	return s.finish(ctx, leaseID, runID, "failed", reason)
}

// Abandon marks a run abandoned so a new generation can be created. It
// leaves the lease unfenced when the caller presents none, which is the
// ordinary restart path: dagger's release has already happened, so there is
// no lease left to compare against.
func (s *Store) Abandon(ctx context.Context, leaseID, runID, reason string) (*millv1.Run, error) {
	return s.finish(ctx, leaseID, runID, "abandoned", reason)
}

// finish is the shared tail of the three terminal transitions.
func (s *Store) finish(ctx context.Context, leaseID, runID, status, reason string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		var state fenceState
		var err error
		if leaseID == "" {
			// Abandon after the release: there is no lease to present and
			// none to compare against, so the lock alone serializes this
			// against a concurrent claim.
			state, err = lockRunRow(ctx, tx, runID)
		} else {
			state, err = fence(ctx, tx, runID, leaseID)
		}
		if err != nil {
			return err
		}
		row, err := q.FinishRun(ctx, FinishRunParams{ID: runID, Status: status})
		if err != nil {
			return fmt.Errorf("mark run %s %s: %w", runID, status, err)
		}
		run = runProto(row)
		after := map[string]any{"status": row.Status}
		if reason != "" {
			after["reason"] = reason
		}
		return record(ctx, q, event{
			runID:     runID,
			op:        status,
			before:    map[string]any{"status": state.status},
			after:     after,
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// RequestCancel records a cancellation request on a run. It is a request to
// the worker rather than a transition: the flag is set, the run's status is
// left alone, and the worker reads the flag at its next heartbeat, stops the
// step cleanly and releases its lease. The run stays resumable.
//
// The caller holds no lease — cancel comes from a human or an agent, not
// from the worker — so the row lock alone serializes it against a concurrent
// claim. requestedBy is the caller the transport resolved, recorded on the
// event.
func (s *Store) RequestCancel(ctx context.Context, runID, requestedBy, reason string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := lockRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		row, err := q.RequestRunCancel(ctx, runID)
		if err != nil {
			return fmt.Errorf("request cancel of run %s: %w", runID, err)
		}
		run = runProto(row)
		after := map[string]any{"status": row.Status, "cancel_requested": true, "requested_by": requestedBy}
		if reason != "" {
			after["reason"] = reason
		}
		return record(ctx, q, event{
			runID:     runID,
			op:        "cancel_requested",
			before:    map[string]any{"status": state.status, "cancel_requested": false},
			after:     after,
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// StartStepRunParams opens the next attempt at a step.
type StartStepRunParams struct {
	// RunID is the run the attempt belongs to.
	RunID string
	// StepName is the step from the run's workflow definition.
	StepName string
	// Kind is copied onto the attempt so the row reads alone.
	Kind millv1.StepKind
	// SessionID is the session a skill step's attempt runs in.
	SessionID string
	// WorkerRef is the worker running it.
	WorkerRef string
	// FeedbackFrom is the step run whose failure this attempt revises.
	FeedbackFrom string
}

// StartStepRun opens the next attempt at a step, numbered max + 1 for the
// (run, step) pair by the insert itself (Invariant 7). At most one attempt
// per run is running, which the partial unique index of Invariant 3
// enforces: a second start while one is open is rejected, not queued.
func (s *Store) StartStepRun(ctx context.Context, leaseID string, p StartStepRunParams) (*millv1.StepRun, error) {
	var stepRun *millv1.StepRun
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, p.RunID, leaseID); err != nil {
			return err
		}
		row, err := q.InsertStepRun(ctx, InsertStepRunParams{
			ID:           newULID(),
			RunID:        p.RunID,
			StepName:     p.StepName,
			Kind:         enumText(p.Kind),
			SessionID:    nullable(p.SessionID),
			LeaseID:      nullable(leaseID),
			WorkerRef:    p.WorkerRef,
			FeedbackFrom: nullable(p.FeedbackFrom),
		})
		if err != nil {
			return fmt.Errorf("start step run %s on run %s: %w", p.StepName, p.RunID, err)
		}
		stepRun, err = stepRunProto(row)
		if err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     p.RunID,
			stepRunID: row.ID,
			op:        "start_step_run",
			after:     map[string]any{"step_name": row.StepName, "attempt": row.Attempt, "kind": row.Kind},
			workerRef: p.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return stepRun, nil
}

// GetStepRun returns one attempt, or [ErrNotFound]. The domain reads it to
// judge a transition before asking for the write that performs it.
func (s *Store) GetStepRun(ctx context.Context, stepRunID string) (*millv1.StepRun, error) {
	row, err := New(s.pool).GetStepRun(ctx, stepRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("step run %s: %w", stepRunID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get step run %s: %w", stepRunID, err)
	}
	return stepRunProto(row)
}

// WorkflowOfRun returns the workflow version a run follows, or
// [ErrNoWorkflow] while it is still selecting.
func (s *Store) WorkflowOfRun(ctx context.Context, runID string) (*millv1.Workflow, error) {
	return workflowOfRun(ctx, New(s.pool), runID)
}

// CloseStepRunParams records the evidence that closes an attempt.
type CloseStepRunParams struct {
	// StepRunID is the attempt to close.
	StepRunID string
	// Evidence is the fact that closes it. Insert-only, and a step run has
	// at most one.
	Evidence *millv1.Evidence
	// WorkflowID is set only by the selection step: the workflow version
	// the assessment named, applied to the run once.
	WorkflowID string
	// Tier is set only by the selection step: what gate auto predicates
	// compare against.
	Tier millv1.Tier
	// AutoApproved marks a gate step the auto predicate closed without a
	// human.
	AutoApproved bool
}

// CloseStepRun records the evidence that closes an attempt and marks it
// closed, in one transaction. It is the only way a step becomes closed, and
// the two halves are inseparable: the deferred constraint triggers of
// Invariant 5 refuse a closed attempt with no evidence and evidence for an
// attempt that is not being closed.
//
// Closing a gate step clears the run's gate: the gate was read and
// answered, so the next gate step is free to file one.
func (s *Store) CloseStepRun(ctx context.Context, leaseID string, p CloseStepRunParams) (*millv1.StepRun, *millv1.Evidence, error) {
	var stepRun *millv1.StepRun
	var evidence *millv1.Evidence
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		open, err := q.GetStepRun(ctx, p.StepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s: %w", p.StepRunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step run %s: %w", p.StepRunID, err)
		}
		if _, err := fence(ctx, tx, open.RunID, leaseID); err != nil {
			return err
		}

		row, err := q.CloseStepRun(ctx, p.StepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s is %s: %w", p.StepRunID, open.Status, ErrNotRunning)
		}
		if err != nil {
			return fmt.Errorf("close step run %s: %w", p.StepRunID, err)
		}
		if p.AutoApproved {
			if row, err = q.MarkStepRunAutoApproved(ctx, p.StepRunID); err != nil {
				return fmt.Errorf("mark step run %s auto approved: %w", p.StepRunID, err)
			}
		}
		value, err := evidenceValue(p.Evidence)
		if err != nil {
			return err
		}
		stored, err := q.InsertEvidence(ctx, InsertEvidenceParams{
			StepRunID:  p.StepRunID,
			Kind:       enumText(p.Evidence.GetKind()),
			Value:      value,
			VerifiedBy: p.Evidence.GetVerifiedBy(),
		})
		if err != nil {
			return fmt.Errorf("record evidence for step run %s: %w", p.StepRunID, err)
		}
		if _, err := q.RevokeStepTokensForStepRun(ctx, p.StepRunID); err != nil {
			return fmt.Errorf("revoke step tokens of %s: %w", p.StepRunID, err)
		}

		if p.WorkflowID != "" {
			if _, err := q.SetRunWorkflow(ctx, SetRunWorkflowParams{
				ID:         open.RunID,
				WorkflowID: nullable(p.WorkflowID),
				Tier:       nullable(enumText(p.Tier)),
			}); err != nil {
				return fmt.Errorf("set workflow of run %s: %w", open.RunID, err)
			}
		}
		if row.Kind == enumText(millv1.StepKind_STEP_KIND_GATE) {
			if _, err := q.ClearRunGate(ctx, open.RunID); err != nil {
				return fmt.Errorf("clear gate of run %s: %w", open.RunID, err)
			}
		}

		if stepRun, err = stepRunProto(row); err != nil {
			return err
		}
		if evidence, err = evidenceProto(stored); err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     open.RunID,
			stepRunID: row.ID,
			op:        "close_step_run",
			before:    map[string]any{"status": open.Status},
			after:     map[string]any{"status": row.Status, "evidence_kind": stored.Kind, "auto_approved": row.AutoApproved},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return stepRun, evidence, nil
}

// FailStepRun marks an attempt failed with how its process ended, so the
// step's on_failure can be applied. Its tokens are revoked in the same
// transaction (Invariant 8).
func (s *Store) FailStepRun(ctx context.Context, leaseID, stepRunID string, exit *harnessv1.ProcessExit) (*millv1.StepRun, error) {
	var stepRun *millv1.StepRun
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		open, err := q.GetStepRun(ctx, stepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s: %w", stepRunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step run %s: %w", stepRunID, err)
		}
		if _, err := fence(ctx, tx, open.RunID, leaseID); err != nil {
			return err
		}
		encoded, err := jsonbMessage(exit)
		if err != nil {
			return fmt.Errorf("encode exit of step run %s: %w", stepRunID, err)
		}
		row, err := q.FailStepRun(ctx, FailStepRunParams{ID: stepRunID, Exit: encoded})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s is %s: %w", stepRunID, open.Status, ErrNotRunning)
		}
		if err != nil {
			return fmt.Errorf("fail step run %s: %w", stepRunID, err)
		}
		if _, err := q.RevokeStepTokensForStepRun(ctx, stepRunID); err != nil {
			return fmt.Errorf("revoke step tokens of %s: %w", stepRunID, err)
		}
		if stepRun, err = stepRunProto(row); err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     open.RunID,
			stepRunID: stepRunID,
			op:        "fail_step_run",
			before:    map[string]any{"status": open.Status},
			after:     map[string]any{"status": row.Status, "exit": json.RawMessage(encoded)},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return stepRun, nil
}

// AbortOpenStepRuns marks any still-running attempt aborted, and revokes
// its tokens. A running row on resume belongs to a dead or fenced-out
// worker; the next worker starts a new attempt rather than continuing it.
//
// docs/ledger.md §Resume orders this call before SetLease, when the stored
// lease is still the dead worker's, so the presented lease cannot be
// compared against it. It takes the same row lock and admits the same
// statuses SetLease does instead.
func (s *Store) AbortOpenStepRuns(ctx context.Context, leaseID, runID, reason string) ([]*millv1.StepRun, error) {
	var aborted []*millv1.StepRun
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := lockRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		if state.lease != nil && *state.lease == leaseID {
			// The caller already holds the run: an ordinary fenced write.
		} else if !claimableStatuses[state.status] {
			return fmt.Errorf("run %s is %s: %w", runID, state.status, ErrNotClaimable)
		}
		rows, err := q.AbortOpenStepRuns(ctx, runID)
		if err != nil {
			return fmt.Errorf("abort open step runs of %s: %w", runID, err)
		}
		for _, row := range rows {
			if _, err := q.RevokeStepTokensForStepRun(ctx, row.ID); err != nil {
				return fmt.Errorf("revoke step tokens of %s: %w", row.ID, err)
			}
			if err := record(ctx, q, event{
				runID:     runID,
				stepRunID: row.ID,
				op:        "abort_step_run",
				before:    map[string]any{"status": "running"},
				after:     map[string]any{"status": row.Status, "reason": reason},
				workerRef: row.WorkerRef,
			}); err != nil {
				return err
			}
		}
		aborted, err = stepRunProtos(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return aborted, nil
}

// SupersedeSteps marks the latest attempt at every step from a target
// through a failing step superseded, which is what moves the current step
// backwards for a revise. The evidence those attempts produced stays,
// immutable, and closes nothing: the ledger never claims pass-one evidence
// closed pass-two work.
//
// The span comes from the run's workflow definition, so a target that is
// not in the workflow, or one that is not earlier than the failing step, is
// refused rather than silently narrowed.
func (s *Store) SupersedeSteps(ctx context.Context, leaseID, runID, fromStep, throughStep string) ([]*millv1.StepRun, error) {
	var superseded []*millv1.StepRun
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, runID, leaseID); err != nil {
			return err
		}
		workflow, err := workflowOfRun(ctx, q, runID)
		if err != nil {
			return err
		}
		span, err := stepSpan(workflow, fromStep, throughStep)
		if err != nil {
			return err
		}
		rows, err := q.SupersedeStepRuns(ctx, SupersedeStepRunsParams{RunID: runID, StepNames: span})
		if err != nil {
			return fmt.Errorf("supersede steps of run %s: %w", runID, err)
		}
		for _, row := range rows {
			if err := record(ctx, q, event{
				runID:     runID,
				stepRunID: row.ID,
				op:        "supersede_step_run",
				after:     map[string]any{"status": row.Status, "from": fromStep, "through": throughStep},
				workerRef: row.WorkerRef,
			}); err != nil {
				return err
			}
		}
		superseded, err = stepRunProtos(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return superseded, nil
}

// stepSpan is the step names from fromStep through throughStep inclusive,
// in workflow order.
func stepSpan(workflow *millv1.Workflow, fromStep, throughStep string) ([]string, error) {
	from, through := -1, -1
	names := make([]string, 0, len(workflow.GetSteps()))
	for i, step := range workflow.GetSteps() {
		names = append(names, step.GetName())
		switch step.GetName() {
		case fromStep:
			from = i
		case throughStep:
			through = i
		}
	}
	if from < 0 {
		return nil, fmt.Errorf("workflow %s: step %q: %w", workflow.GetName(), fromStep, ErrUnknownStep)
	}
	if through < 0 {
		return nil, fmt.Errorf("workflow %s: step %q: %w", workflow.GetName(), throughStep, ErrUnknownStep)
	}
	if through < from {
		return nil, fmt.Errorf("workflow %s: step %q is before %q: %w",
			workflow.GetName(), throughStep, fromStep, ErrUnknownStep)
	}
	return names[from : through+1], nil
}

// workflowOfRun loads the workflow version a run follows.
func workflowOfRun(ctx context.Context, q *Queries, runID string) (*millv1.Workflow, error) {
	run, err := q.GetRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	if run.WorkflowID == nil {
		return nil, fmt.Errorf("run %s: %w", runID, ErrNoWorkflow)
	}
	row, err := q.GetWorkflowByID(ctx, *run.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("get workflow %s: %w", *run.WorkflowID, err)
	}
	return workflowProto(row)
}

// IssueStepToken mints the step-scoped bearer a session presents on the
// workspace socket. The bearer is returned once, here, and never again: the
// store keeps only its SHA-256, so a leaked ledger yields no credential. A
// live token belongs to a running step run (Invariant 8), which the
// deferred constraint trigger checks.
func (s *Store) IssueStepToken(ctx context.Context, leaseID, stepRunID string, capabilities []millv1.Capability) (*millv1.StepToken, error) {
	bearer, err := newBearer()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(bearer))
	var token *millv1.StepToken
	err = s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		stepRun, err := q.GetStepRun(ctx, stepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s: %w", stepRunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step run %s: %w", stepRunID, err)
		}
		if _, err := fence(ctx, tx, stepRun.RunID, leaseID); err != nil {
			return err
		}
		row, err := q.InsertStepToken(ctx, InsertStepTokenParams{
			ID:           newULID(),
			StepRunID:    stepRunID,
			TokenHash:    hex.EncodeToString(sum[:]),
			Capabilities: enumTexts(capabilities),
		})
		if err != nil {
			return fmt.Errorf("issue step token for %s: %w", stepRunID, err)
		}
		token = stepTokenProto(row)
		token.Token = bearer
		return record(ctx, q, event{
			runID:     stepRun.RunID,
			stepRunID: stepRunID,
			op:        "issue_step_token",
			after:     map[string]any{"step_token_id": row.ID, "capabilities": row.Capabilities},
			workerRef: stepRun.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

// RevokeStepToken revokes a bearer. The store also revokes on its own,
// inside the transaction that moves a step run out of running, so this is
// for a rotation the worker drives.
func (s *Store) RevokeStepToken(ctx context.Context, leaseID, stepTokenID string) (*millv1.StepToken, error) {
	var token *millv1.StepToken
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		existing, err := q.GetStepToken(ctx, stepTokenID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step token %s: %w", stepTokenID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step token %s: %w", stepTokenID, err)
		}
		stepRun, err := q.GetStepRun(ctx, existing.StepRunID)
		if err != nil {
			return fmt.Errorf("get step run %s: %w", existing.StepRunID, err)
		}
		if _, err := fence(ctx, tx, stepRun.RunID, leaseID); err != nil {
			return err
		}
		row, err := q.RevokeStepToken(ctx, stepTokenID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already revoked: the token is not live either way.
			token = stepTokenProto(existing)
			return nil
		}
		if err != nil {
			return fmt.Errorf("revoke step token %s: %w", stepTokenID, err)
		}
		token = stepTokenProto(row)
		return record(ctx, q, event{
			runID:     stepRun.RunID,
			stepRunID: stepRun.ID,
			op:        "revoke_step_token",
			after:     map[string]any{"step_token_id": row.ID},
			workerRef: stepRun.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

// newBearer mints the opaque credential a session holds: 32 random bytes in
// URL-safe base64.
func newBearer() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint step token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// RecordUsage appends what a turn cost, per model. One row per turn per
// model, as the adapter reports: a turn that bills two models writes two
// rows. Counts, never currency.
func (s *Store) RecordUsage(ctx context.Context, leaseID, stepRunID string, usage []*millv1.Usage) ([]*millv1.Usage, error) {
	var recorded []*millv1.Usage
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		stepRun, err := q.GetStepRun(ctx, stepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s: %w", stepRunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step run %s: %w", stepRunID, err)
		}
		if _, err := fence(ctx, tx, stepRun.RunID, leaseID); err != nil {
			return err
		}
		recorded = make([]*millv1.Usage, 0, len(usage))
		for _, turn := range usage {
			row, err := q.InsertUsage(ctx, InsertUsageParams{
				ID:               newULID(),
				StepRunID:        stepRunID,
				Model:            turn.GetModel(),
				InputTokens:      turn.GetInputTokens(),
				OutputTokens:     turn.GetOutputTokens(),
				CacheReadTokens:  turn.GetCacheReadTokens(),
				CacheWriteTokens: turn.GetCacheWriteTokens(),
			})
			if err != nil {
				return fmt.Errorf("record usage for step run %s: %w", stepRunID, err)
			}
			recorded = append(recorded, usageProto(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// MarkUsagePosted marks exactly the unposted rows of one step run, after
// dagger's ReportUsage has accepted them. A worker that dies mid-step
// leaves dagger's per-lease total short by one step and the ledger complete;
// there is no reconciliation job, so the mark is per step run and never
// wider.
func (s *Store) MarkUsagePosted(ctx context.Context, leaseID, stepRunID string) (int64, error) {
	var marked int64
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		stepRun, err := q.GetStepRun(ctx, stepRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step run %s: %w", stepRunID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("get step run %s: %w", stepRunID, err)
		}
		if _, err := fence(ctx, tx, stepRun.RunID, leaseID); err != nil {
			return err
		}
		rows, err := q.MarkUsagePosted(ctx, stepRunID)
		if err != nil {
			return fmt.Errorf("mark usage posted for step run %s: %w", stepRunID, err)
		}
		marked = int64(len(rows))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return marked, nil
}

// PutSession records the deterministic id and adapter reference of a
// session, so `mill inspect` can say which session produced which verdict.
// It is not a StoreService RPC: the proto names none, and the worker node
// that drives sessions will need one.
func (s *Store) PutSession(ctx context.Context, leaseID string, session *millv1.Session) (*millv1.Session, error) {
	var stored *millv1.Session
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, session.GetRunId(), leaseID); err != nil {
			return err
		}
		adapterRef, err := jsonbMessage(session.GetAdapterRef())
		if err != nil {
			return fmt.Errorf("encode adapter_ref of session %s: %w", session.GetId(), err)
		}
		row, err := q.UpsertSession(ctx, UpsertSessionParams{
			RunID:      session.GetRunId(),
			Name:       session.GetName(),
			ID:         session.GetId(),
			Harness:    session.GetHarness(),
			AdapterRef: adapterRef,
			WorkerRef:  session.GetWorkerRef(),
		})
		if err != nil {
			return fmt.Errorf("put session %s: %w", session.GetId(), err)
		}
		stored, err = sessionProto(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// GetWorkflowParams asks for one workflow version.
type GetWorkflowParams struct {
	// ID is the version's id. When set, the rest is ignored.
	ID string
	// Name is the workflow's name, resolved to the newest version in scope.
	Name string
	// Scope is where to resolve. Unspecified tries project, then org.
	Scope millv1.Scope
	// Project is the project, when the scope is project.
	Project string
	// Version pins a specific content hash.
	Version string
}

// GetWorkflow returns one workflow version, by id or by name in a scope. A
// project scope falls back to org, because a project that names no workflow
// of its own is worked through the org's.
func (s *Store) GetWorkflow(ctx context.Context, p GetWorkflowParams) (*millv1.Workflow, error) {
	q := New(s.pool)
	if p.ID != "" {
		row, err := q.GetWorkflowByID(ctx, p.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("workflow %s: %w", p.ID, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("get workflow %s: %w", p.ID, err)
		}
		return workflowProto(row)
	}
	for _, scope := range resolutionOrder(p.Scope, p.Project) {
		project := p.Project
		if scope == millv1.Scope_SCOPE_ORG {
			project = ""
		}
		var row Workflow
		var err error
		if p.Version != "" {
			row, err = q.GetWorkflowByVersion(ctx, GetWorkflowByVersionParams{
				Name: p.Name, Scope: enumText(scope), Project: project, Version: p.Version,
			})
		} else {
			row, err = q.GetNewestWorkflow(ctx, GetNewestWorkflowParams{
				Name: p.Name, Scope: enumText(scope), Project: project,
			})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get workflow %s: %w", p.Name, err)
		}
		return workflowProto(row)
	}
	return nil, fmt.Errorf("workflow %s: %w", p.Name, ErrNotFound)
}

// resolutionOrder is the scopes a name is looked for in, most specific
// first. A project scope with no project named cannot resolve, so it is
// dropped.
func resolutionOrder(scope millv1.Scope, project string) []millv1.Scope {
	switch {
	case scope == millv1.Scope_SCOPE_PROJECT && project != "":
		return []millv1.Scope{millv1.Scope_SCOPE_PROJECT}
	case scope == millv1.Scope_SCOPE_ORG:
		return []millv1.Scope{millv1.Scope_SCOPE_ORG}
	case project != "":
		return []millv1.Scope{millv1.Scope_SCOPE_PROJECT, millv1.Scope_SCOPE_ORG}
	default:
		return []millv1.Scope{millv1.Scope_SCOPE_ORG}
	}
}

// WorkflowFilter asks for the workflow versions in a scope.
type WorkflowFilter struct {
	// Name lists only this name. Empty lists every name.
	Name string
	// Scope lists only this scope. Unspecified lists both.
	Scope millv1.Scope
	// Project is the project, when the scope is project.
	Project string
	// AllVersions lists every version rather than the newest of each name.
	AllVersions bool
}

// ListWorkflows lists workflow versions, newest first.
func (s *Store) ListWorkflows(ctx context.Context, p WorkflowFilter) ([]*millv1.Workflow, error) {
	rows, err := New(s.pool).ListWorkflows(ctx, ListWorkflowsParams{
		Name:    nullable(p.Name),
		Scope:   nullable(enumText(p.Scope)),
		Project: nullable(p.Project),
	})
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	// The query orders by name then newest first, so the first row of each
	// name is the newest version of it.
	seen := map[string]bool{}
	workflows := make([]*millv1.Workflow, 0, len(rows))
	for _, row := range rows {
		key := row.Name + "\x00" + row.Scope + "\x00" + row.Project
		if !p.AllVersions && seen[key] {
			continue
		}
		seen[key] = true
		workflow, err := workflowProto(row)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, workflow)
	}
	return workflows, nil
}

// PutWorkflow stores a workflow version, idempotent by content hash: the
// version is computed from the definition, and storing the same definition
// twice returns the row already there. A workflow is never edited, so a
// changed definition is a new version and runs in flight keep the one they
// started with.
//
// This is one of the two mutations Invariant 9 does not fence: a workflow
// belongs to no run, so there is no lease to compare against.
func (s *Store) PutWorkflow(ctx context.Context, workflow *millv1.Workflow) (*millv1.Workflow, bool, error) {
	definition := canonicalWorkflow(workflow)
	version, err := contentHash(definition)
	if err != nil {
		return nil, false, err
	}
	encoded, err := jsonbMessage(definition)
	if err != nil {
		return nil, false, fmt.Errorf("encode workflow %s: %w", workflow.GetName(), err)
	}
	source, err := jsonbMessage(workflow.GetSource())
	if err != nil {
		return nil, false, fmt.Errorf("encode source of workflow %s: %w", workflow.GetName(), err)
	}
	scope := enumText(workflow.GetScope())
	project := workflow.GetProject()

	var stored *millv1.Workflow
	created := false
	err = s.withTx(ctx, func(ctx context.Context, _ pgx.Tx, q *Queries) error {
		row, err := q.GetWorkflowByVersion(ctx, GetWorkflowByVersionParams{
			Name: workflow.GetName(), Scope: scope, Project: project, Version: version,
		})
		if err == nil {
			stored, err = workflowProto(row)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get workflow %s: %w", workflow.GetName(), err)
		}
		row, err = q.InsertWorkflow(ctx, InsertWorkflowParams{
			ID:         newULID(),
			Name:       workflow.GetName(),
			Scope:      scope,
			Project:    project,
			Version:    version,
			Definition: encoded,
			Source:     source,
		})
		if err != nil {
			return fmt.Errorf("put workflow %s: %w", workflow.GetName(), err)
		}
		created = true
		stored, err = workflowProto(row)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// canonicalWorkflow is the definition without the fields that identify the
// version, which is what the version hashes over: two definitions that
// differ only by id, stored version, source or creation time are the same
// version.
func canonicalWorkflow(workflow *millv1.Workflow) *millv1.Workflow {
	canonical := &millv1.Workflow{}
	proto.Merge(canonical, workflow)
	canonical.Id = ""
	canonical.Version = ""
	canonical.Source = nil
	canonical.CreatedAt = nil
	return canonical
}

// contentHash is the SHA-256 of a message's deterministic binary encoding,
// hex. Deterministic marshalling is stable for a given schema, which is
// what makes a version reproducible from its definition.
func contentHash(message proto.Message) (string, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("hash definition: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// PutSkill stores a skill by name and digest, so a workflow version pins
// the exact prompt it ran. Idempotent by digest, and unfenced for the same
// reason PutWorkflow is.
func (s *Store) PutSkill(ctx context.Context, skill *millv1.Skill) (*millv1.Skill, bool, error) {
	digest := skill.GetDigest()
	if digest == "" {
		content := &millv1.Skill{}
		proto.Merge(content, skill)
		content.Id = ""
		content.Digest = ""
		content.Source = nil
		content.CreatedAt = nil
		var err error
		if digest, err = contentHash(content); err != nil {
			return nil, false, err
		}
	}
	files, err := jsonbMap(skill.GetFiles())
	if err != nil {
		return nil, false, fmt.Errorf("encode files of skill %s: %w", skill.GetName(), err)
	}
	metadata, err := jsonbMap(skill.GetMetadata())
	if err != nil {
		return nil, false, fmt.Errorf("encode metadata of skill %s: %w", skill.GetName(), err)
	}
	source, err := jsonbMessage(skill.GetSource())
	if err != nil {
		return nil, false, fmt.Errorf("encode source of skill %s: %w", skill.GetName(), err)
	}

	var stored *millv1.Skill
	created := false
	err = s.withTx(ctx, func(ctx context.Context, _ pgx.Tx, q *Queries) error {
		row, err := q.GetSkillByDigest(ctx, GetSkillByDigestParams{Name: skill.GetName(), Digest: digest})
		if err == nil {
			stored, err = skillProto(row)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get skill %s: %w", skill.GetName(), err)
		}
		row, err = q.InsertSkill(ctx, InsertSkillParams{
			ID:       newULID(),
			Name:     skill.GetName(),
			Digest:   digest,
			Body:     skill.GetBody(),
			Files:    files,
			Metadata: metadata,
			Source:   source,
		})
		if err != nil {
			return fmt.Errorf("put skill %s: %w", skill.GetName(), err)
		}
		created = true
		stored, err = skillProto(row)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// RunState loads a run and the latest attempt at each of its steps: what
// [CurrentStep] derives from.
func (s *Store) RunState(ctx context.Context, runID string) (RunState, error) {
	return runState(ctx, New(s.pool), runID)
}

func runState(ctx context.Context, q *Queries, runID string) (RunState, error) {
	row, err := q.GetRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunState{}, fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return RunState{}, fmt.Errorf("get run %s: %w", runID, err)
	}
	return latestState(ctx, q, row)
}

// latestState pairs a run row with the latest attempt at each of its steps.
func latestState(ctx context.Context, q *Queries, row Run) (RunState, error) {
	rows, err := q.LatestStepRuns(ctx, row.ID)
	if err != nil {
		return RunState{}, fmt.Errorf("latest step runs of %s: %w", row.ID, err)
	}
	latest := make(map[string]*millv1.StepRun, len(rows))
	for _, stepRunRow := range rows {
		stepRun, err := stepRunProto(stepRunRow)
		if err != nil {
			return RunState{}, err
		}
		latest[stepRun.GetStepName()] = stepRun
	}
	return RunState{Run: runProto(row), Latest: latest}, nil
}

// Status lists runs by state with what the ledger derives rather than
// stores: the current step, the attempts against it, the cost so far, and
// how many leases the node has held. Empty statuses means every
// non-terminal state, which is what a human asking "what is running?"
// means.
func (s *Store) Status(ctx context.Context, statuses []millv1.RunStatus, project string) ([]*millv1.RunSummary, error) {
	wanted := enumTexts(statuses)
	if len(wanted) == 0 {
		wanted = []string{"selecting", "running", "waiting", "parked"}
	}
	q := New(s.pool)
	rows, err := q.ListRuns(ctx, ListRunsParams{Statuses: wanted, Project: nullable(project)})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	summaries := make([]*millv1.RunSummary, 0, len(rows))
	for _, row := range rows {
		summary, err := summarize(ctx, q, row)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// summarize derives one run's summary.
func summarize(ctx context.Context, q *Queries, row Run) (*millv1.RunSummary, error) {
	state, err := latestState(ctx, q, row)
	if err != nil {
		return nil, err
	}
	var workflow *millv1.Workflow
	if row.WorkflowID != nil {
		workflowRow, err := q.GetWorkflowByID(ctx, *row.WorkflowID)
		if err != nil {
			return nil, fmt.Errorf("get workflow %s: %w", *row.WorkflowID, err)
		}
		if workflow, err = workflowProto(workflowRow); err != nil {
			return nil, err
		}
	}
	totals, err := q.TotalsForRun(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("usage totals of run %s: %w", row.ID, err)
	}
	leases, err := q.CountRunLeases(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("lease count of run %s: %w", row.ID, err)
	}
	currentStep, attempt := CurrentStep(state, workflow)
	summary := &millv1.RunSummary{
		Run:            state.Run,
		CurrentStep:    currentStep,
		CurrentAttempt: attempt,
		Totals:         make([]*millv1.ModelTotals, 0, len(totals)),
		Leases:         leases,
	}
	for _, total := range totals {
		summary.Totals = append(summary.Totals, &millv1.ModelTotals{
			Model:            total.Model,
			InputTokens:      total.InputTokens,
			OutputTokens:     total.OutputTokens,
			CacheReadTokens:  total.CacheReadTokens,
			CacheWriteTokens: total.CacheWriteTokens,
		})
	}
	return summary, nil
}

// RunDetail is one run in full: what `mill inspect` renders.
type RunDetail struct {
	// Summary is the run and what the ledger derives about it.
	Summary *millv1.RunSummary
	// Workflow is the version the run follows, nil while selecting.
	Workflow *millv1.Workflow
	// Steps is every attempt in order, with its evidence, session and cost.
	Steps []*millv1.StepRunDetail
}

// InspectParams names the run to inspect.
type InspectParams struct {
	// ID is the run's id. When set, NodeRef is ignored.
	ID string
	// NodeRef is the node's display key, for a human who has that instead.
	NodeRef string
}

// Inspect returns one run's every attempt in order with its evidence, exit,
// session, and usage.
func (s *Store) Inspect(ctx context.Context, p InspectParams) (*RunDetail, error) {
	q := New(s.pool)
	var row Run
	var err error
	switch {
	case p.ID != "":
		row, err = q.GetRun(ctx, p.ID)
	case p.NodeRef != "":
		row, err = q.GetRunByNodeRef(ctx, p.NodeRef)
	default:
		return nil, fmt.Errorf("inspect needs a run id or a node ref: %w", ErrNotFound)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s%s: %w", p.ID, p.NodeRef, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}

	summary, err := summarize(ctx, q, row)
	if err != nil {
		return nil, err
	}
	detail := &RunDetail{Summary: summary}
	if row.WorkflowID != nil {
		workflowRow, err := q.GetWorkflowByID(ctx, *row.WorkflowID)
		if err != nil {
			return nil, fmt.Errorf("get workflow %s: %w", *row.WorkflowID, err)
		}
		if detail.Workflow, err = workflowProto(workflowRow); err != nil {
			return nil, err
		}
	}

	sessionRows, err := q.ListSessionsForRun(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("sessions of run %s: %w", row.ID, err)
	}
	sessions := make(map[string]*millv1.Session, len(sessionRows))
	for _, sessionRow := range sessionRows {
		session, err := sessionProto(sessionRow)
		if err != nil {
			return nil, err
		}
		sessions[session.GetId()] = session
	}

	stepRows, err := q.ListStepRuns(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("step runs of run %s: %w", row.ID, err)
	}
	for _, stepRow := range stepRows {
		stepRun, err := stepRunProto(stepRow)
		if err != nil {
			return nil, err
		}
		step := &millv1.StepRunDetail{StepRun: stepRun, Session: sessions[stepRun.GetSessionId()]}
		evidenceRow, err := q.GetEvidence(ctx, stepRow.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("evidence of step run %s: %w", stepRow.ID, err)
		default:
			if step.Evidence, err = evidenceProto(evidenceRow); err != nil {
				return nil, err
			}
		}
		totals, err := q.TotalsForStepRun(ctx, stepRow.ID)
		if err != nil {
			return nil, fmt.Errorf("usage totals of step run %s: %w", stepRow.ID, err)
		}
		step.Totals = make([]*millv1.ModelTotals, 0, len(totals))
		for _, total := range totals {
			step.Totals = append(step.Totals, &millv1.ModelTotals{
				Model:            total.Model,
				InputTokens:      total.InputTokens,
				OutputTokens:     total.OutputTokens,
				CacheReadTokens:  total.CacheReadTokens,
				CacheWriteTokens: total.CacheWriteTokens,
			})
		}
		detail.Steps = append(detail.Steps, step)
	}
	return detail, nil
}

// Events returns a run's audit trail, oldest first: what
// `mill status --history` reads.
func (s *Store) Events(ctx context.Context, runID string) ([]Event, error) {
	rows, err := New(s.pool).ListEvents(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("events of run %s: %w", runID, err)
	}
	return rows, nil
}
