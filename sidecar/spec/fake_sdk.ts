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

import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";

import type { Options, QueryFn, SDKMessage } from "../src/sdk.js";

// SCENARIO_VAR names the scenario the fake plays.
export const SCENARIO_VAR = "MILL_FAKE_SCENARIO";
// OPTIONS_VAR names a file the fake writes the options it was handed to, so
// a spec can assert what policy the session would have run under.
export const OPTIONS_VAR = "MILL_FAKE_OPTIONS";
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

function assistantText(text: string): SDKMessage {
  return {
    type: "assistant",
    message: {
      model: "claude-opus-5",
      content: [{ type: "text", text }],
      usage: { input_tokens: 4, output_tokens: 150, cache_read_input_tokens: 25513, cache_creation_input_tokens: 25789 },
    },
  } as unknown as SDKMessage;
}

function toolCall(id: string, name: string, input: Record<string, unknown>): SDKMessage {
  return {
    type: "assistant",
    message: {
      model: "claude-opus-5",
      content: [{ type: "tool_use", id, name, input }],
      usage: { input_tokens: 1, output_tokens: 2, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    },
  } as unknown as SDKMessage;
}

function toolResult(id: string, content: string, isError = false): SDKMessage {
  return {
    type: "user",
    message: { content: [{ type: "tool_result", tool_use_id: id, content, is_error: isError }] },
  } as unknown as SDKMessage;
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

async function* play(scenario: string, options: Options): AsyncIterable<SDKMessage> {
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
    default:
      throw new Error(`fake harness has no scenario ${JSON.stringify(scenario)}`);
  }
}

// fakeQuery is a QueryFn that plays the scenario named in the environment.
export function fakeQuery(env: Record<string, string | undefined>): QueryFn {
  return ({ options }) => {
    const target = env[OPTIONS_VAR];
    if (target !== undefined) {
      writeFileSync(target, JSON.stringify({ ...options, abortController: undefined }, null, 2));
    }
    return play(env[SCENARIO_VAR] ?? "trivial", options);
  };
}
