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
| `definition` | The definition as stored, JSONB. Steps, evidence specs, gates, capability sets, harness tables. A step's `harness` is a map from harness name to that harness's **options document** — the workflow's yaml block as JSON, stored as given and interpreted by nobody but the harness |
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
| `kind` | `skill`, `check`, `gate`, `preparatory`, copied at start so the row reads alone |
| `session_id` | Skill steps: the session this attempt ran in |
| `lease_id` | The dagger lease held when this attempt ran |
| `worker_ref` | |
| `status` | `running`, `closed`, `failed`, `aborted`, `superseded` |
| `session_started` | Skill steps: the harness produced at least one event in this attempt's session. What says the next attempt at that session resumes it rather than starts it |
| `exit` | JSONB `ProcessExit{status, signal, reason}` for skill and check steps. `status = -1` means never ran |
| `feedback_from` | The `step_runs.id` whose failure this attempt is the `revise` for, or null |
| `auto_approved` | Gate steps: true when the `auto` predicate closed it without a human |
| `env_passthrough` | Skill steps: the variables the step asked to have carried from the mill's environment into its session's process, as the adapter applied them |
| `env_stripped` | Skill steps: the deny-listed variables that were present in the mill's environment and removed. The deny list is not configurable, so this is a record and not a setting |
| `base_commit` | The head of the run's branch when this attempt started, or empty when the run had no checkout to read one from |
| `started_at`, `ended_at` | |

`base_commit` is what makes `commit` evidence a fact about the step rather than about the branch: a commit step closes only on a commit that descends from its attempt's base and is not it, made by the mill (docs/domain-model.md §Evidence). Every kind records it, because what a superseded or interrupted span produced is the range from its base to the branch head, which is what a rerun reads. An attempt that continues an *aborted* one inherits that attempt's base rather than taking the current head: a worker killed between the session's commit and the write that closes the step left its work on the branch, and the next attempt closes on it without a second session. A failed or superseded attempt is not continued, so it carries nothing forward.

The one case a `commit` step closes on a head equal to *its own* `base_commit` is a `revise` that changed nothing: the branch already carried the commit an earlier attempt made, and nothing after it moved. What decides whether that unmoved head is still the step's work, rather than a session that simply made no commit, is not this attempt's own base but the step's *earliest* `base_commit` in this generation — the head the step was first handed, before any `revise` — read from this step's earlier, superseded rows. The head closes when the tree is clean and it descends from, and is not, that earliest base; a dirty tree, or a head that has not moved even from there, refuses in its own words (docs/domain-model.md §Evidence). `mill rerun` reads the same rows and applies the same rule.

`session_started` is written when the session's first event arrives, not when the turn ends. Which of `Start` and `Resume` the harness is sent cannot be derived from the attempt number: an attempt that ended before the harness said anything left no conversation behind, and resuming one fails for a reason that has nothing to do with the work — while an attempt killed mid-turn did leave one, and starting a second under the same id loses it. The log directory and the harness's own transcript files are not the record either (Invariant 10); this column is.

`closed` means evidence exists for this row. `failed` means the attempt ended and `on_failure` applies. `aborted` means the worker stopped or was fenced out before the attempt ended; the next worker starts a new attempt. `superseded` means a later `revise` reopened this step; the row and its evidence stay as the record of pass one.

The circuit breaker counts `attempt` on the **failing** step — `review` fails, and `review.retries` is what it compares against, spent on `review`'s own attempts before `revise implement` is consulted at all. This is a different counter from the run's derived `leases`, which counts dagger claims and moves on worker crashes, not on review rounds.

### `evidence`

Immutable. One row per closed step run.

| Column | Notes |
|---|---|
| `step_run_id` | Unique. A step run has at most one closing evidence |
| `kind` | `commit`, `exit`, `verdict`, `structured`, `url`, `status`, `artifact`, `gate` |
| `value` | JSONB. The fact, in the shape the kind defines. A preparatory step's `exit` value carries `output` beside the process exit: what the command wrote, whole, because the record of the step's run is the fact it closes on (`docs/sandbox.md` §"Preparatory steps") |
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

`adapter_ref` is where a live session's transcript is read from; once its attempt is sealed, the same transcript lives on as the frame's twin (`docs/activity.md` §"The twin", §Seal), independent of this row.

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
| `source` | Who counted: `sdk`, the adapter's report of what the harness billed, or `gateway`, the credential gateway's count of the upstream's `usage` block on a response it forwarded |
| `recorded_at` | |
| `posted_at` | When this row's step totals were posted to dagger; null until then |

One row per turn, per model, as the adapter reports — a turn that bills two models writes two rows — and, where the credential gateway sits in front of the session, one more per response it forwarded, with source `gateway` (`docs/sandbox.md` §Credentials). The adapter's rows are the step's cost: they are what `status` and `inspect` sum and what is posted to dagger. The gateway's are the count taken outside the session, carried beside them as the attempt's `gateway_totals` and listed by `cost` under their own source; they are never posted. Cost is a rate table applied at query time. The step's totals are posted to dagger through `ReportUsage` as one record per `(lease, model)` **when the step run closes or fails**, and the rows are marked `posted_at`. Dagger sums records per attempt and each report renews the lease, so per-step posting is both the contract's intended shape and a heartbeat. A worker that dies mid-step loses only that step's usage from dagger's view; the ledger keeps it. There is no reconciliation job: dagger's per-lease total for an abandoned attempt is allowed to be short by one step, and the ledger is the complete record.

`cost`'s per-step funnel — `reached`, `passed`, `died`, `retried`, `revises`, `attempts` — is not derived from `usage` at all: a check or gate step never bills a model and so never has a `usage` row, and would otherwise be invisible to it. It is folded from `step_runs` in the same window instead, windowed on `started_at` rather than `recorded_at`. `reached` counts a run once it has opened at least one attempt of the step; `passed` once one of those attempts closed; `retried` once a run needed more than one; `attempts` is every attempt, closed or not. `died` counts a run against the step its *last-started* step run belongs to, when the run itself ended `failed` or `abandoned` — not the step whose last attempt failed, since an abandoned run's last attempt is aborted, not failed, and it died there regardless. `revises` counts against the step named by another step run's `feedback_from`: the verdict that sent the work back, not the step the work went back to.

### `workspaces`

| Column | Notes |
|---|---|
| `id` | ULID |
| `run_id` | Unique while live |
| `kind` | `local`, `machine`, `container`, `remote` |
| `worker_ref` | Where it is |
| `ref` | The git ref it was created at |
| `state` | `live`, `retained`, `destroyed` |
| `created_at`, `destroyed_at` | |
| `sandbox` | The machine behind a `machine` workspace, on the wire as one `Sandbox` message and in the table as the columns below; every one empty for a workspace with no machine |

The sandbox's columns, which `docs/sandbox.md` §Reconciliation names:

| Sandbox column | Holds |
|---|---|
| `machine` | The machine's name, `mill-<id>`: what reconciliation matches `machinectl list` against |
| `unit` | The host unit that owns it, `mill-machine@<id>.service` |
| `image_digest` | The derived image the machine booted |
| `veth` | The host side of the machine's link, as the host named it — truncated from `ve-mill-<id>`, so recorded rather than derived |
| `lineages` | `[{scope, name, restored, promoted?}]`: each cache lineage the run restored a copy of, the generation the copy came from (0 for one that started empty), and the generation the run promoted at its end — 0 for a reset, absent until the run has finished and for a copy that was discarded. Null for a workspace with no machine |

The row is written twice by the worker: `RecordWorkspace` once the workspace is open — at creation, and again on a resume that reattaches it, which brings the same row up to date rather than adding one — and `FinishWorkspace` after the run is done and the workspace destroyed, which marks it `destroyed` and writes each lineage's promoted generation. The second write is the one the resume algorithm makes after `store.Done`, when the run holds no lease; the store admits it for a run that has ended (`FinishWorkspaceRequest` says so).

**Which record is authoritative.** The machine backend keeps a copy of the same facts on the host, `runs/<id>/run.json` (`docs/sandbox.md` §Provision), because `Attach` has to find the machine before the ledger is asked anything and a worker that lost its ledger still has to be able to tell an orphan from a machine it made. The ledger is the record: `mill inspect`, `mill status`, reconciliation and the end-to-end proof read from it, and when a reconciliation pass finds the two naming different machines for one run it records a `disagreement` finding naming both and acts on the ledger's. `run.json` is for the host — what it reads before the ledger answers, and what it has when the ledger is gone — and it carries what the ledger does not need: the base the image derived from, whether it fell back, the provisioning findings, when the machine reported ready, and the step slice in flight.

A `machine` workspace's twin capture (`docs/activity.md` §Seal) reaches into the machine's `HOME` before `Destroy` tears it down — the one step of sealing that is backend-specific, and the reason a seal on this backend must run ahead of the workspace closing rather than after.

### `reconciliations`

One row per pass a worker ran: the host's, at start before its first claim (`docs/sandbox.md` §Reconciliation), and the sweep's, at start and between claims (§Sweep). The worker, when the pass began, and its findings as `[{kind, name, action, detail}]` — orphans stopped, lost sandboxes noted, slices reaped for resume, leftovers removed, scratch projects swept, stranded runs failed or caught up, failures. A record of the host and of the ledger against dagger rather than of a run: an orphan machine has no live run to hang an event on, so it is its own table and no lease fences it. Append-only.

### `events`

Append-only. Every change to `runs.status`, every step run start and close, every token issue and revoke, every gate filed and every auto-approval, every attempt to carry a filed gate to a person (`notified`: the kind, the gate, the notify command's exit status and its diagnostic), and every package a session installed into its machine with the `dnf` shim (`installed`: the packages and the line "installed `<packages>`, not declared in `mill.toml`", against the attempt whose session made it — the run's provenance of an undeclared install, `docs/sandbox.md` §"The dnf shim"). Fields: `id` (ULID), `run_id`, `step_run_id` (nullable), `op`, `before`, `after`, `worker_ref`, `occurred_at`. This is the audit trail and what `mill status --history` reads. `WatchRuns` reads it too, as a change stream from a cursor of `(occurred_at, id)` (`events_occurred_at_id_idx`) — the stream `mill status --follow` follows (`docs/tools.md` §status).

Not time-partitioned, unlike dagger's events. Dagger's are partitioned from the start because they are a change stream many consumers resume from at volume; this table is a change stream now too, but for one operator running tens of runs a day, not at a volume the machinery earns yet. Partitioning is a migration away if that changes.

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
9. **Every store write is fenced by the lease, atomically.** Every statement that mutates `step_runs`, `evidence`, `step_tokens`, `usage`, or `runs` for a run takes the `runs` row lock (`SELECT … FOR UPDATE`), re-reads `lease_id` in a separate statement after the lock is granted — a locked row is not re-read, so a lease a concurrent claim committed while waiting would otherwise be missed, which is exactly the trap dagger's `fence()` comments on — compares it to the lease the worker presented, and rolls back on mismatch. This is the mechanism behind "a stale worker cannot corrupt the record," and it lives in the store, not in the worker's good behaviour. The one exception is `SetLease` itself, which is the write that changes what later writes compare against; it takes the same lock and requires the run's current status to be one a new claim may take over (`selecting`, `running` with an expired lease, `waiting`, `parked`, or `done` with the node still open). A terminal write that presents no lease — `Abandon` after a restart's release, and the sweep's `Fail` or `Done` on a run whose node dagger has closed (§Sweep) — takes the same lock, compares nothing, and nulls `lease_id` as it ends the run, so whatever still believed it held the run is fenced by its next write; it is admitted because dagger's own fence has already proved no live lease exists. `selecting` is among them for the same reason `running` is: a run created holding its claim's lease, whose worker then died before it chose a workflow, is otherwise a node no mill command can recover — and dagger, which grants a second lease only once the first is gone, has already decided who holds the node.
10. **`current_step` is derived.** There is no column to drift.
11. **Dagger first, store second.** A run is marked `done` only after dagger has confirmed the completing patch; `gate_node_id` is recorded only after dagger has returned the gate's id and before the lease is released. The store never asserts a fact about dagger that dagger has not yet made true.
12. **A gate is filed once per step run.** `gate_node_id` set and the current step a gate step means resume reads the gate; it never files another. Invariant 11's order leaves one window this cannot close on its own: the patch that files the gate lands and the process dies before the store write, so dagger has a gate the ledger knows nothing about. A worker therefore asks dagger before it files — the node's user-assigned children carrying the run's and the step's labels — and records what it finds instead of filing a second one. That is the only place in the loop where dagger's graph, rather than the ledger, decides what to write, and it is the only place it can be: the gate exists in exactly one of them.

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
  run.status in (running, selecting):
      store.AbortOpenStepRuns(run)                 # a 'running' row here belongs to a dead or fenced-out worker
      # not when waiting: that open row is the gate step's own attempt, left open so the answer closes it
      # selecting: the worker died before the workflow was chosen. Nothing else is special —
      #   the lease is taken over below and the selection is made again: a project default
      #   resolves to the same version, and an assessment is asked again as a second attempt
      #   at the `assess` step. A selecting run never refuses a claim
store.SetLease(run, node.lease_id, self)          # from here every write is fenced by this lease

go heartbeat(node.lease_id) until run leaves this worker   # renews well inside the TTL; a fenced heartbeat, or cancel_requested, cancels the step
#   cancel_requested: abort the step run (reason cancelled), Release(PREEMPTED, 'cancel'), return — the run stays resumable

ws := workspace.Attach(run) or workspace.Create(run)   # local? reattach. elsewhere? clone run.branch (pushed, by the loader's rule)
store.RecordWorkspace(run, ws.handle)             # kind, worker, ref; and the machine, unit, digest, veth and restored lineages on the machine backend
#   a reattached workspace brings its live row up to date; nothing adds a second

if run.workflow_id == nil:                         # docs/domain-model.md §Selection
    if project default: w, tier := that                    # a person pinned this project to one workflow
    else:               w, tier := assess(run, ws)         # build-workflow's `assess` step, as step 0 of the run
    #   the assessment is an ordinary skill step — its own session, token, evidence and usage —
    #   and its retries are spent here, before there is a workflow for the breaker to park in
    sr := store.StartStepRun(run, 'select')
    if w not in scope: store.Fail(sr); return              # an assessment cannot invent a workflow
    store.Close(sr, evidence{kind: structured, value: {workflow, version, tier, basis, ...}}, workflow=w, tier=tier)

if run was parked:                                 # the breaker stopped it and a human was asked
    if gate(run.gate_node_id).status != done: dagger.Release(node.lease_id, GATE); return
    store.Resume(run)                              # gate cleared; attempts at the parked step start again

for step in run.workflow.steps starting at run.current_step():

    if step.kind == gate and run.gate_node_id != nil:          # resume through a gate
        g := gate(run.gate_node_id)                # by label among the node's children: dagger resolves refs, not ids
        sr := store.LatestStepRun(run, step)       # the attempt that filed it; there is no second one
        if g.status == done:     store.Close(sr, evidence{kind: gate, value: g.result}); run.gate_node_id = nil; continue
        if g.status == open:     dagger.Release(node.lease_id, GATE); return   # nobody has answered yet
        else:                    dagger.Complete(node, deny_result); store.Fail(run, denied); return  # dropped: a denial

    sr := store.StartStepRun(run, step)            # attempt = max+1 for (run, step)
    tok := store.IssueToken(sr, step.capabilities)
    ws.Rotate(tok)
    result := execute(step, sr, ws)                # by kind; skill steps speak to the session with the UUIDv5 id
    #   Resume when some attempt of that session id has session_started, else Start.
    #   The harness sets it on the session's first event, so an attempt that died before the
    #   harness spoke is started and one killed mid-turn is resumed
    store.RevokeToken(tok)
    dagger.ReportUsage(node.lease_id, usage(sr))   # one record per model; renews the lease

    switch:
      evidence verified:
          store.Close(sr, evidence)                # one transaction; sr -> closed
      gate to file:                                # step.kind == gate, auto did not hold
          gate := dagger.Gates(node, labels(run, step))                      # 0. a gate a crash left behind
          gate ?= dagger.ApplyPatch(create gate + blocks edge, request_id)   # 1. dagger
          store.MarkWaiting(run, gate.id)          # 2. store, before the release
          dagger.Release(node.lease_id, GATE)      # 3. dagger
          return                                   # the attempt stays open: the answer closes it
      failed:
          store.Fail(sr, exit)
          attempts = store.Attempt(run, step)                 # since the last attempt a human paid for
          if result.verdict == revise:                        # a judgment, not the step's own failure
              if step.on_failure == revise <target> and attempts <= step.retries: supersede; continue
              else: park
          if attempts <= step.retries:                        # the step's own retries come first
              feedback = result.feedback; continue            # same step, same session, next prompt
          if step.on_failure == revise <target> and attempts == step.retries + 1:
              store.Supersede(run, from=target, through=step)   # their latest rows -> superseded
              feedback = result.feedback; continue            # loop recomputes current_step() == target
          else:
              dagger.Comment(node, last findings)
              gate := dagger.ApplyPatch(create park gate, request_id)   # body: every attempt in the span
              store.Park(run, gate.id, reason)
              dagger.Release(node.lease_id, BUDGET)
              return

# terminal step closed
run.complete_request_id = store.SetCompleteRequestId(run)      # idempotency key, durable before use
dagger.ApplyPatch(complete_node(result, artifacts), lease, run.complete_request_id)   # releases the lease as part of the patch
store.Done(run)                                    # only now
ws.Destroy()                                       # stops the machine and promotes the last evidence-closed step's candidate
store.FinishWorkspace(run, ws.promoted())          # the row destroyed, and each lineage's promoted generation; admitted after Done because the run has ended
```

Three properties fall out:

- **Steps are re-run, sessions are resumed.** An interrupted step starts a new attempt from the top; the session it runs in is the same one, found by its deterministic id, so the model picks up its own prior context. A step is never resumed mid-flight — there is no mid-flight state to resume into. Whether the session is resumed or started is `session_started`'s answer and not the attempt number's: the id is deterministic, so a session that was never begun has an id and no conversation. On the `machine` backend the transcript `sessions.adapter_ref` names lives in the machine's ephemeral `~/.claude`, so a session resumes while its machine lives and a machine that is gone is a lost sandbox, not a resumable session (`docs/sandbox.md` §Reconciliation); the transcript is not exported.
- **Resume is the same code as a first run.** There is no separate recovery path except the gate branch at the top of the loop. `current_step()` is derived from step-run status, and a fresh run's `current_step()` is simply the first step.
- **A stale worker cannot corrupt the record.** Its dagger writes are fenced out by dagger; its store writes are rejected by Invariant 9. Its heartbeat fails first, which is how it learns.
- **`revise` actually revises.** Superseding every step from the target through the failure means the revised work is re-simplified, re-committed, re-pushed, and re-reviewed; the ledger never claims pass-one evidence closed pass-two work.

`Complete` is not an RPC. It is `ApplyPatch` with a `complete_node` operation, and dagger releases the lease inside that same patch — which is why `ReportUsage` for the final step must precede it, why there is no separate `Release` on the done path, and why `store.Done` comes last.

## Sweep

Every way a run ends in §Resume happens inside a claim. A node dagger has closed — dropped, or completed by someone other than the run — never enters the ready set again, so no claim ever comes to end the run it strands: the run stays live in the ledger, its workspace retained, its branch on the forge, for as long as the ledger exists. The host's reconciliation (`docs/sandbox.md` §Reconciliation) cannot help; it compares machines with live runs and never asks dagger what became of a run's node. The sweep is that missing axis: the ledger reconciled against dagger.

The worker runs it before its first claim, ahead of the host's pass, and again at the top of its claim loop whenever `sweep-interval` has elapsed — a busy worker between runs, an idle one every `wait` — with no timer and no goroutine of its own. A second process sweeping the same ledger would race the live worker, which is why it is the worker's and not a unit's.

```
for run in store.Status(live):                     # selecting, running, waiting, parked
    if run.worker_ref != self: continue            # cleanup is host-local; another host's run is its own
    if driving(run.node_id): continue              # this process is on it; its own goroutine writes its ending
    if run.updated_at > now - TTL: continue        # another process on this host may be mid-write; by age, never by "not in the ledger" (mill#49)
    node := dagger.Node(run.node_ref)              # the reference is stored qualified; any project's client reads it
    error?             -> finding{failure}; continue
    node.status open   -> continue                 # whatever its readiness: a parked run's node is blocked by its own gate
    store.AbortOpenStepRuns(run)                   # no token outlives the run
    switch:
      dropped:                                   store.Fail(run, "node dropped underneath the run")
      done and run.complete_request_id != nil:   store.Done(run)          # Invariant 11's crash window: the patch landed, the store missed it
      done otherwise:                            store.Fail(run, "node completed outside the run")
    ws.Destroy(); store.FinishWorkspace(run)       # through the attach-only seam: nothing is made in order to be destroyed
    if failed and run.workflow_id != nil:          # a selecting run pushed nothing; a run that delivered has a merged PR to leave alone
        forge.ClosePR(comment); forge.DeleteBranch()
    finding{stranded, run, failed|done, node status and the ledger's step}
store.RecordReconciliation(findings)
```

Two things make this safe to write under no lease. Dagger refuses `drop_node` and a user's `complete_node` on a node with a live lease, so a run whose node is closed is provably unleased there: nothing is driving it. And the store's terminal write with no lease presented takes the row lock alone and nulls `lease_id` as it ends the run (Invariant 9's leaseless door, the one Abandon uses), so a worker that still believed it held the run is fenced on its next write. No dagger write is made: a closed node is outside every ready set, nothing is held, and the ledger row with its `failed` event and the `reconciliations` row are the record. `mill run` on a closed node resolves its run the same way, through the same code, so the two cannot disagree; `mill restart` refuses a node that is not open, because a restart of one would open a run nothing can claim.

What the sweep does not reach, and says so: a run whose `worker_ref` names a host that no longer exists stays until a person acts, and a `waiting` run whose node a person completed leaves its gate open in dagger, because the sweep writes nothing there.

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

- `evidence`, `step_runs`, `events`, `reconciliations`: kept indefinitely. They are the audit record.
- `usage`: kept indefinitely; small.
- `step_tokens`: revoked rows kept 30 days for attribution, then deleted. Hashes only, so the risk is nil either way.
- `sessions`: kept while the run is not terminal, then 30 days. The transcript they point at is the harness's to retain.
- `workspaces`: rows kept; the directories are destroyed at `done`/`failed` and retained on `waiting`/`parked` until the run resumes or a human clears them.

## Queries the CLI needs

- `status`: runs by state with `current_step`, elapsed, cost, and gate node for waiting ones — one query over `runs` joined to the latest `step_runs` and a usage rollup. The same query, unfiltered, is what the sweep reads, so a run `status` lists as live is one the sweep will ask dagger about.
- `inspect <run>`: every step run in order with its evidence, exit, session, and usage.
- `history <run>`: the `events` rows.
- `cost --since`: usage rolled up by run, by workflow, by step name (keyed by workflow version too, since a step's shape can change between them), by model, and — `--bucket day` — by UTC calendar day, priced by the rate table. The by-step rows also carry the funnel: how many runs reached the step, passed it, died there, needed more than one attempt, and how many revise rounds it answered.
- `restart <node>`: release, abandon, destroy, create the next generation — one transaction on the store side, with the dagger release first. A lease dagger says is not live is skipped rather than fatal: the run a human restarts is usually the one whose worker died holding it, and there is nothing to take back.
- `explain <run>`: current step, its latest step run's status and exit, the evidence kind that would close it, the gate it waits on if any, the attempts used against `retries`.
- `logs <run> [--step]`: the per-step-run log files, redacted at write time; `--follow` tails the running one.
- `cancel <run>`, `rerun <run> --from <step>`: set `cancel_requested`; `rerun` also supersedes from the step once the worker has released. A run that ended `failed` is reopened by the same transaction — status back to `running`, no lease, `ended_at`, gate and park reason cleared — because the supersede is worth nothing on a run Invariant 9 admits no claim on. `abandoned` is refused: Invariant 1 allows one live run per node.
- `workspace show|clean <run>`: the retained workspace's location and state; `clean` destroys one a resumed run no longer needs.
- `doctor`: no store query — probes the socket, migrations, dagger as the mill principal, the sidecar, the signing key, the forge credential.

## Open questions

- Whether `skills.files` bytes belong in this database or in dagger's blob store by digest. Leaning here: a workflow must be loadable without dagger.
