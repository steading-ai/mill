---
status: accepted
date: 2026-09-09
---

# Workflows and their skills live in mill's database, versioned by digest

## Context and Problem Statement

[ADR-010](010-workflows-in-the-store-skills-portable.md) put workflow definitions in the store and left one thing open, as [Ledger §Open questions](../ledger.md#open-questions) does: *"Whether `skills.files` bytes belong in this database or in dagger's blob store by digest."* [Roadmap §Decisions still open](../roadmap.md#decisions-still-open) carries the same question into O1 with the leaning *"mill's, so a workflow loads without dagger."* O0's `workflows` node built it that way; this record settles it.

## Decision Drivers

- A run must be able to load its workflow without a second service being reachable
- A ledger entry names one immutable version, so the bytes it names must not move ([Domain model §Workflow](../domain-model.md#workflow))
- Dagger's blob store is dagger's; mill depends on dagger for the work graph, not for its own configuration
- Skill payloads are prompts, scripts and assets — kilobytes, not artifacts

## Considered Options

1. Skill bytes in dagger's blob store, referenced by digest
2. Workflow files read from disk at run time
3. Definition and skill bytes in mill's own database, versioned by digest

## Decision Outcome

Chosen option: **mill's database**. `mill workflow push <dir>` stores the workflow definition as one immutable row keyed by the content hash of the canonical definition, and every referenced skill as a `skills` row keyed by the digest of its directory: body, frontmatter metadata, and a manifest of `scripts/`, `assets/` and `references/` whose bytes live in the store's blob table by digest ([Ledger §`skills`](../ledger.md#skills)). A run pins the version it selected, and *"runs in flight keep the version they started with"* ([Domain model §Workflow](../domain-model.md#workflow)).

The set of workflows is therefore **whatever has been pushed**, not a list mill is built with. Any directory under `workflows/` — or any directory at all, authored outside this repository — is pushable, and `mill workflow list` answers from the store. `workflows/` in this repo is a seed, not a registry.

### Consequences

- **Good**: A worker that can reach its database can start a run. Dagger being down stops mill claiming work, not loading a workflow it already selected.
- **Good**: One store, one transaction, one retention policy. `push` and `pull` round-trip without a second protocol.
- **Bad**: Mill's database grows with every pushed version, and nothing garbage-collects a skill no live definition references.
- **Bad**: Nothing detects a repository edit that was never pushed; `push` is the only reconciliation ([ADR-010](010-workflows-in-the-store-skills-portable.md)).
- **Neutral**: Artifacts a *run* produces still go to dagger. This is about mill's own configuration, not about a run's output.

## Rejected Options

### Dagger's blob store

Skill files stored in dagger by digest, mill holding references.

- **Good**: One content-addressed store for the whole system, already built, already backed up.
- **Bad**: It makes dagger a dependency of loading a workflow, which is a dependency mill does not otherwise have, and it puts mill's configuration in a service whose retention is decided elsewhere. A digest mill cannot resolve is a run that cannot start.

### Files read from disk at run time

The worker reads `workflow.yaml` and `SKILL.md` from a checkout when the run starts.

- **Good**: No push step and no schema; the repository is unambiguously the source.
- **Bad**: A definition edited mid-run changes what a resumed run believes it is following, and citing a path and a commit is not citing the bytes that ran. It also admits no workflow authored outside mill's repository.

## More Information

Related: [ADR-010](010-workflows-in-the-store-skills-portable.md), [ADR-016](016-one-workflow-yaml-skills-by-name-and-digest.md). `internal/workflow` loads; `internal/cli/workflow.go` pushes and pulls.
