// A harness the specs can predict.
//
// Everything the sidecar does to a session's messages — the framing, the
// exit mapping, the usage accumulation — is testable without a credential,
// because the SDK reaches the sidecar as one function. This file is that
// function, driven by a scenario named in the environment, and it produces
// the fields the sidecar reads and no others.
//
// The scenarios are not inventions. Each mirrors an outcome observed in
// docs/spikes/sdk-sessions.md, including the SDK's own wording where the
// sidecar keys off it.

import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import type { Readable, Writable } from "node:stream";

import type { Options, QueryFn, SDKMessage, SDKUserMessage } from "../src/sdk.js";
import { taskOutputPath } from "../src/tasks.js";

// McpChild is a mounted server spawned with a piped stdin/stdout — the shape
// `stdio: ["pipe", "pipe", "inherit"]` produces — and the type mcpLine and
// mcpRequest speak the fixture's protocol over.
type McpChild = ChildProcessByStdio<Writable, Readable, null>;

// SCENARIO_VAR names the scenario the fake plays.
export const SCENARIO_VAR = "MILL_FAKE_SCENARIO";
// OPTIONS_VAR names a file the fake writes the options it was handed to, so
// a spec can assert what policy the session would have run under.
export const OPTIONS_VAR = "MILL_FAKE_OPTIONS";
// PID_VAR names a file the fake writes the process id of the child it
// spawned for a background task to, so a spec can watch the breaker kill it.
export const PID_VAR = "MILL_FAKE_PID";
// WORK_MS_VAR is how long a working task keeps writing before it ends.
export const WORK_MS_VAR = "MILL_FAKE_WORK_MS";
// ABORT_VAR says how the fake answers an abort: "throw" like the SDK does,
// or "ignore" to stand for a harness that does not stop.
export const ABORT_VAR = "MILL_FAKE_ABORT";

// The message the SDK's iterator throws when the caller aborts, and the
// error it throws when the CLI rejects a session id. Both are quoted from
// docs/spikes/sdk-sessions.md.
const ABORT_ERROR = "Claude Code process aborted by user";
const BAD_SESSION_ERROR =
  "Claude Code process exited with code 1. stderr: Error: Invalid session ID. Must be a valid UUID.";

// MODEL_USAGE is the per-model bill the fake reports: two models on one
// turn, which is why the wire's Usage is a map.
export const MODEL_USAGE = {
  "claude-opus-5": {
    inputTokens: 4,
    outputTokens: 150,
    cacheReadInputTokens: 25513,
    cacheCreationInputTokens: 25789,
  },
  "claude-haiku-4-5-20251001": {
    inputTokens: 3870,
    outputTokens: 76,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
  },
};

function assistantText(text: string, parentToolUseId: string | null = null): SDKMessage {
  return {
    type: "assistant",
    parent_tool_use_id: parentToolUseId,
    message: {
      model: "claude-opus-5",
      content: [{ type: "text", text }],
      usage: { input_tokens: 4, output_tokens: 150, cache_read_input_tokens: 25513, cache_creation_input_tokens: 25789 },
    },
  } as unknown as SDKMessage;
}

function toolCall(
  id: string,
  name: string,
  input: Record<string, unknown>,
  parentToolUseId: string | null = null,
): SDKMessage {
  return {
    type: "assistant",
    parent_tool_use_id: parentToolUseId,
    message: {
      model: "claude-opus-5",
      content: [{ type: "tool_use", id, name, input }],
      usage: { input_tokens: 1, output_tokens: 2, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    },
  } as unknown as SDKMessage;
}

function toolResult(id: string, content: string, isError = false, parentToolUseId: string | null = null): SDKMessage {
  return {
    type: "user",
    parent_tool_use_id: parentToolUseId,
    message: { content: [{ type: "tool_result", tool_use_id: id, content, is_error: isError }] },
  } as unknown as SDKMessage;
}

// streamEvent wraps one raw Anthropic streaming event as the SDK forwards it
// under includePartialMessages, scoped to parentToolUseId exactly as
// "assistant"/"user" messages are (session.ts's StreamBlocks).
function streamEvent(event: Record<string, unknown>, parentToolUseId: string | null = null): SDKMessage {
  return { type: "stream_event", parent_tool_use_id: parentToolUseId, event } as unknown as SDKMessage;
}

function messageStart(id: string): Record<string, unknown> {
  return { type: "message_start", message: { id } };
}

function blockStart(index: number, kind: "text" | "thinking"): Record<string, unknown> {
  return { type: "content_block_start", index, content_block: { type: kind } };
}

function textDelta(index: number, text: string): Record<string, unknown> {
  return { type: "content_block_delta", index, delta: { type: "text_delta", text } };
}

function thinkingDelta(index: number, thinking: string): Record<string, unknown> {
  return { type: "content_block_delta", index, delta: { type: "thinking_delta", thinking } };
}

function blockStop(index: number): Record<string, unknown> {
  return { type: "content_block_stop", index };
}

function success(result: string, structured?: unknown): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    result,
    structured_output: structured,
    modelUsage: MODEL_USAGE,
    permission_denials: [],
  } as unknown as SDKMessage;
}

function failure(subtype: string, errors: string[]): SDKMessage {
  return {
    type: "result",
    subtype,
    is_error: true,
    errors,
    modelUsage: MODEL_USAGE,
    permission_denials: [],
  } as unknown as SDKMessage;
}

// probeEnvironment runs `env` under exactly the environment the sidecar
// built for the session, which is what the real harness does with its CLI
// child, and returns its output.
function probeEnvironment(options: Options): string {
  const probe = spawnSync("env", { env: options.env as NodeJS.ProcessEnv, encoding: "utf8" });
  return probe.stdout ?? "";
}

// The session id the task scenarios report. The sidecar reconstructs the
// path of a task's output file from it, exactly as it does for the real
// harness, so the fake writes where the harness would.
const TASK_SESSION = "5d2b7a1c-4e11-4d3a-9e2c-0f2f4d3a6d6f";
const TASK_ID = "task-01JB";
const TASK_DESCRIPTION = "Bash: make test";

function taskStarted(): SDKMessage {
  return {
    type: "system",
    subtype: "task_started",
    task_id: TASK_ID,
    description: TASK_DESCRIPTION,
    is_backgrounded: true,
    session_id: TASK_SESSION,
  } as unknown as SDKMessage;
}

// systemInit stands for one of the three "system"/"init" messages a real
// session sends before its first turn (docs/spikes/sdk-sessions.md §Q5) —
// bookkeeping, never a turn, so the "bare-result" scenario below yields
// three of them to prove they are not mistaken for one.
function systemInit(): SDKMessage {
  return { type: "system", subtype: "init", session_id: TASK_SESSION } as unknown as SDKMessage;
}

function taskNotification(status: string): SDKMessage {
  return {
    type: "system",
    subtype: "task_notification",
    task_id: TASK_ID,
    status,
    output_file: "",
    summary: `the task ${status}`,
    session_id: TASK_SESSION,
  } as unknown as SDKMessage;
}

// spawnTask starts a real process to stand for the background task's shell,
// as a child of the sidecar — which is where the harness's own child, and
// the task's shell under it, would be. Its process id is written where the
// spec can watch for it.
function spawnTask(env: Record<string, string | undefined>): void {
  const child = spawn("sleep", ["30"], { stdio: "ignore" });
  child.unref();
  const target = env[PID_VAR];
  if (target !== undefined) {
    writeFileSync(target, String(child.pid));
  }
}

// grow appends to the task's output file, which is the only thing that
// tells the sidecar the task is working.
function grow(options: Options): void {
  const path = taskOutputPath(process.getuid?.() ?? 0, options.cwd ?? "", TASK_SESSION, TASK_ID);
  mkdirSync(dirname(path), { recursive: true });
  appendFileSync(path, `${new Date().toISOString()} still going\n`);
}

// checked runs the sidecar's own PreToolUse hooks over a Bash command, which
// is what the harness does with them before it runs one.
async function checked(options: Options, command: string): Promise<string> {
  for (const matcher of options.hooks?.PreToolUse ?? []) {
    if (matcher.matcher !== undefined && matcher.matcher !== "Bash") {
      continue;
    }
    for (const hook of matcher.hooks) {
      const decision = await hook(
        { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command } } as never,
        "toolu_hook",
        { signal: new AbortController().signal },
      );
      const specific = "hookSpecificOutput" in decision ? decision.hookSpecificOutput : undefined;
      if (specific !== undefined && "permissionDecision" in specific && specific.permissionDecision === "deny") {
        return `denied: ${specific.permissionDecisionReason ?? ""}`;
      }
    }
  }
  return "allowed";
}

// mcpLine reads one line of the newline-delimited JSON-RPC a mounted stdio
// MCP server answers on — the framing the SDK's own stdio transport uses
// (spikes/mcp/workspace-server.ts's LineTransport frames a socket the same
// way).
function mcpLine(child: McpChild): Promise<{ result?: unknown }> {
  return new Promise((resolve, reject) => {
    let buffer = "";
    const onData = (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      const newline = buffer.indexOf("\n");
      if (newline === -1) {
        return;
      }
      child.stdout.off("data", onData);
      resolve(JSON.parse(buffer.slice(0, newline)));
    };
    child.stdout.on("data", onData);
    child.stdout.once("error", reject);
  });
}

let mcpRequestID = 0;

function mcpRequest(child: McpChild, method: string, params?: unknown): void {
  mcpRequestID += 1;
  child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: mcpRequestID, method, params }) + "\n");
}

async function* play(
  scenario: string,
  options: Options,
  prompts: AsyncIterator<SDKUserMessage>,
): AsyncIterable<SDKMessage> {
  // Every scenario begins with the step's own prompt, exactly as the harness
  // takes it off the stream before it answers.
  await prompts.next();
  switch (scenario) {
    case "trivial":
      yield assistantText("Read the node body and summarised it.");
      yield success("Read the node body and summarised it.");
      return;
    case "structured":
      yield assistantText("Reviewed the diff.");
      yield success("Reviewed the diff.", {
        decision: "approve",
        findings: [],
        summary: { headline: "every criterion holds", confidence: 0.9 },
      });
      return;
    case "structured-missing":
      yield assistantText("Reviewed the diff.");
      yield success("Reviewed the diff.");
      return;
    case "env-probe":
      yield toolCall("toolu_env", "Bash", { command: "env" });
      yield toolResult("toolu_env", probeEnvironment(options));
      yield success("Printed the environment.");
      return;
    case "failed":
      yield assistantText("Working on it.");
      yield failure("error_during_execution", ["the harness lost its connection"]);
      return;
    case "bad-session-id":
      throw new Error(BAD_SESSION_ERROR);
    case "bare-result":
      // A resume whose first SDK messages are its own "system" init
      // sequence and then, with no "assistant"/"user" turn in between, its
      // result — the shape mill#19's attempt 1 left behind, suspected of a
      // resume racing the predecessor session's own teardown.
      yield systemInit();
      yield systemInit();
      yield systemInit();
      yield success("Looks like nothing to do here.");
      return;
    case "hang": {
      yield toolCall("toolu_hang", "Bash", { command: "python3 -c 'import time; time.sleep(120)'" });
      const signal = options.abortController?.signal;
      await new Promise<never>((_resolve, reject) => {
        if (signal === undefined) {
          return;
        }
        signal.addEventListener("abort", () => {
          if (process.env[ABORT_VAR] !== "ignore") {
            reject(new Error(ABORT_ERROR));
          }
        });
      });
      return;
    }
    case "silent-task": {
      // A turn that ends with a background task the session means to be
      // woken for, whose output never grows.
      spawnTask(process.env);
      yield assistantText("Started the suite and I am waiting on it.");
      yield toolCall("toolu_bg", "Bash", { command: "make test", run_in_background: true });
      yield toolResult("toolu_bg", "running in the background");
      yield taskStarted();
      yield success("Started the suite and I am waiting on it.");
      // Each heartbeat is answered, and the answer says nothing new: the
      // task stays silent, so the strikes go on accumulating.
      while (true) {
        const heartbeat = await prompts.next();
        if (heartbeat.done === true) {
          return;
        }
        yield assistantText("Still waiting on the suite.");
        yield success("Still waiting on the suite.");
      }
    }
    case "working-task": {
      // The same wait, over a task that keeps writing. The output grows for
      // as long as the scenario's window, and then the task ends and the
      // harness wakes the session with a last turn.
      yield assistantText("Started the suite and I am waiting on it.");
      yield taskStarted();
      yield success("Started the suite and I am waiting on it.");
      const until = Date.now() + Number.parseInt(process.env[WORK_MS_VAR] ?? "2000", 10);
      while (Date.now() < until) {
        grow(options);
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
      yield taskNotification("completed");
      yield assistantText("The suite passed.");
      yield success("The suite passed.");
      return;
    }
    case "answered-heartbeat": {
      // A silent task whose session, asked once, stops it and says so.
      spawnTask(process.env);
      yield assistantText("Started the suite and I am waiting on it.");
      yield taskStarted();
      yield success("Started the suite and I am waiting on it.");
      const heartbeat = await prompts.next();
      if (heartbeat.done === true) {
        return;
      }
      yield taskNotification("stopped");
      yield assistantText("Done; I stopped the task.");
      yield success("Done; I stopped the task.");
      return;
    }
    case "streamed-text": {
      // A text block that arrives as deltas, the way includePartialMessages
      // reports it, followed by the completed assistant message the SDK
      // still sends for it (docs/activity.md §"Streaming text").
      yield streamEvent(messageStart("msg_stream_text"));
      yield streamEvent(blockStart(0, "text"));
      yield streamEvent(textDelta(0, "Hello"));
      yield streamEvent(textDelta(0, ", world!"));
      yield streamEvent(blockStop(0));
      yield assistantText("Hello, world!");
      yield success("Hello, world!");
      return;
    }
    case "streamed-thinking": {
      yield streamEvent(messageStart("msg_stream_thinking"));
      yield streamEvent(blockStart(0, "thinking"));
      yield streamEvent(thinkingDelta(0, "weighing "));
      yield streamEvent(thinkingDelta(0, "the options"));
      yield streamEvent(blockStop(0));
      yield assistantText("Decided.");
      yield success("Decided.");
      return;
    }
    case "subagent": {
      // An Agent call whose own text, tool call and tool result stream under
      // its call id as parent_tool_use_id, folding under the call that
      // started it rather than the top-level turn (docs/activity.md
      // §Subagents).
      const agentCallId = "toolu_agent";
      yield toolCall(agentCallId, "Agent", { description: "look something up" });
      yield streamEvent(messageStart("msg_sub"), agentCallId);
      yield streamEvent(blockStart(0, "text"), agentCallId);
      yield streamEvent(textDelta(0, "Looking into it."), agentCallId);
      yield streamEvent(blockStop(0), agentCallId);
      yield toolCall("toolu_sub_read", "Read", { file_path: "/tmp/x" }, agentCallId);
      yield toolResult("toolu_sub_read", "file contents", false, agentCallId);
      yield assistantText("Looking into it.", agentCallId);
      yield toolResult(agentCallId, "the subagent's answer");
      yield assistantText("Done.");
      yield success("Done.");
      return;
    }
    case "subagent-open": {
      // Same shape as "subagent", except the Agent call's own result never
      // arrives: the turn ends with the subagent still running.
      const agentCallId = "toolu_agent_open";
      yield toolCall(agentCallId, "Agent", { description: "look something up" });
      yield streamEvent(messageStart("msg_sub_open"), agentCallId);
      yield streamEvent(blockStart(0, "text"), agentCallId);
      yield streamEvent(textDelta(0, "Still looking into it."), agentCallId);
      yield streamEvent(blockStop(0), agentCallId);
      yield assistantText("Waiting on the subagent.");
      yield success("Waiting on the subagent.");
      return;
    }
    case "large-result": {
      // A tool result several hundred KB long: forwarded whole, with no
      // limit of the sidecar's own (docs/activity.md §"What exists today").
      const large = "x".repeat(300 * 1024);
      yield toolCall("toolu_large", "Bash", { command: "cat big.txt" });
      yield toolResult("toolu_large", large);
      yield success("Read the big file.");
      return;
    }
    case "poll-loop": {
      // The harness checking two Bash commands against the sidecar's own
      // PreToolUse hooks: the self-matching wait, and a `pgrep` that is not
      // a loop at all.
      const loop = 'until ! pg' + 'rep -f "x"; do sleep 1; done';
      const plain = 'pg' + 'rep -f "^/usr/bin/x"';
      yield toolCall("toolu_loop", "Bash", { command: loop });
      yield toolResult("toolu_loop", await checked(options, loop), true);
      yield toolCall("toolu_plain", "Bash", { command: plain });
      yield toolResult("toolu_plain", await checked(options, plain));
      yield success("Checked both commands.");
      return;
    }
    case "mcp-mount": {
      // Stands in for the SDK spawning a session's own `options.mcpServers`:
      // the process and the protocol spoken to it are real, down to killing
      // the server when the turn it was mounted for is done — the part
      // "the server process is gone when the step ends" needs proved.
      const [name, server] = Object.entries(options.mcpServers ?? {})[0] as [
        string,
        { command: string; args?: string[]; env?: Record<string, string> },
      ];
      const child = spawn(server.command, server.args ?? [], {
        env: server.env as NodeJS.ProcessEnv,
        stdio: ["pipe", "pipe", "inherit"],
      }) as McpChild;
      const target = process.env[PID_VAR];
      if (target !== undefined) {
        writeFileSync(target, String(child.pid));
      }
      mcpRequest(child, "initialize", {
        protocolVersion: "2024-11-05",
        capabilities: {},
        clientInfo: { name: "fake", version: "0.0.0" },
      });
      await mcpLine(child);
      mcpRequest(child, "tools/list");
      const listed = await mcpLine(child);
      const tool = (listed.result as { tools: Array<{ name: string }> }).tools[0];
      yield toolCall("toolu_mcp", `mcp__${name}__${tool.name}`, { echo: "ping" });
      mcpRequest(child, "tools/call", { name: tool.name, arguments: { echo: "ping" } });
      const called = await mcpLine(child);
      const content = (called.result as { content: Array<{ text: string }> }).content;
      yield toolResult("toolu_mcp", content[0]?.text ?? "");
      yield success(`Called ${name}.`);
      child.kill();
      return;
    }
    default:
      throw new Error(`fake harness has no scenario ${JSON.stringify(scenario)}`);
  }
}

// fakeQuery is a QueryFn that plays the scenario named in the environment.
export function fakeQuery(env: Record<string, string | undefined>): QueryFn {
  return ({ options, prompt }) => {
    const target = env[OPTIONS_VAR];
    if (target !== undefined) {
      writeFileSync(target, JSON.stringify({ ...options, abortController: undefined }, null, 2));
    }
    return play(env[SCENARIO_VAR] ?? "trivial", options, prompt[Symbol.asyncIterator]());
  };
}
