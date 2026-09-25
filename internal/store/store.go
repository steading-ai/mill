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
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/store/migrations"
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
	// ErrLeased: an operator's write to a run a worker is holding. The
	// wrapping error names the holder, because what the operator has to do
	// about it is stop that worker.
	ErrLeased = errors.New("run is held under a live lease")
	// ErrNotAttempted: a step the run has never made an attempt at. The
	// wrapping error names the steps it has.
	ErrNotAttempted = errors.New("run has no attempt at that step")
)

// claimableStatuses are the run statuses a new claim may take over, from
// Invariant 9: selecting, running with an expired or absent lease, waiting,
// parked, or done with the node still open. Whether a lease has expired is
// dagger's judgement, not the ledger's — dagger issues a new lease only when
// the old one is gone — so a status is admitted on the strength of the
// caller holding a lease dagger just granted.
//
// `selecting` is among them for the same reason `running` is. A run is
// created holding the lease of the claim that created it, so ordinarily
// there is nothing for a later SetLease to take over; but a worker that died
// between CreateRun and the selection left the run exactly there, holding a
// lease dagger has since expired, and refusing that claim leaves the node
// unrecoverable by any mill command. A selecting run is never a reason to
// refuse a claim: dagger has already decided who holds the node.
var claimableStatuses = map[string]bool{
	"selecting": true,
	"running":   true,
	"waiting":   true,
	"parked":    true,
	"done":      true,
}

// claimable admits a run a new claim may take over.
func claimable(state fenceState) bool {
	return claimableStatuses[state.status]
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

// NextGeneration is the generation a new run on the node takes: one past
// the newest run it has had, abandoned included, and 1 for a node that has
// never run. It is what CreateRun must be given after a restart, and what
// a claim that finds no live run gives it.
func (s *Store) NextGeneration(ctx context.Context, nodeID string) (int64, error) {
	next, err := New(s.pool).NextGenerationByNode(ctx, nodeID)
	if err != nil {
		return 0, fmt.Errorf("next generation for node %s: %w", nodeID, err)
	}
	return int64(next), nil
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
		if !claimable(state) {
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

// RecordNotice appends what carrying a filed gate to a person did. It
// changes no run state: the notification is the mill's own errand, and the
// only trace it leaves is the row of the audit trail written here. It is
// fenced like every other write, which is why it is called while the gate's
// lease is still held rather than after the run has been parked or set
// waiting (docs/ledger.md §events).
func (s *Store) RecordNotice(ctx context.Context, leaseID, runID, stepRunID, kind, gateRef string, exitCode int32, detail string) error {
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, runID, leaseID); err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     runID,
			stepRunID: stepRunID,
			op:        "notified",
			after: map[string]any{
				"kind":      kind,
				"gate":      gateRef,
				"exit_code": exitCode,
				"detail":    detail,
			},
		})
	})
}

// RecordInstall appends what a session installed into its machine with the
// `dnf` shim. It changes no run state: the install is the session's own
// errand and the ephemeral root it went into dies with the run, so the row
// of the audit trail is the whole record — the run's provenance of a package
// `mill.toml` does not declare (docs/sandbox.md §"The dnf shim"). Fenced by
// the lease like every other write.
func (s *Store) RecordInstall(ctx context.Context, leaseID, runID, stepRunID string, packages []string) error {
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, runID, leaseID); err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     runID,
			stepRunID: stepRunID,
			op:        "installed",
			after: map[string]any{
				"packages": packages,
				"detail":   InstallDetail(packages),
			},
		})
	})
}

// InstallDetail is the line the ledger keeps for an undeclared install, in
// the words docs/sandbox.md §"The dnf shim" fixes.
func InstallDetail(packages []string) string {
	return "installed " + strings.Join(packages, ", ") + ", not declared in mill.toml"
}

// RecordHeartbeat appends one heartbeat the harness sent a session that had
// gone quiet. Like a notice it changes no run state — the wait is the
// harness's business and the row of the audit trail is the whole record —
// and like every other write it is fenced by the lease the caller holds.
func (s *Store) RecordHeartbeat(ctx context.Context, leaseID, runID, stepRunID string, strike, strikes int32, waitingSeconds int64, tasks []string) error {
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, runID, leaseID); err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     runID,
			stepRunID: stepRunID,
			op:        "session_heartbeat",
			after: map[string]any{
				"strike":          strike,
				"strikes":         strikes,
				"waiting_seconds": waitingSeconds,
				"tasks":           tasks,
			},
		})
	})
}

// ReconciliationFinding is one line of a reconciliation pass, as the
// `findings` column holds it.
type ReconciliationFinding struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RecordReconciliation appends one reconciliation pass: what a worker found
// and did at start, before it claimed anything. Nothing fences it — the
// pass runs before the worker holds a lease, and it is a record of the host
// rather than of a run — and it changes no run state (docs/sandbox.md
// §Reconciliation).
func (s *Store) RecordReconciliation(ctx context.Context, workerRef string, startedAt time.Time, findings []ReconciliationFinding) error {
	if findings == nil {
		findings = []ReconciliationFinding{}
	}
	encoded, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("encode reconciliation findings: %w", err)
	}
	if _, err := New(s.pool).InsertReconciliation(ctx, InsertReconciliationParams{
		ID:        newULID(),
		WorkerRef: workerRef,
		StartedAt: startedAt,
		Findings:  encoded,
	}); err != nil {
		return fmt.Errorf("record reconciliation: %w", err)
	}
	return nil
}

// ReconciliationPass is one recorded pass, its findings decoded.
type ReconciliationPass struct {
	ID         string
	WorkerRef  string
	StartedAt  time.Time
	RecordedAt time.Time
	Findings   []ReconciliationFinding
}

// Reconciliations is the newest passes recorded, newest first.
func (s *Store) Reconciliations(ctx context.Context, limit int) ([]ReconciliationPass, error) {
	rows, err := New(s.pool).ListReconciliations(ctx, int32(limit)) //nolint:gosec // a page size, never large
	if err != nil {
		return nil, fmt.Errorf("list reconciliations: %w", err)
	}
	out := make([]ReconciliationPass, 0, len(rows))
	for _, row := range rows {
		var findings []ReconciliationFinding
		if err := json.Unmarshal(row.Findings, &findings); err != nil {
			return nil, fmt.Errorf("decode reconciliation %s: %w", row.ID, err)
		}
		out = append(out, ReconciliationPass{
			ID: row.ID, WorkerRef: row.WorkerRef, StartedAt: row.StartedAt, RecordedAt: row.RecordedAt, Findings: findings,
		})
	}
	return out, nil
}

// ClearGate takes the answered gate off a run and leaves it running, which
// is what lets the next gate step file one of its own (Invariant 12).
//
// Closing a gate step already does this in the same transaction that records
// its evidence. This is the other case: a run the circuit breaker parked,
// whose current step is not a gate step at all, so nothing else would ever
// clear the park gate.
func (s *Store) ClearGate(ctx context.Context, leaseID, runID string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := fence(ctx, tx, runID, leaseID)
		if err != nil {
			return err
		}
		row, err := q.ClearRunGate(ctx, runID)
		if err != nil {
			return fmt.Errorf("clear gate of run %s: %w", runID, err)
		}
		run = runProto(row)
		return record(ctx, q, event{
			runID:     runID,
			op:        "resume",
			before:    map[string]any{"status": state.status},
			after:     map[string]any{"status": row.Status, "gate_node_id": nil},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// SetCompleteRequestID mints the idempotency key of the patch that completes
// the node, or returns the one the run already carries. It is called before
// that patch is sent, so that a worker resuming a run whose patch may have
// landed retries the same patch rather than sending a second one
// (docs/ledger.md §Resume, Invariant 11).
func (s *Store) SetCompleteRequestID(ctx context.Context, leaseID, runID, requestID string) (*millv1.Run, error) {
	var run *millv1.Run
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		if _, err := fence(ctx, tx, runID, leaseID); err != nil {
			return err
		}
		row, err := q.SetRunCompleteRequestID(ctx, SetRunCompleteRequestIDParams{
			ID:                runID,
			CompleteRequestID: requestID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("set complete request id of run %s: %w", runID, err)
		}
		run = runProto(row)
		return nil
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
	// BaseCommit is the head of the run's branch as the attempt starts.
	BaseCommit string
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
			BaseCommit:   p.BaseCommit,
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

// RecordStepEnvironmentParams records what an attempt's session process was
// given of the mill's own environment.
type RecordStepEnvironmentParams struct {
	// StepRunID is the attempt whose session was spawned.
	StepRunID string
	// EnvPassthrough are the variables the step asked to have carried.
	EnvPassthrough []string
	// EnvStripped are the deny-listed variables that were present and
	// removed.
	EnvStripped []string
}

// RecordStepEnvironment writes what the environment mode gave an attempt's
// session process. It is a record of a decision already made, not a setting:
// the adapter has decided and spawned by the time this is called, so the
// write neither gates the session nor changes it.
func (s *Store) RecordStepEnvironment(ctx context.Context, leaseID string, p RecordStepEnvironmentParams) (*millv1.StepRun, error) {
	var stepRun *millv1.StepRun
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
		row, err := q.RecordStepRunEnvironment(ctx, RecordStepRunEnvironmentParams{
			ID: p.StepRunID,
			// The columns are arrays and not nullable: a step that asked
			// for nothing asked for nothing, which is the empty list rather
			// than the absence of an answer.
			EnvPassthrough: names(p.EnvPassthrough),
			EnvStripped:    names(p.EnvStripped),
		})
		if err != nil {
			return fmt.Errorf("record the environment of step run %s: %w", p.StepRunID, err)
		}
		stepRun, err = stepRunProto(row)
		if err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     row.RunID,
			stepRunID: row.ID,
			op:        "record_step_environment",
			after: map[string]any{
				"env_passthrough": row.EnvPassthrough,
				"env_stripped":    row.EnvStripped,
			},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return stepRun, nil
}

// MarkSessionStarted records that the harness has produced an event in an
// attempt's session. The fact is the ledger's, not the transcript's: which
// of Start and Resume the next attempt at the session asks for is decided
// from it, and an attempt that died before the harness said anything has no
// conversation for a Resume to find.
//
// It is written when the first event arrives rather than when the turn ends,
// so a worker killed mid-turn leaves the fact behind. A second call is not
// an error: the column is set to the value it already has.
func (s *Store) MarkSessionStarted(ctx context.Context, leaseID, stepRunID string) (*millv1.StepRun, error) {
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
		if open.SessionStarted {
			stepRun, err = stepRunProto(open)
			return err
		}
		row, err := q.MarkStepRunSessionStarted(ctx, stepRunID)
		if err != nil {
			return fmt.Errorf("mark the session of step run %s started: %w", stepRunID, err)
		}
		stepRun, err = stepRunProto(row)
		if err != nil {
			return err
		}
		return record(ctx, q, event{
			runID:     row.RunID,
			stepRunID: row.ID,
			op:        "session_started",
			after:     map[string]any{"session_id": text(row.SessionID)},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return stepRun, nil
}

// names is a list of variable names as the column stores it: never nil, so
// a nil slice from a caller is an empty array and not a null.
func names(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
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
		} else if !claimable(state) {
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

// RerunResult is what a rerun undid: the run it was asked of, the step it
// was on, and the attempts that no longer close anything.
type RerunResult struct {
	// Run is the run as the rerun leaves it.
	Run *millv1.Run
	// ThroughStep is the last step of the span: the step the run was on.
	ThroughStep string
	// Superseded is the attempt superseded at each step of the span, in
	// workflow order.
	Superseded []*millv1.StepRun
	// Reopened says the run had ended and is live again. Only a `failed`
	// run is reopened; every other rerun leaves the run's own row alone.
	Reopened bool
}

// Rerun supersedes the latest attempt at every step from fromStep through
// the run's current step, so the next claim resumes at fromStep. It is the
// revise mechanism driven by a person rather than by a verdict: the same
// rows move the same way, and the generation, the branch and the sessions
// are left alone. Restart is the verb that replaces those.
//
// The caller holds no lease, so the row lock alone serializes this against a
// concurrent claim — and a run that holds a live lease is refused outright,
// because superseding the span under a worker that is running one of its
// steps would have the worker close a step the ledger has already put
// behind it.
//
// A step the run has never attempted is refused rather than accepted as a
// no-op: `--from` naming a step nothing ran is an operator's mistake about
// where the run got to, and the answer they need is which steps it did run.
//
// A `failed` run is reopened as part of the same transaction. Failure is
// terminal to the run loop — nothing mill does on its own leaves that state
// — but a rerun is a person saying the ending was wrong, and a span
// superseded on a run no claim can take over would leave the node stuck for
// good. The run goes back to `running` holding no lease, which is the state
// a claim resumes (Invariant 9), and the gate and the park reason of the
// ending it left are cleared. `abandoned` is not reopened: a later
// generation has taken the node, and reviving the old one would put two
// live runs on it.
func (s *Store) Rerun(ctx context.Context, runID, fromStep, requestedBy, reason string) (*RerunResult, error) {
	if reason == "" {
		reason = "rerun"
	}
	var result *RerunResult
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		state, err := lockRunRow(ctx, tx, runID)
		if err != nil {
			return err
		}
		row, err := q.GetRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("get run %s: %w", runID, err)
		}
		if leaseText(state.lease) != "none" {
			return fmt.Errorf("run %s is %s under lease %s held by %s: %w",
				runID, state.status, leaseText(state.lease), workerText(row.WorkerRef), ErrLeased)
		}
		workflow, err := workflowOfRun(ctx, q, runID)
		if err != nil {
			return err
		}
		run, err := latestState(ctx, q, row)
		if err != nil {
			return err
		}
		if _, attempted := run.Latest[fromStep]; !attempted {
			return fmt.Errorf("run %s: step %q: %w; it has attempted %s",
				runID, fromStep, ErrNotAttempted, attemptedText(workflow, run))
		}
		through, _ := CurrentStep(run, workflow)
		if through == "" {
			// Every step is closed: the run is at its end, and the span
			// runs through the last step the workflow names.
			steps := workflow.GetSteps()
			through = steps[len(steps)-1].GetName()
		}
		span, err := stepSpan(workflow, fromStep, through)
		if err != nil {
			return err
		}
		rows, err := q.SupersedeStepRuns(ctx, SupersedeStepRunsParams{RunID: runID, StepNames: span})
		if err != nil {
			return fmt.Errorf("rerun steps of run %s: %w", runID, err)
		}
		for _, stepRunRow := range rows {
			if err := record(ctx, q, event{
				runID:     runID,
				stepRunID: stepRunRow.ID,
				op:        "supersede_step_run",
				after: map[string]any{
					"status": stepRunRow.Status, "from": fromStep, "through": through,
					"reason": reason, "requested_by": requestedBy,
				},
				workerRef: stepRunRow.WorkerRef,
			}); err != nil {
				return err
			}
		}
		superseded, err := stepRunProtos(rows)
		if err != nil {
			return err
		}
		sortBySpan(superseded, span)
		reopened := state.status == "failed"
		if reopened {
			if err := noLiveRun(ctx, q, row); err != nil {
				return err
			}
			if row, err = q.ReopenRun(ctx, runID); err != nil {
				return fmt.Errorf("reopen run %s: %w", runID, err)
			}
			if err := record(ctx, q, event{
				runID:     runID,
				op:        "reopen",
				before:    map[string]any{"status": state.status},
				after:     map[string]any{"status": row.Status, "step": fromStep, "requested_by": requestedBy},
				workerRef: row.WorkerRef,
			}); err != nil {
				return err
			}
		}
		result = &RerunResult{
			Run: runProto(row), ThroughStep: through, Superseded: superseded, Reopened: reopened,
		}
		return record(ctx, q, event{
			runID:  runID,
			op:     "rerun",
			before: map[string]any{"status": state.status, "step": through},
			after: map[string]any{
				"from": fromStep, "through": through, "steps": span,
				"reason": reason, "requested_by": requestedBy, "status": row.Status,
			},
			workerRef: row.WorkerRef,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// noLiveRun refuses to reopen a run whose node another generation is
// already working. Invariant 1 admits one live run per node, and the unique
// index holds it; catching the case here names the generation in the way an
// operator can act on.
func noLiveRun(ctx context.Context, q *Queries, row Run) error {
	live, err := q.GetRunByNode(ctx, row.NodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get live run of node %s: %w", row.NodeID, err)
	}
	if live.ID == row.ID || terminalStatuses[live.Status] {
		return nil
	}
	return fmt.Errorf("run %s cannot be reopened: generation %d of %s is %s: %w",
		row.ID, live.Generation, row.NodeRef, live.Status, ErrNotClaimable)
}

// terminalStatuses are the run statuses that have left the live-run index.
var terminalStatuses = map[string]bool{"done": true, "failed": true, "abandoned": true}

// attemptedText is the steps a run has an attempt at, in workflow order, for
// the refusal of a `--from` naming one it does not.
func attemptedText(workflow *millv1.Workflow, run RunState) string {
	var names []string
	for _, step := range workflow.GetSteps() {
		if _, ok := run.Latest[step.GetName()]; ok {
			names = append(names, step.GetName())
		}
	}
	if len(names) == 0 {
		return "no step"
	}
	return strings.Join(names, ", ")
}

// sortBySpan puts superseded attempts in workflow order, which is the order
// an operator reads them in and the order the span was given in.
func sortBySpan(stepRuns []*millv1.StepRun, span []string) {
	order := make(map[string]int, len(span))
	for i, name := range span {
		order[name] = i
	}
	sort.SliceStable(stepRuns, func(i, j int) bool {
		return order[stepRuns[i].GetStepName()] < order[stepRuns[j].GetStepName()]
	})
}

// workerText names the holder of a lease for a refusal, or says there is
// none recorded.
func workerText(workerRef string) string {
	if workerRef == "" {
		return "an unnamed worker"
	}
	return workerRef
}

// stepSpan is the step names from fromStep through throughStep inclusive,
// in workflow order.
func stepSpan(workflow *millv1.Workflow, fromStep, throughStep string) ([]string, error) {
	from, through := -1, -1
	names := make([]string, 0, len(workflow.GetSteps()))
	for i, step := range workflow.GetSteps() {
		names = append(names, step.GetName())
		// Both ends may be the same step: a rerun from the step a run is
		// parked on is a span of one.
		if step.GetName() == fromStep {
			from = i
		}
		if step.GetName() == throughStep {
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
				Source:           usageSourceText(turn.GetSource()),
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

// SealAttempt records a closed attempt's index rows, once its frame and
// twin are written on disk. Idempotent: a row whose (step_run_id, seq) is
// already recorded is skipped, not replaced, so a seal retried after a
// lost response never double-counts. Not lease-fenced: activity is not one
// of Invariant 9's fenced tables, and a sealed attempt's own lease is
// already gone by the time a seal runs.
func (s *Store) SealAttempt(ctx context.Context, stepRunID string, rows []*millv1.ActivityIndexRow) ([]*millv1.ActivityIndexRow, error) {
	var sealed []*millv1.ActivityIndexRow
	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx, q *Queries) error {
		sealed = make([]*millv1.ActivityIndexRow, 0, len(rows))
		for _, r := range rows {
			row, err := q.InsertActivityIndex(ctx, InsertActivityIndexParams{
				StepRunID:    stepRunID,
				Seq:          r.GetSeq(),
				At:           r.GetAt().AsTime(),
				EndedAt:      unstamp(r.GetEndedAt()),
				Kind:         r.GetKind(),
				Source:       enumText(r.GetSource()),
				Turn:         r.GetTurn(),
				CallID:       nullable(r.GetCallId()),
				ParentCallID: nullable(r.GetParentCallId()),
				Tool:         nullable(r.GetTool()),
				Bytes:        r.GetBytes(),
				Truncated:    r.GetTruncated(),
				Offset:       r.GetOffset(),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("seal attempt %s: %w", stepRunID, err)
			}
			sealed = append(sealed, activityIndexRowProto(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// ActivityForStepRun lists a closed attempt's sealed index rows in seq
// order, for a Logs read of its frame.
func (s *Store) ActivityForStepRun(ctx context.Context, stepRunID string) ([]*millv1.ActivityIndexRow, error) {
	rows, err := New(s.pool).ListActivityForStepRun(ctx, stepRunID)
	if err != nil {
		return nil, fmt.Errorf("list activity for step run %s: %w", stepRunID, err)
	}
	out := make([]*millv1.ActivityIndexRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, activityIndexRowProto(row))
	}
	return out, nil
}

// ActivitySince lists every sealed index row since a point in time, across
// every run, in log order — the index-backed half of an every-run Logs
// read. The window is open-ended when since is nil.
func (s *Store) ActivitySince(ctx context.Context, since *time.Time) ([]*millv1.ActivityIndexRow, error) {
	rows, err := New(s.pool).ListActivitySince(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("list activity since: %w", err)
	}
	out := make([]*millv1.ActivityIndexRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, activityIndexRowProto(row))
	}
	return out, nil
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

// ListWorkflows lists workflow versions, newest first. A row that will not
// decode is left out of the listing and named in the findings, so one row
// another mill's schema wrote does not hide every other workflow; the row
// itself is not touched.
func (s *Store) ListWorkflows(ctx context.Context, p WorkflowFilter) ([]*millv1.Workflow, []UndecodableWorkflow, error) {
	rows, err := New(s.pool).ListWorkflows(ctx, ListWorkflowsParams{
		Name:    nullable(p.Name),
		Scope:   nullable(enumText(p.Scope)),
		Project: nullable(p.Project),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list workflows: %w", err)
	}
	// The query orders by name then newest first, so the first row of each
	// name is the newest version of it. An undecodable row still counts as
	// that name's newest: the listing says what is stored, and an older
	// version standing in for a newer one that cannot be read would not.
	seen := map[string]bool{}
	workflows := make([]*millv1.Workflow, 0, len(rows))
	var findings []UndecodableWorkflow
	for _, row := range rows {
		key := row.Name + "\x00" + row.Scope + "\x00" + row.Project
		if !p.AllVersions && seen[key] {
			continue
		}
		seen[key] = true
		workflow, err := workflowProto(row)
		if err != nil {
			findings = append(findings, undecodable(row, err))
			continue
		}
		workflows = append(workflows, workflow)
	}
	return workflows, findings, nil
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
			ID:          newULID(),
			Name:        skill.GetName(),
			Digest:      digest,
			Description: skill.GetDescription(),
			Body:        skill.GetBody(),
			Files:       files,
			Metadata:    metadata,
			Source:      source,
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

// GetSkill returns the skill a workflow step pins, by name and digest. A
// step carries only the reference, so reading the prompt back out — which
// is what `mill workflow pull` does — needs the row itself.
func (s *Store) GetSkill(ctx context.Context, name, digest string) (*millv1.Skill, error) {
	row, err := New(s.pool).GetSkillByDigest(ctx, GetSkillByDigestParams{Name: name, Digest: digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("skill %s@%s: %w", name, digest, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get skill %s: %w", name, err)
	}
	return skillProto(row)
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
//
// A run whose workflow row will not decode is still listed — the run is the
// ledger's, whatever its definition's bytes say — with no current step,
// since the step is derived from the definition, and the row is named once
// in the findings.
func (s *Store) Status(ctx context.Context, statuses []millv1.RunStatus, project string) ([]*millv1.RunSummary, []UndecodableWorkflow, error) {
	wanted := enumTexts(statuses)
	if len(wanted) == 0 {
		wanted = []string{"selecting", "running", "waiting", "parked"}
	}
	q := New(s.pool)
	rows, err := q.ListRuns(ctx, ListRunsParams{Statuses: wanted, Project: nullable(project)})
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	summaries := make([]*millv1.RunSummary, 0, len(rows))
	var findings []UndecodableWorkflow
	reported := map[string]bool{}
	for _, row := range rows {
		summary, finding, err := summarize(ctx, q, row)
		if err != nil {
			return nil, nil, err
		}
		if finding != nil && !reported[finding.WorkflowID] {
			reported[finding.WorkflowID] = true
			findings = append(findings, *finding)
		}
		summaries = append(summaries, summary)
	}
	return summaries, findings, nil
}

// listRunsDefaultLimit and listRunsMaxLimit are ListRuns's page-size
// default and ceiling, matching the work order's acceptance criterion.
const (
	listRunsDefaultLimit = 50
	listRunsMaxLimit     = 200
)

// ListRunsPageParams is the page ListRuns asks for. Its type is not named
// ListRunsParams because that name already belongs to the sqlc query behind
// [Store.Status] (internal/store/queries/runs.sql).
type ListRunsPageParams struct {
	// Statuses restricts the listing to these run states. Empty means every
	// terminal state (done, failed, abandoned).
	Statuses []millv1.RunStatus
	// Project, NodeRef, WorkerRef, WorkflowID narrow the listing the same
	// way Status's Project does; empty means unrestricted.
	Project    string
	NodeRef    string
	WorkerRef  string
	WorkflowID string
	// EndedAfter and EndedBefore bound the window on ended_at. Nil means no
	// bound. Ignored when the statuses filter is not all-terminal.
	EndedAfter  *time.Time
	EndedBefore *time.Time
	// Limit is how many rows to return, before clamping. 0 means the
	// default.
	Limit int32
	// Cursor replays a previous response's NextCursor. Empty asks for the
	// first page.
	Cursor string
	// View is "refs", "summary", or "full"; empty means "summary".
	View string
}

// ListRuns returns one page of runs, filtered and windowed, ordered
// newest-first by ended_at when every requested status is terminal (or the
// filter is empty and so defaults to the three terminal states), or by
// updated_at when any requested status is not — a live run has no ended_at
// to order by. nextCursor is empty when this page was the last, and
// clamped reports whether the request's limit exceeded the 200 ceiling.
func (s *Store) ListRuns(ctx context.Context, p ListRunsPageParams) (runs []*millv1.RunSummary, nextCursor string, clamped bool, findings []UndecodableWorkflow, err error) {
	limit := p.Limit
	switch {
	case limit <= 0:
		limit = listRunsDefaultLimit
	case limit > listRunsMaxLimit:
		limit = listRunsMaxLimit
		clamped = true
	}
	cursorValue, cursorID, err := decodeRunsCursor(p.Cursor)
	if err != nil {
		return nil, "", false, nil, err
	}

	wanted := enumTexts(p.Statuses)
	if len(wanted) == 0 {
		wanted = []string{"done", "failed", "abandoned"}
	}
	q := New(s.pool)
	var rows []Run
	byEnded := allTerminal(wanted)
	if byEnded {
		var cursorEndedAt *time.Time
		var cursorRowID *string
		if p.Cursor != "" {
			cursorEndedAt, cursorRowID = &cursorValue, &cursorID
		}
		rows, err = q.ListRunsByEnded(ctx, ListRunsByEndedParams{
			Statuses:      wanted,
			Project:       nullable(p.Project),
			NodeRef:       nullable(p.NodeRef),
			WorkerRef:     nullable(p.WorkerRef),
			WorkflowID:    nullable(p.WorkflowID),
			EndedAfter:    p.EndedAfter,
			EndedBefore:   p.EndedBefore,
			CursorEndedAt: cursorEndedAt,
			CursorID:      cursorRowID,
			Lim:           limit + 1,
		})
	} else {
		var cursorUpdatedAt *time.Time
		var cursorRowID *string
		if p.Cursor != "" {
			cursorUpdatedAt, cursorRowID = &cursorValue, &cursorID
		}
		rows, err = q.ListRunsByUpdated(ctx, ListRunsByUpdatedParams{
			Statuses:        wanted,
			Project:         nullable(p.Project),
			NodeRef:         nullable(p.NodeRef),
			WorkerRef:       nullable(p.WorkerRef),
			WorkflowID:      nullable(p.WorkflowID),
			CursorUpdatedAt: cursorUpdatedAt,
			CursorID:        cursorRowID,
			Lim:             limit + 1,
		})
	}
	if err != nil {
		return nil, "", false, nil, fmt.Errorf("list runs: %w", err)
	}

	if int32(len(rows)) > limit {
		last := rows[limit-1]
		if byEnded {
			nextCursor = encodeRunsCursor(*last.EndedAt, last.ID)
		} else {
			nextCursor = encodeRunsCursor(last.UpdatedAt, last.ID)
		}
		rows = rows[:limit]
	}

	summaries := make([]*millv1.RunSummary, 0, len(rows))
	reported := map[string]bool{}
	for _, row := range rows {
		if p.View == "refs" {
			summaries = append(summaries, &millv1.RunSummary{Run: runProto(row)})
			continue
		}
		summary, finding, err := summarize(ctx, q, row)
		if err != nil {
			return nil, "", false, nil, err
		}
		if finding != nil && !reported[finding.WorkflowID] {
			reported[finding.WorkflowID] = true
			findings = append(findings, *finding)
		}
		summaries = append(summaries, summary)
	}
	return summaries, nextCursor, clamped, findings, nil
}

// allTerminal reports whether every status in a ListRuns request orders by
// ended_at: true for the empty (default) filter, since that defaults to the
// three terminal states.
func allTerminal(statuses []string) bool {
	for _, status := range statuses {
		if !terminalStatuses[status] {
			return false
		}
	}
	return true
}

// runsCursorSep separates a ListRuns cursor's sort value from its row id.
// Neither an RFC3339Nano timestamp nor a ULID contains it.
const runsCursorSep = "|"

// encodeRunsCursor renders ListRuns's opaque cursor: the sort column's value
// and the row's id, which together are what its keyset predicate compares
// the next page against.
func encodeRunsCursor(sortValue time.Time, id string) string {
	raw := sortValue.UTC().Format(time.RFC3339Nano) + runsCursorSep + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeRunsCursor reads a cursor back. Empty is the first page and decodes
// to the zero value with no error.
func decodeRunsCursor(cursor string) (sortValue time.Time, id string, err error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decode cursor: %w", err)
	}
	sortText, id, ok := strings.Cut(string(raw), runsCursorSep)
	if !ok {
		return time.Time{}, "", fmt.Errorf("decode cursor: malformed")
	}
	sortValue, err = time.Parse(time.RFC3339Nano, sortText)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decode cursor: %w", err)
	}
	return sortValue, id, nil
}

// summarize derives one run's summary. The finding is set when the run's
// workflow row would not decode, and the summary is then derived without
// it.
func summarize(ctx context.Context, q *Queries, row Run) (*millv1.RunSummary, *UndecodableWorkflow, error) {
	state, err := latestState(ctx, q, row)
	if err != nil {
		return nil, nil, err
	}
	var workflow *millv1.Workflow
	var finding *UndecodableWorkflow
	if row.WorkflowID != nil {
		workflowRow, err := q.GetWorkflowByID(ctx, *row.WorkflowID)
		if err != nil {
			return nil, nil, fmt.Errorf("get workflow %s: %w", *row.WorkflowID, err)
		}
		if workflow, err = workflowProto(workflowRow); err != nil {
			found := undecodable(workflowRow, err)
			finding = &found
		}
	}
	totals, err := q.TotalsForRun(ctx, row.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("usage totals of run %s: %w", row.ID, err)
	}
	leases, err := q.CountRunLeases(ctx, row.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("lease count of run %s: %w", row.ID, err)
	}
	workspace, err := latestWorkspace(ctx, q, row.ID)
	if err != nil {
		return nil, nil, err
	}
	feedback, err := q.CountRunFeedback(ctx, row.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("feedback count of run %s: %w", row.ID, err)
	}
	currentStep, attempt := CurrentStep(state, workflow)
	lastStep, lastStepStatus := LastStep(state)
	summary := &millv1.RunSummary{
		Run:            state.Run,
		CurrentStep:    currentStep,
		CurrentAttempt: attempt,
		Totals:         make([]*millv1.ModelTotals, 0, len(totals)),
		Leases:         leases,
		Workspace:      workspace,
		LastStep:       lastStep,
		LastStepStatus: lastStepStatus,
		Rounds:         feedback + 1,
	}
	// The run's cost is what its sessions reported. The gateway's count is
	// read per attempt, beside the adapter's, and never summed into it.
	for _, total := range totals {
		if total.Source == usageSourceGateway {
			continue
		}
		summary.Totals = append(summary.Totals, &millv1.ModelTotals{
			Model:            total.Model,
			Source:           usageSourceProto(total.Source),
			InputTokens:      total.InputTokens,
			OutputTokens:     total.OutputTokens,
			CacheReadTokens:  total.CacheReadTokens,
			CacheWriteTokens: total.CacheWriteTokens,
		})
	}
	return summary, finding, nil
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

	// One run's own workflow failing to decode is not a finding beside
	// the answer here: the definition is what an inspection is of, and the
	// decode below refuses with the row named.
	summary, _, err := summarize(ctx, q, row)
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
	// How many times each attempt's session was asked about a task that had
	// gone quiet. The rows of the audit trail are the record, so the count
	// is read off them rather than kept on the attempt.
	beatRows, err := q.CountHeartbeats(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("heartbeats of run %s: %w", row.ID, err)
	}
	beats := make(map[string]int64, len(beatRows))
	for _, beatRow := range beatRows {
		if beatRow.StepRunID != nil {
			beats[*beatRow.StepRunID] = beatRow.Heartbeats
		}
	}
	for _, stepRow := range stepRows {
		stepRun, err := stepRunProto(stepRow)
		if err != nil {
			return nil, err
		}
		step := &millv1.StepRunDetail{
			StepRun:    stepRun,
			Session:    sessions[stepRun.GetSessionId()],
			Heartbeats: beats[stepRow.ID],
		}
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
			counts := &millv1.ModelTotals{
				Model:            total.Model,
				Source:           usageSourceProto(total.Source),
				InputTokens:      total.InputTokens,
				OutputTokens:     total.OutputTokens,
				CacheReadTokens:  total.CacheReadTokens,
				CacheWriteTokens: total.CacheWriteTokens,
			}
			// The adapter's rows are the attempt's cost; the gateway's are
			// the count taken outside the session, kept beside it.
			if total.Source == usageSourceGateway {
				step.GatewayTotals = append(step.GatewayTotals, counts)
				continue
			}
			step.Totals = append(step.Totals, counts)
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

// History returns a run's audit trail on the wire, resolving the run the way
// [Store.Inspect] does so a caller with a node reference and no run id can
// ask for it (docs/ledger.md §"Queries the CLI needs").
func (s *Store) History(ctx context.Context, p InspectParams) ([]*millv1.Event, error) {
	q := New(s.pool)
	var row Run
	var err error
	switch {
	case p.ID != "":
		row, err = q.GetRun(ctx, p.ID)
	case p.NodeRef != "":
		row, err = q.GetRunByNodeRef(ctx, p.NodeRef)
	default:
		return nil, fmt.Errorf("history needs a run id or a node ref: %w", ErrNotFound)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s%s: %w", p.ID, p.NodeRef, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	rows, err := q.ListEvents(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("events of run %s: %w", row.ID, err)
	}
	events := make([]*millv1.Event, 0, len(rows))
	for _, eventRow := range rows {
		event, err := eventProto(eventRow)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// LatestEventCursor is the position a fresh WatchRuns subscription opens
// at: the newest event already on record, by the database's own clock. The
// zero time and an empty id when the table holds no event yet.
func (s *Store) LatestEventCursor(ctx context.Context) (time.Time, string, error) {
	row, err := New(s.pool).LatestEventCursor(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("latest event cursor: %w", err)
	}
	return row.OccurredAt, row.ID, nil
}

// WatchableEvent is one events-table row as WatchRuns reads it: the wire
// RunEvent, plus the id the cursor advances by. events.id breaks ties
// within occurred_at (WatchableEventsRow.ID) but RunEvent's wire shape
// carries no more than a subscriber needs, so it travels alongside the
// proto rather than in it.
type WatchableEvent struct {
	ID    string
	Event *millv1.RunEvent
}

// WatchableEvents reads events across every run, from after (exclusive) in
// (occurred_at, id) order, optionally narrowed to one dagger project. What
// WatchRuns streams from a cursor. See docs/ledger.md §events.
func (s *Store) WatchableEvents(ctx context.Context, after time.Time, afterID, project string) ([]WatchableEvent, error) {
	rows, err := New(s.pool).WatchableEvents(ctx, WatchableEventsParams{
		AfterOccurredAt: after, AfterID: afterID, Project: nullable(project),
	})
	if err != nil {
		return nil, fmt.Errorf("watchable events: %w", err)
	}
	out := make([]WatchableEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, WatchableEvent{ID: row.ID, Event: runEventProto(row)})
	}
	return out, nil
}

// CostParams is the window `mill cost` asks about.
type CostParams struct {
	// Since bounds the window below. Zero means every row ever recorded.
	Since time.Time
	// Until bounds the window above. Zero means no upper bound.
	Until time.Time
	// Project narrows to one dagger project. Empty means every project.
	Project string
	// Bucket says whether, and how, to also group the window over time.
	Bucket millv1.TimeBucket
}

// Cost is a window of usage rolled up four ways at once, plus the per-step
// funnel and, when asked, a day bucket. One pass over the rows answers "what
// did this run cost", "what does this workflow cost", "which step is
// expensive, and where does it die" and "which model billed", which is the
// whole of what docs/ledger.md §"Queries the CLI needs" asks of
// `cost --since`.
type Cost struct {
	// ByRun is keyed by node reference, ByWorkflow by name and version,
	// ByStep by workflow name, version and step name, ByModel by model,
	// ByDay by UTC calendar date. ByDay is empty unless CostParams.Bucket
	// asked for it.
	ByRun, ByWorkflow, ByStep, ByModel, ByDay []*millv1.CostBucket
}

// Cost rolls a window of usage up by run, workflow, step, model and,
// optionally, day, and folds in the per-step funnel. Two queries — usage is
// windowed on when it was recorded, the funnel on when its step runs
// started, because a check or gate step never bills usage and would
// otherwise have no funnel at all — but still the one pass over each this
// type's doc comment promises: nothing here re-reads a row.
func (s *Store) Cost(ctx context.Context, p CostParams) (*Cost, error) {
	q := New(s.pool)
	window := UsageSinceParams{Project: nullable(p.Project)}
	if !p.Since.IsZero() {
		since := p.Since
		window.Since = &since
	}
	if !p.Until.IsZero() {
		until := p.Until
		window.Until = &until
	}
	rows, err := q.UsageSince(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("usage rollup: %w", err)
	}
	byRun, byWorkflow, byStep, byModel, byDay := newRollup(), newRollup(), newRollup(), newRollup(), newRollup()
	for _, row := range rows {
		counts := &millv1.ModelTotals{
			Model:            row.Model,
			Source:           usageSourceProto(row.Source),
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
		}
		key := workflowKey(row.WorkflowName, row.WorkflowVersion)
		byRun.add(row.NodeRef, counts)
		byWorkflow.add(key, counts)
		byStep.add(key+"/"+row.StepName, counts)
		byModel.add(row.Model, counts)
		if p.Bucket == millv1.TimeBucket_TIME_BUCKET_DAY {
			byDay.add(dayKey(row.RecordedAt), counts)
		}
	}

	stepRows, err := q.StepRunsSince(ctx, StepRunsSinceParams(window))
	if err != nil {
		return nil, fmt.Errorf("step run funnel: %w", err)
	}

	cost := &Cost{
		ByRun:      byRun.buckets(),
		ByWorkflow: byWorkflow.buckets(),
		ByStep:     stepBuckets(byStep, foldFunnel(stepRows)),
		ByModel:    byModel.buckets(),
	}
	if p.Bucket == millv1.TimeBucket_TIME_BUCKET_DAY {
		cost.ByDay = byDay.buckets()
	}
	return cost, nil
}

// dayKey is a timestamp's UTC calendar day, the key `mill cost --bucket day`
// groups usage under.
func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// workflowKey names a workflow version: what usage and step runs are billed
// or attempted under. A run still selecting has none, and says so rather
// than rolling up under "".
func workflowKey(name, version *string) string {
	if name == nil {
		return "(selecting)"
	}
	return text(name) + "@" + short(text(version))
}

// foldFunnel turns a window's step runs into per-step counts: how many runs
// reached a step, passed it, died at it (its run's last-started step run,
// by started_at, whose run ended failed or abandoned — docs/domain-model.md
// on why the last attempt is not the same thing), needed more than one
// attempt, and how many revise rounds it answered, plus every attempt seen.
// Keyed the same way by_step's usage rollup is, so the two merge by key.
func foldFunnel(rows []StepRunsSinceRow) map[string]*millv1.StepCounts {
	counts := map[string]*millv1.StepCounts{}
	get := func(key string) *millv1.StepCounts {
		c, ok := counts[key]
		if !ok {
			c = &millv1.StepCounts{}
			counts[key] = c
		}
		return c
	}

	type attempts struct {
		count  int64
		closed bool
	}
	seen := map[string]map[string]*attempts{} // run id -> step key -> attempts
	last := map[string]StepRunsSinceRow{}     // run id -> its last-started row

	for _, row := range rows {
		key := workflowKey(row.WorkflowName, row.WorkflowVersion) + "/" + row.StepName
		byStep, ok := seen[row.RunID]
		if !ok {
			byStep = map[string]*attempts{}
			seen[row.RunID] = byStep
		}
		a, ok := byStep[key]
		if !ok {
			a = &attempts{}
			byStep[key] = a
		}
		a.count++
		if row.Status == "closed" {
			a.closed = true
		}
		if row.FeedbackFrom != nil && row.FeedbackFromStep != nil {
			target := workflowKey(row.WorkflowName, row.WorkflowVersion) + "/" + *row.FeedbackFromStep
			get(target).Revises++
		}
		last[row.RunID] = row
	}
	for _, byStep := range seen {
		for key, a := range byStep {
			c := get(key)
			c.Reached++
			if a.closed {
				c.Passed++
			}
			if a.count > 1 {
				c.Retried++
			}
			c.Attempts += a.count
		}
	}
	for _, row := range last {
		if row.RunStatus != "failed" && row.RunStatus != "abandoned" {
			continue
		}
		key := workflowKey(row.WorkflowName, row.WorkflowVersion) + "/" + row.StepName
		get(key).Died++
	}
	return counts
}

// stepBuckets merges by_step's usage rollup with its funnel counts, so a
// step with counts but no billed usage — a check or a gate, which never
// runs a model — still gets a bucket, rather than vanishing from the funnel
// the way it would if by_step were derived from usage rows alone.
func stepBuckets(usage *rollup, counts map[string]*millv1.StepCounts) []*millv1.CostBucket {
	buckets := usage.buckets()
	byKey := make(map[string]*millv1.CostBucket, len(buckets))
	for _, bucket := range buckets {
		byKey[bucket.GetKey()] = bucket
	}
	for key, c := range counts {
		bucket, ok := byKey[key]
		if !ok {
			bucket = &millv1.CostBucket{Key: key}
			buckets = append(buckets, bucket)
			byKey[key] = bucket
		}
		bucket.Counts = c
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].GetKey() < buckets[j].GetKey() })
	return buckets
}

// short is a content hash as a human reads it.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// rollup sums usage under a key, per model, keeping the keys sorted so two
// calls over the same rows render identically.
type rollup struct {
	order  []string
	totals map[string]map[string]*millv1.ModelTotals
}

func newRollup() *rollup {
	return &rollup{totals: map[string]map[string]*millv1.ModelTotals{}}
}

func (r *rollup) add(key string, counts *millv1.ModelTotals) {
	byModel, seen := r.totals[key]
	if !seen {
		byModel = map[string]*millv1.ModelTotals{}
		r.totals[key] = byModel
		r.order = append(r.order, key)
	}
	// One total per model and per who counted it, so the gateway's count
	// sits beside the adapter's rather than inside it.
	counted := counts.GetModel() + "\x00" + counts.GetSource().String()
	total, seen := byModel[counted]
	if !seen {
		total = &millv1.ModelTotals{Model: counts.GetModel(), Source: counts.GetSource()}
		byModel[counted] = total
	}
	total.InputTokens += counts.GetInputTokens()
	total.OutputTokens += counts.GetOutputTokens()
	total.CacheReadTokens += counts.GetCacheReadTokens()
	total.CacheWriteTokens += counts.GetCacheWriteTokens()
}

func (r *rollup) buckets() []*millv1.CostBucket {
	sort.Strings(r.order)
	out := make([]*millv1.CostBucket, 0, len(r.order))
	for _, key := range r.order {
		byModel := r.totals[key]
		models := make([]string, 0, len(byModel))
		for model := range byModel {
			models = append(models, model)
		}
		sort.Strings(models)
		bucket := &millv1.CostBucket{Key: key, Totals: make([]*millv1.ModelTotals, 0, len(models))}
		for _, model := range models {
			bucket.Totals = append(bucket.Totals, byModel[model])
		}
		sort.SliceStable(bucket.Totals, func(i, j int) bool {
			if bucket.Totals[i].GetModel() != bucket.Totals[j].GetModel() {
				return bucket.Totals[i].GetModel() < bucket.Totals[j].GetModel()
			}
			return bucket.Totals[i].GetSource() < bucket.Totals[j].GetSource()
		})
		out = append(out, bucket)
	}
	return out
}

// Health is what the store can say about itself: whether the database
// answers, and whether the schema is the one this binary carries.
type Health struct {
	// Database is whether the pool answered a ping.
	Database bool
	// Version is the newest migration applied, and Wanted the newest this
	// binary carries. They differ when a deploy has not migrated.
	Version, Wanted int64
	// Detail says what went wrong, empty when nothing did.
	Detail string
}

// Health probes the database and compares its schema version against the
// migrations this binary embeds. It never fails: an unreachable database is
// an answer, which is the whole point of a probe.
func (s *Store) Health(ctx context.Context) *Health {
	health := &Health{Wanted: migrations.Wanted()}
	if err := s.pool.Ping(ctx); err != nil {
		health.Detail = err.Error()
		return health
	}
	health.Database = true
	version, err := s.schemaVersion(ctx)
	if err != nil {
		health.Detail = err.Error()
		return health
	}
	health.Version = version
	return health
}

// schemaVersion reads goose's own bookkeeping table. It is not a sqlc query
// because the table is goose's, not the ledger's: no migration creates it and
// no other query touches it.
func (s *Store) schemaVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
