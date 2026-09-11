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
| `/build-workflow` — the `/work-on` tier assessment as a skill step emitting `{workflow, tier, signals, reasoning}`; the schema is settled in Domain Model §Selection, "The assessment's schema" | B1, C4, Domain Model §Selection |
| `/review` skill: `/simplify`'s reuse/quality/efficiency plus criteria met, encoded in tests, tests pass; structured verdict `approve \| revise` | B3, C10, Domain Model §Verdict |
| Plan approval as a gate; `auto` when review-plan approves and tier ≤ feature | R-21, Domain Model §Gate |
| Gate reaches Slack: `thicket send` from the gate step as the stopgap until the thicket bridge consumes dagger events | D1, B14, Architecture §Gate steps |
| CI watch: `status` evidence polls the pipeline; failure returns the log to the implementer as `revise`, bounded | R-28, `evidence` node |
| The seeded-bad-implementation acceptance test — a change that compiles, lints, and violates a criterion; the pipeline rejects it | R-25, M.2, `e2e` node Notes |
| `rerun <run> --from <step>`, `workspace show\|clean`, `workflow show` | R-52, R-53, `tools` node Notes |
| `plugin/mill` — skill plus `.mcp.json` so thicket agents pick up `mill mcp` like dagger's | R-56 |
| ADR recording the reviewer-principal decision: same principal, read-only policy, revisit only if dagger policy demands | C2 |

**Done when** M.2 holds, and a feature node the reference harness would have worked is delivered with a plan gate answered from Slack.

## O2 — Isolated

A run's sessions execute inside a machine the worker started without root: an ephemeral `systemd-nspawn` snapshot of an image the repo's `mill.toml` declared, the build cache warm from a btrfs lineage, egress only through the host, and no credential in the machine. The design is `docs/sandbox.md`, its behaviour `docs/sandbox-scenarios.md`, its decision [ADR-018](adrs/018-a-runs-sandbox-is-a-systemd-nspawn-machine.md). Sandboxing was made the priority on 2026-09-10, ahead of unattended operation and pluggability, which moved down one milestone each with their content unchanged.

| Item | Decided |
|---|---|
| The sandbox design lands in docs/ — the spec, the scenarios, ADR 018, and the roadmap reordered | mill#55, ADR-018 |
| Spike — a machine boots from the operator's uid through polkit, execs, runs podman, reaps a step, and shares a workspace | mill#73, Sandbox §Privilege, §Lifecycle |
| Spike — a session answers through a host gateway holding the subscription token, with no login file in the machine | mill#74, Sandbox §Credentials |
| Spike — egress through squid only, the db suite inside a machine, Harbor-proxied pulls, and the dnf shim | mill#75, Sandbox §Network, §The dnf shim |
| Spike — a machine inside a machine, so the sandbox can test itself; then unprivileged nspawn and vmspawn as informants | mill#76, Sandbox §Self-hosting, §Privilege |
| Spike — cache lineages restore and promote by snapshot in constant time, and a derived image builds, boots, and falls back | mill#77, Sandbox §Caching, §Images |
| The acceptance suite runs against a disposable dagger, not the operator's deployed one | mill#51, Sandbox §Testing |
| The proxy and gateway variables cross both environment floors as workspace decisions | mill#54, Sandbox §Network, §Credentials |
| The workspace backend is chosen by configuration, and the machine backend has a driver seam | mill#56, Sandbox §Self-hosting, Architecture §The workspace |
| The credential gateway — the session holds the step token, the host holds the subscription | mill#57, Sandbox §Credentials, principle 9 |
| mill.toml — what a repo declares about its machine, validated, with defaults from mise.toml | mill#58, Sandbox §mill.toml |
| Images — the base Containerfile, derived images by digest, the build lock and the fallback | mill#59, Sandbox §Images |
| Cache lineages — restore by snapshot, promote only from evidence, keep two, reset globals over cap | mill#60, Sandbox §Caching |
| The machine backend — create, attach, destroy, rotate: a run's machine through mill-machine@ | mill#61, Sandbox §Lifecycle, §Privilege |
| Exec into the machine under a step slice, and a step boundary that reaps everything | mill#62, Sandbox §Exec, §Step boundary |
| Reconciliation on resume, and the sweep of every kind of external state a machine can leave | mill#63, Sandbox §Reconciliation on resume, §External state |
| Preparatory steps degrade into findings — toolchain, image build, mill.toml, and the dnf shim's report | mill#64, Sandbox §Preparatory steps, §The dnf shim |
| The mill skill rides with the worker — mounted read-only into the machine, and it documents mill.toml and the proxy | mill#65, Sandbox §Skill mount |
| nested — a repo whose tests boot machines may do so inside its own machine | mill#66, Sandbox §Self-hosting |
| Eviction — expiry, then least-recently-used under free-space watermarks, images included | mill#67, Sandbox §Eviction |
| A session waits in the foreground for as long as its step may run | mill#68, Sandbox §Preparatory steps |
| End to end inside a machine — a node delivered, the test suite run, the boundaries proven | mill#69, Sandbox §Testing |
| The documents catch up with what was built — runbook, tools, architecture, the repo map | mill#70 |
| Prove O2 — the live tier run on core by a person, then mill moved into its own machines | mill#71, Sandbox §Self-hosting |
| Mill moves into its own machines — the deployed worker delivers a node inside a machine with the live tier as a check step, and the O2 retrospective | mill#72, Sandbox §Self-hosting |

**Done when** the live tier has passed on `core` with the ginkgo summary as the gate's result, and mill's own sessions run inside machines with `[machine].nested`, so the sandbox tests itself from then on.

## O3 — Unattended, for real

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

## O4 — Pluggable

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

Parked here from the former O4 — *Isolated* — because O2 took its sandbox and left these, each with its provenance; they are not in this milestone's done-when:

| Item | Decided |
|---|---|
| Capability sets enforced by the workspace, not the adapter's tool policy; `exec` gated against `write`; reviewer genuinely read-only | R-32, C3, Domain Model §Capability set |
| Reviewer end-to-end verification: runs the thing (e.g. dagger) to confirm the bug is fixed; needs the machine workspace first | B3 (post-MVP) |
| Secrets service: step credentials minted from Vault per step, none on disk | R-37, bet *Agents hold no secrets* |
| Per-run SSH certificates from a CA, principal `mill/<node>`, TTL = the run; `git verify-commit` as provenance | C1 |
| Mill as its own account and `agent` principal — requires dagger's `onlyOrchestrators` to admit it or a stored delegation grant; every gate veto then applies | R-35, Dagger principal check §5–6 |
| microVM runtime for untrusted workloads and edge services: `systemd-vmspawn` boots the same tree, Sandbox §Scope | bet *The workspace seam is enough* |
| Ephemeral sandbox uids mapped to a principal (dagger's open item) | Dagger principal check §6 |

**Done when** M.3 holds, the third workflow costs less to add than the second, and Codex runs `quick-fix` unchanged.

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
| `skills.files` bytes in mill's database or dagger's blob store | mill's, so a workflow loads without dagger | O1 |
| Gate filed in dagger but not recorded in the store (crash between) — search open user-assigned children before filing | as `gates` node Notes | O0 |
| `revise` semantics when the target step is in a different session | supersede as usual; the target session resumes with feedback | O1 |
| Stale-lease usage: reconcile abandoned attempts' usage into dagger, or accept the undercount | accept; the ledger is complete | O3 |
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
| The `mill` role on `core`: the btrfs volume, `mill-machine@.service` and its polkit rule, the nftables table, squid, the SELinux module, the model credential by `LoadCredential` (Sandbox §Sequencing across repositories) | homestead | O2 |
| Attested releases and a CI runner for mill's own repo | homestead, the delivery bet | O3 |
| Vault | homestead, the identity bet | O4 |
