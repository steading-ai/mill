---
status: accepted
date: 2026-09-08
---

# Every store write is fenced by the lease, inside the writing transaction

## Context and Problem Statement

A worker can be fenced out — its lease expires, another claims the node and resumes the run — while it is still executing a step and writing to the ledger. Skift fences its own writes; nothing yet stops the stale worker corrupting mill's record of the run it no longer owns.

## Decision Drivers

- The ledger is what a new holder resumes from, so a stale write there is worse than one to skift
- A worker that has lost its lease is the *normal* path, not an error: it should fail silently and correctly
- Correctness that depends on a worker checking before it writes is not correctness

## Considered Options

1. The worker checks its lease before writing
2. The store compares the presented lease, read before the row lock is taken
3. The store takes the row lock, re-reads the lease, and compares, inside the writing transaction

## Decision Outcome

Chosen option: **fence inside the writing transaction, after the row lock**. [Ledger invariant 9](../ledger.md#invariants): every statement mutating `step_runs`, `evidence`, `step_tokens`, `usage`, or `runs` takes the `runs` row lock (`SELECT … FOR UPDATE`), **re-reads `lease_id` in a separate statement after the lock is granted**, compares it to the lease the worker presented, and rolls back on mismatch.

The re-read is the subtle part, borrowed from skift's `fence()`: a locked row is not re-read, so a lease a concurrent claim committed while this transaction waited on the lock would otherwise be missed.

`SetLease` is the sole exception, because it is the write that changes what later writes compare against. It takes the same lock and requires the run to be in a status a new claim may take over. The mechanism *"lives in the store, not in the worker's good behaviour."*

### Consequences

- **Good**: The guarantee holds against every worker, including a buggy one, because it is enforced where the write lands.
- **Good**: A fenced-out worker learns from a rejected write or a fenced heartbeat, abandons the run locally, and returns to `ClaimNext`. Nothing is lost: the ledger is the truth and the new holder owns it ([Architecture](../architecture.md#failure-and-stop)).
- **Bad**: Every ledger write serializes on the run's row — a real ceiling if a run ever wants concurrent steps.
- **Bad**: Every mutating store RPC must carry a lease id, which shapes the whole `mill.v1` surface.
- **Neutral**: `SetLease`'s takeover predicate is written in prose ([invariant 9](../ledger.md#invariants)); the design does not say what a status outside it should return.

## Rejected Options

### The worker checks its lease before writing

The worker heartbeats, notices a fence, and stops writing.

- **Good**: No lock, no re-read, no lease on every RPC.
- **Bad**: Check-then-write is a race with the exact window the fence closes — the heartbeat succeeds, the lease expires, the write lands. It makes correctness a property of every call site rather than of the store.

### Compare a lease read before the lock

Read `runs.lease_id`, take the lock, write.

- **Good**: One fewer statement, and it looks correct.
- **Bad**: It is the trap skift's `fence()` comments on. The value read before the lock is stale by the time the lock is granted, so a claim that committed during the wait is invisible and the stale write succeeds.

## More Information

Related: [ADR-005](005-skift-first-store-second.md), [ADR-007](007-one-live-run-per-node.md).
