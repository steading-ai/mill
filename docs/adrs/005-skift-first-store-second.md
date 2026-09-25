---
status: accepted
date: 2026-09-08
---

# Skift first, store second

## Context and Problem Statement

A run's facts live in two systems — skift holds the node, the lease and the gate; the ledger holds the run — and the worker writes to both, non-atomically. Which order leaves a recoverable state when the process dies in between?

## Decision Drivers

- Skift is the authority on nodes; the ledger records what happened inside a run
- Every crash window must resolve to a state the next claim can repair
- A node nothing can ever claim again is unrecoverable without a human
- `ApplyPatch` takes an idempotent `request_id`, so a skift write survives retry

## Considered Options

1. Store first, then skift
2. Skift first, then store
3. A distributed transaction or an outbox with a reconciliation worker

## Decision Outcome

Chosen option: **skift first, store second, and the store write is what makes the skift write "count."** [Ledger invariant 11](../ledger.md#invariants): *"The store never asserts a fact about skift that skift has not yet made true."*

On the done path: complete the node in skift, then mark the run `done`. A crash in between leaves a `running` run whose node is done, and the next claim simply marks it `done` — the resume algorithm's first branch ([Ledger §Resume](../ledger.md#resume)).

The gate path still writes to skift first, but the store write moves between two skift calls: `ApplyPatch` files the gate and returns its id; the store records `gate_node_id` and marks the run `waiting`; `Release` ends the lease. Putting the store write last would leave a gate the ledger knows nothing about, and the run would file a second one ([Architecture §Write ordering](../architecture.md#request-path)).

### Consequences

- **Good**: Every crash window is repairable by the next claim, with no reconciliation job.
- **Good**: The ledger cannot claim a node is complete when skift disagrees, so `mill status` cannot lie in the direction that matters.
- **Bad**: The gate window is not fully closed by ordering alone. A crash between the patch and the store write leaves an orphan gate, so a worker must ask skift for the node's user-assigned children carrying the run's and the step's labels before filing — *"the only place in the loop where skift's graph, rather than the ledger, decides what to write"* ([invariant 12](../ledger.md#invariants)).
- **Neutral**: The ledger is briefly behind skift by design; a reader between the two writes sees a running run on a done node.
- **Neutral**: A `done` run on an open node is invariant 11 violated: park with `refused` and alert. The design does not say what the alert is.

## Rejected Options

### Store first, then skift

Mark the run `done`, then complete the node.

- **Good**: The ledger, which the worker owns, is never stale.
- **Bad**: *"A run marked `done` before skift confirmed would strand a node nothing can ever claim"* ([Architecture §Write ordering](../architecture.md#request-path)) — the run looks finished and the node stays open.

### A distributed transaction, or an outbox with a reconciler

Make the pair atomic, or queue the skift write durably and drain it.

- **Good**: No crash window; the two systems cannot disagree.
- **Bad**: Two-phase commit across a service mill does not own is not on offer, and an outbox adds a second loop and its own failure modes to close a window that a fixed order plus an idempotent `request_id` already closes.

## More Information

Related: [ADR-006](006-lease-fenced-store-writes.md), the other half of "the record cannot be corrupted".
