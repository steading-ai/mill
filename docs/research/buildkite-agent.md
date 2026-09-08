# buildkite-agent as prior art for the orchestrator

Source: `/Users/ivy/src/github.com/buildkite/agent` @ `256e04e` (v4 line, 2026-09-07). Paths below are relative to that root.

## 1. Work pickup

**Protocol, in order** (all in `api/`, driven by `core/client.go` retry wrappers):

1. `POST register` with the *registration token* → `AgentRegisterResponse{AccessToken, Endpoint, PingInterval, HeartbeatInterval, JobStatusInterval}` (`api/agents.go`). The client is then re-created with the access token: `FromAgentRegisterResponse` at `api/client.go:165` — "Override the registration token with the access token". Retries 30×10s.
2. `POST connect` (10×5s).
3. Heartbeat loop: `POST heartbeat` every `HeartbeatInterval` (`agent/agent_worker_heartbeat.go`). This is the *only* liveness signal; an unrecoverable heartbeat error calls `a.StopUngracefully()`.
4. Work discovery is **poll with a streaming fast path**. `GET ping` every `PingInterval` + jitter (`agent/agent_worker_ping.go:runPingLoop`) returns `Ping{Action, Job, Endpoint}` with `Action ∈ {"", "pause", "disconnect"}`. In auto mode a gRPC `StreamPings` (`api/pings_streaming.go`) runs alongside; a `baton` (`agent/baton.go`) lets the poll loop fire only while the stream is unhealthy, and a debouncer collapses pause/resume/job bursts into "0 or 1 correct next action".
5. **Accept**: `PUT jobs/:id/accept` (`agent_worker_action.go:AcceptAndRunJob`, 30×5s; non-retryable → re-ping). Returns the *finalized* job: env with agent tags applied plus a per-job `Token` (`api/jobs.go`).
6. **Acquire** (`--acquire-job`): `PUT jobs/:id/acquire` with `X-Buildkite-Lock-Acquire-Job: 1` (`core/client.go:AcquireJob`). `423 Locked` = waiting on dependencies, retry honouring `Retry-After`; `422` = already acquired/started/finished → `ErrJobAcquisitionRejected`, no retry; 7 attempts in a 330s ceiling; `agent_start.go` maps these to exit codes 27/28.
7. `PUT jobs/:id/start` (`core/client.go:StartJob`).
8. While running, `GET jobs/:id` every `JobStatusInterval` (`agent/job_runner.go:jobCancellationChecker`); `canceling|canceled|timing_out|timed_out` triggers `Cancel`; a `401` means the job token was revoked.
9. `PUT jobs/:id/finish{exit_status, signal, signal_reason, chunks_failed_count}` (`core/client.go:FinishJob`, 12 exponential ≈10min). A 422 is treated as "already finished, go find more work".

**What the agent holds:** the job token, the `api.Job`, two env files, the child process. There is no job-level lease renewal; liveness is agent-scoped. **If the agent dies mid-job nothing agent-side recovers it** — `JobRunner` persists no state, a restarted agent registers fresh, and the server decides (third Ctrl-C: "agents will be considered lost!"). Reassignment is a control-plane concern the agent never sees.

## 2. Job lifecycle on the agent

No state-machine type; `JobRunner.Run` (`agent/run_job.go:75`) is linear code whose result is a value, `core.ProcessExit{Status int, Signal, SignalReason string}`, with two `atomic.Bool`s (`cancelled`, `agentStopping`) modulating interpretation.

Order: `StartJob` → `logStreamer.Start` → JWKS signature verification plus job-vs-signed-step consistency (`verify_job.go`) → agent allowlists (`validateConfigAllowlists`: repo, env names, plugin sources) → `pre-bootstrap` hook (runs *in the agent process*, gate only) → goroutines `streamJobLogsAfterProcessStart` + `jobCancellationChecker` → `runJob` (blocks on `process.Run`) → deferred `cleanup`: flush output, stop streamer, delete env files, `FinishJob` on a **non-cancellable context**.

Refusals before the process starts report `Status=-1` plus a `SignalReason` (`agent_refused`, `signature_rejected`, `process_run_error`); `run_job.go:31-66` explains the reluctance to add more — customers key `automatic_retry` on them.

Reporting: start/finish/chunks go from the agent; artifacts, annotations, meta-data, secrets and OIDC are the *executor subprocess* calling the API with the job token via `buildkite-agent` subcommands.

## 3. The bootstrap / job executor

Separate process: `process.New(Config{Path: "buildkite-agent bootstrap", Dir: BuildPath, Env: os.Environ()+jobEnv, PTY, InterruptSignal, SignalGracePeriod})` at `agent/job_runner.go:325`. Kubernetes mode swaps this for a unix-socket `kubernetes.Runner` that other containers dial — the executor can be split from the worker across a socket.

Env assembly is `createEnvironment` (`job_runner.go:363`): copy job env, `delete(env, "BUILDKITE_AGENT_TOKEN")`, write `BUILDKITE_ENV_FILE` (`%q`-quoted, explicitly *not* meant to be sourced) and a JSON twin, then layer agent config with `setEnv` recording collisions into `BUILDKITE_IGNORED_ENV` so the executor can warn about protected vars.

Executor phases (`internal/job/executor.go:131 Run`): `setUp` (secrets, `environment` hook) → plugin → checkout → vendored plugin → command (pre/command/post) → artifact (in the grace period) → deferred `tearDown` (`pre-exit`). Exit precedence: setup failure → hook's code or `ExitCodeSetupFailure=125` (parent maps to -1); phase error → its code; else `BUILDKITE_COMMAND_EXIT_STATUS`; a failing `post-command` overrides a passing command.

Checkout dir: `Join(BuildPath, dirForAgentName(AgentName), OrgSlug, PipelineSlug)` (`executor.go:945`). **Concurrency isolation is by agent name**: `--spawn N` interpolates `%spawn` into each worker's name (`agent_start.go:1249`), so each worker owns a distinct build dir and plugin dir. Checkouts are *reused* across jobs (fetch, not clone) and self-heal via `removeCheckoutDir` on classified git errors (`checkout.go:117`). Git mirrors are shared host-wide behind a file lock. The checkout is opened as an `*os.Root` so local hooks and vendored plugins cannot escape the tree.

## 4. Hooks and plugins

**Scopes**: `agent` (hooks-path + additional-hooks-paths), `repository` (`.buildkite/hooks` under the checkout root; can be disabled with `LocalHooksEnabled=false` or `BUILDKITE_NO_LOCAL_HOOKS`), `plugin` (`<plugin>/hooks`) — `executor.go:432`. **Points**: `pre-bootstrap` (agent only), `environment`, `pre-checkout`, `checkout`, `post-checkout`, `pre-command`, `command`, `post-command`, `pre-artifact`, `post-artifact`, `pre-exit`. Forward order global→local→plugin; `post-*`/`pre-exit` run in reverse (`hooks.go`). `command` and `checkout` are single-winner (`strictSingleHookTypes`).

**Invocation** (`executeHook`): `hook.Type` sniffs binary magic / shebang. POSIX-shell hooks are *wrapped* (`hook/wrapper.go`): `env dump > before; . hook; export BUILDKITE_HOOK_EXIT_STATUS=$?; export BUILDKITE_HOOK_WORKING_DIR=$PWD; env dump > after`, then `Changes()` diffs the dumps. `applyEnvironmentChanges` strips protected keys, `Chdir`s, re-seeds redactors, and re-reads `ExecutorConfig` by reflection over `env:"BUILDKITE_…"` struct tags (`config.go:ReadFromEnvironment`) — that is how a hook reconfigures checkout mid-job. Binary/polyglot hooks are unwrapped and can only mutate state through the Job API. **Failure**: non-zero → `shell.ExitError{Code}`, remaining phases skipped except `post-command`/`tearDown`.

**Plugins** (`internal/job/plugin.go`, `agent/plugin/plugin.go`): the step's `plugins:` arrives as JSON in `BUILDKITE_PLUGINS`; `org/repo#ref` becomes a git URL; `checkoutPlugin` clones to a temp dir, `git checkout -f <version>`, renames into `PluginsPath/AgentName/<id>`; an existing `.git` short-circuits unless `PluginsAlwaysCloneFresh`. Config flattens to `BUILDKITE_PLUGIN_<NAME>_<KEY>`. Optional `plugin.yml` validation, `AllowedPlugins` regex, vendored plugins from the checkout root.

**As "pluggable workflow steps"**: it is an *aspect* system — every plugin hooks the same fixed lifecycle, composition is lexical order, the interface is env-in / env-diff-and-exit-code-out, identity is a mutable git ref. Good for cross-cutting concerns, poor for typed inputs/outputs or evidence.

## 5. Environment, secrets, capability

**Three credential tiers**: registration token (register only) → agent access token (session) → per-job token (`NewJobRunner` swaps the client if `conf.Job.Token != ""`, `job_runner.go:172`). The job token is what the executor gets as `BUILDKITE_AGENT_ACCESS_TOKEN`; every job-scoped endpoint embeds the job ID in its path.

**Per-job derived credentials**, all minted server-side against the job ID: git via a credential helper (`clicommand/git_credentials_helper.go` → `POST jobs/:id/repository_access_token`, with ssh→https rewrite), `POST jobs/:id/oidc/tokens{audience, lifetime, claims}`, `GET jobs/:id/secrets?key=`, and artifact uploads via server-issued signed `ArtifactUploadInstructions`. Secrets are fetched in `setUp`, **registered with the redactor before being set in env**, and refused for protected names.

**Redaction** happens in the executor, so the agent never sees plaintext: `setupRedactors` (`executor.go:1457`) puts a `replacer.Replacer` in front of both stdout and the shell logger, seeded from `RedactedVars` globs, extended by secrets and by `POST /redactions` on the Job API, flushed at hook boundaries.

**Protected env** (`env/protected.go`): agent-authoritative vars with a `mutableFromWithinJob` bit, plus a `checkoutOverrideScope` governed by `from-job|strict|none` because "git is riddled with shell injections".

**Job API** (`jobapi/`): unix socket, 32-byte bearer per job, routes `GET/PATCH/DELETE /env`, `PUT /workdir`, `POST /redactions`, `POST /promise-failure`; protected keys → 422. This is the nearest thing to a capability handle — a local authenticated endpoint the step talks to instead of holding raw state — but its scope is env/log mutation. No FS capability exists; `os.Root` confines the executor's own lookups, not the command.

## 6. Checks and status

Success is the bootstrap's exit status, full stop (`run_job.go:runJob` → `process.WaitStatus().ExitStatus()`), decorated with `Signal`/`SignalReason`. Timeouts are server-decided and arrive as job state; `Cancel(CancelReasonJobTimeout)` writes `BUILDKITE_AGENT_JOB_TIMEOUT_FILE` so `post-command` sees `BUILDKITE_JOB_TIMED_OUT=true`.

There is **no structured output channel for a step**. Side channels, each a job-token API call from inside the job: annotations (`api/annotations.go`), build-scoped meta-data KV with transactional `set-batch` (`api/meta_data.go`), artifacts with sha256 and glob paths, dynamic pipeline upload, and the newest — `PUT jobs/:id/promise_failure{exit_status, reason}` to declare failure early while the job keeps running (`api/jobs.go`, `jobapi/promise_failure.go`).

## 7. Logs and streaming

stdout+stderr → `process.NewTimestamper` (injects `\x1b_bk;t=<ms>\x07` per line) → `process.Buffer`. `streamJobLogsAfterProcessStart` (`run_job.go`) drains it every `ChunksIntervalSeconds` (32-tick runs, re-jittered to defeat fleet synchronisation). `LogStreamer.Process` (`agent/log_streamer.go`) cuts `≤ChunksMaxSizeBytes` chunks tagged `{Sequence, Offset, Size}` onto a 1024-deep queue; 3 workers gzip and `POST jobs/:id/chunks?sequence&offset&size` (`api/chunks.go`) with the 12-attempt retry; any 4xx breaks. Failed chunks are counted and reported in `finish`. Sequence+offset make the server idempotent/reorderable; **the client keeps nothing durable** — unsent chunks die with the process. If `Process` errors the buffer is closed so the child gets SIGPIPE. Redaction already happened upstream (§5).

## 8. Cancellation, retries, signals

Agent process (`clicommand/agent_start.go:poolSignals`): first SIGINT/SIGTERM → `StopGracefully` (stop pinging, let the job finish, `POST stop`); second → `StopUngracefully` (cancel jobs with `agent_stop`, hard `os.Exit(1)` after `CancelSignalTimeout + CancelCleanupTimeout + 1s`); third → immediate exit; SIGQUIT → ungraceful.

Job cancel (`run_job.go:Cancel`): optional timeout marker file → `process.Interrupt()` (SIGTERM to the *process group*; a configured SIGKILL is downgraded to SIGTERM so the bootstrap can still clean up) → wait `CancelSignalTimeout` → `Terminate()` (SIGKILL group). Inside the bootstrap the signal handler calls `Executor.Cancel`, the main ctx is cancelled, but `post-command`, artifact upload and `pre-exit` run on `WithGracePeriod(ctx, SignalGracePeriod)` (`internal/job/grace.go`). Finally `bootstrap` re-signals itself (`signalSelf`) so the parent's `WaitStatus` reads "signaled".

Retries are `roko` retriers with explicit per-call budgets and `api.BreakOnNonRetryable` (`api/retryable.go`: 429/5xx/undecodable/network → retry; other 4xx → break). Job-level `automatic_retry` is server-side; the agent only supplies `signal_reason`. Checkout gets 6 exponential attempts with per-error-class cleanup (`checkout.go:169`).

Partial-job guarantees: `finish` is always attempted, logs flushed, env files removed; the workspace is **not** rolled back and is reused next job; there is no resume.

## 9. What to steal

1. **Token laddering** — `api/client.go:165`, `job_runner.go:172`: store issues a lease-scoped token per (node, step) and that is the *only* credential the session ever sees; revoke on release.
2. **Acquire status vocabulary** — `core/client.go:AcquireJob`: 423 + `Retry-After` for "blocked by DAG deps", 422 for "someone else holds it", lock header naming the intent. Map directly onto fenced-lease claim responses.
3. **`ProcessExit{Status, Signal, SignalReason}` with `-1 = never ran`** — `run_job.go`: keep "agent refused / signature / timeout / cancel" distinct from "check failed" so retry policy can key on it.
4. **Executor-as-subprocess with an env-file handoff and a pre-bootstrap gate that validates without sourcing** — `job_runner.go:363,840`: your step runner should receive the workspace handle as a file/socket, and a policy hook should be able to reject before anything executes.
5. **Hook env-diff + protected-key filter + reflective config refresh** — `hook/wrapper.go`, `executor.go:706`, `env/protected.go`: steps emit a *diff* to session context; the orchestrator owns which keys are writable and re-derives its own config from the accepted diff.
6. **Redactor seeded before secrets touch env, extendable at runtime** — `executor.go:1457`, `jobapi/redactions.go`: seed from the capability set, let checks register new needles, flush at step boundaries.
7. **Job API socket with per-job bearer** — `jobapi/server.go`: this is your workspace handle's transport; add `/fs`, `/git`, `/checks` verbs rather than raw creds.
8. **Interrupt → grace → kill, with cleanup on `WithGracePeriod`** — `run_job.go:Cancel`, `grace.go`: sessions get SIGTERM, evidence-collection runs in the grace window, then SIGKILL the group.

## 10. What doesn't map

- **Buildkite is two processes, not three.** The hosted control plane is store + API + scheduler + pipeline compiler; the agent is worker + executor subprocess. Your process (2) is what Buildkite does server-side. `core.Controller` (`core/controller.go`) is the library shape of "worker without bootstrap" and is closer to what your (3) should be.
- **No job lease, no fencing.** Liveness is the agent heartbeat; the agent never proves it still holds the job when it writes `finish` or chunks. With fenced leases, send the fence on every write and treat 422-on-finish as the *normal* fenced-out path.
- **No structured step output.** "Required evidence" must be a first-class artifact the store validates, not an annotation.
- **Hooks are aspects, not steps.** Borrow the env/protection mechanics; do not build workflow steps on a fixed lifecycle with git-ref identity.
- **Mutable, reused checkouts keyed by agent name** assume one pipeline ≈ one repo on a long-lived host. Sessions want disposable per-step worktrees; the `Repository == ""` branch (`MkdirTemp` + `cleanupDirs`) is the closer pattern.
- **Scale-driven complexity**: baton/debouncer/stream fallback, jitter windows, `%spawn`. A single operator should watch the store and skip all of it.
- **Cancellation by polling job state each second** is a CI compromise; you own the socket, so push cancel through the handle.
- **"Stateless, runs anywhere" is only true at the network layer.** The agent caches checkouts, plugins and mirrors on disk keyed by agent name. Decide up front whether workers own persistent workspaces or the store hands them a fresh one per step.
