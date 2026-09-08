---
status: accepted
date: 2026-09-08
---

# Mill is just a workflow runner

## Context and Problem Statement

Three previous attempts at this layer failed, and each failed by growing past the loop: hive owned its own backlog, work-on had to be its own planner and was never trusted, the reference harness ran one manually launched epic ([PRD §1](../prd.md#1-problem)). The surrounding factory already has the other pieces — dagger holds the plan and computes readiness, thicket makes agents reachable, homestead runs the hosts. What, then, is mill allowed to be?

## Decision Drivers

- Every attempt that acquired a second responsibility stopped being used
- Dagger was scoped the same way, to just project management, and that scoping is why it works
- A scope statement is only useful if it can be violated, so it has to name the temptations

## Considered Options

1. A runner that also plans, prioritizes, sandboxes, and converses
2. A runner plus a local backlog cache, so it can order work itself
3. Just a workflow runner: claim a ready node, drive a workflow, complete it

## Decision Outcome

Chosen option: **just a workflow runner**, recorded in [PRD §3](../prd.md#3-goals-and-non-goals). Mill does not plan, decompose, or prioritize; holds no backlog; does not sandbox; does not converse; is not a general workflow engine; is not an interactive coding assistant. `dg ready` is the truth about what is next. Decomposition is a workflow whose terminal step files child nodes in dagger and closes ([core principle 2](../core-principles.md)). Questions and approvals leave as dagger gates ([PRD §6.5](../prd.md#65-gates-and-approvals)); thicket carries them.

The non-goals are written as a scope-creep detector: *any feature that pulls toward one of these is the signal. Cut it.*

### Consequences

- **Good**: Mill holds one node's worth of state, which is what makes resume tractable.
- **Good**: Each excluded responsibility already has an owner — dagger, thicket, homestead — so exclusion is delegation, not a gap.
- **Bad**: Mill is useless alone; it cannot be demonstrated without a populated dagger, which makes its acceptance suite expensive ([runbook](../runbook.md)).
- **Neutral**: "Not a sandbox" is true of the design, not the deployment: MVP runs steps in the operator's account, and the isolation the workspace seam anticipates arrives at O4 ([roadmap](../roadmap.md)).
- **Neutral**: The boundary against a general workflow engine is stated by intent — "software-delivery workflows for one operator" — not by a mechanism.

## Rejected Options

### A runner that also plans

The runner assesses a node, decides it is too large, and decomposes it in place.

- **Good**: No round trip through dagger; the runner can always make progress on whatever it is handed.
- **Bad**: This is exactly work-on's failure. A planner needs the whole graph, so it grows a shadow backlog, and then two systems disagree about what is next. [PRD §10](../prd.md#10-risks) keeps it as a named risk.

### A local backlog cache

Mill mirrors the ready set so it can order work without asking dagger.

- **Good**: Fewer dagger calls; ordering across projects becomes possible locally.
- **Bad**: A cache is state about someone else's domain, and a stale one silently works the wrong node. `ClaimNext` already returns exactly one ready node under a lease, which is all a worker needs.

## More Information

Related: [ADR-002](002-claimnext-wait-is-the-loop.md), which is this scoping applied to scheduling. Revisit when a second operator exists, which is the first thing that would make ordering mill's problem.
