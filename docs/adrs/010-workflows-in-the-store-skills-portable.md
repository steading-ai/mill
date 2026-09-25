---
status: accepted
date: 2026-09-08
---

# Workflow definitions live in the store, seeded from files; the skill is portable and mill's record sits beside it

## Context and Problem Statement

A workflow is data, not code ([PRD §6.2](../prd.md#62-workflow-selection), R-8): loadable without a rebuild, and a ledger entry must name exactly which definition produced it. Its steps are agent skills. Where does a definition live, and how much orchestration may be written into a skill?

## Decision Drivers

- A run in flight must keep the version it started with, so a definition cannot be edited under it
- Skills are shared across workflows; the same reviewer is worth a cheaper model in `docs` than in `feature`
- A skill should survive switching harnesses ([ADR-003](003-harness-adapter-and-the-sidecar.md))
- [Core principle 7](../core-principles.md): routing may be judgement, sequencing must not be

## Considered Options

1. Definitions are files the worker reads at run time
2. Orchestration metadata in `SKILL.md` frontmatter
3. Definitions in the store, versioned by content hash, seeded from files in the repo

## Decision Outcome

Chosen option: **definitions in the store, seeded from files**. [Domain model §Workflow](../domain-model.md#workflow): a workflow is *"named, immutable, versioned… Lives in the store; seeded from files in the repo; pushed and pulled by the CLI."* `version` is the content hash; a change is a new row and *"runs in flight keep the version they started with."*

The split is [core principle 6](../core-principles.md): the **skill** is the portable part — name, description, body, scripts, assets — stored in `skills` with a digest, referenced by a step as `(name, digest)` and shared across workflows. The **mill record** sits beside it in the step: evidence, ordering, gates, capabilities, `on_failure`, `retries`, `timeout`, and per-harness settings. *"Harness settings live on the step, not on the skill record"* ([Domain model §Step](../domain-model.md#step)), because a shared skill cannot hold a setting that differs between workflows. Nothing mill-specific goes in `SKILL.md` frontmatter beyond a flat breadcrumb.

### Consequences

- **Good**: A ledger entry names one immutable version, so what ran is reconstructable from the store.
- **Good**: `workflow push`/`pull` and a definition authored outside the repo work identically.
- **Bad**: A definition now exists twice — in the repo and in the store — and `push` is what reconciles them. Nothing detects a repo edit that was never pushed.
- **Neutral**: [Ledger §Open questions](../ledger.md#open-questions) leaves it undecided whether `skills.files` bytes belong in mill's database or skift's blob store, leaning the former so a workflow loads without skift.
- **Neutral**: A project-scoped definition shadows an org one with a warning at load; nothing prevents accidental shadowing.

## Rejected Options

### Files read at run time

The worker loads `workflow.yaml` from disk when a run starts.

- **Good**: No push step, no schema, and the repo is unambiguously the source.
- **Bad**: A definition edited mid-run changes what a resumed run believes it is following, and citing a path and a commit is not the same as citing the bytes that ran. It also admits no definition authored outside mill's repo.

### Orchestration metadata in `SKILL.md`

Evidence, gates, ordering, and model settings in the skill's frontmatter.

- **Good**: One artifact per step; nothing to keep in sync.
- **Bad**: It welds a shared skill to one workflow's sequencing and one model, which the differing reviewer settings between `docs` and `feature` already break, and no other harness agrees to honour mill's keys in that frontmatter.

## More Information

Related: [ADR-003](003-harness-adapter-and-the-sidecar.md). `internal/workflow` is the loader.
