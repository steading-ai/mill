---
status: accepted
date: 2026-09-08
---

# `revise` supersedes every step from its target through the failure

## Context and Problem Statement

A reviewer returns `revise implement`. The steps between `implement` and `review` — `simplify`, `commit`, `pr` — already closed with evidence from pass one. If the implementer now changes the code, what happens to that evidence?

## Decision Drivers

- [Core principle 8](../core-principles.md): the ledger's record of what shipped must be true
- Evidence is immutable ([Ledger invariant 6](../ledger.md#invariants)); there is no update path
- `current_step` is derived from step-run status ([ADR-004](004-evidence-closes-steps.md)), so moving a run backwards means changing status, not a column
- The breaker must count rounds against a step's `retries` and eventually park

## Considered Options

1. Re-run only the failing step's target and resume at the failure
2. Delete or update the intervening evidence
3. Supersede every step from the target through the failure inclusive

## Decision Outcome

Chosen option: **supersede the whole span**. [Domain model §Revise](../domain-model.md#revise): `on_failure: revise <target>` on step *F* marks the latest step run of every step from *target* through *F* inclusive `superseded`. Their evidence rows remain, immutable, but no longer close anything, so the derived `current_step` is *target* again and every step in the span runs afresh.

*"A re-implemented change is re-simplified, re-committed, re-pushed, and re-reviewed. The alternative, in which `commit` and `pr` keep pass-one evidence while `implement` produced pass two, would make the ledger's record of what shipped false."*

The counter is the **failing** step's: `review` fails with `revise implement`, and `review.retries` is what parks the run after three rounds. A `revise` whose target is not earlier in the workflow is a definition error caught at load.

### Consequences

- **Good**: The ledger never claims pass-one evidence closed pass-two work.
- **Good**: Both passes are on the record; the superseded rows are how a human sees what the first attempt did.
- **Good**: Nothing special-cases the loop: superseding changes status and the derivation does the rest.
- **Bad**: A revise re-pays for every step in the span, including expensive ones far from the actual change.
- **Bad**: The counter living on the failing step is easy to misread — `implement` is what is being redone, but `implement.retries` is not what parks the run. It is also a different counter from the run's derived `leases`, which moves on worker crashes ([Ledger §step_runs](../ledger.md#step_runs)).
- **Neutral**: The model has no branching beyond `on_failure`, so a workflow that wants a cheaper partial redo cannot express one.

## Rejected Options

### Re-run only the target, then resume at the failure

`implement` runs again; `simplify`, `commit`, `pr` keep their closed rows; the run goes straight back to `review`.

- **Good**: Cheapest possible revise, and the fewest sessions touched.
- **Bad**: The commit evidence names a SHA that no longer contains the shipped work, so `mill inspect` describes something that never existed — the ledger lying about delivery.

### Delete or update the intervening evidence

Reopen the steps by removing the rows that closed them.

- **Good**: No `superseded` status; `closed` keeps its simple meaning.
- **Bad**: [Invariant 6](../ledger.md#invariants) forbids it, and for a reason: the audit record of pass one is exactly what a human reviewing a bad outcome needs. `superseded` gets the same reopening with none of the loss.

## More Information

Related: [ADR-004](004-evidence-closes-steps.md). `rerun <run> --from <step>` is the same supersede without a review round ([Architecture](../architecture.md#request-path)).
