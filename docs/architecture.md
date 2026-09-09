# Architecture

How the pieces fit. The decisions behind them are the PRD's principles and, once the repo exists, its ADRs; this describes the arrangement. The vocabulary is [the domain model](domain-model.md)'s.

## One binary, two faces

```
mill serve     The store and its API. Connect handlers over PostgreSQL
mill work      A worker. ClaimNext --wait, then drive the run
mill run       Single-run mode. One node, given, same code path as work
mill restart   Abandon a node's run and start a new generation. Human-only
mill mcp       The same client as the CLI, as a stdio MCP server
mill <read>    status · inspect · explain · logs · history · cost · workflow · workspace · doctor
mill <operate> cancel · rerun · restart · workflow push/pull · workspace clean
```

The read and operate verbs are one client with two framings — the CLI for a shell, `mill mcp` for a session — exactly as `dg` and `dg mcp` are. `run` is the exception: it executes in the calling process and is local to the mill's host and account; a session elsewhere files a dagger node instead.

`serve` owns workflows, runs, the ledger, and step tokens. `work` owns nothing durable: it claims, executes, and writes evidence back over Connect. `run` is `work` with the claim replaced by an argument and gates answerable from the terminal. There is no scheduler and no event consumer: readiness is dagger's, and a worker asking for work *is* the scheduling.

MVP runs `serve` and `work` in one process on one host — `mill serve --work`, which is what `mill.service` starts. The Connect boundary between them exists from the first commit so that a second worker on another host is a configuration change. On stop the worker goes first and the store drains after it, because the worker is one of the store's clients.

A worker's settings — its projects, its labels, its concurrency, and the rest of a run's configuration — come from flags, from a YAML file at `$XDG_CONFIG_HOME/mill/config.yaml` whose keys are the long flag names, or from both: a flag the command line sets wins, and anything it leaves alone the file may fill in. A missing file is not an error; every setting has a default. This is what keeps a unit file one line and lets an operator change what the mill works on without editing a unit.

## Request path

```
              dagger                                  the store
                │                                        │
   ClaimNext ───┤                                        │
   --wait       ▼                                        │
         ┌─────────────┐   lookup / create run           │
         │   worker    │ ───────────────────────────────▶│
         │             │                                 │
         │  for each   │   step token                    │
         │  open step: │ ◀───────────────────────────────│
         │             │                                 │
         │   ┌───────┐ │   workspace socket              │
         │   │ step  │◀┼──────────┐                      │
         │   └───┬───┘ │          │                      │
         │       │     │   ┌──────┴──────┐               │
         │       ▼     │   │  workspace  │               │
         │   harness   │   │  (checkout, │               │
         │   sidecar   │   │   git, exec,│               │
         │   (stdio)   │   │   creds)    │               │
         │             │   └─────────────┘               │
         │   evidence  │ ───────────────────────────────▶│
         └──────┬──────┘                                 │
                │  Complete / file gate / ReportUsage    │
                ▼                                        │
              dagger                                     │
```

1. **Claim.** `ClaimNext --wait` with the worker's project and label filters. Returns a node, already leased, with its context id. The lease id is carried on every dagger write from here.
2. **Run.** The worker asks the store for the run by node id. None: create one in `selecting`. Exists: resume at `current_step`. Either way the worker records itself as `worker_ref`.
3. **Workspace.** Create or reattach: a fresh clone at the project's default branch for a new run; the run's branch for a resume. Start the workspace socket.
4. **Steps.** For each open step in order: request a step token with the step's capabilities; rotate the socket's bearer; execute by kind (below) while a heartbeat goroutine renews the lease; verify and record evidence; post the step's usage to dagger; revoke the token. A failure follows `on_failure`.
5. **Finish.** In this order: `ReportUsage` for anything unposted; complete the node — an `ApplyPatch` carrying `complete_node`, the result, and the artifacts, which releases the lease as part of the same patch; then mark the run `done` in the store; then destroy the workspace. Or file a gate, or park.

**Cancel.** `cancel <run>` sets `cancel_requested` on the run in the store. The worker reads it on every heartbeat tick: it cancels the running step through the adapter (SIGTERM, grace, SIGKILL), records the step run as `aborted` with reason `cancelled`, releases the lease with `preempted` and detail `cancel`, and leaves the run resumable at that step. `rerun <run> --from <step>` is `cancel` followed by the same supersede `revise` performs, so the next claim starts at the named step.

**Heartbeat.** Dagger's default lease TTL is fifteen minutes and a single `implement` step can run an hour. The worker heartbeats every lease it holds on an interval well inside the TTL, for as long as any step runs, in a goroutine independent of the step. A heartbeat that comes back fenced — the lease expired and another worker holds a new one — is the signal to **stop**: cancel the running step, abandon the run locally, write nothing further to the store or dagger, and return to `ClaimNext`. Every `ReportUsage` also renews the lease, per dagger's contract, so posting usage per step is a second heartbeat for free. A step's `timeout` is enforced by the worker and has nothing to do with the TTL.

Dagger's `dg mcp` heartbeats for the life of an agent session, which works there because the agent is the lease holder. Here the worker is the holder and a run outlives any one session — check steps, the gap between steps, a reviewer starting after the implementer ends — so the worker owns the heartbeat. The sidecar may heartbeat too, as belt and braces; it must never be the only thing that does.

**Write ordering.** Two systems, two writes, and the process can die between them. The rule: **dagger first, store second, and the store write is what makes the dagger write "count."** Complete the node in dagger, then mark the run `done`; if the process dies between, the next claim finds a run marked `running` whose node is done and simply marks it `done`. Never the reverse — a run marked `done` before dagger confirmed would strand a node nothing can ever claim. The one exception is the gate: `ApplyPatch` files the gate → the store records `gate_node_id` → `Release` ends the lease. A crash after the patch leaves a lease that expires harmlessly (the gate's edge already blocks the node); a crash after the store write leaves the same; a crash *before* the store write would leave a gate the store doesn't know about and would file again — which is why the store write sits between the two dagger calls and not after them. `ApplyPatch` takes an idempotent `request_id`, so every dagger write on this path can be retried with the same id after a crash.

Every dagger write carries the lease id, and so does every store write: the store compares the presented lease against the run's current one inside the same transaction as the write, and a mismatch is rejected. A fenced-out write on either side is the *normal* path for a stale worker, not an error to retry: the worker abandons the run locally and goes back to `ClaimNext`. The store's ledger is what the new worker resumes from, so nothing is lost.

## Executing a step

### Skill steps: the harness adapter

A skill step becomes a session in a harness. The mill does not implement an agent runtime; it drives one through an adapter behind a Go interface:

```go
type Harness interface {
    Start(ctx, Session, Prompt, Attachments) (Turn, error)
    Resume(ctx, Session, Prompt, Attachments) (Turn, error)
    Cancel(ctx, Session) error
}
```

A `Turn` streams events (text, tool calls, tool results, usage) and ends with a terminal record: the final message, optional structured output, usage, and a `ProcessExit{Status, Signal, Reason}` where `Status = -1` means the session never ran and `Reason` says why — `refused`, `policy`, `timeout`, `cancelled`. Keeping "never ran" distinct from "ran and failed" is what lets retry policy do the right thing.

**Usage is part of the adapter's contract, not an optional event.** The adapter is the only component that sees the harness's numbers, so it must return them in dagger's shape: per assistant turn *and* as a terminal total, keyed by **model** — a Sonnet or Opus turn also bills an auxiliary model, so this is a map, not a string — with `input`, `output`, `cache_read`, and `cache_write` as token counts, never currency. The worker writes each turn to the ledger and posts the step's totals to dagger as one `ReportUsage` per `(lease, model)` when the step closes or fails.

The first adapter is a **TypeScript sidecar over the Claude Agent SDK**, spawned per step as a child process and spoken to over **newline-delimited JSON on stdio**. Message types are defined once in proto and generated to Go and TypeScript with buf, so both sides are typed without a transport heavier than a pipe. A child process on stdio has the right lifecycle for free: it dies with its parent, it needs no port or socket, and it is how MCP and LSP servers already work. Connect over a unix socket is the upgrade if a sidecar ever has to outlive its worker; nothing in MVP needs that.

The sidecar receives: the session id (deterministic, so `Resume` finds the transcript), the prompt (the skill body plus the node body and inputs plus prior artifacts, rendered with predecessors' content as attributed data), the step's **options document** for its entry in the step's `harness` table, the workspace socket path and step token, and the structured-output schema if the step has one.

**The options are the harness's, not mill's.** `Session.Options` is the yaml block as the workflow wrote it, and nothing on this side of the wire reads a key of it: the loader checks only that it is a mapping, the adapter hands it over whole, and the sidecar validates it against its own schema (`sidecar/src/options.ts`) before the SDK is loaded. A key the sidecar does not have, or a value of the wrong type, ends the request as a terminal record with `Status = -1` and `Reason = policy` naming the key, which the worker records as a failed attempt. That is what keeps mill harness-agnostic in fact and not only in shape: a knob the Claude SDK grows next quarter costs a change to the sidecar and to a workflow, and no change to the proto, the loader, or the worker. The one thing mill checks is the harness *name*, at run start, against the adapters it has.

`tools.allow` and `tools.deny` are the Claude sidecar's keys for the tool policy, and in MVP they are the harness's half of a read-only step: the SDK enforces them and the model does not get a vote. Mill's half is the capability set — the workspace refuses `fs.write` and `git.commit` under a step token that lacks `write` — and it becomes the whole of it when the socket is the session's tool server and containers arrive.

**A session runs under the model classifier.** The Claude sidecar sets the SDK's `permissionMode` to `auto` unless the step names another in `permission_mode`; a step may name `default`, `acceptEdits`, `dontAsk` or `plan`, and `bypassPermissions` is refused with `Reason = policy` before the SDK is loaded. A mill session is unattended, and under the SDK's `default` mode a tool call that needs permission raises a prompt nobody answers — which is where the first end-to-end run stopped, at `git add`. The classifier is a permission *answerer*, not a boundary: `tools.deny` and the workspace's socket remain the enforcement. `bypassPermissions` stays refused until the workspace is a sandbox (`docs/roadmap.md` §O4), because today a session runs as the mill's own account on the mill's own host and a mode that says yes to everything would have only the tool policy left between it and that host.

**The environment a sidecar is spawned with is a mode.** The mill's own process is not a floor: it carries `DATABASE_URL`, `MILL_FORGE_TOKEN`, the dagger project it was started for, and — when someone runs `mill run` from a Claude Code session — the markers that make the SDK think it is nested. A mode rather than one fixed floor, because the audiences differ. A person trying mill on a laptop wants their sessions to see what their shell sees (`GOFLAGS`, `GOPRIVATE`, a proxy, `SSH_AUTH_SOCK`) and should not have to learn a carve-out first; a deployment wants the opposite. `--env-mode inherit` is the default, `--env-mode strict` is what `contrib/systemd/mill.service` sets, and `env_mode` in the configuration file says the same thing.

A **deny list** applies in both modes and is not configurable. It is never given to a sidecar or a session, whatever the step asks for:

| Name | Why |
|---|---|
| `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` | the login comes from `HOME`; a key in a session's environment is a key its `Bash` tool can print |
| `DATABASE_URL`, `MILL_*` | the mill's own store and credentials, the forge token among them |
| `DG_*` | dagger's socket and project: a session reaches the work graph through nothing |
| `CLAUDECODE`, `CLAUDE_CODE_*` | an interactive session's markers make the SDK think it is nested |

`inherit` is then the mill's environment minus that list. `strict` is a floor, each variable with a reason:

| Variable | Why it is on the floor |
|---|---|
| `HOME` | the SDK finds the account's login (`~/.claude/.credentials.json`) and writes transcripts under `~/.claude/projects`; git reads `~/.gitconfig` |
| `PATH` | the sidecar must find `node`, the SDK its `claude` binary, the session `git` and the project's toolchain |
| `LANG`, `LC_ALL` | encoding; without them Node and git fall back to ASCII and mangle non-ASCII in bodies and diffs |
| `LOGNAME`, `USER` | git's identity fallback; a clean `git commit` without `user.name` set |
| `SHELL` | the SDK's `Bash` tool runs the account's shell |
| `TERM` | tools that check for a terminal behave predictably; a deployment pins it |
| `TMPDIR` | the SDK and git write temporary files; a deployment points it inside the runtime directory |
| `TZ` | timestamps in commits and logs match the host's |
| `XDG_RUNTIME_DIR` | the sidecar's own sockets and the SDK's IPC live under it |

The workspace socket and the step token are added on top in both modes, last, so nothing a step asked for can shadow them.

**The carve-out is the step's, where a reviewer can see it.** A step's `harness.claude` options may name `env.passthrough: [NAME, …]`, which copies those variables from the mill's process, and `env.set: {NAME: value}`, which sets literals. Both are the sidecar's schema; the adapter reads `env.passthrough` too, because what a child process is given is not the child's to decide. A step naming a deny-listed variable is refused before the sidecar is spawned, with a message naming the variable. In `inherit` mode `env.passthrough` is accepted and redundant; `env.set` applies in both. The attempt records what the step asked for and what was stripped (`step_runs.env_passthrough`, `step_runs.env_stripped`), and `mill inspect` shows both when either is non-empty. The floor is not widened to make a step work: the carve-out exists so that a workflow says what it needs.

The sidecar applies the same shape one hop further along — the session's tools inherit that floor from the sidecar, plus the step's carve-out, plus the socket and the token — so `ENVIRONMENT_PASSTHROUGH` in `sidecar/src/session.ts` and `envFloor` in `internal/harness/claude/environment.go` are the same list stated at two boundaries.

Codex, Pi, and a native Go Claude adapter are further implementations of the same interface. The adapter, not the workflow, translates a skill into whatever the harness wants; a skill is used as a *prompt*, never installed as a harness-native skill, so no harness's discovery rules leak into the workflow.

### Check steps

`Workspace.Exec(command, args, capabilities)` and read the exit code and stdout. No session, no model. The check's `feedback` setting decides whether stderr becomes the next prompt to a named session or parks the run.

### Gate steps

If the run's `gate_node_id` is already set for this step — this is a resume — read the gate node instead of filing one: `done` closes the existing step run with `gate` evidence and the gate's result; a denial fails the run. A gate is filed once.

Otherwise, build the gate node from the step's `ask` template and the run's artifacts. Evaluate `auto` over prior evidence and the run's `tier`: if it holds, record an auto-approval and continue. If not, three writes in this order: `ApplyPatch` creates the gate node as a child of this node with a `blocks` edge into it and returns its id; the store records `gate_node_id` and marks the run `waiting`; `Release` ends the lease with reason `gate`. Then return to `ClaimNext`. Post any unposted usage before the release, because the release ends the lease `ReportUsage` needs.

Single-run mode is the one exception to "then return to `ClaimNext`", because there is a person at the terminal. `mill run` files and records the gate exactly as a worker does, then puts the ask on the terminal, writes the answer to dagger as that person — never as the mill — and carries on under the lease it still holds. It does not release, and it must not: dagger's rule is that a dropped gate never satisfies the edge out of it, so a node whose gate was denied is never ready again, and a run that had given its lease back could not be claimed to record the denial. `mill work` has nobody to ask and always releases.

The mill never notifies anyone. Dagger's notifier — thicket, to Slack or a phone — carries the gate. Until that bridge exists, the stopgap is a `thicket send` from the gate step naming the ask and the node; it is a stopgap and lives behind one function.

## The workspace

The workspace is the seam that makes sandboxing a later decision instead of a rewrite. A step never receives a filesystem path it may treat as the host, and never a credential in its environment. It receives a **unix socket and a step token**, and everything it does to the world goes through that socket.

```
session ──socket, bearer──▶ workspace ──▶ checkout / git / commands / forge
```

Verbs on the socket, gated by the step's capability set:

| Verb | Capability | Does |
|---|---|---|
| `fs.read`, `fs.list` | `read` | Files in the checkout |
| `fs.write` | `write` | Files in the checkout, inside the tree only |
| `exec` | `exec` (+ `network` for egress) | Run a command with cwd, uid, and environment set by the workspace |
| `git.status`, `git.diff`, `git.log` | `read` | |
| `git.commit` | `write` | On the run's branch, signed with the mill's key |
| `git.push` | `forge` | With a forge credential minted for this step |
| `forge.pr`, `forge.checks` | `forge` | Open or query the PR; read CI status |
| `redact` | any | Register a value the workspace must scrub from logs |

`Workspace.Exec` is the **single chokepoint** for every process a step spawns. It sets cwd to the checkout, uid to the workspace's account, and environment to exactly what the step's capabilities allow. `exec.Command` is forbidden everywhere else in the mill's code by lint — the same depguard/forbidigo pattern dagger uses to keep `internal/store` out of handlers — so the guarantee is checkable, not aspirational.

Implementations behind the interface:

| Kind | Checkout | Isolation | When |
|---|---|---|---|
| `local` | A clone under the mill's account | The account, the tool policy | MVP |
| `container` | A clone in a rootless podman container with a devcontainer, so tests can run their own services | The container | O4 |
| `remote` | A clone on another host reached over the tailnet | That host's account | when a worker lives elsewhere |

A workflow definition does not change across these. If it has to, the seam leaked.

A session gets the socket path and token, over the floor its harness's environment mode allows and nothing else (§"Skill steps: the harness adapter"). The adapter is the socket's client, not the session: the Claude sidecar holds the socket and re-exposes the verbs as an MCP server hosted in its own process, and the session's built-in `Read`/`Write`/`Edit`/`Bash` are removed by policy so the workspace's tools are the only filesystem and the only way to a process. A session cannot be pointed at the socket directly — the SDK's MCP transports are stdio, HTTP and in-process, and no URL spelling reaches a unix socket (`docs/spikes/mcp-workspace.md`). The mount is what makes every file and command one auditable call at a seam the mill owns; it is not what enforces. Enforcement is the process boundary `Workspace.Exec` sets — cwd, uid, environment — because a command run through `exec` is bounded by the process it spawns, not by the tool that spawned it.

In MVP the adapter's tool allowlist is the **only** enforcement: the workspace's `local` implementation does not yet gate `exec` against `write`, so a reviewer allowed `Bash` to run tests can also write. MVP detects rather than prevents: a `clean-tree` check after every read-only step fails it if the tree changed, and the ledger says which session did it. The capability set becomes a second, independent layer with the container workspace.

## Identity and credentials

Three rungs, each scoped tighter than the one above:

| Credential | Scope | Held by |
|---|---|---|
| The operator's dagger identity — mill runs as the operator | The service | `serve` and `work`, via the unix socket's peer uid. Attribution to mill is by `harness`/`agent_ref` on every lease and a `mill-` request-id prefix on every lease-less write, never by principal |
| A role on the mill's own socket | `operator` or `reader`, by peer uid from configuration | The operator's sessions and thicket agents, through `mill` and `mill mcp` |
| The mill's signing key | Every commit it makes | The workspace, never a session |
| A step token | One step run, its capability set | The session, as its only credential |

Derived credentials — a forge token for `push`, a registry pull — are minted by the workspace against a live step token, scoped to the project's repository, and revoked when the step closes. The session's environment holds the socket path and the step token; a transcript that leaks contains nothing reusable.

Commit signing: an SSH signing key owned by the mill's account in MVP, listed in each project's `allowed_signers`; per-run SSH certificates from a CA once a secrets service exists, with the certificate's principal naming the node so `git verify-commit` is provenance.

A redactor sits in front of every log the mill writes, seeded from the step's credentials before they touch any environment, extended by `redact` calls from checks and sessions, flushed at step boundaries.

## Trust boundaries

- **Between a session and the host.** The session holds a socket and a token. It reaches files, commands, git, and the forge only through the workspace, with the step's capabilities. Nothing it can read is reusable elsewhere.
- **Between the implementer and the reviewer.** Different sessions, different step tokens, different tool policies. The reviewer's token grants no `write`. The ledger records which session produced the verdict.
- **Between the mill and dagger.** The mill holds no database credential for dagger; it is a transport-resolved principal like any agent. Every write is fenced by the lease.
- **Between runs.** One workspace per run, never shared; one step token per step, never reused.
- **Between predecessor content and instructions.** A node's inputs are other principals' claims. They are rendered as delimited, attributed data in prompts. The mill is dagger's first consumer that has to take that rule seriously, because it is the one that turns content into actions.
- **What the mill is not.** Not a sandbox — the workspace implementation and homestead are. Not a policy engine for dagger — it obeys dagger's gate rules; it does not add its own. Not a planner — one node's worth of state per run, always.

## Deployment

A homestead service under the operator's own account at this maturity: a `mill.service` *user* unit running `serve` and `work` in one process, PostgreSQL on the shared platform instance under its own database, `dg` already on the account's `PATH`, a Node runtime for the sidecar, a signing key of mill's own beside the operator's, and a polkit rule letting the account start `homestead-apply.service` so a `deploy` step can apply host changes — copied from the `homestead_apply` role, not widened. Deployed from a checkout by the homestead `mill` role until an attested release exists; migrations run on start.

```
wellspring                              mill account
  dagger.service   ◀── unix socket ────── mill.service (serve + work)
  postgresql       ◀── loopback ─────────         │
                                                  ├── workspace/<run>/   (clone, branch)
                                                  └── sidecar (per step, stdio)
```

Workspaces live under the account. When runs move to containers, the account is where podman runs them. The mill never runs across zones; it is one more thing in the operator's account that reaches dagger over the socket — which is why dagger sees the operator, and why `harness: mill` on every lease is not optional.

## Failure and stop

- **A step times out or a session is cancelled.** SIGTERM the session's process group; the workspace collects what evidence it can in a grace window; SIGKILL; record a step run with `Status = -1` and the reason. `on_failure` decides what happens next.
- **The worker crashes mid-step.** Heartbeats stop; the lease expires on dagger's timer; the node returns to the ready set; the next `ClaimNext` returns it; the new worker finds the run in the store — `running`, with a step run still `running` — marks that step run `aborted`, and resumes at `current_step`. Because a live worker heartbeats, "lease expired with the run still `running`" means the previous worker is dead or has already fenced itself out; it is not ambiguous. The workspace is recreated from the run's branch if it is not local to the new worker — which the workflow loader guarantees is pushed before any gate that follows a write. The interrupted step runs again from the top — steps are not resumed mid-flight, sessions are.
- **A stale worker writes.** Fenced out — by dagger on a dagger write, by the store on a store write. Either one is the signal to drop the run and ask for more work. Nothing to recover: the ledger is the truth and the new holder owns it.
- **Stop.** `SIGTERM` to the process: stop claiming, let the current step reach a boundary if it can inside the grace window, post unposted usage, release every lease with reason `preempted` and detail `shutdown` — dagger's `ReleaseReason` has no shutdown value and an operator taking the lease away is what preempted means — leave workspaces intact. The attempt in flight is left `aborted` and the run `running`, so the next claim starts the next attempt at the same step, in the same session. A second signal, or the grace window elapsing, kills sessions and exits non-zero. Resume is the crash path.
- **Restart.** A human runs `mill restart <node>`: the live run's lease is released with reason `preempted` and detail `restart`, its step cancelled, the run marked `abandoned`, its workspace destroyed, its branch and PR closed with a comment, and a new run created at the next generation with fresh session ids. Never automatic.
- **The circuit breaker.** The failing step's `retries` exhausted and its `on_failure` spent, or the budget cap reached: post unposted usage; comment the last findings on the node; file a gate asking a human what to do (`ApplyPatch`), whose body is every attempt of every step in the span since a human was last asked — step, attempt number, exit, and the last lines of what that session or check said, because the attempt that parks is regularly not the one that caused it; record it in the store and mark the run `parked`; release with reason `budget`. Asking a human costs the attempts it took to get there. The human's answer is `resume` (complete the gate; the run continues at the parked step, attempts reset) or `restart`.

## Observability

- Every session's event stream is written to a log the operator can tail, one file per step run, through the redactor. Full tool output is never logged; a summary line per tool result is.
- The ledger is the record. `mill status` reads it: runs by state, current step, elapsed, cost so far, gates waiting on humans.
- Usage per step run is in the ledger; per lease it is posted to dagger. Cost is a rate table applied at query time, as in dagger.
- A run's artifacts — plan, verdicts, PR, merge commit, usage — are dagger artifacts on the result, with `run_id` in their metadata. A Slack rendering of any of this is thicket's job.

## What is borrowed and from where

| Mechanism | From | Becomes |
|---|---|---|
| A credential ladder ending in a per-unit token that is the only thing the work holds | buildkite-agent's registration → agent → job token | Step tokens |
| A local socket with a per-unit bearer as the work's only interface to the system | buildkite-agent's Job API | The workspace socket |
| An executor subprocess with an env-file handoff and a gate that can refuse before anything runs | buildkite-agent's bootstrap and `pre-bootstrap` hook | The sidecar, and the capability check before a step starts |
| A redactor seeded before secrets touch env and extended at runtime | buildkite-agent's redactor | The log redactor |
| `ProcessExit{Status, Signal, Reason}` with `-1` = never ran | buildkite-agent | The adapter's terminal record |
| Lease fencing re-checked inside the writing transaction, after the row lock | dagger's `fence()` | The store's own fencing on every ledger write |
| Interrupt → grace → kill, cleanup on a grace-period context | buildkite-agent's cancel path | Step cancellation |
| One binary, several faces; the CLI and the service share the client | dagger | `serve` / `work` / `run` |
| Fenced leases; a late write from a stale holder fails silently and correctly | dagger | Every write the worker makes |
| Skills as the portable unit; orchestration metadata beside them, never inside | the agent-skills spec | Workflow and step records |

Not borrowed: hooks as workflow steps, reused checkouts keyed by worker name, an event-watching scheduler, polling for cancellation, and every piece of fleet-scale machinery a single operator doesn't need.
