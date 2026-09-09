---
status: accepted
date: 2026-09-09
---

# One `workflow.yaml` per workflow; skills are referenced by name and digest

## Context and Problem Statement

[Roadmap §Decisions still open](../roadmap.md#decisions-still-open) parks one question for O1: *"`workflow.yaml` per workflow or per step, given shared skills"*, leaning *"per workflow, steps reference skills by name+digest"*. The `feature` workflow forced it: four of its twelve steps — `gather-context`, `implement`, `commit`, `pr` — are `quick-fix`'s skills unchanged, and it was the first workflow that had to reach outside its own directory.

## Decision Drivers

- [Core principle 6](../core-principles.md): the skill is portable, the mill record is ours
- A shared skill cannot hold a setting that differs between workflows ([Domain model §Step](../domain-model.md#step))
- The sequencing of a workflow is one thing a reader must be able to see whole ([Core principle 7](../core-principles.md))
- Duplicating a skill's bytes to reuse it makes "the same skill" unprovable

## Considered Options

1. A file per step, beside each `SKILL.md`
2. A global skills directory outside `workflows/`, shared by every workflow
3. One `workflow.yaml` per workflow directory, steps naming a skill as `<name>` or `<workflow>/<name>`

## Decision Outcome

Chosen option: **one `workflow.yaml` per workflow**. It carries the ordered steps and, for each, the mill record: evidence, capabilities, artifact path, `on_failure`, `retries`, `timeout`, and the per-harness options document. A step's `skill:` is `<name>` for a skill in its own directory, or `<workflow>/<name>` for one a sibling under `workflows/` carries; the loader resolves the reference and the step pins the resolved name and digest, so both workflows share one `skills` row ([Domain model §Step](../domain-model.md#step)).

Reuse is therefore by reference, not by copy: `feature/gather-context` and `quick-fix/gather-context` load with the same digest, and a shared script is a symlink to the one file it already is.

### Consequences

- **Good**: A workflow's whole sequence is one file to read and one hash to version ([ADR-010](010-workflows-in-the-store-skills-portable.md)).
- **Good**: The same skill can be cheap in `docs` and expensive in `feature`, because the model and tool policy live on the step and not on the skill.
- **Bad**: A reference reaches across directories, so `workflows/` is one namespace and a skill cannot be renamed without finding its referrers. The loader refuses a reference it cannot resolve.
- **Neutral**: `SKILL.md` frontmatter stays the agent-skills spec's six fields. Nothing mill-specific goes in it, so a skill is still usable outside mill.

## Rejected Options

### A file per step

Each step's mill record lives beside its `SKILL.md`, and the workflow is the directory listing.

- **Good**: One artifact per step; a step's prompt and its record are edited together.
- **Bad**: Order has to come from somewhere — a numbering convention, or a manifest that is `workflow.yaml` again — and a reviewer cannot see a twelve-step sequence without opening twelve files. Worse, it welds the record to the skill, which is exactly what a shared skill cannot carry.

### A global skills directory

All skills under one top-level directory, workflows holding only sequences.

- **Good**: Sharing needs no cross-directory reference; a skill has one obvious home.
- **Bad**: It separates a workflow from the prompts it is made of, so `mill workflow push workflows/feature` no longer names a self-contained thing. Every workflow would pay for a layout only reuse wanted.

## More Information

Related: [ADR-010](010-workflows-in-the-store-skills-portable.md), [ADR-017](017-workflow-and-skill-bytes-in-mills-database.md). `internal/workflow` resolves the reference.
