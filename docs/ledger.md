# Ledger

The mill's only durable state: what happened inside each run. This document specifies what the ledger records, what it refuses to record, the invariants that make resume correct, and the algorithm a worker follows to resume from it. PostgreSQL, goose migrations, sqlc queries — dagger's conventions, so the two repos read alike.

The ledger exists for one reason: **a step is closed by evidence the mill verified, and a run resumes from the record of that evidence, never from a transcript.** Everything below serves that.

## What the ledger is and is not

**Is:** the record of selection, step runs, evidence, sessions, tokens, and usage for every run. Append-mostly. The truth about where a run is.

**Is not:** a copy of dagger. Node bodies, results, artifacts, comments, leases, and the graph live in dagger and are read from it. The ledger stores dagger *identifiers* — node id, context id, lease id, gate node id — never dagger content. If the mill's database were lost, every run in flight could be re-derived from dagger's events plus a re-run of each open step; what would be lost is cost attribution per step and the record of which session produced which verdict.

**Is not:** a session store. Transcripts live with the harness. The ledger knows a session's deterministic id and the adapter's reference to it; it cannot replay one.

## Tables

### `workflows`

Immutable definitions. One row per version.

| Column | Notes |
|---|---|
| `id` | ULID |
| `name`, `scope`, `project` | Unique together with `version` |
| `version` | Content hash of the canonical definition |
| `definition` | The definition as stored, JSONB. Steps, evidence specs, gates, capability sets, harness tables |
| `source` | JSONB: `{kind: repo|cli, repo, commit, path, pushed_by}` |
| `created_at` | |

Never updated. A new definition with the same name and scope is a new row; loaders pick the newest unless a run pins one.

### `skills`

The portable part of a step, stored so a workflow version pins the exact prompt it ran.

| Column | Notes |
|---|---|
| `id` | ULID |
| `name` | From `SKILL.md` frontmatter |
| `digest` | Content hash of the skill directory |
| `description` | From `SKILL.md` frontmatter. What a router matches a node against |
| `body` | The `SKILL.md` body, the prompt |
| `files` | JSONB manifest of `scripts/`, `assets/`, `references/`: path → digest. Bytes are stored in the store's blob table by digest |
| `metadata` | The flat string map from frontmatter |
| `source` | As for workflows |
| `created_at` | |

A workflow step references a skill by `(name, digest)`. Two workflows sharing a skill share the row.

### `runs`

| Column                                 | Notes                                                                                                                                                      |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`                                   | ULID                                                                                                                                                       |
| `node_id`                              | Dagger node ULID. **Unique.** One run per node, ever<br>
 |
| `node_ref`                             | Display key, e.g. `dagger#18`                                                                                                                              |
| `context_id`                           | Copied from the node at creation                                                                                                                           |
| `workflow_id`                          | Null while `selecting`; set once and never changed                                                                                                         |
| `status`                               | `selecting`, `running`, `waiting`, `parked`, `done`, `failed`                                                                                              |
| `lease_id`                             | Current dagger lease or null                                                                                                                               |
| `worker_ref`                           | Current or last worker                                                                                                                                     |
| `gate_node_id`                         | Set while `waiting` or `parked`                                                                                                                            |
| `parked_reason`                        | `retries`, `budget`, `denied`, `timeout`, `refused`                                                                                                        |
| `branch`                               | The run's git branch                                                                                                                                       |
| `created_at`, `updated_at`, `ended_at` |                                                                                                                                                            |

`current_step` is not a column. It is the first step in `workflow.definition.steps` whose latest `step_runs` row is not `closed` — no row at all, or a latest row that is `running`, `failed`, `aborted`, or `superseded`. A `revise` marks the latest rows of every step from its target through the failing step `superseded`, which is what moves `current_step` backwards; the evidence rows those step runs produced are untouched and no longer close anything. Storing `current_step` would let it drift from this, which is the failure the ledger exists to prevent.

### `step_runs`

One row per attempt at a step. Append-only.

| Column | Notes |
|---|---|
| `id` | ULID |
| `run_id`, `step_name` | The step from the run's workflow definition |
| `attempt` | 1-based per `(run_id, step_name)` |
| `kind` | `skill`, `check`, `gate`, copied at start so the row reads alone |
| `session_id` | Skill steps: the session this attempt ran in |
| `lease_id` | The dagger lease held when this attempt ran |
| `worker_ref` | |
| `status` | `running`, `closed`, `failed`, `aborted`, `superseded` |
| `exit` | JSONB `ProcessExit{status, signal, reason}` for skill and check steps. `status = -1` means never ran |
| `feedback_from` | The `step_runs.id` whose failure this attempt is the `revise` for, or null |
| `auto_approved` | Gate steps: true when the `auto` predicate closed it without a human |
| `started_at`, `ended_at` | |

`closed` means evidence exists for this row. `failed` means the attempt ended and `on_failure` applies. `aborted` means the worker stopped or was fenced out before the attempt ended; the next worker starts a new attempt. `superseded` means a later `revise` reopened this step; the row and its evidence stay as the record of pass one.

The circuit breaker counts `attempt` on the **failing** step — `review` fails with `revise implement`, and `review.retries` is what it compares against. This is a different counter from the run's derived `leases`, which counts dagger claims and moves on worker crashes, not on review rounds.

### `evidence`

Immutable. One row per closed step run.

| Column | Notes |
|---|---|
| `step_run_id` | Unique. A step run has at most one closing evidence |
| `kind` | `commit`, `exit`, `verdict`, `structured`, `url`, `status`, `artifact`, `gate` |
| `value` | JSONB. The fact, in the shape the kind defines |
| `verified_by` | The check name or mill routine that established it |
| `verified_at` | |

Insert-only. There is no update path. A step whose evidence was wrong is a new attempt with new evidence; the old row stays, and the workflow definition says whether the step may be re-run.

### `sessions`

| Column | Notes |
|---|---|
| `run_id`, `name` | Primary key |
| `id` | UUIDv5 of `(context_id, name)`. Stored for lookup; recomputable |
| `harness` | Adapter name |
| `adapter_ref` | JSONB. Whatever the adapter needs to resume: for Claude Code, the config dir and cwd the transcript lives under |
| `worker_ref` | The host the transcript is on. A different worker cannot transcript-resume; it graph-resumes with a fresh session of the same id |
| `started_at`, `last_turn_at` | |

### `step_tokens`

| Column | Notes |
|---|---|
| `id` | ULID |
| `step_run_id` | Unique per live token |
| `token_hash` | SHA-256 of the bearer. The bearer itself is never stored |
| `capabilities` | Text array, copied from the step |
| `issued_at`, `revoked_at` | Revoked when the step run leaves `running` |

The workspace validates a presented bearer by hash and by `revoked_at IS NULL`. Every derived credential the workspace mints is minted in memory against a validated token and never written here.

### `usage`

| Column | Notes |
|---|---|
| `step_run_id` | |
| `model` | Free string |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | Counts, never currency |
| `recorded_at` | |
| `posted_at` | When this row's step totals were posted to dagger; null until then |

One row per turn, per model, as the adapter reports — a turn that bills two models writes two rows. Cost is a rate table applied at query time. The step's totals are posted to dagger through `ReportUsage` as one record per `(lease, model)` **when the step run closes or fails**, and the rows are marked `posted_at`. Dagger sums records per attempt and each report renews the lease, so per-step posting is both the contract's intended shape and a heartbeat. A worker that dies mid-step loses only that step's usage from dagger's view; the ledger keeps it. There is no reconciliation job: dagger's per-lease total for an abandoned attempt is allowed to be short by one step, and the ledger is the complete record.

### `workspaces`

| Column | Notes |
|---|---|
| `id` | ULID |
| `run_id` | Unique while live |
| `kind` | `local`, `container`, `remote` |
| `worker_ref` | Where it is |
| `ref` | The git ref it was created at |
| `state` | `live`, `retained`, `destroyed` |
| `created_at`, `destroyed_at` | |

### `events`

Append-only. Every change to `runs.status`, every step run start and close, every token issue and revoke, every gate filed and every auto-approval. Fields: `id` (ULID), `run_id`, `step_run_id` (nullable), `op`, `before`, `after`, `worker_ref`, `occurred_at`. This is the audit trail and what `mill status --history` reads. It is not a change stream for consumers; nothing subscribes to it.

Not time-partitioned, unlike dagger's events. Dagger's are partitioned from the start because they are a change stream many consumers resume from; these are neither, and one operator running tens of runs a day does not justify the machinery. Partitioning is a migration away if volume ever asks for it.

## Invariants

The store enforces these with constraints or in the writing transaction. A violation is a bug in the mill, not a condition to handle.

1. **One live run per node.** Partial unique index on `runs.node_id` where status is not terminal; `(node_id, generation)` unique overall.
2. **A run's workflow is set once.** `workflow_id` transitions from null to a value exactly once.
3. **At most one `running` step run per run.** Partial unique index on `(run_id) WHERE status = 'running'`.
4. **Evidence closes exactly one step run, and a step run has at most one evidence.** `evidence.step_run_id` is unique and a foreign key.
5. **A `closed` step run has evidence; a step run with evidence is `closed`.** Written in one transaction.
6. **Evidence is never updated or deleted.** No `UPDATE` or `DELETE` grants on the table for the application role.
7. **Attempts are contiguous.** `attempt` for `(run_id, step_name)` is `max + 1`, assigned in the insert.
8. **A live token belongs to a `running` step run.** Revocation is written in the same transaction that moves the step run out of `running`.
9. **Every store write is fenced by the lease, atomically.** Every statement that mutates `step_runs`, `evidence`, `step_tokens`, `usage`, or `runs` for a run takes the `runs` row lock (`SELECT … FOR UPDATE`), re-reads `lease_id` in a separate statement after the lock is granted — a locked row is not re-read, so a lease a concurrent claim committed while waiting would otherwise be missed, which is exactly the trap dagger's `fence()` comments on — compares it to the lease the worker presented, and rolls back on mismatch. This is the mechanism behind "a stale worker cannot corrupt the record," and it lives in the store, not in the worker's good behaviour. The one exception is `SetLease` itself, which is the write that changes what later writes compare against; it takes the same lock and requires the run's current status to be one a new claim may take over (`running` with an expired lease, `waiting`, `parked`, or `done` with the node still open).
10. **`current_step` is derived.** There is no column to drift.
11. **Dagger first, store second.** A run is marked `done` only after dagger has confirmed the completing patch; `gate_node_id` is recorded only after dagger has returned the gate's id and before the lease is released. The store never asserts a fact about dagger that dagger has not yet made true.
12. **A gate is filed once per step run.** `gate_node_id` set and the current step a gate step means resume reads the gate; it never files another.

## Resume

The worker's algorithm on receiving a node from `ClaimNext`. Every store call presents `node.lease_id` and is fenced by Invariant 9; every dagger call carries it too.

```
run := store.RunByNode(node.id)                    # the live run, if any
switch:
  run == nil:
      run = store.CreateRun(node, generation=1)    # status: selecting
  run.status == done:                              # dagger confirmed Complete but the store missed it?
      if dagger.Node(node.id).status == done: store.Done(run); return   # no — the node is done; nothing to do
      # else: a done run on an open node is Invariant 11 violated; park with reason `refused` and alert
  run.status in (running, waiting, parked):
      store.AbortOpenStepRuns(run)                 # a 'running' row here belongs to a dead or fenced-out worker
store.SetLease(run, node.lease_id, self)          # from here every write is fenced by this lease
go heartbeat(node.lease_id) until run leaves this worker   # renews well inside the TTL; a fenced heartbeat, or cancel_requested, cancels the step
#   cancel_requested: abort the step run (reason cancelled), Release(PREEMPTED, 'cancel'), return — the run stays resumable

ws := workspace.Attach(run) or workspace.Create(run)   # local? reattach. elsewhere? clone run.branch (pushed, by the loader's rule)

for step in run.workflow.steps starting at run.current_step():

    if step.kind == gate and run.gate_node_id != nil:          # resume through a gate
        g := dagger.Node(run.gate_node_id)
        sr := store.LatestStepRun(run, step)
        if g.status == done:  store.Close(sr, evidence{kind: gate, value: g.result}); run.gate_node_id = nil; continue
        else:                 store.Fail(run, denied); dagger.Complete(node, deny_result); store.Failed(run); return

    sr := store.StartStepRun(run, step)            # attempt = max+1 for (run, step)
    tok := store.IssueToken(sr, step.capabilities)
    ws.Rotate(tok)
    result := execute(step, sr, ws)                # by kind; skill steps Resume a session by its UUIDv5
    store.RevokeToken(tok)
    dagger.ReportUsage(node.lease_id, usage(sr))   # one record per model; renews the lease

    switch:
      evidence verified:
          store.Close(sr, evidence)                # one transaction; sr -> closed
      gate to file:                                # step.kind == gate, auto did not hold
          gate := dagger.ApplyPatch(create gate + blocks edge, request_id)   # 1. dagger
          store.MarkWaiting(run, gate.id)          # 2. store, before the release
          dagger.Release(node.lease_id, GATE)      # 3. dagger
          return
      failed:
          store.Fail(sr, exit)
          if step.on_failure == revise <target> and store.Attempt(run, step) < step.retries:
              store.Supersede(run, from=target, through=step)   # their latest rows -> superseded
              feedback = result.feedback; continue            # loop recomputes current_step() == target
          else:
              dagger.Comment(node, last findings)
              gate := dagger.ApplyPatch(create park gate, request_id)
              store.Park(run, gate.id, reason)
              dagger.Release(node.lease_id, BUDGET)
              return

# terminal step closed
run.complete_request_id = store.SetCompleteRequestId(run)      # idempotency key, durable before use
dagger.ApplyPatch(complete_node(result, artifacts), lease, run.complete_request_id)   # releases the lease as part of the patch
store.Done(run)                                    # only now
ws.Destroy()
```

Three properties fall out:

- **Steps are re-run, sessions are resumed.** An interrupted step starts a new attempt from the top; the session it runs in is the same one, found by its deterministic id, so the model picks up its own prior context. A step is never resumed mid-flight — there is no mid-flight state to resume into.
- **Resume is the same code as a first run.** There is no separate recovery path except the gate branch at the top of the loop. `current_step()` is derived from step-run status, and a fresh run's `current_step()` is simply the first step.
- **A stale worker cannot corrupt the record.** Its dagger writes are fenced out by dagger; its store writes are rejected by Invariant 9. Its heartbeat fails first, which is how it learns.
- **`revise` actually revises.** Superseding every step from the target through the failure means the revised work is re-simplified, re-committed, re-pushed, and re-reviewed; the ledger never claims pass-one evidence closed pass-two work.

`Complete` is not an RPC. It is `ApplyPatch` with a `complete_node` operation, and dagger releases the lease inside that same patch — which is why `ReportUsage` for the final step must precede it, why there is no separate `Release` on the done path, and why `store.Done` comes last.

## What goes to dagger, and when

| To dagger | When | As |
|---|---|---|
| The run's branch and PR | PR opened | Artifact `reference`, scheme `git`, on the result |
| The merge commit | Merged | Artifact `reference` |
| Plan, verdicts, usage summary | `Complete` | Artifact `text` |
| The implementer's summary | `Complete` | Result body |
| Per-step usage | Every step run close or failure | `ReportUsage`, one record per `(lease, model)`, kind `actual` |
| The last findings on a park | Park | Comment |
| A gate | Gate step or breaker | Node, `assignee_kind: user`, child of and blocking this node; `ApplyPatch`, then the store, then `Release` |

Every artifact's metadata carries `run_id`. Every `ApplyPatch` mill sends uses `request_id = mill-<run_id>-<step>-<attempt>`; it is the idempotency key and, for writes with no lease, the only thing in dagger's event log that says mill did it. Nothing else the ledger holds is dagger's business: step attempts, session ids, tokens, per-step usage, workspace state.

## Retention

- `evidence`, `step_runs`, `events`: kept indefinitely. They are the audit record.
- `usage`: kept indefinitely; small.
- `step_tokens`: revoked rows kept 30 days for attribution, then deleted. Hashes only, so the risk is nil either way.
- `sessions`: kept while the run is not terminal, then 30 days. The transcript they point at is the harness's to retain.
- `workspaces`: rows kept; the directories are destroyed at `done`/`failed` and retained on `waiting`/`parked` until the run resumes or a human clears them.

## Queries the CLI needs

- `status`: runs by state with `current_step`, elapsed, cost, and gate node for waiting ones — one query over `runs` joined to the latest `step_runs` and a usage rollup.
- `inspect <run>`: every step run in order with its evidence, exit, session, and usage.
- `history <run>`: the `events` rows.
- `cost --since`: usage rolled up by run, by workflow, by step name, by model, priced by the rate table.
- `restart <node>`: release, abandon, destroy, create the next generation — one transaction on the store side, with the dagger release first.
- `explain <run>`: current step, its latest step run's status and exit, the evidence kind that would close it, the gate it waits on if any, the attempts used against `retries`.
- `logs <run> [--step]`: the per-step-run log files, redacted at write time; `--follow` tails the running one.
- `cancel <run>`, `rerun <run> --from <step>`: set `cancel_requested`; `rerun` also supersedes from the step once the worker has released.
- `workspace show|clean <run>`: the retained workspace's location and state; `clean` destroys one a resumed run no longer needs.
- `doctor`: no store query — probes the socket, migrations, dagger as the mill principal, the sidecar, the signing key, the forge credential.

## Open questions

- Whether `skills.files` bytes belong in this database or in dagger's blob store by digest. Leaning here: a workflow must be loadable without dagger.
- Whether the store should expose a `WatchRuns` stream for a future thicket bridge to render live status, or whether polling `status` is enough for a long time. Leaning polling.
