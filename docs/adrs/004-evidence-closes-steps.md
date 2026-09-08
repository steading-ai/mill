---
status: accepted
date: 2026-09-08
---

# Evidence closes steps, agents never do, and `current_step` is derived

## Context and Problem Statement

The `/work-on` skill encodes the right workflow, but every guardrail in it is prose the model polices itself with: steps get skipped, and nothing records which one ran ([PRD §1](../prd.md#1-problem)). The reference harness approved 59 nodes in a row without ever rejecting one. What makes a step actually complete, and where does a run's position live?

## Decision Drivers

- [Core principle 8](../core-principles.md): *"An agent saying 'done,' a transcript that reads well, or a task list marked complete is not evidence and never closes a step."*
- Resume must not read a transcript ([PRD §6.4](../prd.md#64-ledger-and-evidence), R-17)
- Stored state that can be derived will drift from what it is derived from

## Considered Options

1. The session reports step completion; the mill records it
2. The mill verifies an externally checkable fact per step, and stores `current_step` alongside it
3. The mill verifies evidence, and derives `current_step` from step-run status

## Decision Outcome

Chosen option: **evidence closes steps and `current_step` is derived**.

Each step definition names one evidence kind — `commit`, `exit`, `verdict`, `structured`, `url`, `status`, `artifact`, `gate` — and what verifies it ([Domain model §Evidence](../domain-model.md#evidence)). A SHA closes `commit` only when `git cat-file -e` resolves it on the run's branch; a verdict closes `review` only when it parses and says `approve`.

[Ledger invariant 5](../ledger.md#invariants) pairs the two facts in one transaction: a `closed` step run has evidence, and a step run with evidence is `closed`. [Invariant 10](../ledger.md#invariants) removes the column: *"`current_step` is derived. There is no column to drift."* It is the first step whose latest `step_runs` row is not `closed`.

### Consequences

- **Good**: Resume is the same code as a first run — a fresh run's `current_step` is simply the first step.
- **Good**: A workflow is enforced rather than described. A skipped step has no evidence, so the run cannot move past it.
- **Good**: `revise` can move a run backwards by marking rows `superseded` without touching evidence or a position column ([ADR-008](008-revise-supersedes-the-span.md)).
- **Bad**: Every step needs a verifiable fact, which is a real design cost for steps whose product is judgement. `structured` output validated against a schema is the fallback, and it verifies shape rather than truth.
- **Bad**: `current_step` is a query over `step_runs` per loop iteration rather than a column read.
- **Neutral**: The evidence taxonomy is closed. A workflow needing a fact none of the eight kinds expresses is a change to the model, not to a definition.

## Rejected Options

### The session reports completion

The step's final message says what it did, and the mill records it.

- **Good**: Every step is expressible; nothing has to be independently checkable.
- **Bad**: The failure mode the project exists to fix. A transcript that reads well and a task list marked complete are what the harness and `/work-on` already trust, and both are wrong in ways nobody can see afterwards.

### Store `current_step`

Keep the derivation but write the answer down as a column.

- **Good**: One cheap read, and `status` needs no join.
- **Bad**: Two representations of the same fact, and a crash between writing the column and writing the evidence leaves them disagreeing — *"which is the failure the ledger exists to prevent"* ([Ledger §runs](../ledger.md#runs)).

## More Information

Related: [ADR-008](008-revise-supersedes-the-span.md). [PRD §6.6](../prd.md#66-verification-and-review) R-25 — the seeded bad implementation — is the test that this holds in practice.
