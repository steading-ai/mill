---
status: accepted
date: 2026-09-09
---

# Sessions run under the permission classifier, not `bypassPermissions`

## Context and Problem Statement

The first end-to-end `mill run` stopped at the `commit` step. The session called `git add`, the Claude Agent SDK's permission layer answered "This command requires approval", and the session — unattended, with nobody at a terminal — gave up. The sidecar set a tool policy and never set `permissionMode`, so the SDK used its `default`: prompt, and wait. Which permission mode does a mill session run under?

## Decision Drivers

- A mill session is unattended, so a permission prompt is a hang rather than a question
- The mode is not the boundary: `tools.deny` and the workspace socket are ([Architecture §Skill steps](../architecture.md#skill-steps-the-harness-adapter))
- A session runs as the mill's own account, in a clone on the mill's own host ([Domain model §Principal](../domain-model.md#principal))
- The `sdk-sessions` spike recommended `bypassPermissions`, having only established that a removed tool stays removed under it

## Considered Options

1. `bypassPermissions` now, as the spike recommended
2. A hand-written permission allowlist per skill
3. `auto`, the model classifier, with `bypassPermissions` refused until the workspace is a sandbox

## Decision Outcome

Chosen option: **`auto`, the classifier**. The sidecar sets `permissionMode: auto` unless a step names `default`, `acceptEdits`, `dontAsk` or `plan`; `bypassPermissions` is refused with exit reason `policy` before the SDK is loaded, in `sidecar/src/options.ts`. [Architecture §Skill steps](../architecture.md#skill-steps-the-harness-adapter): *"The classifier is a permission answerer, not a boundary."*

The refusal is scoped, not permanent. It is spelled as a `policy` refusal naming the sandbox because the mode becomes reasonable once the workspace is a container ([Roadmap §O4](../roadmap.md#o4--isolated)), at which point it is one line off a list.

### Consequences

- **Good**: The failure the decision exists for — a step that hangs on a prompt nobody answers — cannot recur, and the live sidecar spec asserts no tool result says "requires approval".
- **Good**: Under `auto` a classifier refuses in addition to the deny list. `bypassPermissions` would leave the deny list as the only thing between a session and the operator's host.
- **Bad**: A classifier call per permitted tool use, and a mode whose judgement mill does not own. A skill that trips it fails as a step rather than asking.
- **Neutral**: The `sdk-sessions` spike still reads as recommending `bypassPermissions`. It is not edited; its dated addendum records why the recommendation was not taken, and this record supersedes it.

## Rejected Options

### `bypassPermissions` now

Set the mode the spike ran its scripts under.

- **Good**: No classifier, no latency, no mode-dependent refusal to debug.
- **Bad**: The spike established that `disallowedTools` survives the mode, not that a session needs a mode saying yes to everything. Today that mode buys nothing but the removal of the one refusal mill did not have to write, on a host mill shares with the operator.

### An allowlist per skill

Each skill names the exact commands its session may run.

- **Good**: Explicit, reviewable, no model in the permission path.
- **Bad**: It is the same list a workflow author cannot get right in advance — `git add -A` and `git add .` are different strings — and every miss is a parked run. Enforcement belongs to `tools.deny` and the capability set, which bound a class rather than a spelling.

## More Information

Related: [ADR-003](003-harness-adapter-and-the-sidecar.md), [ADR-012](012-workspace-exec-is-the-chokepoint.md). Revisit at O4.
