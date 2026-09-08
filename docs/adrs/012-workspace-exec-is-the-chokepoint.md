---
status: accepted
date: 2026-09-08
---

# `Workspace.Exec` is the single chokepoint, enforced by lint

## Context and Problem Statement

Mill's first workspace is a local clone under the operator's account; containers and remote hosts come later ([roadmap](../roadmap.md) O4, O5). [PRD §10](../prd.md#10-risks) names the risk: *"One `os.Open(path)` in step code and O4 becomes a rewrite."*

## Decision Drivers

- The workspace is the seam that makes sandboxing a later decision instead of a rewrite ([Architecture §The workspace](../architecture.md#the-workspace))
- cwd, uid, and environment are what actually bound a spawned process — not the tool that spawned it
- A step must never receive a host path or a credential in its environment ([PRD §6.8](../prd.md#68-workspace-and-sandboxing), R-31)
- A guarantee nothing checks is aspirational

## Considered Options

1. A convention: everything should go through the workspace
2. Code review as the enforcement
3. `Workspace.Exec` as the only caller of `os/exec`, enforced by a lint rule

## Decision Outcome

Chosen option: **one chokepoint, enforced by lint**. [Core principle 3](../core-principles.md): *"`Workspace.Exec` is the single chokepoint for anything a step runs — it sets cwd, uid, and environment. `exec.Command` is forbidden outside the workspace package."* The rule is golangci-lint's forbidigo, scoped to `internal/workspace`, the same depguard/forbidigo pattern dagger uses to keep `internal/store` out of handlers. Checks are `Workspace.Exec` calls with a contract ([Domain model §Check](../domain-model.md#check)).

Enforcement is the process boundary `Exec` establishes, not the tool the session was offered: *"a command run through `exec` is bounded by the process it spawns, not by the tool that spawned it"* ([Architecture §The workspace](../architecture.md#the-workspace)).

### Consequences

- **Good**: `local` → `container` → `remote` is a configuration change; a workflow definition does not move. *"If it has to, the seam leaked."*
- **Good**: Every command a run issues is one auditable call at a seam mill owns, which is what makes per-step logging and redaction possible.
- **Good**: The guarantee is checkable in CI, and a violation fails `make lint` on the branch that introduced it.
- **Bad**: Mill's own code pays too — anything legitimately needing a subprocess outside a run routes through the workspace package or is excluded, and the exclusion list is itself something to keep honest.
- **Neutral**: The rule bounds mill's Go code, not a session. In MVP the adapter's tool allowlist is the only thing keeping a session inside the workspace, and the `local` implementation does not gate `exec` against `write`: a reviewer allowed to run tests can also write. MVP **detects** rather than prevents, with a `clean-tree` check after every read-only step. The second, independent layer arrives with containers.

## Rejected Options

### A convention

The design says everything goes through the workspace; developers comply.

- **Good**: No lint configuration, no exclusions, no fights with the tool.
- **Bad**: `exec.Command` is two lines and always closer to hand than the seam. The place it slips in is the one nobody remembers when O4 starts, and the failure surfaces as a rewrite rather than a diff.

### Code review as the enforcement

A reviewer rejects direct process spawning.

- **Good**: More flexible; a reviewer can weigh a genuine exception.
- **Bad**: Mill's reviewers are model steps checking acceptance criteria, and the reference harness approved 59 nodes without ever saying no ([PRD §1](../prd.md#1-problem)). Delegating an architectural invariant to that is the flaw this project exists to fix.

## More Information

Related: [ADR-003](003-harness-adapter-and-the-sidecar.md) for the tool policy that is MVP's other half of this boundary. Revisit at O4.
