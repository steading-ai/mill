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
