---
status: accepted
date: 2026-09-08
---

# A harness-agnostic Go interface, first implemented by a Claude Agent SDK sidecar over NDJSON

## Context and Problem Statement

A skill step is a headless agent session. Mill is Go; the harness with the richest contract today is Claude Code, whose SDK is TypeScript. Mill must not become an agent runtime, and it must not be welded to one vendor's CLI. How does a Go worker drive a session?

## Decision Drivers

- Codex, Pi, and a native Go adapter are foreseen; the workflow definition must not change when the harness does ([Architecture §Skill steps](../architecture.md#skill-steps-the-harness-adapter))
- [PRD §7](../prd.md#7-constraints): design for the CLI's contract, and *"do not depend on a language SDK that may not exist for Go"*
- Usage numbers are only visible to whatever talks to the harness, and dagger needs them per model
- A step must be cancellable, and "never ran" must be distinguishable from "ran and failed"

## Considered Options

1. Shell out to `claude -p` and parse its output
2. A TypeScript sidecar over the Agent SDK, spoken to over NDJSON on stdio
3. A sidecar over Connect on a unix socket
4. Wait for a Go SDK

## Decision Outcome

Chosen option: a **`Harness` interface** (`Start`, `Resume`, `Cancel`) whose first implementation is a **TypeScript sidecar over the Claude Agent SDK**, spawned per step and spoken to over **newline-delimited JSON on stdio**, with the message types defined once in `harness.v1` proto and generated to both Go and TypeScript by buf.

Usage is part of the interface's contract, not an optional event: the adapter returns per-turn and terminal totals keyed by model, in dagger's shape. A `Turn` ends with `ProcessExit{Status, Signal, Reason}`, `Status = -1` meaning the session never ran. A skill is used as a *prompt*, never installed as a harness-native skill, so no harness's discovery rules reach a workflow.

### Consequences

- **Good**: A child process on stdio dies with its parent, needs no port, and is how MCP and LSP already work.
- **Good**: Proto-typed messages mean both sides change together and neither hand-writes a parser.
- **Bad**: Deployment now needs a Node runtime beside the Go binary ([Architecture](../architecture.md#deployment)), and `make generate` has two language outputs to keep committed.
- **Neutral**: Stdio cannot outlive the worker. Connect over a unix socket is the named upgrade if a sidecar ever must; nothing in MVP needs it.
- **Neutral**: In MVP the adapter's tool policy is the *only* enforcement of a step's capabilities ([ADR-012](012-workspace-exec-is-the-chokepoint.md)).

## Rejected Options

### Shell out to `claude -p`

The worker runs the CLI and reads its JSON output.

- **Good**: One binary, no Node, no proto, no second language in the repo.
- **Bad**: The CLI's output shape is not a contract, and usage, session-resume semantics, and tool policy are the SDK's richest surface. The reference harness already uses the SDK; matching it through argv breaks on upgrade.

### A sidecar over Connect on a unix socket

Same sidecar, heavier transport.

- **Good**: Uniform with mill's own API; the sidecar could outlive a worker.
- **Bad**: A socket path, a lifecycle, and orphan cleanup, for a process that is per-step and short-lived anyway.

### Wait for a Go SDK

- **Bad**: It may never exist, and [PRD §7](../prd.md#7-constraints) is precisely the instruction not to bet on it. A native Go adapter remains a later implementation of the same interface.

## More Information

Related: [ADR-010](010-workflows-in-the-store-skills-portable.md), [ADR-011](011-usage-per-step-per-model.md).
