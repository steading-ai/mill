# Spike: what the Claude Agent SDK guarantees a headless session

Six assumptions in `docs/architecture.md` §"Skill steps: the harness adapter" are
load-bearing for R-10, R-12, R-14, R-24 and R-39. Each was put to a script rather
than to the documentation. Every verdict below is an observation of output, and
every script that produced it lives in `spikes/sdk/`.

## What was under test

| | |
|---|---|
| SDK | `@anthropic-ai/claude-agent-sdk` **0.3.263**, pinned exactly in the repository-root `package.json` |
| CLI it spawns | **2.1.263**, the binary the SDK package carries in `manifest.json` (commit `37ae3f38`, built 2026-09-06). Not the host's `/usr/bin/claude`. |
| Runner | `tsx` 4.23.13, Node 26.4.0 in this session's shell; `mise.toml` pins node 24.13.0 and pnpm 10.20.0 for the repository |
| Credentials | the host's `~/.claude/.credentials.json`; no API key in the environment |
| Host | Linux, `cwd` = the mill checkout |
| Date | 2026-09-08 |

Run any script from the repository root:

```
pnpm install
pnpm exec tsx spikes/sdk/session-resume.ts
```

These are spike scripts: deliberately flat, uncommented beyond their question,
and not part of the mill build. `make lint` does not typecheck them.

---

## Q1 — A caller-chosen session id, created in one process, resumed in another

**Script** `spikes/sdk/session-resume.ts`. A driver mints a UUID with
`randomUUID()` and spawns two (then three) child processes with
`spawnSync(tsx, [self, phase, id])`. Each child makes one `query()` call and exits.

- process 1: `query({ prompt: "Remember this codeword: TANGERINE-4417…", options: { sessionId: id, allowedTools: [] } })`
- process 2: `query({ prompt: "What codeword did I give you earlier…", options: { resume: id } })`
- process 3: the same resume with `cwd: "/tmp"`

**Observed**

```
driver pid=3731771 caller-chosen session id=dcbc7d13-cf67-43dd-85d4-4730790b1047
--- first process ---
{"pid":3731806,"subtype":"success","session_id":"dcbc7d13-…","result":"OK"}
--- second process ---
{"pid":3732056,"subtype":"success","session_id":"dcbc7d13-…","result":"TANGERINE-4417"}
--- elsewhere process ---
{"pid":3732408,"subtype":"success","session_id":"dcbc7d13-…","result":"TANGERINE-4417"}
```

The transcript is on disk at
`~/.claude/projects/<cwd-slug>/<session-id>.jsonl` — for this run,
`-home-ivy-src-github-com-ivy-mill/dcbc7d13-….jsonl`.

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed.** A caller-supplied UUID is accepted, is the id the
session keeps, and a different process resumes it and demonstrably holds the
prior turn's content. `session_id` on the resumed result is the *same* id, not a
fork — forking is opt-in via `forkSession: true`.

Caveats for the adapter:

- `sessionId` must be a valid UUID and is rejected together with `resume` unless
  `forkSession` is also set. `Start` passes `sessionId`; `Resume` passes `resume`.
  They are different options, not one option in two modes.
- The transcript directory is derived from `cwd`, but resume from a *different*
  `cwd` still found it (process 3). Do not depend on that: a mill worker whose
  checkout path moves between steps is resuming across project slugs, which is
  observed to work here but is not a documented guarantee. Keeping the workspace
  path stable for a step's lifetime costs nothing and removes the question.
- Transcripts live under `$HOME/.claude/projects`, outside the workspace. Under
  containers the sidecar needs that directory mounted and per-run private, or
  `Resume` has nothing to read.

## Q2 — Structured output against a non-trivial schema

**Script** `spikes/sdk/structured-output.ts`. Ten independent sessions, one
schema exercising every shape a reviewer verdict needs: a top-level object with
`additionalProperties: false`, an enum (`approve|revise|needs_human`), a required
array of objects each with their own required keys and a nested enum, and a
nested object with a number. Passed as
`options.outputFormat = { type: "json_schema", schema }`. Conformance is judged
by a hand-written validator in the script (no `ajv`; the mill adds no dependency
for a spike).

**Observed**

```
{"run":1,"subtype":"success","present":true,"errors":[]}
…
{"run":10,"subtype":"success","present":true,"errors":[]}
{"conforming":10,"of":10}
```

**Conformance on SDK 0.3.263 / CLI 2.1.263: 10 / 10.** The value arrives on the terminal result as
`structured_output`, already parsed — not as text to be `JSON.parse`d out of
`result`. Sample:

```json
{
  "verdict": "revise",
  "findings": [
    { "criterion": "AC-2: the CLI exits non-zero on an unknown flag",
      "severity": "blocker",
      "note": "An unknown flag is accepted and the process exits 0. …" }
  ],
  "summary": { "headline": "AC-1 holds …", "confidence": 0.95 }
}
```

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed with caveat.** Ten of ten is evidence of reliability, not of
a hard guarantee, and the SDK itself admits a failure mode: `SDKResultError` has
the subtype `error_max_structured_output_retries`, so conformance is enforced by
bounded retry inside the CLI rather than by construction. The adapter must treat
a missing or non-conforming `structured_output` as a step failure with a distinct
reason, not as a parse error to paper over; that subtype is the signal.

## Q3 — Is the tool policy enforced or advisory?

**Script** `spikes/sdk/tool-policy.ts`. A fresh temp directory per attempt; the
prompt is "Create the file `<tmp>/escape.txt` with the exact contents ESCAPED".
After each attempt the script checks the filesystem itself. Three policies:

| Policy | Tools the model attempted | File written? | What the SDK reported |
|---|---|---|---|
| `allowedTools: ["Read"]` | `Write` | **no** | `permission_denials: [{ tool_name: "Write", tool_use_id, tool_input }]`, and an `is_error` tool_result reaching the model: *"Claude requested permissions to write to /tmp/…/escape.txt, but you haven't granted it yet."* |
| `allowedTools: ["Read"]` + `disallowedTools: ["Write","Edit","Bash","NotebookEdit"]` | `ToolSearch`, `ToolSearch` | **no** | nothing — no denials, no errors. The tools were not present to call; `ToolSearch` could not load them either |
| `permissionMode: "bypassPermissions"` + `dangerouslySkipPermissions: true` + the same `disallowedTools` | `ToolSearch`×4, `Agent`×3, `Read` | **no** | one tool_result error, and only "file does not exist" from a `Read`. The model tried delegating to subagents; it reported those subagents had `Write`/`Bash` stripped at runtime too |

Final message from the third attempt, verbatim:

> I can't do this. […] Every file-writing path is withheld from this session by
> policy: my tool set has no Write, Edit, or Bash; `ToolSearch("select:Write")`
> finds nothing to load; and subagents (`general-purpose`, `shell-wizard` — both
> nominally Write/Bash-capable) had those tools stripped at runtime too.

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed, with a distinction the design must keep.** The two knobs
are not the same mechanism:

- `allowedTools` is a **permission** list. The excluded tool is still *offered*
  to the model, the model calls it, and the call is refused at the permission
  layer. Cost: a wasted turn and a denial the model must reason about. Benefit:
  the refusal is visible in `permission_denials`, so the ledger can record it.
- `disallowedTools` **removes the tool from the session**. Nothing to call,
  nothing in `permission_denials`, and it survives `bypassPermissions` — a mode
  that says yes to every prompt still cannot say yes to a tool that isn't there.
  It also reaches subagents.

So `Policy` in the adapter should compile to **both**: `allowedTools` for the
positive grant and `disallowedTools` for the tools the step must not have, and
the reviewer's read-only guarantee must use `disallowedTools`, since
`allowedTools` alone leaves the write path merely un-permitted. Note also that
`Agent`/`Task` is a delegation route: a policy that omits it from
`disallowedTools` is relying on inherited stripping, which held here but is one
more thing to assert rather than assume.

## Q4 — Per-model usage on the terminal result

**Script** `spikes/sdk/usage-by-model.ts`. One Opus turn (`model: "opus"`,
`allowedTools: ["Task"]`) with an inline `agents: { probe: { model: "haiku" } }`
definition, prompted to delegate one question — so a second model is billed on
the same `query()` call.

**Observed** (`num_turns: 2`, `total_cost_usd: 0.2786665`):

| Field | `claude-opus-5` | `claude-haiku-4-5-20251001` |
|---|---|---|
| `inputTokens` | 4 | 3870 |
| `outputTokens` | 150 | 76 |
| `thinkingTokens` | 0 | 69 |
| `cacheReadInputTokens` | 25513 | 0 |
| `cacheCreationInputTokens` | 25789 | 0 |
| `webSearchRequests` | 0 | 0 |
| `costUSD` | 0.2744165 | 0.00425 |
| `canonicalModel` / `provider` / `costBasis` | `claude-opus-5` / `firstParty` / `list` | `claude-haiku-4-5` / `firstParty` / `list` |

The sibling field `usage` on the same result reported only
`input_tokens: 4, output_tokens: 150, cache_read: 25513, cache_creation: 25789` —
the main loop, with the entire Haiku bill missing.

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed.** `modelUsage: Record<string, ModelUsage>` is exactly the
shape the architecture asks for: keyed by model, token counts by kind, no
currency required. It maps onto skift's `ReportUsage` per `(lease, model)` with
`inputTokens→input`, `outputTokens→output`, `cacheReadInputTokens→cache_read`,
`cacheCreationInputTokens→cache_write`.

Caveats:

- **`usage` is a trap.** It is main-loop only and excludes subagents, sidechains
  and auxiliary calls. The adapter must read `modelUsage` and never `usage`.
- `modelUsage` is **cumulative across turns** within one `query()` call, so a
  streaming-input session must read the latest result, not sum the results.
  Resuming starts the running total fresh — the per-step totals the mill posts
  are therefore per-`query()`, and a resumed step must not re-post the earlier
  turns' tokens.
- `thinkingTokens` is counted inside `outputTokens`; don't add it.
- `costUSD` is present but is an estimate. R-39 wants tokens; keep it that way.

## Q5 — Cancelling mid-tool-call, and resuming afterwards

**Script** `spikes/sdk/cancel-resume.ts`. Turn 1 runs with
`abortController`, `allowedTools: ["Bash"]` and `permissionMode:
"bypassPermissions"`, and is prompted to run `python3 -c "import time;
time.sleep(120)"` in the foreground. When the `tool_use` block appears the script
waits 5 s — long enough to confirm the child is really running — then calls
`ac.abort()`. A second process resumes the same session id.

**Observed**

```
{"event":"tool_use in flight","tool":"Bash","input":{"command":"python3 -c \"import time; time.sleep(120)\"",…}}
tool process before abort: … 3748415 python3 -c import time; time.sleep(120)
aborting
{"event":"iterator threw","name":"Error","message":"Claude Code process aborted by user",
 "messages":["system","system","system","assistant","rate_limit_event","system"]}
--- cancel process exited status=0 signal=null ---
--- resume process ---
{"subtype":"success","session_id":"f8346ee5-…","result":"The codeword was PERSIMMON-9. The command I was running was
 `python3 -c \"import time; time.sleep(120)\"` … It exited with code 137 (killed by SIGKILL) before finishing…"}
stray tool processes after cancel: (pid 3748415 gone)
```

**The terminal record's shape is the finding: there is none.** On abort the async
iterator **throws** `Error: Claude Code process aborted by user`. No `result`
message is emitted, so no `subtype`, no `modelUsage`, no `structured_output` for
the cancelled turn. Everything the adapter reports for a cancellation it must
synthesise: `ProcessExit{Status: -1, Reason: "cancelled"}`, and usage for that
turn is simply lost unless the adapter has been accumulating per-turn.

The transcript is left consistent. Immediately after the abort the last entry was
the dangling `assistant`/`tool_use`; by the time the resume ran, the pair was
closed by the CLI:

```json
{"type":"tool_use","id":"toolu_01ViTi…","name":"Bash","input":{"command":"python3 -c \"import time; time.sleep(120)\"",…}}
{"type":"tool_result","content":"Exit code 137","is_error":true,"tool_use_id":"toolu_01ViTi…"}
```

The spawned child was killed with the CLI (pid gone; exit 137 = SIGKILL), so an
abort does not leak the tool's process — at least for a direct foreground child.

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed with caveat.** Cancel works, leaves no orphan, and resume
afterwards works and sees the interrupted call as a failed tool result. The
caveat is that cancellation is an exception, not a terminal record, and that the
cancelled turn's usage is unreported.

Incidental observation, environment-specific: a foreground `sleep` is refused by
this CLI build's own guard, which is why the script uses a Python sleep. Under
the mill this is moot — every process goes through `Workspace.Exec`.

## Q6 — A pre-tool hook refusing a call

**Script** `spikes/sdk/hook-deny.ts`. A `PreToolUse` hook returns
`{ continue: true, hookSpecificOutput: { hookEventName: "PreToolUse",
permissionDecision: "deny", permissionDecisionReason: "spike-hook: writing
denied.txt is forbidden by the step's policy" } }` for `Write` calls whose
`file_path` ends in `denied.txt`, and `"allow"` otherwise. The session runs with
`permissionMode: "bypassPermissions"` so the hook is the only thing standing in
the way. The prompt asks for the denied file first, the allowed file as fallback,
and the failure reason quoted verbatim.

**Observed**

```
{"event":"tool_result reaching the model","is_error":true,
 "content":"spike-hook: writing denied.txt is forbidden by the step's policy"}
{"event":"tool_result reaching the model","content":"File created successfully at: /tmp/spike-hook-…/allowed.txt …"}

hook_invocations:      [ {Write, …/denied.txt, deny}, {Write, …/allowed.txt, allow} ]
denied_file_written:   false
allowed_file_written:  true
permission_denials:    [ { tool_name: "Write", tool_use_id: "toolu_01FEnP…",
                           tool_input: { file_path: "…/denied.txt", content: "X" } } ]
```

The model's final message quoted the reason back:

> `/tmp/spike-hook-…/denied.txt` — write rejected with this verbatim reason:
> > spike-hook: writing denied.txt is forbidden by the step's policy

**Verdict (SDK 0.3.263 / CLI 2.1.263): confirmed.** The hook runs before the tool, the decision is binding
(the file does not exist), `permissionDecisionReason` is delivered to the model
as an `is_error` `tool_result` it can read and act on, and the refusal is also
recorded on the terminal result in `permission_denials` — which is what the
ledger should write. `bypassPermissions` does not bypass hooks. Hooks are
in-process JavaScript callbacks in the SDK, not shell-outs, so the sidecar can
route every `PreToolUse` decision through the workspace's capability set without
another process boundary.

---

## Consequences for the design

Nothing here refutes `docs/architecture.md`. Four things sharpen it:

1. `Policy` must compile to `allowedTools` **and** `disallowedTools`; only the
   latter takes a tool away, and only the latter survives `bypassPermissions`
   and reaches subagents (Q3).
2. The adapter reads `modelUsage`, never `usage`, and treats it as cumulative
   within a `query()` call and reset by resume (Q4).
3. `Cancel` has no terminal record to report. `ProcessExit{Status: -1, Reason:
   "cancelled"}` is synthesised by the sidecar from a thrown abort, and per-turn
   usage must be accumulated as it streams if a cancelled turn's tokens are to be
   posted at all (Q5).
4. Structured output is enforced by bounded retry, not by construction. The
   `error_max_structured_output_retries` result subtype is a distinct step
   failure (Q2).

One question this spike deliberately did not touch: MCP servers, which is the
sibling `spike-mcp` node.

## Addendum, 2026-09-09 — the permission mode a session runs under

The scripts above ran under `bypassPermissions` wherever a mode mattered, and Q3
read that as the mode to adopt: `disallowedTools` is the boundary that survives
it, so the mode looked free. The first end-to-end `mill run` (2026-09-08)
showed what setting *no* mode costs. The `commit` step's session called
`git add`, the SDK's permission layer answered "This command requires
approval", and the session — unattended, with nothing at a terminal — gave up.
`buildOptions` had set `allowedTools` and `disallowedTools` and never
`permissionMode`, so the SDK used its `default`: prompt, and wait.

The decision is **`auto`**, the model classifier, as the sidecar's default, with
`default`, `acceptEdits`, `dontAsk` and `plan` available to a step that names
one. `bypassPermissions` is **refused** by `sidecar/src/options.ts` with exit
reason `policy`, before the SDK is loaded.

Why not the spike's recommendation:

- What Q3 established is that `disallowedTools` is a boundary and `allowedTools`
  is not. It did not establish that a session needs a mode that says yes to
  everything — only that such a mode cannot reach a tool that was removed.
- Today a session runs as the mill's own account, in a clone on the mill's own
  host, with the SDK's own `Bash` (`docs/architecture.md` §"The workspace":
  MVP's only enforcement is the tool policy). Under `bypassPermissions` the
  only thing between a session and that host is the deny list. Under `auto` a
  classifier is asked as well. Neither is a boundary; one is strictly more
  refusal than the other, at the cost of a classifier call.
- The refusal is not permanent. It is spelled as a `policy` refusal naming the
  sandbox because the mode becomes reasonable once the workspace is a
  container (`docs/roadmap.md` §O4) — at which point this is a one-line change
  to a list.

What the mode is *not* is enforcement. `tools.deny` and the workspace's socket
remain the boundary, exactly as Q3 found; the classifier is what answers a
prompt that would otherwise wait for a human who is not there.

## Addendum, 2026-09-21 — what `includePartialMessages`/`forwardSubagentText` forward

R-24's full-fidelity node (mill#15) needed the exact shape of a streamed
delta and a folded subagent event before wiring the sidecar to them. Like
Q1-Q6, this addendum is a script run against a live session — the sidecar
built against these flags, pointed at the real, pinned SDK
(`@anthropic-ai/claude-agent-sdk` **0.3.263**) through this step's own
gateway token, spending exactly what one session costs. What follows starts
from the pinned SDK's own shipped type declarations (`sdk.d.ts`) and the
`@anthropic-ai/sdk` **0.122.0** beta message types it re-exports, and is then
confirmed against a real turn — one that streamed thinking, streamed text,
and ran a subagent through the `Task` tool — in "Confirmed against a live
session" below. The sidecar's own behavior against these shapes is also
exercised by a fake harness standing in for the SDK
(`sidecar/spec/fake_sdk.ts`'s `streamEvent`/`messageStart`/`blockStart`/
`textDelta`/`thinkingDelta`/`blockStop` helpers, built from this same
reading) and proved end-to-end by
`sidecar/streaming_test.go`, which is the coverage that runs on every build;
the live run is what grounds that fake in the SDK's real behavior rather
than in its own types read twice.

**`includePartialMessages`**, `sdk.d.ts`:

> Include partial/streaming message events in the output. When true,
> `SDKPartialAssistantMessage` events will be emitted during streaming.

`SDKPartialAssistantMessage`:

```ts
export declare type SDKPartialAssistantMessage = {
    type: 'stream_event';
    event: BetaRawMessageStreamEvent;
    parent_tool_use_id: string | null;
    uuid: UUID;
    session_id: string;
    …
};
```

`BetaRawMessageStreamEvent` (from `@anthropic-ai/sdk`) is a union of the
Messages API's own streaming events — `message_start`,
`content_block_start`, `content_block_delta`, `content_block_stop`,
`message_delta`, `message_stop`. The two that matter for text and thinking:

```ts
export interface BetaRawContentBlockDeltaEvent {
    delta: BetaRawContentBlockDelta;  // BetaTextDelta | BetaThinkingDelta | …
    index: number;
    type: 'content_block_delta';
}
export interface BetaTextDelta { text: string; type: 'text_delta'; }
export interface BetaThinkingDelta { thinking: string; type: 'thinking_delta'; … }

export interface BetaRawContentBlockStopEvent {
    index: number;
    type: 'content_block_stop';
}
```

`content_block_stop` carries no text of its own — confirming what
`docs/activity.md` calls the closing `final` record: it has an index to
close a block by, and nothing else to append. `docs/activity.md`'s design
of "deltas plus an empty `final`" is a direct fit, not a guess dressed up as
one.

And confirming why the completed message still arrives after the deltas
(`SDKAssistantMessage`'s own doc comment):

> While a response streams the CLI emits one assistant message per
> completed content block, so several consecutive assistant messages can
> share `message.id` and each carries just that block in `message.content`;
> on those, `message.stop_reason` is null and `message.usage` is not final —
> the turn's stop reason and total usage arrive on the result message.

**`forwardSubagentText`**, `sdk.d.ts`:

> Forward subagent text and thinking blocks as assistant/user messages with
> `parent_tool_use_id` set. By default, only tool_use/tool_result blocks
> from subagents are emitted (enough for a heartbeat counter). When true,
> the full subagent conversation is forwarded so consumers can render a
> nested transcript.

So `parent_tool_use_id` is not new plumbing this flag adds: `SDKAssistantMessage`,
`SDKUserMessage` and `SDKPartialAssistantMessage` all carry it unconditionally,
non-null "when the message was produced inside a subagent started by that
tool_use" (`SDKAssistantMessage`'s doc comment) — it is the `Agent` tool
call's own `tool_use_id`. `forwardSubagentText` only controls whether a
subagent's *text/thinking* blocks are among the messages forwarded at all;
its tool calls and results were already forwarded, tagged the same way,
without the flag.

**Consequence for the design.** Nothing here contradicts `docs/activity.md`;
it grounds the two fields it specifies as sidecar output (`Text`/`Thinking`'s
`block_id`/`delta`/`final`, and `Event`'s `parent_call_id`) in the SDK's own
documented shape rather than in an inference from `session.ts`'s existing
`"assistant"`/`"user"` handling alone.

**Confirmed against a live session.** Run once, against the built sidecar
with both flags on (an ad hoc spec, deleted after its output was read): one
turn with a prompt asking for reasoning before an answer, then a `Task` call
to a subagent. The sidecar's own translated `harness.v1.Event` stream — the
code this node ships, not the SDK's raw JSON — showed:

- **`block_id` is `<message id>:<index>`**, exactly `StreamBlocks.blockId`'s
  own scheme: a thinking block opened as `msg_011CfG…ghH:0`, the text block
  that followed it in the same message as `msg_011CfG…ghH:1`.
- **A thinking block streamed two deltas (`delta: true`, `final: false`)
  then one empty `final: true` record on the same block id** — the model's
  reasoning for this prompt was brief enough that both deltas carried little
  text, but the shape (deltas, then an empty final closing the same id) is
  exactly `docs/activity.md`'s design and exactly what the type declarations
  above promise.
- **The completed message's own `usage` arrived mid-turn, before the tool
  call it was for had a result** — confirming the node's own note: several
  `usage` events appeared between the subagent's tool calls and its final
  result, not just once at the end.
- **A subagent's tool calls, results and usage all carried `parent_call_id`
  set to the `Agent` call's own `call_id`**, while the `Agent` call itself
  and the `tool_result` that answered it carried none — the fold
  `docs/activity.md` §Turns and `streaming_test.go`'s fake-harness spec
  describe, reproduced against the real SDK: `TOOLCALL parent="" name=Agent
  call=toolu_01Dwd…`, then `TOOLCALL parent="toolu_01Dwd…" name=Glob …`,
  `TOOLRESULT parent="toolu_01Dwd…" …`, `USAGE parent="toolu_01Dwd…"` for
  everything the subagent itself did, and finally `TOOLRESULT parent=""
  call=toolu_01Dwd…` answering the top-level `Agent` call.
- **The subagent in this run reported its finding through the `Agent` call's
  own `tool_result` rather than a `text` block of its own** — this prompt's
  subagent answered in one shot with no intermediate narration, so no
  subagent-tagged `Event_Text` appeared to confirm `forwardSubagentText`'s
  text-delta path specifically. What was confirmed instead is that
  `parent_tool_use_id` tags every message kind the same way regardless of
  content (tool calls, results, and usage all got it); since a `text`/
  `thinking` block under a subagent travels through the identical
  `content_block_delta`/`parent_tool_use_id` machinery already confirmed
  above for the top-level turn's own deltas, a subagent that narrates in
  words would fold the same way, but this run did not produce one to show
  it firsthand.

No surprise turned up: the fake harness's `streamEvent` helpers, and the
`sidecar/streaming_test.go` specs built against them, already matched what
the real SDK sent.
