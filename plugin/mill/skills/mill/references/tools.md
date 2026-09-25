# Tools

Every tool the `mill` MCP server registers, with its arguments and one example
response. Each is one `mill` CLI command with its flags as arguments, so
translating either way is mechanical: `mill inspect mill#18 --view full` is
`{"run": "mill#18", "view": "full"}`.

The server is authoritative. `mill mcp --describe` prints the same list this
document describes — name, description and input schema, exactly as
`tools/list` answers it — and a spec in this repository fails when the two
disagree about which tools exist.

Conventions that hold for every entry below:

- **`run`** is a skift node reference (`mill#18`) or the run id `status`
  prints. Anything carrying a `#` is a node reference; a node reference
  resolves to that node's most recent run.
- **`view`** is `refs`, `summary` (the default) or `full`. `refs` is
  identifiers alone, `summary` the fields worth reading at a glance, `full`
  everything the ledger holds in the wire's own spelling. The examples show
  `summary` unless they say otherwise.
- A **misspelt argument is refused**, not ignored.
- Every failure comes back as a tool error carrying the Connect code and, for
  the codes a caller can act on, what to do about it.
- Times are RFC 3339 in UTC; token counts are counts, never currency.

Three tools change something — `cancel`, `rerun`, `workspace_clean` — and are
marked. The role that decides whether they are allowed is the unix account
this server runs as, not anything a session says.

## status

What is live. Start here.

| Argument | Type | |
|---|---|---|
| `project` | string | Only runs on nodes in this skift project |
| `statuses` | string[] | Only these states: `selecting`, `running`, `waiting`, `parked`, `done`, `failed`, `abandoned`. Empty means every state that has not ended |
| `view` | string | `refs`, `summary`, `full` |

```json
{
  "runs": [
    {
      "node": "mill#18",
      "run": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
      "status": "running",
      "step": "implement",
      "attempt": 2,
      "elapsed": "18m4s",
      "input_tokens": 184213,
      "output_tokens": 9820
    },
    {
      "node": "mill#21",
      "run": "01JB8F9R7T2M4K0X5Z9C6T1A9D",
      "status": "waiting",
      "step": "plan-gate",
      "attempt": 1,
      "elapsed": "2h11m0s",
      "input_tokens": 40122,
      "output_tokens": 2210,
      "gate": "01JB8FA0000000000000000000"
    }
  ]
}
```

`gate` is present only when the run is blocked on one.

## inspect

One run in full: the workflow version it follows and every attempt at every
step, in order, with the evidence that closed it.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |
| `view` | string | `refs`, `summary`, `full` |

```json
{
  "node": "mill#18",
  "run": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
  "status": "running",
  "step": "implement",
  "attempt": 2,
  "elapsed": "18m4s",
  "input_tokens": 184213,
  "output_tokens": 9820,
  "workflow": "quick-fix@fb2764fd70fd",
  "steps": [
    {
      "step": "select",
      "attempt": 1,
      "status": "closed",
      "kind": "skill",
      "exit": "ok",
      "evidence_kind": "structured",
      "evidence": "workflow=quick-fix"
    },
    {
      "step": "gather-context",
      "attempt": 1,
      "status": "closed",
      "kind": "skill",
      "exit": "ok",
      "evidence_kind": "artifact",
      "evidence": "sha256:aaaa…"
    },
    {
      "step": "implement",
      "attempt": 1,
      "status": "failed",
      "kind": "skill",
      "exit": "error",
      "evidence_kind": "commit",
      "evidence": "-",
      "base_commit": "9f1c0d2"
    },
    {
      "step": "implement",
      "attempt": 2,
      "status": "running",
      "kind": "skill",
      "exit": "-",
      "evidence_kind": "commit",
      "evidence": "-",
      "base_commit": "9f1c0d2"
    }
  ]
}
```

`view: "refs"` drops everything after `status` on each step; `view: "full"` is
the `InspectResponse` message verbatim, including sessions and per-model usage.

## explain

Where one run is and what it is waiting on. The tool to reach for when asked
why a run is stuck. It returns no transcript content; the ledger holds none.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |

```json
{
  "node": "mill#21",
  "run": "01JB8F9R7T2M4K0X5Z9C6T1A9D",
  "status": "parked",
  "step": "implement",
  "step_kind": "skill",
  "step_run_status": "failed",
  "exit": "error",
  "evidence": "commit",
  "attempts_used": 3,
  "retries": 3,
  "gate": "mill#63",
  "gate_title": "implement failed three times on mill#21",
  "parked_reason": "retries",
  "waiting_on": "a human to answer park gate mill#63"
}
```

Only a person completes a gate. `waiting_on` is the sentence to read first.

## logs

One attempt at one step: the session's events, one summary line each, as the
worker wrote them through the redactor. Redaction happened at write time and
full tool output was never written.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |
| `step` | string | Defaults to the step the run is on, or the last one attempted |
| `tail` | integer | Only the last N lines. Zero means the whole log |

```json
{
  "run": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
  "step": "implement",
  "attempt": 2,
  "status": "running",
  "lines": [
    "2026-09-08T16:04:08Z text reading the work order",
    "2026-09-08T16:04:11Z tool Read call=toolu_017",
    "2026-09-08T16:04:11Z result call=toolu_017 ok out=package cli assembles the cobra command tree…",
    "2026-09-08T16:05:52Z usage claude-sonnet-4-5 in=184213 out=9820"
  ]
}
```

A check step has no session, so its lines are what its command wrote: the exit
status, then the output its contract keeps. A worker started with no log
directory keeps no logs and this tool has nothing to read.

## history

Every event recorded against a run, oldest first. The audit trail, and the
record of a run that has already ended.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |

```json
{
  "events": [
    {
      "id": "01JB8F3K2M7Q4X0V5Z9C6T1A2C",
      "run_id": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
      "op": "create_run",
      "after": {"generation": 1, "status": "selecting"},
      "worker_ref": "mill@forge-1",
      "occurred_at": "2026-09-08T16:04:08Z"
    },
    {
      "id": "01JB8F3K3P0000000000000001",
      "run_id": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
      "step_run_id": "01JB8F3K3P0000000000000000",
      "op": "close_step_run",
      "before": {"status": "running"},
      "after": {"status": "closed", "evidence_kind": "structured", "auto_approved": false},
      "worker_ref": "mill@forge-1",
      "occurred_at": "2026-09-08T16:04:31Z"
    }
  ]
}
```

## cost

One window of usage rolled up four ways, because "what did this cost" is asked
of a run, of a workflow, of a step and of a model. Counts, never currency: a
rate table is applied by whoever wants money.

| Argument | Type | |
|---|---|---|
| `since` | string | A window such as `24h` or `7d`. Empty means everything |
| `project` | string | Only runs on nodes in this skift project |

```json
{
  "by_run": [
    {
      "key": "mill#18",
      "totals": [
        {
          "model": "claude-sonnet-4-5",
          "input_tokens": "184213",
          "output_tokens": "9820",
          "cache_read_tokens": "1204331",
          "cache_write_tokens": "0"
        }
      ]
    }
  ],
  "by_workflow": [{"key": "quick-fix@fb2764fd70fd", "totals": [{"model": "claude-sonnet-4-5", "input_tokens": "184213", "output_tokens": "9820"}]}],
  "by_step": [{"key": "implement", "totals": [{"model": "claude-sonnet-4-5", "input_tokens": "184213", "output_tokens": "9820"}]}],
  "by_model": [{"key": "claude-sonnet-4-5", "totals": [{"model": "claude-sonnet-4-5", "input_tokens": "184213", "output_tokens": "9820"}]}]
}
```

Token counts are 64-bit and come back as JSON strings, which is how proto3
spells an `int64`. `by_model` is summed from the rows `by_step` renders, so
the two agree by construction.

## workflow_list

The workflow versions the store holds: what a run on a project would be able
to follow.

| Argument | Type | |
|---|---|---|
| `name` | string | Only this workflow |
| `project` | string | Only this project's scope |
| `all_versions` | boolean | Every version, not just the newest of each name |

The response is the wire's own shape: one full `Workflow` message per
version, each carrying its whole step list. The example abbreviates a
six-step workflow to one step.

```json
{
  "workflows": [
    {
      "id": "01JB6Q0000000000000000000A",
      "name": "quick-fix",
      "version": "fb2764fd70fd9c0a…",
      "scope": "SCOPE_ORG",
      "steps": [
        {"name": "implement", "kind": "STEP_KIND_SKILL", "evidence": "EVIDENCE_KIND_COMMIT", "retries": 3}
      ],
      "source": {"repo": "https://forge.example/mill/mill.git", "commit": "9f1c0d2", "path": "workflows/quick-fix"},
      "created_at": "2026-09-01T09:12:44Z"
    }
  ]
}
```

`SCOPE_ORG` is the scope every project may select from; `SCOPE_PROJECT` names
its project alongside it. Reach for `workflow_show` when the steps are what
you want to read — it renders them for a person and takes `view`.

## workflow_show

One workflow version as a run would walk it. This is the definition; `inspect`
shows what a run made of it.

| Argument | Type | |
|---|---|---|
| `name` | string | **required**, such as `quick-fix` |
| `project` | string | Resolve in this project's scope |
| `version` | string | A specific version, rather than the newest |
| `view` | string | `refs`, `summary`, `full` |

```json
{
  "name": "quick-fix",
  "scope": "global",
  "version": "fb2764fd70fd9c0a…",
  "steps": [
    {
      "step": "select",
      "kind": "skill",
      "evidence": "structured",
      "skill": "mill/select@1",
      "capabilities": ["read"],
      "retries": 1,
      "on_failure": "fail",
      "session": "fresh"
    },
    {
      "step": "implement",
      "kind": "skill",
      "evidence": "commit",
      "skill": "mill/implement@1",
      "capabilities": ["read", "write", "commit"],
      "retries": 3,
      "on_failure": "park",
      "session": "resume"
    },
    {
      "step": "clean-tree",
      "kind": "check",
      "evidence": "exit",
      "skill": "-",
      "capabilities": ["read"],
      "retries": 0,
      "on_failure": "fail",
      "session": ""
    }
  ]
}
```

## workspace_show

Where a run's workspace is on this host and what state it is in. This host's
workspaces only: a run worked elsewhere, or one whose workspace has been
destroyed, has nothing to show.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |

```json
{
  "run": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
  "node": "mill#18",
  "status": "running",
  "path": "/home/mill/.local/state/mill/workspaces/01JB8F3K2M7Q4X0V5Z9C6T1A2B/checkout",
  "branch": "mill/18",
  "head": "9f1c0d2",
  "clean": false,
  "changed": ["internal/cli/mcp.go"],
  "socket": "/home/mill/.local/state/mill/workspaces/01JB8F3K2M7Q4X0V5Z9C6T1A2B/ws.sock",
  "socket_present": true,
  "retained": "the run is being worked"
}
```

## doctor

Probe everything a run depends on and report each as pass or fail with one
line of reason. Takes no arguments; every probe is reported.

```json
{
  "probes": [
    {"name": "socket", "ok": true, "reason": "/run/user/1000/mill/mill.sock answers; mill 0.0.0-dev"},
    {"name": "schema", "ok": true, "reason": "migration 14, current"},
    {"name": "skift", "ok": true, "reason": "/run/skift/skift.sock answers; this account is a principal on mill, 1 node(s) ready for an agent"},
    {"name": "sidecar", "ok": false, "reason": "sidecar/dist/main.js did not answer: claude: the sidecar answered nothing before it exited"},
    {"name": "signing", "ok": false, "reason": "no signing key under ~/.local/state/mill/workspaces/signing"},
    {"name": "forge", "ok": false, "reason": "no repository to probe: forge: project mill has no repository: set it with `skift project update mill --vcs-url <url>`"},
    {"name": "notify", "ok": true, "reason": "contrib/notify/thicket-send.sh accepts a probe notice"}
  ]
}
```

A probe answers about **this host** — the one the server runs on, which is not
necessarily the one that worked the run being read.

## cancel

**Changes something.** Ask the worker holding a run to stop the step it is on.
This sets a flag; the worker acts on it at its next heartbeat, records the
attempt aborted with reason `cancelled`, and releases its lease as preempted.

The run is not failed and not abandoned: it keeps its workflow and its branch,
and the next claim resumes at that step.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |
| `reason` | string | Why, recorded on the run's audit trail |

```json
{
  "run": {
    "id": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
    "node_ref": "mill#18",
    "status": "RUN_STATUS_RUNNING",
    "cancel_requested": true
  }
}
```

## rerun

**Changes something. Needs the `operator` role.** Redo a run from a named
step: every step from it through the step the run is on is superseded, and the
run is left resumable at the named step. The superseded attempts stay in the
ledger with the evidence they produced; they close nothing, which is what
moves the run backwards.

This keeps the generation, the branch and the sessions. A `failed` run is
reopened by it; an `abandoned` one is refused, because a later generation owns
the node. A run a worker is still holding is refused naming the holder —
`cancel` it first. `restart`, which replaces the generation, the branch and
the sessions, is CLI-only and not offered here.

| Argument | Type | |
|---|---|---|
| `run` | string | **required** |
| `from` | string | **required**, the step to redo from, such as `implement` |
| `reason` | string | Why, recorded on the run's audit trail |

```json
{
  "run": "01JB8F3K2M7Q4X0V5Z9C6T1A2B",
  "node": "mill#18",
  "from": "implement",
  "through": "clean-tree",
  "generation": 1,
  "superseded": ["implement", "clean-tree"],
  "what_happens": "superseded implement, clean-tree; resumable at implement in generation 1"
}
```

`what_happens` gains `; the run had ended and is live again` when the rerun
reopened a failed run.

## workspace_clean

**Changes something. Needs the `operator` role.** Remove the workspaces of
runs that have ended: `done`, `failed`, or `abandoned`. A run that is
`selecting`, `running`, `waiting` or `parked` keeps its checkout whatever its
age, because that is what its resume attaches to.

| Argument | Type | |
|---|---|---|
| `older_than` | string | Only runs that ended longer ago than this, such as `7d` |
| `dry_run` | boolean | List what would be removed and remove nothing |

```json
{
  "root": "/home/mill/.local/state/mill/workspaces",
  "dry_run": true,
  "count": 1,
  "workspaces": [
    {
      "run": "01JB7Z0000000000000000000A",
      "node": "mill#11",
      "status": "done",
      "path": "/home/mill/.local/state/mill/workspaces/01JB7Z0000000000000000000A"
    }
  ]
}
```
