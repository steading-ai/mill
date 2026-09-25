# Mill (PRD)

**Mill** — the piece of the factory that consumes skift tickets and delivers running software.

**Status:** draft for review. Numbered requirements (`R-n`) are meant to be cited from ADRs and skift nodes.

---

## 1. Problem

> I discuss some things with a thicket agent, they put tickets into skift, and they're immediately worked on and delivered — *running*. Without that, I might as well just use Claude Code directly.

Every other layer of the factory exists. Thicket makes agents reachable; skift holds the plan and computes what is ready; homestead runs it all. Nothing consumes a ready node and turns it into a running change without a human starting it, scoping it, and babysitting it. The backlog is growing faster than it drains.

Three attempts have been made at this layer. Each failed at the same place, and the failures are the specification:

| Attempt | What it did | Why it stopped |
|---|---|---|
| hive | GitHub Projects → sandboxed agent → PR | Wrong backlog (no graph, no readiness), agents carried the human's identity |
| work-on (Go) | Read GitHub issues, "shaped" them, drove a fleet | Had to be its own planner; never trusted; never used |
| Reference harness (Python) | Claims from skift by epic label; implementer → reviewer → commit → push → complete | Works, ~5× hands-on throughput, unattended once started — but one epic in one project, manually launched, reviewer has never rejected anything in 59 nodes, zero design attention |

And the `/work-on` Claude Code skill encodes the *right* workflow — tiers, gather-context, plan, review-plan, implement, simplify, commit, PR — but every guardrail in it is prose the model is asked to police itself. Steps get skipped. Nothing records which step ran. When it works it is almost perfect; it cannot be trusted to work.

The gap is not "we need a loop." The loop has been written three times. The gap is a runner that **enforces** the workflow with evidence, runs without being started, and can be trusted with more than one epic.

## 2. Who it is for

- **The operator** (one person today). Files or discusses work, approves gates from Slack or a phone, reads results. Never sits in a session for work to progress.
- **Zone agents** (thicket). File nodes in skift from conversations. Do not run the mill; are notified by it.
- **The factory's future departments** (Janitor, SRE, …). Each is a workflow definition plus an account. The mill is what makes "adding a department" configuration instead of code.

Not for: other operators, teams, or products. One deployment, one operator, until skift has a second tenant (skift roadmap M4).

## 3. Goals and non-goals

### Goals

1. **Drain the backlog unattended.** A node that becomes ready in skift is claimed and worked with no human action, across every project the mill is configured for.
2. **Enforce the workflow.** Every step's completion is proven by evidence the mill checks, never by an agent reporting it did the step.
3. **Deliver running software**, not PRs. The workflow does not end until the change is merged, deployed where applicable, and verified — or a gate is waiting on a human with the reason attached.

4. **Be trustworthy enough to leave alone.** Deterministic gates before any model judgement; a reviewer that has demonstrably rejected bad work; its own identity so its mistakes have a name.

5. **Be testable one node at a time.** Single-run mode on the same code path, so a new workflow definition can be exercised before it runs unattended.

### Non-goals

The mill is **just a workflow runner**, scoped the way skift was scoped to just project management. It does not:

- **Plan, decompose, or prioritize.** Skift holds the plan. If a node needs decomposition, the workflow files child nodes in skift and stops; it does not decide what runs next.
- **Hold a backlog.** No local queue, no shadow state of what is ready. `skift ready` is the truth.
- **Sandbox.** It hands each step a workspace handle; what enforces isolation behind that handle is homestead's job. (It is designed so that sandboxing can be added without rewriting — see §6.8.)

- **Converse.** No chat surface, no personas. Questions and approvals become skift gates that thicket carries to a human.
- **Be a general workflow engine.** It runs software-delivery workflows for one operator. Not Airflow, not Temporal, not a DAG scheduler for arbitrary jobs.
- **Be an interactive coding assistant.** Claude Code is that.

Any feature that pulls toward one of these is the scope-creep signal. Cut it.

## 4. Success — the milestone

The mill has succeeded when all of the following are true on the homestead deployment:

- **M.1** A node filed from a Slack conversation with a zone agent is claimed, worked, and delivered as a running change with no human action other than answering gates — and this has happened across **two or more projects** in one unattended run.
- **M.2** The acceptance suite includes a seeded bad implementation and the pipeline rejects it. The reviewer has said no, on record, on purpose.
- **M.3** A second workflow type (docs-only or dependency-bump) has shipped as a workflow definition with no change to the mill's code.
- **M.4** The reference harness is retired because this runs its epics better.
- **M.5** Usage and cost per node are in skift, posted by the mill under its own principal.

Nothing moves to a later milestone until the current one is used daily. A milestone that is not in use has not removed anything.

## 5. Concepts

| Term          | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Node**      | A unit of work in skift. Has a body, acceptance criteria, Gherkin scenarios, labels, inputs (results of the nodes that block it), and an `assignee_kind` (`agent` or `user`).                                                                                                                                                                                                                                                                             |
| **Gate**      | A node with `assignee_kind: user`. Only a person completes it. The mill never claims one; it creates them.                                                                                                                                                                                                                                                                                                                                         |
| **Workflow**  | A named, versioned definition: an ordered list of steps, the gates between them, and the evidence each step must produce. Selected per node. Data, not code.                                                                                                                                                                                                                                                                                               |
| **Step**      | One unit of a workflow. Either a **model step** (a headless Claude Code session with a fresh context) or a **check** (a deterministic command with an exit code).<br>                                                                                                                                                                                                                                                          |
| **Ledger**    | The mill's durable record of a run: which workflow was selected, which steps ran, what evidence closed each, what artifacts they produced. Resume reads the ledger, never a transcript.<br>                                       |
| **Evidence**  | An externally checkable fact that closes a step: a commit SHA that exists, a test exit code, a parsed structured verdict, a PR URL from `gh`, a CI status, a deploy healthcheck. An agent's own statement is never evidence.<br>                                                           |
| **Verdict**   | A reviewer's structured output: `approve` / `revise` / `needs-human`, with findings tied to acceptance criteria. Parsed, not read.<br>                                                                                |
| **Workspace** | A handle a step receives: a checkout at a known ref, with a capability set (read, write, network, `gh`, deploy). The step never sees a path or a credential directly.<br> |
| **Principal** | The mill's own identity in skift, git, and the tailnet. It claims, completes, commits, and signs as itself.<br>                                                                                                                                     |
| **Run**       | One node worked end to end through one workflow. Single-run mode executes exactly one; daemon mode executes runs as nodes become ready.<br>                                                                                                                                                                                       |

## 6. Requirements

### 6.1 Claiming

- **R-1** Claim ready nodes from skift with `ClaimNext` under a fenced lease, filtered by the projects and labels the mill is configured for. Heartbeat the lease; release it with a reason on failure.
- **R-2** Claim across projects. The unit of configuration is "which projects and label filters," not "which epic."
- **R-3** Concurrency is a cap, not a mode. Concurrency one is a valid configuration; the design assumes N.
- **R-4** Never claim, complete, drop, reclassify, or unlink a gate — including one mill filed. Skift will not stop mill from doing so while mill runs as the operator's principal (every gate veto in `policy/skift.rego` is scoped to non-user principals, by design — ADR-009); this is mill's own discipline, enforced in mill's code and tested. Prepare a gate with a node it depends on; never touch the gate.
- **R-5** Grouping nodes (children, no criteria) are closed as bookkeeping when skift makes them ready — as the harness does today.

### 6.2 Workflow selection

- **R-6** A workflow is selected per node, in this order: an explicit `workflow:` label on the node → a project default → an **assessment step**.

- **R-7** The assessment step is a model step that emits structured output — `{tier, signals, reasoning}` — validated against the set of available workflows. The tiers and signal table from the `/work-on` skill (`TIERS.md`) are the starting definition. The output is recorded in the ledger; a human can override it by relabelling the node.

- **R-8** Workflow definitions are files, versioned in the mill's repo, loadable without a rebuild. A definition names its steps, the order, which are checks and which are model steps, the evidence that closes each, the gates, retry caps, and the capability set each step receives.

- **R-9** At least these workflows ship in the first cut: `quick-fix`, `feature` (the `/work-on` Medium tier — gather-context, think, plan, review-plan, implement, simplify, commit, PR), and one non-code workflow (`docs`) to prove the abstraction.

### 6.3 Step execution

- **R-10** Each model step is a fresh headless Claude Code session: new context, the step's prompt, the node's work order and inputs, and the artifacts of prior steps. No step depends on a previous step's transcript.

- **R-11** Step prompts are the existing sub-skills (`/gather-context`, `/plan`, `/review-plan`, `/simplify`, `/commit`, `/pr`) run non-interactively: every `AskUserQuestion`, plan-mode exit, or "wait for confirmation" becomes either a default the workflow states or a gate the mill files. There is no code path where a step waits for a human inside the session.

- **R-12** Model steps emit structured output where the next step or a check needs it (plan, verdict, assessment). Prose is allowed only in artifacts destined for humans (PR body, comments).

- **R-13** Checks are commands with exit codes: build, test, lint, executable acceptance scenarios, `gh pr view`, CI status. A check's evidence is its exit code and captured output.

- **R-14** Every step has a timeout and a retry cap from the workflow definition. Exhausting either parks the node with the last output as a comment and a `needs-human` gate, and releases the lease — the harness's `harness-blocked` behaviour, made a state in skift rather than a label.

### 6.4 Ledger and evidence

- **R-15** The ledger is the mill's only state. PostgreSQL, alongside skift's, under the mill's own schema. Nothing in `state.json`, nothing in a transcript.
- **R-16** A step is closed only by the evidence its definition names. "The agent said it committed" does not close `commit`; a SHA that `git cat-file` resolves does.

- **R-17** Resume from the ledger after a crash: the same node, the same workflow, the next unclosed step, with prior artifacts. A resumed run is indistinguishable in the ledger from an uninterrupted one except for the gap.

- **R-18** Every ledger entry carries the node, the workflow and its version, the step, the model and effort, the session id, the evidence, and the usage.

### 6.5 Gates and approvals

- **R-19** Every human decision is a skift gate the mill files, blocking the node's next step: plan approval, an open question `/think` could not resolve, merge, deploy, and any `needs-human` verdict.
- **R-20** The mill does not notify. It files the gate; skift's notifier (thicket, to Slack or a phone) carries it. The answer arrives as the gate's completion in skift, and the run continues from the ledger.
- **R-21** Auto-approval is a workflow policy, per gate, stated in the definition: e.g. plan approval is automatic when `review-plan` returns `approve` and the tier is at most `feature`. Every auto-approval is recorded as such.
- **R-22** A gate that is denied ends the run with the reason as the node's result. The workflow may define a `revise` branch for plan denial; it may not loop unbounded.

### 6.6 Verification and review

- **R-23** Deterministic checks run **before** any model reviewer and are sufficient to fail a step on their own. A model reviewer is asked only what the checks cannot answer.
- **R-24** The reviewer is a separate model step with a read-only workspace and a fixed contract: the node's acceptance criteria and scenarios, nothing else. It emits a structured verdict (`approve` / `revise` / `needs-human`, findings with criterion references). `revise` returns to the implementer with the findings; bounded by the retry cap.

- **R-25** The acceptance suite for the mill itself includes at least one seeded bad implementation per workflow — a change that compiles and passes lint but violates a stated criterion — and asserts that the pipeline rejects it. This test exists before the mill runs unattended.
- **R-26** The reviewer runs under a different session and prompt from the implementer, and its verdict is stored as an artifact with provenance. Whether it should run under a different *principal* is an open question (§9).

### 6.7 Delivery

- **R-27** "Delivered" means the workflow's terminal step closed with evidence, and for code workflows that is at minimum a merged PR. Workflows for deployable projects extend to deploy and verify.
- **R-28** After a PR opens: watch CI; on failure, return the log to the implementer as a `revise` (bounded); on success, proceed to the merge gate. Ingest review-bot comments as findings, never as instructions.
- **R-29** Merge is a gate by default. A workflow may declare it auto-approved for named projects and conditions (all checks green, provenance verified, no `needs-human` findings).
- **R-30** Deploy and verify are workflow steps like any other: a check that the release landed and a healthcheck that passes. Where deployment is "repin a version in homestead," that is a proposal the steward reviews — the mill files it and gates on it.

### 6.8 Workspace and sandboxing

- **R-31** A step receives a workspace handle: `{checkout, ref, capabilities}`. It never receives a path it may treat as the host, and never a credential in its environment.
- **R-32** Capability sets are named in the workflow per step — `read`, `write`, `network`, `gh`, `deploy` — and enforced by the workspace implementation, not by the model's honor. The `allowed-tools` lists in the existing skills are the starting vocabulary.
- **R-33** The first implementation is a local directory: a fresh clone per run, at the node's ref, on the mill's host. The interface must allow a container or a remote zone account as later implementations with no change to workflow definitions. If step code opens a path directly, this requirement has been violated.

- **R-34** Concurrent runs never share a checkout.

### 6.9 Identity and credentials

- **R-35** The mill is its own skift principal, its own unix account on the host, and its own tailnet identity. It never runs as the operator.
- **R-36** It signs its own commits with its own key. Commit signing must not depend on an interactive credential (dotfiles #344 is the failure mode).
- **R-37** Credentials a step needs (`gh`, registry, deploy) are scoped to the step's capability set and injected by the workspace, not held by the session. Until a secrets service exists, the minimum is: per-step environment assembled by the mill from its own store, never inherited from the account's shell.
- **R-38** The reviewer's session and the implementer's session hold different credentials; the reviewer holds none that can write.
- **R-38a** Escalation for deployment steps is the operator's: a step with the `deploy` capability may start `homestead-apply.service` through the polkit rule the homestead `deploy` node installs, and nothing wider. This is acceptable at this maturity because the mill is the operator's process; it is revisited with R-35.

### 6.10 Usage and cost

- **R-39** Per step, record model, effort, tokens, cache reads, and cost from the session's terminal result. Post per-node usage to skift through `ReportUsage` under the mill's principal.
- **R-40** Model and effort per step come from the workflow definition, overridable by a node's estimate band from skift. Static defaults are acceptable in the first cut; the seam must exist.
- **R-41** A per-run and per-day cost cap, configurable, that parks work with a gate when reached.

### 6.11 Observability

- **R-42** Every step streams its session events to a log the operator can tail, and to the ledger as a summary. Full tool output is never logged.
- **R-43** `status` shows: running nodes, current step, elapsed, cost so far, gates waiting on humans. Readable from a terminal; a Slack rendering is thicket's job.
- **R-44** A run's artifacts (plan, verdicts, PR body, logs) are stored as skift artifacts on the node's result, so the record lives with the work and not with the mill.

### 6.12 Single-run mode

- **R-45** `mill run <node>` executes one node end to end on the same code path as the daemon: same ledger, same gates, same evidence rules. Differences are limited to: no `ClaimNext` (the node is given), gates may be answered from the terminal, output streams to the terminal.
- **R-46** `mill run --workflow <name> --dry-run <node>` prints the selected workflow, its steps, and the evidence each will require, without executing.
- **R-47** Single-run mode is the harness for new workflow definitions and the operator's way to try a one-off task. It is not a separate implementation.

### 6.13 Operations

- **R-48** Runs as a systemd unit on the host under its own account, started by homestead like every other service. Manual start is a defect, not a mode.
- **R-49** Deployed by repin from an attested release, like thicket. Migrations run on deploy.
- **R-50** Stop is safe at any time: leases are released with reason `preempted` and detail `shutdown` (skift's `ReleaseReason` has no shutdown value; preempted is the honest one — an operator took the lease away), the ledger has enough to resume, workspaces are left intact for inspection and cleaned on the next start.

### 6.14 Operator tools

- **R-51** `mill mcp` is a stdio MCP server exposing the same client the CLI uses — skift's pattern — so an operator's Claude Code session or a thicket agent can read and operate the mill on the operator's behalf.
- **R-52** Read tools, CLI and MCP alike: `status`, `inspect <run>`, `explain <run>` (what step it is on, why that step is open, what evidence would close it, what it is waiting on), `logs <run> [--step] [--follow]` (the redacted per-step logs), `history <run>`, `cost`, `workflow list|show`, `workspace show <run>` (where a retained workspace is), `doctor` (socket, database, skift reachable as the mill principal, sidecar runnable, signing key, forge credential).
- **R-53** Operate tools: `cancel <run>` (stop the current step cleanly, release with `preempted`, leave the run resumable), `rerun <run> --from <step>` (supersede from that step, as `revise` does, without a human round), `restart <node>`, `workflow push|pull`, `workspace clean <run>`.
- **R-54** Identity on the store's socket is the peer uid, mapped by configuration to a role: the mill's own account may do everything; `operator` uids may read and operate; `reader` uids may read. Anything else is rejected. No token scheme.
- **R-55** `run` — single-run mode — executes in the calling process and is therefore local to the mill's host and account. A remote session that wants work done files a skift node; it does not invoke `run` over MCP.
- **R-56** A Claude Code plugin (`plugin/mill`: skill plus `.mcp.json` registering `mill mcp`) ships with the repo, so thicket agents and the operator's sessions pick the tools up the way they pick up skift's.

## 7. Constraints

- **Stack:** Go; PostgreSQL via sqlc + goose + pgx; Connect-RPC for its own API; Ginkgo/Gomega. The same conventions as skift, so the two repos read alike.
- **Backlog:** skift, only. No GitHub Projects, no issues, no files.
- **Surface:** thicket, only, via skift gates. The mill has no user-facing channel of its own.
- **Agent substrate:** Claude Code, headless. Design for the CLI's contract (session id, structured output, hooks via settings, permission mode, terminal result with cost). Do not depend on a language SDK that may not exist for Go.

- **Deployment:** homestead, as a zone or platform service. Reaches skift over the unix socket on the same host.

- **Philosophy:** small composable units; boring and robust over clever; the domain model is the authority and code follows it.

## 8. Milestones

Dependency order, not dates. Each is defined by what becomes possible. The full plan — every post-MVP item, where it was decided, and what mill needs from skift, thicket, and homestead — is [Roadmap](roadmap.md); this section is the summary.

### O0 — Walking skeleton
One workflow (`quick-fix`), `mill run` and the `work` daemon at concurrency one, local workspace, ledger in Postgres, evidence-based step closure, deployed through homestead on the operator's account. `mill run` takes a node id and ends with a merged change or a parked gate; `mill.service` claims the next ready node on its own.

**Done when:** a real quick-fix node in skift's own project is delivered by `mill run` on the host, the ledger shows every step's evidence, and the daemon claims the next one — the `prove` gate in `Patch/`.

### O1 — Trusted
The `feature` workflow with plan approval as a gate; the reviewer with a structured verdict; the seeded-bad-implementation test passing; CI watch and merge gate. Still single-run.

**Done when:** M.2 holds, and a feature node the reference harness would have worked is delivered with a plan gate answered from Slack.

### O2 — Unattended, for real
Concurrency above one, cost caps, step tunables, model and effort from estimates, attested releases repinned by homestead. The reference harness becomes reference only.

**Done when:** M.1, M.4, M.5 hold.

### O3 — Pluggable
The `docs` workflow as a definition; capability sets enforced by the workspace; a second project onboarded with only a project default. Cost caps.

**Done when:** M.3 holds and the third workflow costs less to add than the second.

### O4 — Isolated
A container workspace behind the same handle; capabilities enforced by the workspace; credentials minted per step from a secrets service; mill as its own principal once skift admits one.

**Done when:** a workflow runs identically on local and container workspaces, no long-lived credential exists in mill's account, and mill is its own principal with `ReportUsage` still accepted.

### O5 — Distributed
`serve` and `work` on separate hosts; `remote` workspaces; the laptop as a client over tsnet; a native Go adapter.

**Done when:** a run claimed on core executes on a second host and the operator watched it from a laptop session.

## 9. Open questions

Decisions the PRD does not make. Each wants an ADR.

1. **Reviewer principal.** Same principal with a read-only capability set, or a separate principal? Separate is stronger provenance; same is simpler until a secrets service exists.

2. **Where step prompts live.** Embedded in the mill's repo, or rendered from the dotfiles skills so the interactive and unattended versions cannot drift? The latter is the "reuse the harness's own machinery" instinct; the former is reproducible builds.

3. **Assessment vs. labels.** Should the model assessment run at all in the first cut, or should every node carry an explicit workflow label until there is data to trust the assessment?

4. **Auto-merge defaults.** Which projects, if any, auto-merge on green in O1? The Renovate pipeline wants it; hand-written features probably do not.

5. **Where CI runs.** The delivery workflow watches CI — GitLab's runner today, Buildkite if that bet lands. The mill should not care, but the check step has to talk to one of them.

6. **Notifier ownership.** Skift's plan puts the notifier as a skift consumer. The mill files gates; who wakes thicket — skift's change stream, or the mill calling `thicket send` directly as a stopgap?

7. **Session substrate.** CLI (`claude -p`) or the Agent SDK via a sidecar? CLI keeps the mill a single Go binary; the SDK is what the harness uses today.

## 10. Risks

- **It grows a planner.** The first time a node is "too big," the temptation is to decompose inside the mill. The answer is a workflow whose terminal step files child nodes in skift and closes. Watch for this in every review.

- **The reviewer stays a rubber stamp.** R-25 exists because 59 approvals in a row proved nothing. If the seeded-bad test is deferred, this project has the harness's flaw in Go.

- **Prompts drift from the skills.** If step prompts are copies, the interactive `/work-on` and the unattended workflow diverge and neither gets the other's fixes. Open question 2.

- **The workspace seam leaks.** One `os.Open(path)` in step code and O4 becomes a rewrite. Enforce with a lint rule from O0.

- **Gates become theater.** If every plan gate is tapped approve without reading, R-21's auto-approval policy should replace the gate honestly rather than pretend.

- **Manual start creeps back.** R-48. If the daemon is flaky, the fix is the daemon, not a cron job that runs single-run mode.

## 11. What to read first

- [The bet](research/the-bet.md) — why this shape
- [The skill analysis](research/work-on-skill-analysis.md) — the workflow being enforced, and every way it currently fails
- `skift/docs/retrospectives/2026-09-05-first-build.md` and `skift/harness/README.md` — what the reference harness does and learned
- `skift/docs/domain-model.md` — nodes, leases, gates, events, the contract this consumes
- Prior-art research — run it before the ADRs
