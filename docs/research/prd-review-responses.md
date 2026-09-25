# PRD Review — Responses

Responses to the inline `%%` comments in [the PRD](../prd.md). Comment inline here the same way; v2 of the PRD gets written from both documents together.

---

## A. Candidate core principles

Things stated as decisions that are actually principles, and belong in the mill's `docs/core-principles.md` with teeth.

### A1. Repo protections are the repo's job
Auto-merge always. If a change needs more care than the mill's own checks, the repository enforces it — CI gating merge, required reviews, branch protection. The mill never second-guesses a forge that let it merge. If nothing blocks, hitting the default branch is done.

### A2. One node's worth of state per run
The mill never holds more than the current node's state in memory or ledger. Decomposition is a workflow that files child nodes in skift and ends; the mill never keeps a sub-plan. This is the rule that stops it growing a planner or a shadow backlog.

### A3. Every spawned process goes through the workspace
`Workspace.Exec` is the single chokepoint for anything a step runs — it sets cwd, uid, and environment. `exec.Command` is forbidden outside the workspace package (depguard/forbidigo, as skift does). Checks are `Workspace.Exec` calls with a contract. This is what makes the local → container → remote swap a config change.

### A4. Escalation has a price
There is no `needs-human` verdict. A reviewer may `approve` or `revise`. A human is reached one way: the circuit breaker — retry cap exhausted, last findings as a comment, skift gate. Asking a human costs three real attempts.

### A5. One way to create work
The workflow that decomposes a node uses the same skill a thicket agent uses to file work from a conversation. There is one path into skift, and the mill is a client of it like everyone else.

### A6. The skill is portable; the record is ours
A workflow step is an agent skill (name, description, body, scripts, assets — the part that survives switching harnesses) plus an mill record beside it (output schema, ordering, the gate check, harness translation, provenance, scope). Nothing mill-specific goes in `SKILL.md` frontmatter beyond a flat breadcrumb.

### A7. Routing may be judgment; sequencing must not be
A description-driven router may *choose* a workflow. Within a workflow, order and gates are explicit metadata. The spec's own docs say activation-by-description is probabilistic; nothing that must happen relies on it.

### A8. Evidence closes steps, agents don't
A step is complete when the mill has verified a fact outside the session: a commit SHA that resolves, a test process that exited 0, a verdict that parsed and said `approve`, a PR URL the forge returns, a merge commit on the default branch, a healthcheck that answered. The workflow names the evidence each step must produce; the mill checks it; nothing else advances the run. An agent saying "done," a transcript that reads well, or a task list marked complete is not evidence and never closes a step. This is the principle every other one serves: it is what makes a workflow enforced rather than described, what makes resume possible, and what makes a run's record trustworthy after the fact.

### A9. A session never sees a credential it could keep
A step is the thing that runs a session and carries a capability set, so a step is the unit a credential is scoped to. The store issues a token per step; that token is the only credential the session holds; every derived credential — git, forge, registry — is minted against it and dies with the step. The session's environment contains nothing worth stealing. Buildkite arrives at a similar ladder by scoping to the job; ours follows from the domain model, where the lease belongs to the run and the capability set to the step.

---

## B. Decisions taken from your comments

Recorded so v2 writes them as text. Push back on any.

- **B1.** A `/build-workflow` skill — the `/work-on` tier assessment, adapted for unattended use — *produces* a workflow for the node; the mill follows it deterministically, activating the selected skills.

- **B2.** Auto-merge always (→ A1). Squash merges: the evidence for `merge` is the merge commit SHA on the default branch from the forge API, never the branch ref.
- **B3.** A `/review` skill is the reviewer: Claude Code's built-in `/simplify` covers reuse, quality, and efficiency; `/review` adds that the acceptance criteria are met, are encoded in the tests, and the tests pass. Post-MVP: end-to-end verification — the reviewer runs the thing (e.g. skift) to confirm the bug is fixed — which needs the reviewer to write to a workspace and needs system containers first. R-25 (seeded-bad test) stays regardless.

- **B4.** Harness-agnostic Go interface. First adapter: a TypeScript Claude Agent SDK sidecar spawned as a child process, speaking **newline-delimited JSON over stdio** — the way MCP and LSP servers do, and the way the Agent SDK itself streams. Message types defined in proto and generated to both Go and TypeScript with buf, so the wire is typed without Connect's transport. Connect over a unix socket is the upgrade if a sidecar ever needs to outlive its parent or serve more than one worker; a child process on stdio doesn't. Spikes scoped as deliverables. Codex and Pi later; native Go for Claude Code eventually.

- **B5.** Workflow definitions live in the mill's store, seeded from the repo, with pinned history as provenance. CLI to push/pull from a directory. Evals-in-a-git-repo later.
- **B6.** Sessions: one *resumable* session carries gather-context → think → plan → implement → simplify → commit → PR. The reviewer runs in a separate context. Review feedback returns to the implementer's session (cached tokens; reviewer memory prevents goalpost drift). A `session:` field on a step names the session it runs in; steps sharing a `session:` value resume the same one (`session: implement`). One word, reads as YAML, no bikeshed.

- **B7.** Session ids are UUIDv5 from the node UUID + step key. Retry and resume are "resume the session you can name."
- **B8.** A check is a command path — script or binary — emitting an exit code, optionally JSON on stdout and diagnostics on stderr. Failure text feeds back into the session ("Workspace is dirty: stage, delete, or ignore uncommitted files").
- **B9.** Capability sets: designed for, built post-MVP. Agents can flag "I needed X." MVP is a system user with scoped workspaces; ephemeral system containers with podman later; never across zones.
- **B10.** Single-run mode works the whole subtree. `mill run <epic>` is what the reference harness does today, so the harness's e2e tests become the mill's.
- **B11.** The reference harness in skift stays as reference and e2e verification, documented as such. Not retired.
- **B12.** CI adapters, GitLab first. Status checks as the integration point so Buildkite or others slot in.
- **B13.** Skift projects need a VCS URL. Check whether skift's project model has the field; if not, it's a skift node.
- **B14.** A thicket bridge that consumes skift events and routes to agents and existing sessions — later, intent documented now. Your "agent waits on / subscribes to a node" stories are skift `WatchEvents` plus that bridge; the mill's only job is that every step and completion is a skift event.
- **B15.** No `workflow:` label on skift nodes (abstraction leak). Selection: project default in the mill's store → the tier assessment. Skift never learns the word "workflow."
- **B16.** Prompt drift from `/work-on` is fine; adapt the skills for unattended use rather than template them.
- **B17.** R-14 tunables (token budgets, turn limits, effort) post-MVP; the schema is designed for them now.
- **B18.** Three docs, in order: domain model, architecture, ledger. The architecture doc is where C12's mechanisms get elaborated.

- **B19.** Skills have a hierarchy: org-wide and project-wide, mirroring the spec's precedence rule (project overrides, warn on shadowing).
- **B20.** Fresh workspace per run, disposable; the unit of distribution across workers is a **run**, not a step. Buildkite's "stateless, runs anywhere" is only true at the network layer — its agents cache checkouts, plugins, and mirrors on disk keyed by agent name, and reuse checkouts across jobs. We take the opposite default (R-33, R-34) and pay the clone cost. Persistent worker-owned workspaces are an O4 option behind the same handle, not the starting point.

---

## C. Your questions, answered

### C1. Commit signing — own key, or ephemeral certs from a CA?
Both, in order. **MVP:** an SSH signing key owned by the mill's account, non-interactive, with `allowed_signers` in each repo naming it. A day of work; resolves #344's failure mode. **End state:** per-run SSH certificates from a CA (step-ca or Vault's SSH engine), principal `mill/<node-id>`, TTL = the run. The certificate *is* provenance — `git verify-commit` tells you which node made the commit — and there is no long-lived key to leak. Sigstore/gitsign (OIDC keyless) matches your OIDC instinct but needs an OIDC issuer for the mill's identity, which doesn't exist until the identity bet lands. Key now, CA certs when Vault exists, gitsign in the back pocket.

### C2. Reviewer under its own principal — what are we guarding against?
Over-reach on my part. I was guarding *provenance*: the verdict should be attributable to something other than the thing it judged. But the ledger records which session produced the verdict, and a reviewer session with no write tools can't fix what it should fail. A separate *principal* only matters when **skift policy** must enforce "completer ≠ reviewer" — your Renovate rule, which is a delivery-pipeline concern. **Conceded:** same principal, reviewer session with read-only tools, ledger attribution. Off O4.

### C3. How is the reviewer's workspace made read-only?
In MVP it isn't, honestly: the adapter passes a tool allowlist (no Edit/Write/Bash-write) — the model's honor plus the SDK's enforcement. Real read-only is a second checkout or a read-only bind mount, which is the O4 workspace implementation. Written as: "enforced by the adapter's tool policy in MVP; by the workspace in O4."

### C4. "Assessment vs. labels" — what did I mean?
R-6 had three ways to pick a workflow: a `workflow:` label on the skift node, a project default, or a model assessment step (the `/work-on` tier prompt, non-interactive, emitting `{tier, signals, reasoning}`). You killed the first, correctly (B15). OQ3 asked whether the assessment should run at all in the first cut or every node be explicitly assigned. Your answer — don't prescribe until it hurts — resolves it: assessment runs, project default overrides, no explicit assignment exists.

### C5. "It grows a planner" — what does the design look like?
Decomposition is a workflow, not mill code. A `decompose` workflow: gather-context → think → emit a structured patch of child nodes, each with criteria and scenarios → `skift patch apply` under the mill's principal → mark the parent a grouping node → end the run. Children become ready on their own; the daemon claims them like anything else. The mill never chose which child runs — skift readiness did. Same skill a thicket agent uses to file work (→ A5). The risk I was naming is the other design: the mill looking at a big node and running sub-steps internally, holding a sub-plan in its own memory. That's the shadow backlog (→ A2).

**Post-MVP.** Seeding a big idea as a project and letting the mill research and build it is the full-autonomy reach, and it's where this design leads — but not from day one. MVP works nodes that a human or a thicket agent already decomposed. The `decompose` workflow lands when the runner is trusted with leaf nodes.

### C6. Workspace seam — "is this true?" (`exec.Command`)
No, not as I wrote it. A lint on `os.Open` catches nothing that matters; `exec.Command` does the damage. The real guarantee is the single chokepoint in A3. Enforceable from day one with the depguard/forbidigo pattern skift already uses; doesn't fight determinism because checks *are* `Workspace.Exec` calls.

### C7. Three-process split — big can of worms?
Half yes — and buildkite-agent sharpens where the line goes.

**Buildkite is two processes, not three.** The hosted control plane is store + API + scheduler + pipeline compiler; the agent is a worker that spawns an executor subprocess (`buildkite-agent bootstrap`) and talks to it over env files, or in Kubernetes mode over a unix socket. Your process (2) — "watches events, picks up work, builds a workflow" — is what Buildkite does *server-side*. And your harness sidecar is exactly their executor subprocess.

**Recommendation, revised twice:** skift's pattern — one binary, **two** faces — and no scheduler at all. Skift's domain model says it directly: *"Orchestrators do not use [the change stream]. They call `ClaimNext` with a wait, which blocks until work matching their filters is available and returns it already claimed. An orchestrator wants work, not notifications."* Readiness is derived, so there is no "node became ready" event to watch. The worker's loop *is* `ClaimNext --wait`.

- `mill serve` — the store and its API: workflows, runs, ledger, step tokens.
- `mill work` — the worker: `ClaimNext --wait`, look up or create the run, own its workspace, spawn the harness sidecar per step, write evidence to the store over Connect.

Design the Connect boundary between them from day one; run both in one process until a second host needs a worker; the split is config.

**The distribution decision, made:** the unit of work a worker takes is a *run*, because steps of a run share a workspace (B20). Buildkite reaches the same place by a different road — it keys build directories by agent name and reuses them, which is the choice we're rejecting.

**Fencing.** Buildkite has no job lease and no fencing; liveness is the agent heartbeat, and the agent never proves it still holds the job when it writes `finish` or a log chunk. If the agent dies mid-job, the server decides and the agent never knows. Skift gives us fenced leases, so: the fence goes on every write the worker makes, and a fenced-out `complete` is the *normal* path for a stale worker, not an error to retry. That's stronger than Buildkite and it costs nothing.

**Acquire vocabulary worth mirroring.** Their acquire returns `423 Locked` + `Retry-After` for "blocked by DAG dependencies" and `422` for "someone else holds it," with a lock header naming the intent. Skift's claim already has fenced semantics; make sure its refusals are that legible to the worker.

### C8. Does structured output blow up the agent-skill idea?
No — but the spec is smaller than expected and the boundary it forces is good (→ A6). Full analysis in [agentskills spec analysis](../research/agentskills.md). The short version:

- The **entire** normative frontmatter is six fields: `name`, `description`, `license`, `compatibility` (free text), `metadata` (string → string only), `allowed-tools` (experimental, Claude syntax, "support may vary"). No output schema, no model/effort, no `version`, no `after`/`next`, no way to name a script as the gate. The spec is designed to stay that small.
- A top-level `x-mill:` block **fails the reference validator**. Nested values under `metadata` are silently stringified. The only compliant extension surface is a flat string map.
- So: skill = portable part; **mill record beside it** (`workflow.yaml` next to `SKILL.md`, or a store row keyed by skill name) carries: output schema (ship it in `assets/`, which the spec allows), ordering, the gate check and its exit-code contract, per-harness model/effort/permissions, provenance (content hash of the skill directory — the spec has no version field), and scope.
- The spec's `using-scripts.mdx` already recommends your check wire contract verbatim: JSON on stdout, diagnostics on stderr, meaningful exit codes.
- The spec sanctions your session model: "subagent delegation — the skill runs in a separate subagent session and returns a summary."
- 46 clients listed including Codex, Gemini CLI, Cursor, OpenHands — no conformance matrix; portable core is name/description/body/relative refs.

### C9. Skill model/effort per harness → the adapter translates
Agree. Belongs in the mill record's harness table (C8). And your instinct that a skill should be *used as a prompt* by the adapter rather than installed as a harness-native skill is right — otherwise you fight each harness's discovery rules.

### C10. `needs-human` — over-escalation
Drop it (→ A4).

### C11. Ledger and domain-model specs
Agreed (B18). Domain model first, following `skift/docs/domain-model.md`'s shape.

### C12. What buildkite-agent gives us
What each of these becomes in our design is specified in the architecture doc; this is the map. Full analysis in [buildkite-agent design analysis](../research/buildkite-agent.md). Mapped to requirements:

**Steal.**
- **Token laddering** (`api/client.go:165`, `job_runner.go:172`) → R-37 and A9. Store issues a lease-scoped token per (node, step); every derived credential — git, forge API, registry — is minted against it server-side and revoked on release.
- **The Job API socket** (`jobapi/server.go`) → *this is the workspace handle's transport.* A unix socket with a per-job bearer, and the step talks to it instead of holding raw state. Theirs has `env`, `workdir`, `redactions`, `promise-failure` verbs; ours adds `fs`, `git`, `checks`. Answers "how does a step receive the handle": not as a path in env — as a socket and a token.
- **Executor as a subprocess with an env-file handoff and a `pre-bootstrap` gate** (`job_runner.go:363, 840`) → the sidecar shape, plus: a policy hook that can reject *before* anything executes. Put the capability-set check there.
- **Redactor seeded before secrets touch env, extendable at runtime, flushed at hook boundaries** (`executor.go:1457`) → seed from the capability set, let checks register new needles, flush at step boundaries. Log redaction is table stakes for streaming session output.
- **`ProcessExit{Status, Signal, SignalReason}` with `-1` = never ran** (`run_job.go`) → R-14. Keep "refused / signature / timeout / cancelled" distinct from "check failed" so retry policy can key on which one. Their comment explains the reluctance to add reasons: customers key `automatic_retry` on them. Ours will too.
- **Interrupt → grace → kill, with cleanup on a grace-period context** (`run_job.go:Cancel`, `grace.go`) → R-50. SIGTERM the session's process group, collect evidence in the grace window, then SIGKILL. `finish` is always attempted on a non-cancellable context.
- **Hook env-diff with a protected-key filter** (`hook/wrapper.go`, `env/protected.go`) → steps emit a *diff* to session context; the mill owns which keys are writable. Their note that `git is riddled with shell injections` is why checkout config has a separate override scope.

**Do not copy.**
- **Hooks as steps.** Their plugin system is an aspect system — every plugin hooks the same fixed lifecycle, composition is lexical order, identity is a mutable git ref. Good for cross-cutting concerns; poor for typed inputs, outputs, or evidence. Borrow the env mechanics, not the model. (→ A6, A7)
- **No structured step output.** Success is the bootstrap's exit status, full stop; everything else is a side channel (annotations, meta-data, artifacts). Confirms evidence must be a first-class artifact the store validates. (→ A8)
- **Reused, mutable checkouts keyed by agent name.** (→ B20)
- **Scale-driven machinery** — the ping baton, debouncer, stream fallback, jitter windows, `%spawn`. One operator watching a store needs none of it.
- **Cancellation by polling job state every second.** We own the socket; push cancel through the handle.

---

## D. Things you didn't comment on that I'd flag

### D1. O1 depends on a notifier that doesn't exist
R-20 says the mill doesn't notify; B14 defers the thicket bridge. O1's done-when ("plan gate answered from Slack") then has no path. Stopgap to name in v2: the mill calls `thicket send` on gate creation until the bridge exists.

### D2. Evidence → skift artifacts vs. ledger-internal
Starting line: anything a human or another agent would want to *see* — plan, verdict, PR URL, merge SHA, cost — is a skift artifact on the result. Anything only resume needs — session ids, step cursors, retry counts — is ledger-only. Every skift artifact the mill files carries the run id in its metadata, so a human can walk from a result back to the ledger.

### D3. R-12 and the skill standard
Resolved by C8: the skill body can describe the output shape in prose (the spec recommends a template); the enforceable schema lives in the mill record and is applied by the adapter.

---

## E. Next

1. Fold the buildkite-agent findings into C7.
2. PRD v2: every `%%` comment and every item above resolved into text; principles A1–A8 pulled out into their own section.
3. Domain-model spec.
4. Ledger spec.
