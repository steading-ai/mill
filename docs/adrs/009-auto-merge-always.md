---
status: accepted
date: 2026-09-08
---

# Auto-merge always; repo protections are the repo's job

## Context and Problem Statement

Mill's goal is running software, not pull requests ([PRD §3](../prd.md#3-goals-and-non-goals)). Every change it produces reaches a merge decision. Mill could hold that decision itself — a gate, or a policy of its own about which changes are safe — or it could merge and let the repository refuse.

## Decision Drivers

- Forges already have the mechanism: CI gating merge, required reviews, branch protection, CODEOWNERS
- A policy in two places drifts, and the copy in mill is the one nobody audits
- [PRD §10](../prd.md#10-risks): *"Gates become theater"* — a gate everyone taps approve on is worse than no gate
- Mill is not a policy engine ([Architecture §Trust boundaries](../architecture.md#trust-boundaries))

## Considered Options

1. Merge is a human gate by default
2. Mill keeps a per-project allowlist of what it may merge
3. Auto-merge always; the repository is the authority

## Decision Outcome

Chosen option: **auto-merge always**. [Core principle 1](../core-principles.md): *"If a change needs more care than the mill's own checks, the repository enforces it… The mill never second-guesses a forge that let it merge. If nothing blocks, hitting the default branch is done."* [Domain model §Gate](../domain-model.md#gate) states the same as a default: *"Merge is auto-approved by default: repo protections are the repo's job."*

A merge gate remains expressible — a workflow may declare one — but it is opt-in for a project that wants it, not the shape mill imposes.

### Consequences

- **Good**: Adding protection is a repository change, where every other contributor's protection already lives, applying to humans and mill alike.
- **Good**: Nothing waits on a human on the happy path, which is what "drain the backlog unattended" requires.
- **Bad**: A repository with no protections gets what it configured. Mill delivering a bad change to `main` there is correct behaviour and will still feel like mill's fault.
- **Bad**: The blast radius of a bad reviewer is a merge rather than a waiting gate, which is why [PRD §6.6](../prd.md#66-verification-and-review) R-25 — a seeded bad implementation the pipeline must reject — is a prerequisite rather than a nicety.
- **Neutral**: [PRD §9](../prd.md#9-open-questions) open question 4 still asks which projects should auto-merge in O1, and [PRD §6.7](../prd.md#67-delivery) R-29 says merge is a gate by default. The principle and the requirement disagree; the principle is the later decision, and the disagreement is recorded rather than resolved here.

## Rejected Options

### Merge is a human gate by default

Every change waits for a person before it lands.

- **Good**: Nothing reaches the default branch unseen, which is the intuitive answer while trust is being built.
- **Bad**: Approval that happens on every change is approval nobody reads, and the design already names that failure. It also puts a human on the critical path of the one thing mill exists to remove them from. Where the caution is real it belongs in branch protection, where it binds everyone.

### A per-project allowlist inside mill

Mill decides what it may merge from its own configuration.

- **Good**: Finer-grained than a forge's rules, and expressible per workflow.
- **Bad**: A second store for a policy the forge already holds. It goes stale silently, it is invisible to anyone reading the repository, and it makes mill a policy engine.

## More Information

Related: [ADR-001](001-just-a-workflow-runner.md). Revisit with open question 4.
