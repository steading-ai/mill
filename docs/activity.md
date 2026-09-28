# The activity record

**Status: draft for review.** Nothing here is built or filed.

What a run did, kept whole: every event of every session, every verb the workspace ran, every check, in the order it happened, per attempt, for as long as the deployment chooses. `docs/ledger.md` is what the mill decided; this is what happened while it decided. The two never overlap: the ledger holds outcomes and evidence, the record holds activity, and each cites the other by `step_run_id`.

Three readers are designed for, in this order. A web UI for skift and mill, the primary one, which shows a run as a tree of folds — attempt, turn, tool call, subagent — filling in live and opened again a month later; the record is shaped so that every fold is a kind or an index column and the UI never parses text to find a boundary. `mill logs`, the way an agent inspects a run and the way a person does when a browser is more than the question deserves; it stays a line renderer over the same stream and grows nothing the UI does better. And queries nobody has written yet, which is why bodies are kept and an index is kept beside them.

## What exists today

One text file per attempt, `<log-dir>/<run>/<step>.<attempt>.log`, written by `internal/run/logs.go` through the redactor as `harness.v1.Event`s arrive: the prompt whole at the head, then one line per event. Read by `mill logs` on the same host, and by nothing else.

Fidelity is lost at three points, in order:

1. The sidecar truncates every `ToolResult.content` to 4,096 characters (`sidecar/src/session.ts`, `TOOL_RESULT_LIMIT`) before it reaches the wire. The worker never sees the rest.
2. `eventLine` drops `ToolCall.arguments` entirely and flattens text and results to 240 characters.
3. The line is what is stored. The event's structure is gone at write time and cannot be recovered.

Measured on a real run (`mill#1`, 268 tool calls, three sessions and four subagents): the harness's own transcripts total 3.1 MB, of which 1.3 MB is content; the mill's log for the whole run is 192 KB, 6% of the transcript, and about 60% of what it drops is content rather than envelope. Across the 156 sessions on a development laptop the shares are the same: content is a third of the file, a duplicated `toolUseResult` field a fifth, repeated harness attachments a fifth, envelope a fifth. Tool results have a median of 465 bytes, a p90 of 5.8 KB and a maximum of 430 KB, because the harness already spills larger output to disk before it reaches its transcript.

Also today: the harness's transcripts under the session's `HOME` are the only full copy. On the `local` backend that is the operator's home and they persist by accident; on the `machine` backend they are destroyed with the workspace.

## Terms

| Term | Meaning |
|---|---|
| **Activity** | One thing that happened inside an attempt: the session said something, called a tool, a tool answered, the workspace ran a verb, a check ran. The unit of the record. |
| **Attempt** | A step run, as the ledger defines it. The unit the record is filed under, sealed by, and retained by, because it is the unit the ledger closes. |
| **Turn** | One request to the model and everything it produced until it stopped: text, thinking, tool calls, and the results that answered them. Numbered by the worker within the attempt. |
| **Frame** | One attempt's activity, whole, compressed. The durable form. |
| **Live file** | The running attempt's activity, uncompressed and append-only, until it is sealed. |
| **Index** | One row per activity, no bodies, in the ledger's database. The finding aid. |
| **Seal** | The transition from live file to frame and index rows, at attempt close. |
| **Twin** | The harness's own transcript for the attempt's session, archived beside the frame, opaque. |

## The activity

```proto
// Activity is one event inside an attempt. The worker assigns seq, turn
// and at as the event arrives; the body is the harness's, the
// workspace's or the mill's own, redacted before it is written.
message Activity {
  string step_run_id = 1;
  // 1-based within the attempt. The order of the record, the cursor a
  // reader resumes from, and the tie-breaker when runs interleave.
  int64 seq = 2;
  google.protobuf.Timestamp at = 3;
  // For a record that spans time — a coalesced text block, a check, a
  // verb — when it ended. Unset for an instant.
  google.protobuf.Timestamp ended_at = 4;
  // Who produced it: harness, workspace, check, mill.
  Source source = 5;
  // 1-based within the attempt: the model turn the record belongs to
  // (§Turns). 0 on a check step's records and the mill's own.
  int64 turn = 6;
  // Set when the event happened inside a subagent: the tool call in the
  // parent session that started it. Folds nest on this.
  string parent_call_id = 7;
  // The body's size before any cap, and whether the stored body is cut.
  // A cap is visible, never silent.
  int64 bytes = 8;
  bool truncated = 9;
  oneof body {
    Prompt prompt = 10;
    Text text = 11;
    Thinking thinking = 12;
    ToolCall tool_call = 13;
    ToolResult tool_result = 14;
    Usage usage = 15;
    Heartbeat heartbeat = 16;
    VerbStarted verb_started = 17;
    Output verb_output = 18;
    Verb verb = 19;
    CheckStarted check_started = 20;
    Output check_output = 21;
    Check check = 22;
    HarnessStderr harness_stderr = 23;
    GateFiled gate_filed = 24;
    AttemptStarted attempt_started = 25;
    AttemptEnded attempt_ended = 26;
  }
}
```

`harness.v1.Event` stays what the sidecar speaks. The worker wraps each one into an `Activity` at the point where `logSink` renders a line today; the mill's own kinds are written by the code that already knows the fact.

| Kind | Source | Body | Today |
|---|---|---|---|
| `prompt` | mill | the rendered prompt, whole | head of the log |
| `text` | harness | a text block or a delta of one: `block_id`, `text`, `delta`, `final` | one line per completed block, 240 chars |
| `thinking` | harness | as `text` | not carried by `harness.v1.Event` |
| `tool_call` | harness | `call_id`, `name`, `arguments` (Struct) | name and id |
| `tool_result` | harness | `call_id`, `content`, `is_error` | 240 chars of 4,096 |
| `usage` | harness | per-model counts, per turn | a line |
| `heartbeat` | harness | strike, strikes, waiting, tasks | a line |
| `verb_started` | workspace | `name` (`git.commit`, `git.push`, `forge.pr`, …), arguments, `verb_id` | absent |
| `verb_output` | workspace | `stream` (stdout, stderr), `data`: a chunk as the verb produced it, `verb_id` | absent |
| `verb` | workspace | `name`, result, error, `verb_id`, `ended_at` set | absent; the outcome is evidence in the ledger |
| `check_started` | check | the command and its contract | absent |
| `check_output` | check | as `verb_output` | absent |
| `check` | check | exit status, the reason when it failed, `ended_at` set | the whole thing, as text, at exit |
| `harness_stderr` | mill | the sidecar's stderr on refusal or crash, bounded as now | as text |
| `gate_filed` | mill | the gate's kind, node id and ref, title | the ledger's `gate_node_id` |
| `attempt_started` | mill | step, attempt, kind, session id and name, skill and its digest, harness, the step's harness options document | the ledger row |
| `attempt_ended` | mill | status, evidence kind, reason | the ledger row |

`attempt_started` is always `seq` 1 and `attempt_ended` always the last record, so a frame reads alone: a viewer that has the frame and nothing else can render the fold with its label (which skill, which session, which model and effort the options asked for), its boundaries and its outcome. `attempt_ended.reason` is written to read alone as the stream's last line: `verdict: revise, 2 blocking`, `exit status 1`, `gate denied`. The findings themselves stay on the ledger and the node.

A check and a verb have the shape a tool call has: a start, output as it is produced, an end. A `toolchain` step that installs for minutes and a suite that runs for twenty are visible while they run, in `mill logs -f` and on a live page, rather than a blank until the exit. The output chunks are kept in the frame as they were streamed; `check` and `verb` carry only what the chunks do not, the exit and the reason.

A `tool_call` whose result never arrived, because the attempt was killed mid-call, stays that way in the record. A viewer that has seen `attempt_ended` renders it as interrupted, not running; the index finds it.

### Turns

Every record inside a session carries the turn it belongs to, assigned by the worker. A turn opens at the first `text`, `thinking` or `tool_call` after a `prompt` or after the previous turn's `usage`; the `usage` closes it. That `usage` is not read off any synthesized `assistant` message — every one of those carries a non-final copy of the response's running total, which would double-count it — but from the stream's own `message_start` and `message_delta`, merged: `message_start` seeds the response's input and cache tokens, and `message_delta` overlays the output tokens it alone guarantees. A `usage` that arrives while a block it belongs to is still open (its final `text`/`thinking` record has not reached the writer yet) does not close the turn; the writer defers the close until that block's final record does, so records that stream out of order relative to their usage still land in one turn. The sidecar emits `usage` before the results that answer the message's tool calls arrive, so a `tool_result` does not take the turn that is open when it lands: it carries the turn of the `tool_call` it answers, by `call_id`, which the writer already pairs. A `verb` and its output carry the turn of the tool call that reached the workspace socket. A record with `parent_call_id` carries the turn of the call it nests under, whatever the subagent's own messages emit, so a subagent's `usage` never closes the parent's turn. A check step has no session and its records carry 0.

The index carries the column, so "what did turn 14 do" and a turn's cost need no bodies, and a viewer's fold for a turn is a filter and never an inference.

### Streaming text

The sidecar today emits one `text` per completed content block, because `includePartialMessages` is off. A live view is better with deltas, so the sidecar turns it on and forwards each `content_block_delta` as a `text` (or `thinking`) with `delta: true` and the block's id; the block's last delta carries `final: true`. The completed `assistant` message still arrives afterwards, but by then its `tool_call`s are the only thing still read from it: its `usage` is not, since it is a non-final copy of the same total the stream's `message_start`/`message_delta` pair already reported (§Turns).

The live file stores deltas as they arrive; the writer is not clever. At seal, deltas of one block are coalesced into one `text` record carrying the first delta's `at` and the last one's as `ended_at`, so the frame holds turns and the index has one row per block. Keeping the deltas instead is a seal-time option, for a viewer that wants to replay the typing; at 6× compression on the redundant form it is not a question of bytes.

The line `mill logs` renders is a consequence: without `-f`, one line per block as now; with `-f`, deltas are printed as they come, so the model is seen thinking. `journalctl` has no analogue and this is the useful choice.

### Subagents

The SDK forwards a subagent's `tool_use` and `tool_result` blocks by default, with `parent_tool_use_id` set to the `Agent` call that started it, and forwards its text and thinking too when `forwardSubagentText` is on. The sidecar today ignores `parent_tool_use_id`. It turns `forwardSubagentText` on and carries `parent_tool_use_id` onto the event, which becomes `parent_call_id`; a viewer folds the subagent's activity under the call, and a subagent's subagent under its own call, since call ids are unique. The SDK writes a subagent's transcript as a separate file, which the twin captures with the session's (see Seal).

### Redaction

Unchanged in guarantee, moved in mechanism. Today the redactor is a byte stream writer over rendered lines, holding back everything after the last newline so a registered value is never split across two writes and missed by both. Here it walks the strings of the body — `prompt`, `text`, `thinking`, `arguments`, `content`, verb arguments, results and output, check output, stderr, and the options document on `attempt_started` — before the record is serialized. A value a session registered with the `redact` verb is masked in every record written after it, as now, and nothing unredacted is ever written, as now: not to the frame, and not to the live file, which a followed stream has already shown before seal could fix anything.

The hold-back is what keeps that true for records that arrive in pieces, and it is the writer's, not the seal's:

- `check_output` and `verb_output` are chunked by line. A chunk is the complete lines a read produced; the trailing partial line is held until its newline arrives or the stream ends, and is flushed with `check` or `verb`. This is the redactor's rule applied to a new stream, and it decides the chunking. Verbs run concurrently, so `verb_output`'s hold-back is kept per `(verb_id, stream)`, not per stream alone — two verbs writing the same stream never share a trailing partial line. Checks run one at a time per attempt, so `check_output` stays keyed by stream.
- `text` and `thinking` deltas have no newlines to wait for. The writer holds back the last L − 1 bytes of each block's accumulated text, where L is the longest registered value, and releases them with the next delta or with `final`. A delta as written may therefore be a few bytes behind the delta as received, and never contains half a secret.

The redactor's tests move from lines to bodies, and gain one per path above: a value split across two reads, and across two deltas, that never reaches the file.

## Three tiers, one type

| Tier | Holds | Where | Written | Read by |
|---|---|---|---|---|
| **Live** | the running attempt | `<log-dir>/<run>/<step>.<attempt>.activity`: length-prefixed `Activity` records, append-only | as events arrive | `Logs` (tail), a live view |
| **Frame** | every closed attempt, whole | `<log-dir>/<run>/<step>.<attempt>.activity.zst`, one zstd frame, level 3; beside it `<step>.<attempt>.twin.tar.zst` with the session's and its subagents' harness transcripts | at seal | `Logs` for history, rehydration, a history view |
| **Index** | one row per activity, no bodies | `activity` in the ledger's database | at seal, one insert | `mill logs` with no run, `--since`, list views, any query that filters before it reads |

The index row:

| Column | Meaning |
|---|---|
| `step_run_id`, `seq` | Primary key. |
| `at`, `ended_at` | The record's time and, for a spanning record, its end. `at` is indexed with `step_run_id` for the interleaved read. |
| `kind`, `source` | As the record. |
| `turn` | As the record. |
| `call_id` | On `tool_call` and `tool_result`, so a call and its answer are paired without reading bodies, and a call that never got one is found. |
| `parent_call_id` | As the record, so a subagent's rows fold under its call. |
| `tool` | On `tool_call`, the tool's name; on the verb and check kinds, the verb's or the check's. |
| `bytes`, `truncated` | As the record. |
| `offset` | The record's position in the uncompressed frame, so one record is reached by decompressing to it and stopping. |

Bodies live in exactly one place, the frame. The index never holds one, and columns are added to it when a query proves it wants them (a hash of the body; the command a `Bash` call ran), which is cheap because the index is rebuilt from the frames.

The live file has no index rows. Live reads go to the file and are ordered by `seq`, which is enough for one attempt.

## Lifecycle

1. **Start.** `StartStepRun` succeeds; the writer opens the live file and writes `attempt_started` as `seq` 1.
2. **Append.** Each event is wrapped, redacted, appended, and the run's observer is signalled (§Observer).
3. **End.** `CloseStepRun`, `FailStepRun` or an abort writes `attempt_ended` as the last record and closes the file.
4. **Seal.** Immediately after, in this order: read the live file; coalesce deltas; write the frame beside it; index it; capture the twin from the session's `HOME` (the session's id is deterministic and in the ledger, `sessions.adapter_ref` says where the transcript is); verify the frame decompresses to what was read; remove the live file. A seal that fails partway leaves the live file, which is the signal to try again.
5. **Recovery.** On worker start, before the first claim, every live file whose attempt the ledger says is over is sealed. Same shape as the ledger's own resume: the file system holds what the process did not finish, the ledger says what it should have.
6. **Retention** is a policy over frames and twins, set by `retention` in `config.yaml` for the deployment and overridden per project by name in `retention-projects`. Both take the same grammar: `keep` (or empty, its equivalent), the default and at the volumes below the one most deployments will never change, or a comma-separated `twin_days=N`/`compact_days=N` pair.
   - Past `twin_days`, an attempt's twin is removed; its frame is untouched.
   - Past `compact_days`, an attempt's frame is **compacted**: rendered to the summary lines `mill logs` prints and stored as the frame's only content, with every row's `truncated` set. The compacted form is exactly the log that exists today, so the code that renders it already exists.

   Both ages are read off the attempt's `ended_at`. The sweep runs on worker start and on the sweep interval, the same two trigger points the ledger sweep runs at, and never touches the index: history stays queryable after bodies are gone, and a read that reaches a compacted body gets the summary and is told so. `mill doctor`'s `activity` line reports the log directory, the frames and twins held under it, their bytes, the oldest one's age, and the policy in force.

### Caps

One ceiling per attempt on raw activity bytes, default 64 MB, after which a `tool_result` body is stored as its summary line with `truncated` set, further `check_output` chunks of a stream and further `verb_output` chunks of a `(verb_id, stream)` are dropped behind one `truncated` marker record per stream (per verb, for `verb_output`), and everything else continues. A loop of large outputs is the only way to reach it. There is no per-result cap in the record: the harness bounds a single result at a few hundred KB before the sidecar sees it, and the sidecar's own 4,096-character cap is removed. A per-result cap exists only where a body reaches a terminal (`Logs` with `follow` cuts a body at 64 KiB with `truncated` set); the frame never cuts, and the whole record is one read away (§The RPC).

### Observer

Nothing polls. The store handles every write that changes what a reader waits on — `StartStepRun`, `CloseStepRun`, `FailStepRun`, `MarkWaiting`, `Park`, `Done`, `Fail`, `CreateRun` — and the activity writer is in the same process; each signals a per-run condition that `Logs` waits on, with a fallback timer so a missed wake cannot hang a stream. `CreateRun` is what lets a stream over every run see a new one start. Postgres `LISTEN/NOTIFY` is what a store and a worker in different processes would subscribe to later; the observer is the seam it would replace.

## Reading

### The RPC

One server-streaming call on `StoreService`, `PermissionRead`. This replaces the `Logs` contract in the held node `mill#7`: frames carry `Activity`, not rendered bytes, and a cursor resumes or pages a stream.

```proto
rpc Logs(LogsRequest) returns (stream LogsResponse);

message LogsRequest {
  // The run, as Inspect takes it. Both empty is every run.
  string id = 1;
  string node_ref = 2;
  // Narrow to one step, and one attempt of it (0 is every attempt).
  string step = 3;
  int64 attempt = 4;
  // Keep the stream open until the run ends, or, with no run, until the
  // client goes.
  bool follow = 5;
  // Send only the last N records on record before going live.
  int64 tail = 6;
  // Resume after this record; what a client that reconnected last saw.
  Cursor after = 7;
  // Page backwards: the `limit` records immediately before this one,
  // in seq order. Not with follow.
  Cursor before = 8;
  // At most this many records, then the stream ends. 0 is no limit. With
  // `after` and limit 1 it is one record, whole: how a body a followed
  // stream cut is fetched entire.
  int64 limit = 9;
  // With no run: records at or after this time.
  google.protobuf.Timestamp since = 10;
}

message Cursor {
  string step_run_id = 1;
  int64 seq = 2;
}

message LogsResponse {
  oneof frame {
    Activity activity = 1;
    // The run left the states a worker acts in; the last frame of a
    // followed stream over one run. Gate is set for waiting and parked.
    RunEnd run = 2;
  }
}

message RunEnd {
  string run_id = 1;
  RunStatus status = 2;
  GateRef gate = 3;
}

message GateRef {
  string node_id = 1;
  string node_ref = 2;
}
```

Semantics:

- One run: frames in `seq` order, attempts in start order, then the live file, then live. `attempt_started` and `attempt_ended` are records in the stream, so boundaries are in-band and a viewer closes a fold when it sees the end rather than inferring it.
- Every run (`id` and `node_ref` empty): rows from the index at or after `since` ordered by `(at, step_run_id, seq)`, bodies fetched from frames in that order, then live files, then live, including runs that start later. `tail` counts across runs, as `journalctl -n` counts across units.
- `follow` over one run ends with `RunEnd` when the run is `done`, `failed`, `abandoned`, `waiting` or `parked`. Over every run it does not end; run ends are `RunEnd` frames in the stream.
- `step` with `follow` ends when that step's latest attempt leaves running, with its `attempt_ended` and no `RunEnd`.
- A body over 64 KiB is cut in a followed stream with `truncated` set; unfollowed reads get bodies whole. A page that opened on `tail` scrolls up with `before` and `limit`, and expands a cut body with `after` one before it and `limit` 1.
- `after` skips everything at or before the cursor, on record and live. `before` and `limit` bound an unfollowed read and are refused with `follow`.

### `mill logs`

`journalctl`'s shape. The client renders lines from `Activity`; the lines are what the log file holds today.

| | Without `-f` | With `-f` |
|---|---|---|
| What | the whole run, or with no run every run since `--since` (default: runs live now), oldest first, each line prefixed with its run when more than one | the last 10 lines (`-n`; `--no-tail` for all), then live; text deltas printed raw |
| Where | `$PAGER` when stdout is a terminal; `--no-pager`; `-e` opens at the end; `LESS=FRX` when unset | stdout |
| Ends | at the last record, exit 0 | one run: 0 `done`; 1 `failed`/`abandoned`; 3 `waiting`; 4 `parked`; 2 the stream broke. Every run: on ctrl-C, or 2 |

`--step`, `--attempt`, and `--log-dir` removed: the server owns the path. `mill logs --json` emits `Activity` as JSON lines for a script. The MCP `logs` tool calls the same RPC with `tail`.

### A web UI

Consumes `Logs` from a Go service beside `mill serve` and `skift serve`, a client of both sockets with its own listener and its own idea of who is asking; the products grow no TCP listeners and no second identity. The fold hierarchy is run → attempt → turn → tool call (+ result, + subagent subtree) → thinking, and every level is an event kind or an index column, so the UI never parses text to find a boundary. A step page opens on `tail`, pages up with `before`, expands a cut body with `limit` 1, and resumes after a reconnect with `after`. The run ↔ node link is `runs.node_id`, so a node page's runs are a join; a gate's is `gate_filed` in the stream, so the fold links to it without one.

## Sizing

From the measurements above, a lossless deduplicated record costs about 5.5 KB per tool call and 3 KB per assistant turn, of which thinking is a quarter, and zstd at level 3 compresses it about 6×. So:

| | Raw | Frame |
|---|---|---|
| One run, 300 tool calls | 2 MB | 0.4 MB |
| A long `feature` run with review loops | 5 MB | 1 MB |
| 75 runs a week, a year | 20 GB | 3 GB |
| Doubled | 40 GB | 6 GB |

The index at ~150 bytes a row is ~50 KB a run, ~200 MB a year doubled. The twin is about the frame's size again. Bodies are half tool results, a quarter thinking, a tenth tool calls, a tenth prompts; a cap anywhere but tool results buys nothing, which is why the only cap is the attempt's. Streamed check and verb output adds what those commands print, which for a test suite is the size of a large tool result, and it is under the same ceiling.

## Changes, by component

| Component | Change |
|---|---|
| `proto/harness/v1` | `Text` gains `block_id`, `delta`, `final`; `Thinking` added; `Event` gains `parent_call_id`. `ToolResult.content` is no longer truncated by the sidecar. |
| `sidecar` | `includePartialMessages` and `forwardSubagentText` on; forwards deltas, thinking and `parent_tool_use_id`; `TOOL_RESULT_LIMIT` removed. |
| `proto/mill/v1` | `Activity` and its bodies; `Logs`, `LogsRequest`, `Cursor`, `LogsResponse`, `RunEnd`, `GateRef`. |
| `internal/run` | `logs.go` becomes the activity writer: live file, `seq` and `turn`, redaction over bodies, the attempt cap, `attempt_started`/`attempt_ended` with the attempt's label, `gate_filed`. Seal and recovery. |
| `internal/check`, `internal/workspace/verbs` | Output streamed to the writer as it is produced, with a start and an end record. |
| `internal/workspace/redact` | A body walker beside the stream writer. |
| `internal/store` | `activity` table and migration; the index insert at seal; the reads behind `Logs`, including `before` and `limit`. |
| `internal/server` | The `Logs` handler and the observer; `serve` gains `log-dir`. |
| `internal/cli` | `logs` on the stream, the pager, the exit table, `--json`, no run; the MCP tool. |
| `docs` | `tools.md` §logs; `architecture.md` §Observability; `ledger.md` cross-reference; this document becomes the record's specification. |

The `machine` backend (`docs/sandbox.md`) writes the same live file from the host side, because the sidecar's events reach the worker over the workspace socket regardless of backend; the twin's capture reaches into the machine's `HOME` before `Destroy`, which is the one backend-specific step.

## Deliberately absent

- **Bodies in the database.** Frames on disk are the record; the table is a finding aid. A remote worker whose store should hold its frames is a transfer at seal, not a change of model.
- **A per-result cap in the record.** One ceiling per attempt, visible.
- **Decoding the twin.** Kept whole and opaque. Everything the mill reasons about is `Activity`, which it wrote.
- **A second event bus.** The observer is a doorbell; the frames are the record.
- **A separate RPC per question.** One record, a page, a tail, a follow and a resume are all `Logs` with a cursor and a limit.
- **A trained zstd dictionary, or a format cleverer than one frame per attempt.** Ten to twenty percent on small frames, not worth the moving part.
- **A TCP listener or a browser identity in the products.** The web UI's service owns both.
- **Evidence that cites the frame.** A preparatory step's output is kept whole in the ledger's evidence (`docs/ledger.md` §evidence) and is streamed here as `check_output` too. The copy is accepted: evidence must read without the frame, because retention may compact the frame and the ledger is never compacted.

## Open questions

1. Seal-time coalescing of deltas: on by default, with the deltas kept as an option, or the reverse?
2. `thinking` in the twin only, or also in the frame? The spec says the frame; it is a quarter of the bytes and the one thing no other source has.
3. Whether `Logs` over every run should also carry ledger transitions that have no attempt (claimed, released, `waiting`), so a stream of everything is a stream of everything. Today they are `RunEnd` frames only.
4. The delta hold-back is L − 1 bytes where L is the longest registered value; a session that registers a very long value makes the live stream lag by that much. Bound L, or accept it?

## Rollout

Five nodes, filed in `mill`'s skift from `patches/mill/activity-*.md` in the stead repository. Each is independently shippable and leaves `mill logs` working.

1. **The record's shape.** `Activity`, the writer with `seq`, `turn` and boundaries, redaction over bodies, the live file replacing the text file, `mill logs` rendering lines from it locally. No RPC yet. The text log is gone at the end of this node and nothing visible changed.
2. **Logs over the socket.** `mill#7` as re-specified here: the RPC with its cursors and limit, the observer, `serve --log-dir`, the client and the MCP tool on the stream, the pager and the exit table.
3. **Seal.** Frames, the index, the twin, recovery, `--since`, and `mill logs` with no run. Done.
4. **Full fidelity from the harness and the workspace.** Deltas, thinking, subagent lineage, the sidecar's cap removed, streamed check and verb output, the attempt cap. This is the only node that touches `proto/harness/v1`.
5. **Retention.** The policy, compaction, and the `mill doctor` line that says what the record on this host holds.
