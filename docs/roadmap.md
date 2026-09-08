# Roadmap

Milestones in dependency order, not on a calendar. Each is defined by what becomes possible and closes on a done-when a person can check. The rule that keeps it honest, borrowed from dagger: **nothing moves to the next milestone until the current one is used daily.** A milestone that is not in use has not removed anything.

Every item below was decided somewhere in this project's record — the PRD's `R-n`, the review Responses' `A/B/C`, a spec's open questions, or a patch node's Notes — and says where. Nothing here is new.

## O0 — Walking skeleton  ·  *the patch*

`mill run <node>` delivers a real quick-fix node end to end; `mill.service` runs `serve` and `work` on the operator's account on core and claims the next ready node on its own. One workflow, no reviewer, local workspace, the operator's principal.

The backlog is the O0 patch applied to dagger — twenty nodes in the mill project, one in homestead; its source is kept in the operator's vault. **Done when** the `prove` gate is completed with the retrospective's path as its result.

## O1 — Trusted

The `feature` workflow with a reviewer that has demonstrably said no, and a plan gate answered from a phone. Still one project at a time; still concurrency one.

| Item | Decided |
|---|---|
| `feature` workflow: gather-context → think → plan → review-plan → implement → simplify → commit → pr → review, one session through `pr`, review in its own | B6, `workflows` node Notes |
| `/build-workflow` — the `/work-on` tier assessment as a skill step emitting `{workflow, tier, signals, reasoning}` | B1, C4, Domain Model §Selection |
| `/review` skill: `/simplify`'s reuse/quality/efficiency plus criteria met, encoded in tests, tests pass; structured verdict `approve \| revise` | B3, C10, Domain Model §Verdict |
| Plan approval as a gate; `auto` when review-plan approves and tier ≤ feature | R-21, Domain Model §Gate |
| Gate reaches Slack: `thicket send` from the gate step as the stopgap until the thicket bridge consumes dagger events | D1, B14, Architecture §Gate steps |
| CI watch: `status` evidence polls the pipeline; failure returns the log to the implementer as `revise`, bounded | R-28, `evidence` node |
| The seeded-bad-implementation acceptance test — a change that compiles, lints, and violates a criterion; the pipeline rejects it | R-25, M.2, `e2e` node Notes |
| `rerun <run> --from <step>`, `workspace show\|clean`, `workflow show` | R-52, R-53, `tools` node Notes |
| `plugin/mill` — skill plus `.mcp.json` so thicket agents pick up `mill mcp` like dagger's | R-56 |
| ADR recording the reviewer-principal decision: same principal, read-only policy, revisit only if dagger policy demands | C2 |

**Done when** M.2 holds, and a feature node the reference harness would have worked is delivered with a plan gate answered from Slack.

## O2 — Unattended, for real

Runs the operator does not think about. Concurrency above one, caps that park rather than surprise, and a deploy that is a repin.

| Item | Decided |
|---|---|
| Concurrency cap > 1: one workspace per run already; add the semaphore's specs and the cost of clones | R-3, B20, `work` node Notes |
| Per-run and per-day cost caps that park with a gate when reached | R-41 |
| Step tunables: token budgets, turn limits, effort per step, in the workflow record | R-14 (post-MVP), B17 |
| Model and effort from dagger's estimate band, overriding the definition's default | R-40 |
| Usage-driven selection: a node class carries a model hint informed by what comparable work cost | Software Factory bet *Cost feedback* |
| Attested release: tag push → per-platform archive → homestead repins; `mill doctor` green after every deploy; no build-from-checkout on the host | R-49, bet *Deploy is a repin*, `deploy` node Notes |
| CI adapter beyond "GitLab pipeline status": status checks from Buildkite or others as the delivery bet chooses | C5 (Responses), R-27 |
| The reference harness is reference only: documented as such, used for e2e verification, never for delivery | B11 |
| `mill history --harness` — the join that answers "everything mill did today" from `harness: mill` leases and the `mill-` request-id prefix | Dagger principal check §3 |

**Done when** M.1, M.4, and M.5 hold: a node filed from a Slack conversation is delivered across two projects in one unattended run; the harness is retired from delivery; usage is in dagger under mill's leases.

## O3 — Pluggable

Adding a workflow, a project, a forge, or a harness is configuration.

| Item | Decided |
|---|---|
| `docs` workflow — the first non-code workflow, proof the abstraction holds; the doc-rot backlog is its first epic | R-9, M.3, bet *Docs are regenerated* |
| `dependency-bump` workflow — the Renovate pipeline: ingest-class review emits a verdict artifact; deterministic gates; a separate identity merges | bet *Deliver means running* |
| Workflow router: choose a workflow by skill `description` where no project default applies; eval suite per the agent-skills guidance (20 queries × 3 runs, 0.5 trigger threshold) | C8, agentskills analysis |
| Second project onboarded with only a project default | O3 done-when in PRD |
| GitHub forge adapter for public repos | `forge` node Notes |
| Second harness (Codex) behind the adapter interface; skills used as prompts, translated by the adapter | B4, C9 |
| Skill evals: a workflow definition ships with fixture nodes; `mill run --dry-run` and single-run mode are the harness | R-47 |
| `decompose` workflow — gather-context → think → patch of child nodes → parent becomes a grouping node → run ends; the path to seeding a big idea as a project | C5, post-MVP |

**Done when** M.3 holds, the third workflow costs less to add than the second, and Codex runs `quick-fix` unchanged.

## O4 — Isolated

Sandboxing and identity, behind the seams O0 drew.

| Item | Decided |
|---|---|
| `container` workspace: rootless podman with a devcontainer so tests run their own services; same handle, no workflow change | R-33, Architecture §The workspace |
| Capability sets enforced by the workspace, not the adapter's tool policy; `exec` gated against `write`; reviewer genuinely read-only | R-32, C3, Domain Model §Capability set |
| Reviewer end-to-end verification: runs the thing (e.g. dagger) to confirm the bug is fixed; needs the container workspace first | B3 (post-MVP) |
| Secrets service: step credentials minted from Vault per step, none on disk | R-37, bet *Agents hold no secrets* |
| Per-run SSH certificates from a CA, principal `mill/<node>`, TTL = the run; `git verify-commit` as provenance | C1 |
| Mill as its own account and `agent` principal — requires dagger's `onlyOrchestrators` to admit it or a stored delegation grant; every gate veto then applies | R-35, Dagger principal check §5–6 |
| microVM runtime for untrusted workloads and edge services | bet *The workspace seam is enough* |
| Ephemeral sandbox uids mapped to a principal (dagger's open item) | Dagger principal check §6 |

**Done when** a workflow runs identically on local and container workspaces, no long-lived credential exists in mill's account, and mill is its own principal with `ReportUsage` still accepted.

## O5 — Distributed

More than one host, and the laptop as a client.

| Item | Decided |
|---|---|
| `serve` and `work` as separate processes on separate hosts over the Connect boundary O0 drew | C7 |
| `remote` workspace on another host over the tailnet | Architecture §The workspace |
| Laptop as a client: `dg` over tsnet, `mill mcp` from a laptop session | bet *The laptop is a client* |
| `WatchRuns` stream for a thicket bridge that renders live status in Slack | Ledger open questions |
| Native Go Claude Code adapter, no sidecar | B4 |
| `events` time-partitioning when volume justifies it | Ledger open questions |

**Done when** a run claimed on core executes on a second host and the operator watched it from a laptop session.

## Deliberately unscheduled

- **Planning inside mill.** Never. Decomposition is a workflow that files nodes and ends (A2, C5).
- **A reviewer as a separate principal.** Dropped (C2). Revisit only when a dagger policy needs "completer ≠ reviewer" — which is the Renovate pipeline's rule, a delivery concern.
- **A chat surface, personas, a UI.** Thicket's. Mill has no user-facing channel of its own (R-20, §3 non-goals).
- **A general workflow engine.** Not Airflow, not Temporal (§3).
- **Metering-grade usage.** Estimate-grade by design, as dagger's is.

## Decisions still open

Each wants an ADR when its milestone starts. From the specs' open-question sections:

| Question | Leaning | Milestone |
|---|---|---|
| `workflow.yaml` per workflow or per step, given shared skills | per workflow, steps reference skills by name+digest | O1 |
| Assessment step's structured-output schema | after the first `/build-workflow` spike | O1 |
| `skills.files` bytes in mill's database or dagger's blob store | mill's, so a workflow loads without dagger | O1 |
| Gate filed in dagger but not recorded in the store (crash between) — search open user-assigned children before filing | as `gates` node Notes | O0 |
| `revise` semantics when the target step is in a different session | supersede as usual; the target session resumes with feedback | O1 |
| Stale-lease usage: reconcile abandoned attempts' usage into dagger, or accept the undercount | accept; the ledger is complete | O2 |
| Polling `status` vs a `WatchRuns` stream for thicket | polling until a consumer asks | O5 |

## What mill needs from the neighbours

Not mill's to build; mill's to wait on or work around.

| Need | Where | Blocks |
|---|---|---|
| A notifier that carries gates to Slack (dagger plan nodes 11–12; thicket bridge consuming dagger events) | dagger, thicket | O1 — `thicket send` stopgap until then |
| Approvals from Slack (`AskUserQuestion` re-ask bug #105, approvals #10) | thicket | O1's plan gate |
| VCS URL on dagger's project model | dagger | O0 `forge` node reads a label until then |
| `dg admin` bootstrap so a second principal is not a `psql` job | dagger | O4 |
| An orchestrator principal kind or stored delegation grants, and M2's "owns its own principal" wording reconciled with `onlyOrchestrators` | dagger | O4 |
| tsnet enabled in dagger's deployment | dagger, homestead | O5 |
| Attested releases and a CI runner for mill's own repo | homestead, the delivery bet | O2 |
| Vault | homestead, the identity bet | O4 |
