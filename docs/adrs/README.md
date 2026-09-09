# Architecture Decision Records

The decisions behind mill's design, one per record. `docs/` says what mill is; these say why it is that and not the alternative, and they are where to look before changing one.

Format is dagger's: frontmatter status and date, context, decision drivers, considered options, the outcome and its consequences, and the options that lost. A decision is superseded by a new record, never edited away.

| ADR | Decision |
|---|---|
| [001](001-just-a-workflow-runner.md) | Mill is just a workflow runner — no planning, no backlog, no sandbox, no surface of its own |
| [002](002-claimnext-wait-is-the-loop.md) | `ClaimNext --wait` is the loop; a worker asking for work is the scheduling, so there is no scheduler and no event consumer |
| [003](003-harness-adapter-and-the-sidecar.md) | Sessions run behind a harness-agnostic Go interface, first implemented by a Claude Agent SDK sidecar speaking proto-typed NDJSON on stdio |
| [004](004-evidence-closes-steps.md) | Evidence mill verified closes a step, agents never do, and `current_step` is derived rather than stored |
| [005](005-dagger-first-store-second.md) | Dagger is written first and the store second; the store never asserts what dagger has not made true |
| [006](006-lease-fenced-store-writes.md) | Every store write is fenced by the lease inside the writing transaction, after the row lock and a re-read |
| [007](007-one-live-run-per-node.md) | One live run per node; a second claim resumes it, and starting over is a new generation only a human opens |
| [008](008-revise-supersedes-the-span.md) | `revise` supersedes every step from its target through the failure, so pass-one evidence never closes pass-two work |
| [009](009-auto-merge-always.md) | Auto-merge always; repo protections are the repo's job |
| [010](010-workflows-in-the-store-skills-portable.md) | Workflow definitions live in the store, seeded from files; the skill is the portable part and mill's record sits beside it |
| [011](011-usage-per-step-per-model.md) | Usage is posted per step, per model, and each report renews the lease |
| [012](012-workspace-exec-is-the-chokepoint.md) | `Workspace.Exec` is the single chokepoint for spawned processes; `os/exec` is forbidden elsewhere by lint |
| [013](013-sessions-under-the-permission-classifier.md) | Sessions run under the SDK's permission classifier; `bypassPermissions` is refused until the workspace is a sandbox |
| [014](014-reviewer-is-the-same-principal.md) | The reviewer is the same principal in its own session, read-only by tool policy and capability set, attributed by the ledger |
| [015](015-commit-evidence-is-a-commit-the-step-made.md) | Commit evidence is a commit the step made: descended from the attempt's `base_commit`, on the run's branch, and the mill's own |
| [016](016-one-workflow-yaml-skills-by-name-and-digest.md) | One `workflow.yaml` per workflow; a step references a skill by name and digest, across workflow directories |
| [017](017-workflow-and-skill-bytes-in-mills-database.md) | Workflows and their skills live in mill's database, versioned by digest; the set is whatever has been pushed |
