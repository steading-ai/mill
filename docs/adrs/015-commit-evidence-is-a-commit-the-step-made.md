---
status: accepted
date: 2026-09-09
---

# Commit evidence is a commit the step made

## Context and Problem Statement

In the first end-to-end run the `commit` step closed on commit evidence four times while the branch head was still the seed commit the workspace was cloned at. `observe` read the head of the run's branch and the verifier proved only that the object existed and was on that branch, so a step that committed nothing closed on somebody else's commit. What fact does a commit step have to produce?

## Decision Drivers

- [Core principle 8](../core-principles.md): a step closes on a fact the mill verified outside the session
- The fact a commit step owes is a commit that did not exist when the step began
- Resume must close a step whose session committed and then died, without a second session ([Ledger §Resume](../ledger.md#resume))

## Considered Options

1. Trust the SHA the session reports in its final message
2. Keep reading the branch head, and require only that it moved
3. Record the branch head when the attempt starts, and verify the new commit against it

## Decision Outcome

Chosen option: **`base_commit` on the step run, and four checks against it**. Every step run records the head of the run's branch when the attempt started ([Ledger §`step_runs`](../ledger.md#step_runs)). A commit closes the step only when `git cat-file -e` resolves it in the workspace, it is on the run's branch, it descends from that attempt's `base_commit` without being it, and the mill is its author and its committer ([Domain model §Evidence](../domain-model.md#evidence)).

`base_commit` is recorded for every step kind, not only commit steps, because what a superseded or interrupted span produced is the range from its base to the head. An attempt continuing an `aborted` one inherits that attempt's base, so a worker killed between the commit and the write that closes the step resumes onto the commit already on the branch.

### Consequences

- **Good**: A commit step that commits nothing fails, naming the head that did not move. The seeded-bad-implementation test has something to fail on.
- **Good**: Resume is unchanged and cheaper than a session: base plus branch is enough to close the step.
- **Bad**: The identity check makes evidence depend on the environment git resolves an identity from. Ambient `GIT_AUTHOR_*` — which git exports to its own hooks — outranks a repository-local `user.name`, so the specs pin it.
- **Neutral**: A step that legitimately produces several commits closes on the head of them. The range from `base_commit` is the record of the rest.

## Rejected Options

### Trust the session's reported SHA

Parse the final message for a SHA and record it.

- **Good**: One string to read; no git in the verifier at all.
- **Bad**: It is the transcript-as-evidence failure mode this project exists to fix. A message can name a SHA that was never created, or one somebody else created, and the ledger would say the step shipped it.

### Head-moved alone

Compare the head to the base and accept any advance.

- **Good**: Almost as cheap, and catches the actual bug — the head that never moved.
- **Bad**: It closes on whatever moved the branch: a concurrent push, an operator's own commit in a shared checkout, a merge. Descent and committer identity are what make the fact about *this step* rather than about the branch.

## More Information

Related: [ADR-004](004-evidence-closes-steps.md), [ADR-008](008-revise-supersedes-the-span.md).
