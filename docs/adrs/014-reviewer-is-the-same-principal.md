---
status: accepted
date: 2026-09-09
---

# The reviewer is the same principal with a read-only policy

## Context and Problem Statement

O1's reviewer must have demonstrably said no ([PRD §4](../prd.md#4-success--the-milestone), M.2). [PRD §9](../prd.md#9-open-questions) left one thing about it undecided: *"Same principal with a read-only capability set, or a separate principal? Separate is stronger provenance; same is simpler until a secrets service exists."* R-26 asks the same question of the identity a verdict is attributable to.

## Decision Drivers

- A verdict must be attributable to something other than the thing it judged
- A reviewer with no writing credential cannot fix what it should fail ([PRD §6.9](../prd.md#69-identity-and-credentials), R-38)
- Mill has one principal — the operator's, resolved by uid — and moving off it is the identity bet's work ([Domain model §Principal](../domain-model.md#principal))
- A second account needs a second Claude login, a second skift identity, and somewhere to keep both

## Considered Options

1. A separate skift principal for the reviewer
2. A separate operating-system account and Claude login for the reviewer
3. The same principal, a separate session, a read-only tool policy and a step token without `write`

## Decision Outcome

Chosen option: **the same principal**, with the separation put where it does work. The reviewer runs in its own session under its own prompt; its step declares `tools.deny: [Write, Edit, Bash, NotebookEdit]` and a capability set without `write`, so the workspace refuses `fs.write` and `git.commit` under its step token ([Domain model §Step](../domain-model.md#step)). The ledger records which session produced which verdict, which is the attribution that was actually wanted ([Domain model §Principal](../domain-model.md#principal)).

[Roadmap §Deliberately unscheduled](../roadmap.md#deliberately-unscheduled): a separate principal is *"dropped (C2). Revisit only when a skift policy needs 'completer ≠ reviewer'"* — which is the Renovate pipeline's rule, a delivery concern rather than a review one.

### Consequences

- **Good**: No second credential to mint, store, rotate, or leak; O1 needs no secrets service to ship a reviewer.
- **Good**: The guarantee that matters is enforced on the side that enforces it. A principal boundary would not have kept a writing tool out of the reviewer's session; the tool policy and the capability set do.
- **Bad**: Skift cannot tell mill's reviewer from mill's implementer. Provenance is the ledger's, not skift's, and a reader who trusts only skift sees one actor.
- **Neutral**: Until the workspace socket is the session's tool server, the SDK's file tools do not pass through it, so MVP needs both halves and a `clean-tree` check after a read-only step ([Domain model §Step](../domain-model.md#step)).

## Rejected Options

### A separate skift principal

The reviewer claims and writes as its own skift identity.

- **Good**: Skift could enforce "completer ≠ reviewer" as policy, and provenance would survive a reader who distrusts the ledger.
- **Bad**: No skift policy demands it today, and mill's principal is the operator's `user` by transport-resolved identity. A second principal is a new principal kind, a change to `ReportUsage` admission, and gate vetoes beginning to apply — a larger change than the problem.

### A second account and login

The reviewer runs as another unix user with its own Claude credentials.

- **Good**: A genuinely different filesystem and login; read-only becomes an operating-system fact.
- **Bad**: Two logins to keep alive and a second checkout to keep in step, for an isolation the O4 workspace gives to every step at once.

## More Information

Related: [ADR-004](004-evidence-closes-steps.md), [ADR-012](012-workspace-exec-is-the-chokepoint.md). Response C2 in `docs/research/prd-review-responses.md` is where it was conceded.
