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

A worker's settings — its projects, its labels, its concurrency, and the rest of a run's configuration — come from flags, from a YAML file at `$XDG_CONFIG_HOME/mill/config.yaml` whose keys are the long flag names, or from both: a flag the command line sets wins, and anything it leaves alone the file may fill in. A missing file is not an error; every setting has a default. This is what keeps a unit file one line and lets an operator change what the mill works on without editing a unit. `project` (`--project` on the command line) is a filter, not a requirement: left empty, a worker claims from every project its principal may read, over one `ClaimNext` loop and one dagger client; named, it claims only from the projects listed. The repository a run delivers to is not among those settings: it is the dagger project's `vcs_url`, read from the claim at every run and used to scope that run's own dagger client, so a worker carries no repository of its own, drives every project's writes against that project's own scope, and two workers on one project cannot disagree about where it lives. A claimed node whose project names no repository is failed and held under that reason rather than reclaimed into the same refusal; the worker logs it once per project and moves on to the next claim.

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

A `Turn` streams events (text, thinking, tool calls, tool results, usage, heartbeats) and ends with a terminal record: the final message, optional structured output, usage, and a `ProcessExit{Status, Signal, Reason}` where `Status = -1` means the session never ran and `Reason` says why — `refused`, `policy`, `timeout`, `cancelled`. Keeping "never ran" distinct from "ran and failed" is what lets retry policy do the right thing.

**A `text` or `thinking` event streams as pieces, not as one block on completion.** The sidecar runs the SDK with `includePartialMessages` and forwards each `content_block_delta` as its own event, carrying the growing block's id and `delta: true`; the block's closing `content_block_stop` becomes one last event for that id with `final: true` and no text of its own, mirroring the Messages API's own shape (`docs/spikes/sdk-sessions.md`'s 2026-09-21 addendum). The SDK still emits the completed assistant message once a block finishes — that message's own `usage` and any tool calls are what the adapter forwards as before; only its text and thinking are redundant with the deltas already sent and are dropped. A consumer that only wants the finished text still gets it, by concatenating a block's deltas up to its `final` (`docs/activity.md` §"Streaming text"); `mill logs -f` does exactly that in reverse, printing each delta raw as it arrives.

**A subagent's own activity folds under the `Agent` call that started it.** The sidecar runs with `forwardSubagentText`, so a subagent's text and thinking stream the same way a top-level turn's do, not just its tool calls and results. Every event the SDK attributes to a subagent — `SDKAssistantMessage`, `SDKUserMessage` and `SDKPartialAssistantMessage` alike — carries `parent_tool_use_id` set to the `Agent` tool_use's own id, which the adapter carries onto the harness event as `parent_call_id` unchanged; a top-level event carries none. The activity writer takes an event's turn from `parent_call_id` when it is set, rather than from the top-level turn counter, so a subagent's records land under the call that opened it even while the top-level session has moved on to other turns (`docs/activity.md` §Subagents, §Turns).

**A tool result is forwarded whole.** The sidecar no longer caps a `tool_result`'s content before it crosses the wire; a result of any size — a large file read, a verbose test run — reaches the activity writer intact, which applies its own ceiling only once the attempt's total has grown past it (`docs/activity.md` §"What exists today").

**The wait a turn can end on is bounded by the adapter.** A session that backgrounds a command and ends its turn to be woken is not finished, so the sidecar keeps reading until a turn ends with no task still running (`docs/domain-model.md` §Session). It watches that wait: every three minutes it looks at each waited-on task's output file, stays silent while the output grows, and sends one turn asking about a task that has gone quiet — emitted as a `heartbeat` event, which the worker writes to the run's history so `mill inspect` shows what an attempt's wait cost. Three silent looks trip a breaker: the sidecar kills the session's child processes and answers with `Reason = stalled` and the last working turn's final message, which the worker treats as a failed attempt with its words kept. The constants and the prompt are in one place, `sidecar/src/tasks.ts`; the reference agent loop in `harness/loop.py` implements the same shape over the same SDK messages, and the two are kept the same on purpose.

**A session can wait in the foreground for as long as its step may run.** The harness backgrounds a shell command that outlives its default timeout — about two minutes — and wakes the session when it ends, and a session with a backgrounded test run is a session that learns to poll: the reference harness lost an hour to a `pgrep` loop that matched its own shell before it set that timeout for itself (`harness/loop.py`). The heartbeat and the breaker above bound that wait; this removes the reason for it. The step's `timeout` crosses the wire as `timeout_ms` on `Start` and `Resume`, and the sidecar sets it as the session's default and maximum shell timeout through the SDK's own options rather than the process environment, so the value is the step's and a sidecar is never spawned with one that fits every step. A step's budget is the longest anything it does may take, so it is the right cap by construction: a command that would outrun it is a step that has already failed. Every step declares one — the loader refuses a definition whose step has none, and the adapter and the sidecar each refuse a session without one rather than guess (`docs/sandbox.md` §Preparatory steps).

**Usage is part of the adapter's contract, not an optional event.** The adapter is the only component that sees the harness's numbers, so it must return them in dagger's shape: per assistant turn *and* as a terminal total, keyed by **model** — a Sonnet or Opus turn also bills an auxiliary model, so this is a map, not a string — with `input`, `output`, `cache_read`, and `cache_write` as token counts, never currency. The worker writes each turn to the ledger and posts the step's totals to dagger as one `ReportUsage` per `(lease, model)` when the step closes or fails.

The first adapter is a **TypeScript sidecar over the Claude Agent SDK**, spawned per step as a child process — through the workspace's `Exec`, so on the `machine` backend it runs inside the machine (§"The workspace") — and spoken to over **newline-delimited JSON on stdio**. Message types are defined once in proto and generated to Go and TypeScript with buf, so both sides are typed without a transport heavier than a pipe. A child process on stdio has the right lifecycle for free: it dies with its parent, it needs no port or socket, and it is how MCP and LSP servers already work. Connect over a unix socket is the upgrade if a sidecar ever has to outlive its worker; nothing in MVP needs that.

The sidecar receives: the session id (deterministic, so `Resume` finds the transcript), the prompt (the skill body plus the node body and inputs plus prior artifacts, rendered with predecessors' content as attributed data), the step's **options document** for its entry in the step's `harness` table, the workspace socket path and step token, the structured-output schema if the step has one, and a `system_prompt` — the mill's facts about the session's surroundings, appended to the harness's own system prompt rather than mixed into the step's instructions. Today it is one paragraph about egress, composed by the adapter from the workspace's decisions and only when it made them: that every outbound connection goes through the proxy named in the environment and the model through the gateway (`docs/sandbox.md` §Network, §"Skill mount"). A workspace that decided neither sends nothing and the harness's prompt stands alone.

**The options are the harness's, not mill's.** `Session.Options` is the yaml block as the workflow wrote it, and nothing on this side of the wire reads a key of it: the loader checks only that it is a mapping, the adapter hands it over whole, and the sidecar validates it against its own schema (`sidecar/src/options.ts`) before the SDK is loaded. A key the sidecar does not have, or a value of the wrong type, ends the request as a terminal record with `Status = -1` and `Reason = policy` naming the key, which the worker records as a failed attempt. That is what keeps mill harness-agnostic in fact and not only in shape: a knob the Claude SDK grows next quarter costs a change to the sidecar and to a workflow, and no change to the proto, the loader, or the worker. The one thing mill checks is the harness *name*, at run start, against the adapters it has.

`tools.allow` and `tools.deny` are the Claude sidecar's keys for the tool policy, and in MVP they are the harness's half of a read-only step: the SDK enforces them and the model does not get a vote. Mill's half is the capability set — the workspace refuses `fs.write` and `git.commit` under a step token that lacks `write` — and it becomes the whole of it when the socket is the session's tool server and machines arrive.

**A session mounts MCP servers named in two catalogs.** A worker's own `mcp-servers` (configuration file) and a workflow's own `mcp-servers` (definition, no `global` — a workflow cannot reach for every step the way the host can) merge into one run-scoped catalog, refused at `mill workflow push` if a workflow's name collides with the worker's own (`docs/tools.md` §Configuration). Every entry marked `global` is mounted on every step; the rest mount only on a step whose `harness.claude.mcp` option names them, a `PolicyError` if it names one neither catalog carries. The sidecar registers each mounted server as an `options.mcpServers` entry with the session's own environment and its working directory set to `<workspace-dir>/<run>/browser` — a directory beside the checkout, not inside it, so a file a mounted server writes there (a Playwright screenshot) never shows up as a stray file to the `clean-tree` check — and appends `mcp__<name>__*` to the allow list the SDK already builds for built-in tools — so `tools.deny` is the only way to keep a mounted server's tools out of a step's session, the same as any other tool. This is a convenience mount, not a capability boundary: an MCP server runs as a child of the sidecar, inside whatever process boundary the workspace already set for the session (§"The workspace"), and proves nothing beyond what that boundary already enforces.

**A session runs under the model classifier.** The Claude sidecar sets the SDK's `permissionMode` to `auto` unless the step names another in `permission_mode`; a step may name `default`, `acceptEdits`, `dontAsk` or `plan`, and `bypassPermissions` is refused with `Reason = policy` before the SDK is loaded. A mill session is unattended, and under the SDK's `default` mode a tool call that needs permission raises a prompt nobody answers — which is where the first end-to-end run stopped, at `git add`. The classifier is a permission *answerer*, not a boundary: `tools.deny` and the workspace's socket remain the enforcement. `bypassPermissions` stays refused on every backend: on `local` a session runs as the mill's own account on the mill's own host, and a mode that says yes to everything would have only the tool policy left between it and that host; on `machine` the classifier is still what stops a session from destroying its own checkout, which the machine does not protect.

**The environment a sidecar is spawned with is a mode.** The mill's own process is not a floor: it carries `DATABASE_URL`, `MILL_FORGE_TOKEN`, the dagger project it was started for, and — when someone runs `mill run` from a Claude Code session — the markers that make the SDK think it is nested. A mode rather than one fixed floor, because the audiences differ. A person trying mill on a laptop wants their sessions to see what their shell sees (`GOFLAGS`, `GOPRIVATE`, a proxy, `SSH_AUTH_SOCK`) and should not have to learn a carve-out first; a deployment wants the opposite. `--env-mode inherit` is the default, `--env-mode strict` is what `contrib/systemd/mill.service` sets, and `env_mode` in the configuration file says the same thing.

A **deny list** applies in both modes and is not configurable. It is never given to a sidecar or a session, whatever the step asks for:

| Name | Why |
|---|---|
| `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` | a key in a session's environment is a key its `Bash` tool can print. `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` are deliberately not here: they are how the workspace's model gateway is reached, and the workspace sets them after this filter (`docs/sandbox.md` §Credentials) |
| `DATABASE_URL`, `MILL_*` | the mill's own store and credentials, the forge token among them |
| `DG_*` | dagger's socket and project: a session reaches the work graph through nothing |
| `CLAUDECODE`, `CLAUDE_CODE_*` | an interactive session's markers make the SDK think it is nested |
| `GIT_CONFIG_*` | the host's git configuration, in the form that outranks a file; the workspace sets its own after this filter (§"The workspace") |

`inherit` is then the mill's environment minus that list. `strict` is a floor, each variable with a reason:

| Variable | Why it is on the floor |
|---|---|
| `HOME` | the shell, the SDK's transcripts under `~/.claude/projects`, and the harness's own state. Not a login: in a machine there is none, and the model is reached through the gateway the workspace names. Not git's identity: the workspace's `GIT_CONFIG_GLOBAL` displaces the account's configuration on every session (§"The workspace") |
| `PATH` | the sidecar must find `node`, the SDK its `claude` binary, the session `git` and the project's toolchain |
| `LANG`, `LC_ALL` | encoding; without them Node and git fall back to ASCII and mangle non-ASCII in bodies and diffs |
| `LOGNAME`, `USER` | the account a tool reports itself running as |
| `SHELL` | the SDK's `Bash` tool runs the account's shell |
| `TERM` | tools that check for a terminal behave predictably; a deployment pins it |
| `TMPDIR` | the SDK and git write temporary files; a deployment points it inside the runtime directory |
| `TZ` | timestamps in commits and logs match the host's |
| `XDG_RUNTIME_DIR` | the sidecar's own sockets and the SDK's IPC live under it |

The workspace socket, the step token and the workspace's own environment are added on top in both modes, last, so nothing a step asked for can shadow them: a step may widen what its session sees, and may not choose who its commits are by or its own way out. The workspace's environment is its git identity (§"The workspace") and its egress — `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` for the proxy every outbound connection takes, `ANTHROPIC_BASE_URL` for the gateway the model is reached through, `ANTHROPIC_AUTH_TOKEN`, the step token as that gateway's bearer, and `ENABLE_TOOL_SEARCH` beside them, because the CLI turns its tool search off at a base URL that is not a first-party host (`docs/sandbox.md` §Network, §Credentials). They are decisions the workspace made, never values inherited from the host: the local workspace decides none and sets none, the machine workspace decides a proxy and a gateway on the host side of its veth, and both reach a session by the same path. The token among them is rotated with the step, so a session's exec carries the bearer its step was issued and never the one before it.

**The carve-out is the step's, where a reviewer can see it.** A step's `harness.claude` options may name `env.passthrough: [NAME, …]`, which copies those variables from the mill's process, and `env.set: {NAME: value}`, which sets literals. Both are the sidecar's schema; the adapter reads `env.passthrough` too, because what a child process is given is not the child's to decide. A step naming a deny-listed variable is refused before the sidecar is spawned, with a message naming the variable. In `inherit` mode `env.passthrough` is accepted and redundant; `env.set` applies in both. The attempt records what the step asked for and what was stripped (`step_runs.env_passthrough`, `step_runs.env_stripped`), and `mill inspect` shows both when either is non-empty. The floor is not widened to make a step work: the carve-out exists so that a workflow says what it needs.

The sidecar applies the same shape one hop further along — the session's tools inherit that floor from the sidecar, plus the step's carve-out, plus the workspace's environment, the socket and the token — so `ENVIRONMENT_PASSTHROUGH` in `sidecar/src/session.ts` and `workspace.FloorNames` in `internal/workspace/exec.go` (which `envFloor` in `internal/harness/claude/environment.go` is) are the same list stated at two boundaries, and the workspace's git and egress variables are named past the floor in both files. The local backend gives every child it spawns — git, a check step's command — that same floor cut from the mill's own environment, so a check's `make test` resolves the toolchain on the PATH the unit was started with rather than a shell's built-in default; the mill's credentials, database and project never reach it.

Codex, Pi, and a native Go Claude adapter are further implementations of the same interface. The adapter, not the workflow, translates a skill into whatever the harness wants; a skill is used as a *prompt*, never installed as a harness-native skill, so no harness's discovery rules leak into the workflow.

### Check steps

`Workspace.Exec(command, args, capabilities)` and read the exit code and stdout. No session, no model. The check's `feedback` setting decides whether stderr becomes the next prompt to a named session or parks the run.

### Catch-up, before a merge gate approves

A merge gate's `approve` (`internal/run/gate.go`) does not ask the forge to merge a stale head. Before it does, it catches the branch up with the target it forked from (`Workspace.CatchUp`, `internal/workspace`): a target that has not moved is `UpToDate` and nothing runs; a file both sides touched is `Overlap`, left staged on the branch as a finding that revises back through `implement` and `commit`, the same as any other; no file in common is `Merged` — catch-up merged, signed and pushed the target's commits onto the branch itself, unreviewed.

A clean merge is still two changes combining into something neither one was alone. Nothing else ran on the tree catch-up produced — no step, no review — so before the forge is asked to merge it, the gate runs the workflow's own check-kind steps between `commit` and `merge` (`checksBetween`, `checkCaughtUp`) against exactly that tree, the same command and contract a step would have run had one run there. A check that fails is a finding, not a park: `catchUpCheckError` names the step and what it found and revises back through `implement`, exactly as an overlap does. The forge is never asked to merge a tree nothing has tested (mill#52 — two migrations with no filename in common still combined into one goose refused to run).

A push this fresh can still outrun the forge: right after catch-up pushes, the forge may not have finished computing whether the result is mergeable, and a second push to the target between catch-up and the merge call can move it again in a way catch-up never saw. Neither is a finding — both clear on their own, the first with a short wait and the second with another catch-up — so `mergeRetrying` (`internal/run/gate.go`) absorbs a `forge.Retryable` refusal inside the same attempt rather than spending one of the step's own retries on it: a refusal that names the mergeability check still pending waits and asks again; a refusal that names a conflict runs `catchUp` again and asks again. A bounded number of these in-attempt retries (`mergeRetryBudget`) still surfaces as an ordinary step failure, so a durably stuck merge does not loop forever — it falls through to the step's own `retries`/`on_failure` like any other refusal.

### Gate steps

If the run's `gate_node_id` is already set for this step — this is a resume — read the gate node instead of filing one: `done` closes the existing step run with `gate` evidence and the gate's result; a denial fails the run. A gate is filed once.

Otherwise, build the gate node from the step's `ask` template and the run's artifacts. Evaluate `auto` over prior evidence and the run's `tier`: if it holds, record an auto-approval and continue. If not, three writes in this order: `ApplyPatch` creates the gate node as a child of this node with a `blocks` edge into it and returns its id; the store records `gate_node_id` and marks the run `waiting`; `Release` ends the lease with reason `gate`. Then return to `ClaimNext`. Post any unposted usage before the release, because the release ends the lease `ReportUsage` needs.

Single-run mode is the one exception to "then return to `ClaimNext`", because there is a person at the terminal. `mill run` files and records the gate exactly as a worker does, then puts the ask on the terminal, writes the answer to dagger as that person — never as the mill — and carries on under the lease it still holds. It does not release, and it must not: dagger's rule is that a dropped gate never satisfies the edge out of it, so a node whose gate was denied is never ready again, and a run that had given its lease back could not be claimed to record the denial. `mill work` has nobody to ask and always releases.

The mill has no channel of its own. Dagger's notifier — thicket, to Slack or a phone — is what will carry the gate; until that bridge consumes dagger's events, the gate step runs one **configured command** and the deployment decides what carrying means (decision D1).

The seam is `internal/notify`: on every gate the mill files — plan approval, a merge gate no policy closed, and the circuit breaker's own park gate — the configured command is spawned through the workspace's chokepoint with the gate as JSON on its standard input, `{"gate", "node", "run", "title", "body", "kind"}`, where `kind` is `plan`, `merge` or `park`. It is bounded at thirty seconds, and its exit status and standard error are a row of the run's audit trail, op `notified`. The command runs while the gate's lease is still held — after `ApplyPatch`, before the write that marks the run waiting or parked — because that row is fenced like every other, and because a crash in the window it widens is the one `findFiledGate` already exists for.

Nothing about the notification is load-bearing. A command that fails, hangs past its bound, or does not exist is logged and recorded and changes nothing: the gate is filed, the run waits, and the answer a human writes is written in dagger. A deployment that configures no command files gates that reach nobody until somebody looks, which is the behaviour before this existed. `mill doctor`'s `notify` probe is what says which of those a host is in, and `contrib/notify/thicket-send.sh` is the shipped example: it renders the JSON to one message and pipes it to `thicket send`, which hands it to a named thicket agent that reaches the person through its own tools.

## The workspace

The workspace is the seam that keeps sandboxing behind one interface. A step never receives a filesystem path it may treat as the host, and never a credential in its environment. It receives a **unix socket and a step token**, and everything it does to the world goes through that socket.

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
| `report.install` | any | The `dnf` shim's report of packages a session installed into its machine; recorded in the run's provenance as undeclared, never routed to a reviewer (`docs/sandbox.md` §"The dnf shim") |

`Workspace.Exec` is the **single chokepoint** for every process a step spawns. It sets cwd to the checkout, uid to the workspace's account — the mill's own on `local`, `agent` inside a machine — and environment to exactly what the step's capabilities allow. `exec.Command` is forbidden everywhere else in the mill's code by lint — the same depguard/forbidigo pattern dagger uses to keep `internal/store` out of handlers — so the guarantee is checkable, not aspirational.

Implementations behind the interface:

| Kind | Checkout | Isolation | When |
|---|---|---|---|
| `local` | A clone under the mill's account | The account, the tool policy. A test backend — the specs and a laptop `mill run` — not one for production work | MVP |
| `machine` | A clone on the host, bind-mounted into a `systemd-nspawn` machine booted per run from an ephemeral snapshot of the image the repo's `mill.toml` declares, so tests run their own services and containers inside it; `docs/sandbox.md` | The machine: a user namespace, its own network namespace behind the host's proxy, no credential inside | O2 |
| `remote` | A clone on another host reached over the tailnet | That host's account | when a worker lives elsewhere |

A workflow definition does not change across these. If it has to, the seam leaked. Which one a worker uses is configuration: `--workspace local|machine` on `serve`, `work` and `run`, or `workspace:` in the configuration file, with `local` the default. The selector is the one place a backend is named — `workspacesFor` in `internal/cli/run.go`, beside `localWorkspaces` and `machineWorkspaces` — and it is asked before a node is claimed, so a backend this host cannot run refuses at start rather than after a claim. `mill doctor`'s `workspace` probe reports the kind in effect (`docs/tools.md` §doctor).

The machine backend, `internal/workspace/machine`, reaches the host through one seam: `internal/workspace/machine/driver`, a `Driver` with a method per host command `docs/sandbox.md` names — `systemctl start` and `stop` of the machine unit under polkit; `machinectl show`, `status` and `list`; the root-owned helper's `exec`, `stop` and `terminate` under `sudo -n`, the exec with the process's stdio streamed; `btrfs subvolume snapshot`, `create` and `delete` (with `btrfs filesystem sync` after it when asked), `btrfs property set ro`, `du -s`, `statfs`; and the image pipeline's `podman pull`, `build`, `export` and `push`. The real driver runs each through `workspace.Spawner.Exec` and nothing else, and the verbs that write the image directory — root's on the host — through `sudo`; a fake records every call and answers as scripted, with a plain directory standing in for a subvolume and a copy for a snapshot, so the backend is specified against the fake and proven against the host under `Label("live")`. Images are `internal/workspace/machine/image` and cache lineages `internal/workspace/machine/cache` over the same seam — the cache store over a driver without `sudo`, because a copy made as root is one the machine's `agent` cannot write; the run loop asks a workspace that implements `workspace.CacheCandidate` for a candidate each time a step closes on evidence (`internal/run/cache.go`), and the machine backend promotes at `Destroy`. `Create` is `docs/sandbox.md` §"Sequence of a run" steps 1–8 — clone, `mill.toml`, image, caches, `machine.json`, start, ready, socket — `Attach` reopens a run from `runs/<id>/run.json`, and the handle carries the machine's name, unit, digest, veth and restored lineages as `workspace.Sandbox` for the ledger, which `RecordWorkspace` writes when the workspace opens and `FinishWorkspace` closes with the generations promoted. The worker never runs `systemd-nspawn` and never holds root: the machine is an instance of homestead's root-owned `mill-machine@.service` template, started and stopped with `systemctl` under a polkit rule that admits that template and nothing else, and entered through the root-owned helper `/usr/libexec/mill-machine` under `sudo -n`, whose sudoers rule admits `exec`, `stop` and `terminate` (`docs/sandbox.md` §Privilege). `Workspace.Exec` under `machine` is that helper's `exec` under a per-step slice, and the session's sidecar is one such exec: the adapter's execer is the run's workspace (`claude.WithExecer`), so a skill step's session runs inside the machine as `agent`, from the sidecar copy the worker mounts read-only, and dies with its step. The git and forge verbs are not execs — they run on the host against the workspace bind through `internal/workspace/verbs`, the code both backends share, so the signing key and the forge credential never enter the machine (`docs/sandbox.md` §Verbs). On worker start the backend reconciles before the first claim — `machinectl list` against the ledger's live runs, orphans stopped, lost sandboxes recorded, scratch projects swept by name — and evicts under its watermarks (`docs/sandbox.md` §"Reconciliation on resume", §Eviction). Everything the machine is — image, binds, limits, network, credentials — is specified in `docs/sandbox.md`, which this section defers to.

**Commit identity and signing are the workspace's, not the session's.** Who a commit is by, and what signs it, is decided once per run by the workspace and imposed on every process it or its harness spawns: an SSH signing key under the mill's account, and a git configuration in the run's directory — beside the checkout, never in it — named by `GIT_CONFIG_GLOBAL`, with `GIT_CONFIG_SYSTEM` and the four `GIT_AUTHOR_*`/`GIT_COMMITTER_*` variables alongside it. So the account's `~/.gitconfig` and `~/.config/git/config` are displaced on every session, a plain `git commit` in a session's own shell is the mill's commit, signed, and the `commit` verifier is comparing against the identity git was actually given rather than against whatever the host was configured for. The mill's own git says the same thing a different way — `-c user.name=`, `-c user.email=`, `-c user.signingkey=`, `-c commit.gpgsign=true` per invocation — and for the same reason: the answer lives outside the checkout, where a session can neither read the key nor change who it is committing as. A run never reads, and never asks an operator to change, any git configuration of the host's.

A session gets the socket path and token, over the floor its harness's environment mode allows and nothing else (§"Skill steps: the harness adapter"). The adapter is the socket's client, not the session: the Claude sidecar holds the socket and re-exposes the verbs as an MCP server hosted in its own process, and the session's built-in `Read`/`Write`/`Edit`/`Bash` are removed by policy so the workspace's tools are the only filesystem and the only way to a process. A session cannot be pointed at the socket directly — the SDK's MCP transports are stdio, HTTP and in-process, and no URL spelling reaches a unix socket (`docs/spikes/mcp-workspace.md`). The mount is what makes every file and command one auditable call at a seam the mill owns; it is not what enforces. Enforcement is the process boundary `Workspace.Exec` sets — cwd, uid, environment — because a command run through `exec` is bounded by the process it spawns, not by the tool that spawned it.

On the `local` backend the adapter's tool allowlist is the **only** enforcement: the workspace's `local` implementation does not gate `exec` against `write`, so a reviewer allowed `Bash` to run tests can also write. It detects rather than prevents — a `clean-tree` check after every read-only step fails it if the tree changed, and the ledger says which session did it — which is fine for a test backend and is why it is not one for production work. On the `machine` backend the capability set is enforced on the filesystem: every process a step without `write` runs sees the checkout read-only (`docs/sandbox.md` §Exec), so a reviewer keeps its shell and cannot write.

## Identity and credentials

Three rungs, each scoped tighter than the one above:

| Credential | Scope | Held by |
|---|---|---|
| The operator's dagger identity — mill runs as the operator | The service | `serve` and `work`, via the unix socket's peer uid. Attribution to mill is by `harness`/`agent_ref` on every lease and a `mill-` request-id prefix on every lease-less write, never by principal |
| A role on the mill's own socket | `operator` or `reader`, by peer uid from configuration | The operator's sessions and thicket agents, through `mill` and `mill mcp` |
| The mill's signing key | Every commit it makes | The workspace, never a session |
| The subscription credential — a `claude setup-token`, one year | Every model request a session makes | The credential gateway on the host, from `$CREDENTIALS_DIRECTORY/claude-oauth`; never a session, never the environment |
| A step token | One step run, its capability set | The session, as its only credential |

Derived credentials — a forge token for `push`, a registry pull — are minted by the workspace against a live step token, scoped to the project's repository, and revoked when the step closes. The session's environment holds the socket path and the step token; a transcript that leaks contains nothing reusable.

On the `machine` backend a session's `~/.claude` — its login-less credential state, its settings and its transcript — is ephemeral with the machine, so a session resumes while the machine lives and a machine machined no longer knows is a lost sandbox (`ErrLostSandbox`), never a resumable session (`docs/sandbox.md` §Reconciliation).

The model is reached the same way. A session is given `ANTHROPIC_BASE_URL` naming the **credential gateway** (`internal/gateway`) and `ANTHROPIC_AUTH_TOKEN` carrying its step token, which Claude Code sends as its bearer above every file-based credential; the gateway checks the bearer against the token the run loop rotated in for the live attempt — the same hash comparison the workspace socket makes, so a rotated token is refused at once — swaps it for the subscription credential, and forwards the Messages API and nothing else. It is the one place the subscription credential is held, and it sees every response, so it counts what the model billed from outside the session — a ledger row per response with source `gateway`, beside the adapter's report — and enforces a step's `token_budget` and a workflow's `models` pin with a 429 and a 400 the run loop turns into the attempt's failure. The run loop installs each attempt in it beside the workspace's `Rotate` and removes it when the attempt ends; `mill doctor`'s `gateway` probe sends one token count through it. The whole of it is `docs/sandbox.md` §Credentials; it is on by configuration (`gateway: true`), and a run without it is a session on the account's own login, which is what a laptop gets.

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

Workspaces live under the account on the `local` backend. Under `workspace: machine` the account is the one that starts machines, through the root-owned template unit it drives under polkit (`docs/sandbox.md` §Privilege), and the host carries what mill cannot ship, as homestead roles (`docs/sandbox.md` §"What the host provides"): `mill_storage`, the btrfs volume at `/var/lib/mill`; `mill_machine`, the template unit, its helper, policy, polkit and sudoers rules; `mill_egress`, the `mill-host` link, the `inet mill` table and squid; `mill_selinux`, the file context; and `mill`, which deploys the units below, writes `~/.config/mill/config.yaml` with `workspace: machine` and the sandbox keys, delivers the model credential and the Harbor robot credential by `LoadCredential`, makes `/run/mill` the account's through `tmpfiles.d`, and keeps a clone of dagger's `main` at `/var/lib/mill/dagger` for nested machines.

`contrib/systemd/` holds every unit. `mill.service` is a system unit, the one
the package installs; the timers are user units under the account:

| Unit | What |
|---|---|
| `contrib/systemd/mill.service` | `mill serve --work --env-mode strict`: the store and a worker in one process, `Type=notify`, `LoadCredential=claude-oauth` for the gateway. `User=mill`, with `/etc/mill`, `/var/lib/mill` and `/run/mill` declared on the unit, so a drop-in that sets `User=` to the operator's account — which is what makes mill the operator's dagger principal — works without a chown |
| `contrib/systemd/mill-image-build.service`, `contrib/systemd/mill-image-build.timer` | `mill image build-base --push` weekly, from the checkout `images/base` is in, `LoadCredential=harbor-robot` — one line, `robot:secret`; homestead renders the same unit as `mill-image.service` |
| `contrib/systemd/mill-cache-prune.service`, `contrib/systemd/mill-cache-prune.timer` | `mill cache prune` daily: expiry, then least recently used under the watermarks, lineages and images both |

The package is the other way in: `make package` builds an rpm per architecture from `packaging/nfpm.yaml` — the binary, the built sidecar with its dependencies under `/usr/lib/mill/sidecar`, the workflows under `/usr/share/mill/workflows`, the unit, a `sysusers.d` fragment for the `mill` account and a default `/etc/mill/config.yaml` on the `local` backend that names the installed paths. It carries none of the sandbox's host half (`docs/sandbox.md` §"What the host provides"); `workspace: machine` is homestead's to provision.

The mill never runs across zones; it is one more thing in the operator's account that reaches dagger over the socket — which is why dagger sees the operator, and why `harness: mill` on every lease is not optional.

## Failure and stop

- **A step times out or a session is cancelled.** SIGTERM the session's process group; the workspace collects what evidence it can in a grace window; SIGKILL; record a step run with `Status = -1` and the reason. `on_failure` decides what happens next.
- **The worker crashes mid-step.** Heartbeats stop; the lease expires on dagger's timer; the node returns to the ready set; the next `ClaimNext` returns it; the new worker finds the run in the store — `running`, with a step run still `running` — marks that step run `aborted`, and resumes at `current_step`. Because a live worker heartbeats, "lease expired with the run still `running`" means the previous worker is dead or has already fenced itself out; it is not ambiguous. The workspace is recreated from the run's branch if it is not local to the new worker — which the workflow loader guarantees is pushed before any gate that follows a write. The interrupted step runs again from the top — steps are not resumed mid-flight, sessions are.
- **A stale worker writes.** Fenced out — by dagger on a dagger write, by the store on a store write. Either one is the signal to drop the run and ask for more work. Nothing to recover: the ledger is the truth and the new holder owns it.
- **A node is closed underneath a run.** Dropped, or completed by someone else, while the run held no lease — waiting, parked, or stopped with its lease released. Nothing will claim the node again, so nothing inside a claim can end the run. The worker's sweep does (`docs/ledger.md` §Sweep): before its first claim and on an interval between claims, it asks dagger about every live run's node and fails the run whose node is closed — or marks it done when its own completing patch had landed — then destroys the workspace and closes the branch. No operator command is involved; a stranded run leaves `mill status` within a sweep interval of the worker noticing.
- **Stop.** `SIGTERM` to the process: stop claiming, let the current step reach a boundary if it can inside the grace window, post unposted usage, release every lease with reason `preempted` and detail `shutdown` — dagger's `ReleaseReason` has no shutdown value and an operator taking the lease away is what preempted means — leave workspaces intact. The attempt in flight is left `aborted` and the run `running`, so the next claim starts the next attempt at the same step, in the same session. A second signal, or the grace window elapsing, kills sessions and exits non-zero. Resume is the crash path. On its next start, before its first `ClaimNext`, a worker reclaims the runs its own prior shutdown left this way — its own `worker_ref`, an aborted attempt — oldest abort first, up to its concurrency; only the slots left over go to `ClaimNext` (`docs/ledger.md` §Resume, mill#44).
- **Restart.** A human runs `mill restart <node>`: the live run's lease is released with reason `preempted` and detail `restart`, its step cancelled, the run marked `abandoned`, its workspace destroyed, its branch and PR closed with a comment, and a new run created at the next generation with fresh session ids. Never automatic.
- **The circuit breaker.** The failing step's `retries` exhausted and its `on_failure` spent, or the budget cap reached: post unposted usage; comment the last findings on the node; file a gate asking a human what to do (`ApplyPatch`), whose body is every attempt of every step in the span since a human was last asked — step, attempt number, exit, and the last lines of what that session or check said, because the attempt that parks is regularly not the one that caused it; record it in the store and mark the run `parked`; release with reason `budget`. Asking a human costs the attempts it took to get there. The human's answer is `resume` (complete the gate; the run continues at the parked step, attempts reset) or `restart`.

## Observability

- Every session's event stream, every verb the workspace runs, and every check is written to an activity record the operator can tail, one file per step run, through the redactor. `docs/activity.md` is the authority on its shape and guarantees; `mill logs` is a line renderer over it, not a separate log.
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
