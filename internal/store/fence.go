package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrFenced is what a write presenting a lease that is not the run's
// current one fails with. It is the normal path for a stale worker, not an
// error to retry: the worker abandons the run locally and goes back to
// ClaimNext. See docs/ledger.md Invariant 9.
var ErrFenced = errors.New("write is fenced: the presented lease is not the run's current lease")

// fenceState is what the fence read back about the run it locked.
type fenceState struct {
	// lease is the run's current lease, or nil when it holds none.
	lease *string
	// status is the run's status at the moment of the read.
	status string
}

// fence admits a write to a run. It takes the run's row lock, re-reads the
// lease in a separate statement after the lock is granted, compares it to
// the lease the caller presented, and returns [ErrFenced] on a mismatch —
// which rolls the transaction back, so a fenced-out write writes nothing.
//
// The re-read is the whole point and is not an accident of style. Under
// read committed a statement's snapshot is taken when the statement starts,
// and a row that is only locked, never updated, is not re-read once the
// lock is granted. Reading the lease in the locking statement would
// therefore miss a lease a concurrent claim committed while this
// transaction waited for the lock — exactly the trap dagger's
// internal/domain/patch.go fence() comments on.
//
// Every mutating method calls this before its first write, with the id of
// the run the write belongs to.
func fence(ctx context.Context, tx pgx.Tx, runID, leaseID string) (fenceState, error) {
	state, err := lockRunRow(ctx, tx, runID)
	if err != nil {
		return state, err
	}
	if state.lease == nil || *state.lease != leaseID {
		return state, fmt.Errorf("run %s holds lease %s, not %s: %w",
			runID, leaseText(state.lease), leaseText(&leaseID), ErrFenced)
	}
	return state, nil
}

// lockRunRow takes the row lock and re-reads the run's lease and status,
// without comparing anything. It is what the two writes that are not
// fenced — SetLease, which changes what later comparisons compare against,
// and AbortOpenStepRuns, which the resume algorithm orders before it — take
// instead of the full fence, so they still serialize against a concurrent
// claim.
func lockRunRow(ctx context.Context, tx pgx.Tx, runID string) (fenceState, error) {
	q := New(tx)
	if _, err := q.LockRun(ctx, runID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fenceState{}, fmt.Errorf("run %s: %w", runID, ErrNotFound)
		}
		return fenceState{}, fmt.Errorf("lock run %s: %w", runID, err)
	}
	row, err := q.GetRunFence(ctx, runID)
	if err != nil {
		return fenceState{}, fmt.Errorf("read lease of run %s: %w", runID, err)
	}
	return fenceState{lease: row.LeaseID, status: row.Status}, nil
}

// leaseText renders a lease for an error message.
func leaseText(lease *string) string {
	if lease == nil || *lease == "" {
		return "none"
	}
	return *lease
}
