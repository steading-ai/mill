---
status: accepted
date: 2026-09-08
---

# `ClaimNext --wait` is the loop; there is no scheduler and no event consumer

## Context and Problem Statement

A worker has to learn that a node became ready. Skift publishes an event stream, and the obvious shapes are a consumer that subscribes to it or a scheduler inside mill that decides what runs next. Both put a second opinion about readiness inside mill. What wakes a worker?

## Decision Drivers

- Readiness is skift's computation; mill holds no backlog ([ADR-001](001-just-a-workflow-runner.md), [PRD §3](../prd.md#3-goals-and-non-goals))
- `ClaimNext` already returns one ready node *and* leases it, atomically
- Concurrency must be a cap, not a mode: one worker and N workers must be the same code ([PRD §6.1](../prd.md#61-claiming), R-3)
- Stop and crash must both be safe, and a lease that expires is the recovery mechanism

## Considered Options

1. A skift event consumer that dispatches work to workers
2. A scheduler inside mill that ranks the ready set and assigns nodes
3. `ClaimNext --wait` in each worker, blocking until a node is ready

## Decision Outcome

Chosen option: **`ClaimNext --wait`**, recorded in [Architecture §One binary, two faces](../architecture.md#one-binary-two-faces): *"There is no scheduler and no event consumer: readiness is skift's, and a worker asking for work **is** the scheduling."*

A worker's only configuration is which projects and labels it claims for ([PRD §6.1](../prd.md#61-claiming), R-2). It blocks on `ClaimNext --wait`, drives the run, and asks again. Adding a worker adds capacity; there is nothing to rebalance, because the next free worker is by construction the one that gets the next ready node.

### Consequences

- **Good**: No dispatch state anywhere. Two workers on two hosts need no coordination beyond skift's lease.
- **Good**: A crashed worker needs no detection: its lease expires and the node returns to the ready set for whoever asks next ([Architecture §Failure and stop](../architecture.md#failure-and-stop)).
- **Bad**: Mill cannot express cross-project ordering — priority interleaving, budget, capacity. If that is ever wanted it needs skift's hold and focus mechanisms, not a mill scheduler.
- **Neutral**: A blocked long-poll is a held connection per worker. At one operator's scale this is uninteresting; at a larger one the wait would want a backoff policy the design does not name.

## Rejected Options

### A skift event consumer

Mill subscribes to skift's change stream and dispatches on node-became-ready.

- **Good**: Lower latency than a poll, and the stream is resumable by cursor.
- **Bad**: Readiness is derived, so a consumer has to re-derive it or re-query anyway, and it still has to claim — which can lose the race. It adds a cursor and a replay path for no capability `--wait` lacks; [Architecture](../architecture.md#what-is-borrowed-and-from-where) lists an event-watching scheduler among the things not borrowed.

### A scheduler inside mill

Mill reads the ready set, ranks it, and assigns nodes to workers.

- **Good**: Makes cost caps and model budgets enforceable across runs before claiming.
- **Bad**: Ranking needs the graph, which means mill holds a view of the backlog — the shadow state [core principle 2](../core-principles.md) exists to forbid. It also makes concurrency a mode rather than a cap: a scheduler with one worker is different code from a scheduler with three.

## More Information

Related: [ADR-001](001-just-a-workflow-runner.md) for the scoping this follows from, [ADR-011](011-usage-per-step-per-model.md) for the heartbeat that keeps a claimed lease alive across a long step.
