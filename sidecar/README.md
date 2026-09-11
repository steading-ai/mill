# The Claude Agent SDK sidecar

The first harness adapter; its Go side is `internal/harness/claude`, which a
step names as `claude` in its `harness` table. The worker spawns one of these
per skill step and speaks `harness.v1` to it over newline-delimited JSON on
stdio:

```
node sidecar/dist/main.js  < one Start or Resume, as protojson on one line
                           > Event lines, then exactly one Terminal line
```

The process handles one request and exits 0. The session's own outcome is in
the terminal record's `exit`, never in the process's status: a status of `-1`
means the session never ran and `reason` says why — `refused`, `policy`,
`cancelled`.

A turn is not the end of a session. A session may background a command and
end its turn to be woken when it finishes, so the sidecar keeps reading until
a turn ends with no task still running. It watches that wait: every three
minutes it looks at each waited-on task's output file, stays silent while the
output grows, and sends one turn asking about a task that has gone quiet —
also written to the worker as a `heartbeat` event. Three silent looks trip the
breaker: the session's child processes are killed, the last *working* turn's
words become the final message, and the record carries reason `stalled`, which
the worker reads as a failed attempt. `src/tasks.ts` holds the constants and
the prompt; `MILL_SIDECAR_HEARTBEAT_MS` shortens the interval for the specs,
and nothing sets it in production. The session gets `TaskOutput` and `TaskStop`
in its allowed tools unless the step denies them, and a `PreToolUse` hook that
refuses a shell loop polling with `pgrep -f` — the wait that matches its own
shell and never ends (`src/hooks.ts`).

A session waits in the foreground for as long as its step may run. The
request's `timeout_ms` is the step's `timeout`, and the sidecar sets it as the
session's `BASH_DEFAULT_TIMEOUT_MS` and `BASH_MAX_TIMEOUT_MS` through the
SDK's `env` option — per step, never on the process — so a test suite or a
toolchain install stays in the turn that started it instead of being
backgrounded at the harness's own default and waited on. A request with no
timeout is refused. The startup line on stderr names the value set, which is
where a step's diagnostics keep it.

Cancellation is not a request. It arrives as **SIGTERM**, because a sidecar
mid-turn is not reading its stdin. The sidecar aborts the turn and answers
with a cancelled terminal record; if the harness has not stopped within the
grace window it reports the cancellation itself and exits, carrying whatever
usage the turn had streamed. `MILL_SIDECAR_GRACE_MS` sets that window (5000ms
by default) — it is on the environment because a signal carries nothing.

A step's options — its `harness.claude` block — reach the sidecar as an
opaque document: mill carries the yaml as written and interprets no key of
it, so this side owns the schema. `src/options.ts` is that schema, and it is
closed: an unknown key or a wrong type ends the request with exit reason
`policy`, naming the key, before the SDK is loaded. Adding a knob is a change
here and in a workflow, and mill is not rebuilt for it.

A session runs under the SDK's `auto` permission mode — the model classifier —
unless the step names another in `permission_mode`. A session is unattended: a
tool call that raises a prompt under the SDK's `default` mode is a prompt
nobody answers. The classifier answers; it does not enforce. `tools.deny` and
the workspace's socket are what enforce, which is why `bypassPermissions` is
refused here until the workspace is a sandbox.

Credentials for the SDK come from the account the sidecar runs under, through
`$HOME`; never from the request. What the *session* gets is the workspace's
socket and step token, as `WORKSPACE_SOCKET` and `STEP_TOKEN`, over a floor of
host variables a process needs to start, plus whatever the step's `env`
carved out — `env.passthrough` names variables to copy from the sidecar's own
environment, `env.set` sets literals. Nothing else the sidecar was spawned
with is passed on. What the *sidecar* is spawned with is the worker's
`--env-mode` (docs/architecture.md §"Skill steps: the harness adapter").

```
main.ts        the entrypoint the worker spawns; dist/main.js
src/main.ts    framing: one request in, events and one terminal record out
src/session.ts SDK wiring: session id, resume, cwd, environment, policy, schema
src/options.ts the schema for a step's options: model, effort, tools,
               permission_mode, env. Owned here
src/usage.ts   per-model token counts, per turn and in total
src/exit.ts    SDK outcomes to ProcessExit
src/tasks.ts   the wait a turn ends on: the heartbeat, the breaker, the kill
src/hooks.ts   the PreToolUse hook that refuses a self-matching `pgrep` wait
gen/           harness.v1 in TypeScript; buf writes it, `make generate` runs buf
spec/          the fake harness the Go specs drive the sidecar against
```

```bash
make sidecar-build                  # compile to dist/
ginkgo ./sidecar/                   # the specs, against the fake harness
MILL_SIDECAR_LIVE=1 ginkgo --label-filter=live ./sidecar/   # against the real SDK
```

The specs are Ginkgo, in `sidecar/*_test.go`: the sidecar is a process, and
what is under test is what that process writes to its stdout.
