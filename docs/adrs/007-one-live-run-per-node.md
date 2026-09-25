---
status: accepted
date: 2026-09-08
---

# One live run per node; starting over is a new generation, and only a human starts it

## Context and Problem Statement

A node can be claimed more than once: a worker crashes, a lease expires, a gate is answered. Each claim could begin fresh work or continue what exists. And when a run has gone sideways, something must be able to start over without re-filing the node.

## Decision Drivers

- Skift's rule that every attempt shares the node's context id, so a retry resumes the prior session
- A run that restarts itself burns budget invisibly and hides the failure from a human
- [Core principle 4](../core-principles.md): *"Asking a human costs three real attempts"* — escalation has a price, and so should starting over
- The record of a failed attempt must survive whatever replaces it

## Considered Options

1. Every claim starts a fresh run
2. One live run per node; a second claim resumes it; automatic restart on repeated failure
3. One live run per node; a second claim resumes it; restart is human-initiated and creates a generation

## Decision Outcome

Chosen option: **one live run per node, resumed by every claim, with restart a deliberate human act**.

`runs.node_id` carries a partial unique index over non-terminal statuses, and `(node_id, generation)` is unique overall ([Ledger invariant 1](../ledger.md#invariants)). A second lease on the same node resumes the same run at its derived `current_step`.

`mill restart <node>` ([Domain model §Restart](../domain-model.md#restart)) releases any lease with `preempted`/`restart`, cancels the running step, marks the run `abandoned`, destroys the workspace, deletes the branch, closes the PR, and creates a run at `generation + 1`. Session ids are UUIDv5 of `(context_id, generation, name)`, so every session in the new run is one the harness has never seen — which is what makes it a start-over rather than a resume.

*"Never automatic. The circuit breaker parks; a human restarts. A step that fails repeatedly must cost a human decision, not a generation."*

### Consequences

- **Good**: Retry resumes the model's own prior context, rather than re-explaining the node to a fresh session.
- **Good**: The abandoned run's step runs, evidence, and usage stay as the record of what went wrong.
- **Good**: A poisoned context has an exit that is not "re-file the node": the graph is untouched.
- **Bad**: A run wedged in a way the breaker does not detect stays wedged; nothing sweeps for it.
- **Neutral**: `restart` is destructive on the forge — branch deleted, PR closed — and the design gives it no confirmation or dry run.

## Rejected Options

### Every claim starts a fresh run

A claim always creates a run; the ledger accumulates one per attempt.

- **Good**: No resume algorithm and no takeover predicate on `SetLease`.
- **Bad**: A crash mid-`implement` would discard every closed step's evidence and re-run the workflow at full cost, and discard the session — the opposite of skift's retry rule.

### Automatic restart after repeated failure

The circuit breaker opens a new generation instead of parking.

- **Good**: Unattended runs never stall on a human.
- **Bad**: It converts a failure nobody sees into spend nobody sees, and it removes the price [core principle 4](../core-principles.md) deliberately attaches to escalation. Three real attempts and a gate is the honest signal that a node needs a person.

## More Information

Related: [ADR-004](004-evidence-closes-steps.md) for the derivation resume relies on, [ADR-006](006-lease-fenced-store-writes.md).
