---
status: accepted
date: 2026-09-08
---

# Usage is posted per step, per model, and each report renews the lease

## Context and Problem Statement

Dagger wants cost attribution per node ([PRD §4](../prd.md#4-success--the-milestone), M.5). A run is many steps and can take an hour; a single `implement` step outruns dagger's fifteen-minute default lease TTL. Usage could be posted once at the end, or as it accrues — and the lease has to survive that hour either way.

## Decision Drivers

- Dagger's contract: reporting again adds a record, the attempt's cost is the sum, and **each report renews the lease**
- A turn can bill more than one model, so usage is keyed by model rather than being a string
- A worker that dies mid-run should not take the whole run's cost with it
- Currency is a rate table applied at query time, as in dagger; the wire carries counts

## Considered Options

1. One `ReportUsage` at `Complete`, with the run's total
2. One `ReportUsage` per step run close or failure, one record per `(lease, model)`
3. Per step, plus a reconciliation job that repairs what a crash lost

## Decision Outcome

Chosen option: **per step, per model**. [Domain model §Result](../domain-model.md#result): usage is posted *"after every step run closes or fails — one record per `(lease, model)` — not only at the end… posting per step is both the intended shape and a heartbeat."*

The ledger keeps the finer record: one row per turn per model with `input`, `output`, `cache_read`, and `cache_write` counts, and `posted_at` marking what has reached dagger ([Ledger §usage](../ledger.md#usage)). The adapter is the only component that sees the harness's numbers, so returning them in dagger's shape is part of the `Harness` contract ([ADR-003](003-harness-adapter-and-the-sidecar.md)).

Because each report renews the lease, per-step posting is a second heartbeat for free — belt and braces beside the worker's own heartbeat goroutine, which remains the one that must not be omitted ([Architecture](../architecture.md#request-path)).

### Consequences

- **Good**: A crash loses at most the in-flight step's usage from dagger's view, and the ledger still has it per turn.
- **Good**: Cost per step, model, and workflow is queryable, which is what `mill cost` reads.
- **Good**: The ordering constraint is explicit rather than incidental: usage is posted *before* the release on every terminal path — gate, park, complete — because the release ends the lease `ReportUsage` needs.
- **Bad**: More dagger calls: one per step per model rather than one per run.
- **Neutral**: Dagger's per-lease total for an abandoned attempt is allowed to be short by one step. *"There is no reconciliation job"*; the ledger is the complete record and dagger's view is deliberately approximate.
- **Neutral**: `ReportUsage` is accepted from a `user` principal today, one of the things pinning mill to the operator's identity ([Domain model §Principal](../domain-model.md#principal)).

## Rejected Options

### One report at `Complete`

Sum the run and post once.

- **Good**: One call, one record, nothing to mark `posted_at`.
- **Bad**: A worker that dies mid-run posts nothing, so dagger's cost for the attempt is zero — worst exactly when a run was most expensive. It forfeits the free heartbeat and gives no per-step attribution, and `Complete` releases the lease inside the same patch anyway.

### Per step plus a reconciliation job

A background pass re-posts what a crash lost.

- **Good**: Dagger's totals become exact.
- **Bad**: A second loop with its own idempotency and fencing problems, for an inaccuracy the design has accepted. The ledger is where exactness lives.

## More Information

Related: [ADR-003](003-harness-adapter-and-the-sidecar.md), [ADR-005](005-dagger-first-store-second.md).
