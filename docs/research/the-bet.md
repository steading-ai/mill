# The orchestrator is just a workflow runner

**The bet.** The thing that consumes dagger tickets should be scoped down the way dagger was: *just* a workflow runner. It claims a node, selects a workflow, drives the steps with gates between them, and reports a result with provenance. It does not plan, decompose, prioritize, sandbox, or review — those are dagger, plugins, and neighbours. Built that narrow, in Go on dagger's stack, it delivers reliably enough that the backlog trends toward zero.

**Why we think so.** The `/work-on` skill is "almost perfect when it works" and unreliable because its runbook lives only in the model's memory of a markdown file — every fix for a skipped step has been more prose (`8d422a8`, `795b192`, #322). Moving the step ledger into a runner that marks steps done from *evidence* (a commit SHA exists, `review-plan` parsed for `Approve`, tests exited 0, `gh pr view` returns a URL) converts every "mandatory" sentence into a gate. Three prior attempts at the loop (hive, work-on, the harness) all failed at the backlog, not the loop; dagger removes that failure.

**We're right when.**
- **The milestone**: a ticket that came out of a Slack conversation with a zone agent is worked and delivered — *running* — without the owner touching it, across at least two projects.
- The acceptance suite includes a seeded bad implementation and the runner rejects it.
- `orchestrator run --node N` executes one node on the same code path the daemon uses, stopping at the same gates — single-run mode is the test harness for new workflows.
- A second workflow type (docs-only or dependency-bump) ships as a definition, not a code change.
- It runs as its own dagger principal, not the owner's uid, with usage posted through `ReportUsage`.

**We're wrong when.** Reliability doesn't beat the harness on the same epics; or the runner grows planning, decomposition, or PM features — that's the scope-creep signal to stop and cut; or the workflow definitions can't express what `/work-on`'s tiers express without falling back to prose.

**What it costs — the kick-ass half.**
1. Vision, principles, domain model, ADRs *first* — the pattern that produced 43/45. Review every harness retrospective before writing them.
2. Step ledger with evidence-based completion; resume from the ledger, not the transcript.
3. Workflow selection: the model's tier assessment emitted as structured `{tier, signals, reasoning}`, validated against a pluggable `Workflow` definition; labels short-circuit it.
4. Each step a headless Claude Code session with a fresh context, artifacts in and out; per-step `allowed-tools` becomes the capability set enforced by the workspace handle.
5. Human checkpoints become node states in dagger — plan approval (auto when `/review-plan` approves and tier ≤ Medium), open questions, blockers — never chat pauses.
6. A workspace interface from day one: local dir today, container or zone account later. Steps never see a path or the owner's keychain. Its own signing key (dotfiles #344).

**Depends on.** Dagger is the backlog. **Feeds.** Deliver means running, The workspace seam is enough, A department is a workflow and an account.

**PRD.** [Mill (PRD)](../prd.md).

**Source.** [work-on skill analysis](../research/work-on-skill-analysis.md) §3, §5, §8, §9; dotfiles ADR-007; the harness bet.
