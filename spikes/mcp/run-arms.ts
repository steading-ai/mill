// The measurement: the same three-file edit task run three ways.
//
//   mcp       built-in file and shell tools removed; only the workspace MCP tools
//   builtins  the built-ins, no MCP server at all
//   both      the built-ins and the MCP server, to see which the model reaches for
//
// Each arm runs REPS times. Per run: wall clock, turns, tool calls by name,
// modelUsage totals, and whether the filesystem actually shows the three edits.
// The first `mcp` run's full message stream is written to
// spikes/mcp/transcripts/mcp-only.jsonl.
//
// Run: pnpm exec tsx spikes/mcp/run-arms.ts [reps]

import { query } from "@anthropic-ai/claude-agent-sdk";
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { makeWorkspace, PROMPT, verify } from "./task.js";

const REPO = process.cwd();
const TSX = join(REPO, "node_modules/.bin/tsx");
const SERVER = join(REPO, "spikes/mcp/workspace-server.ts");
const NAME = "mill_workspace"; // "workspace" is refused: reserved_name
const MODEL = "sonnet";
const REPS = Number(process.argv[2] ?? 3);

const BUILTIN_FILE_TOOLS = ["Read", "Write", "Edit", "NotebookEdit", "Bash", "Glob", "Grep", "Task", "WebFetch", "WebSearch"];
const MCP_TOOLS = [`mcp__${NAME}__fs_read`, `mcp__${NAME}__fs_write`, `mcp__${NAME}__exec`];

type Run = {
  arm: string;
  rep: number;
  ms: number;
  turns: number;
  calls: Record<string, number>;
  usage: { input: number; output: number; cacheRead: number; cacheWrite: number };
  serverMs: number[];
  ok: boolean;
  failed: string[];
  subtype: string;
};

async function run(arm: string, rep: number, transcript?: string): Promise<Run> {
  const root = makeWorkspace();
  const mcp = arm !== "builtins";
  const options: Record<string, unknown> = {
    cwd: root,
    model: MODEL,
    maxTurns: 40,
    settingSources: [],
    strictMcpConfig: true,
    permissionMode: "bypassPermissions",
  };
  if (mcp) {
    options.mcpServers = { [NAME]: { type: "stdio", command: TSX, args: [SERVER, "stdio", root], alwaysLoad: true } };
    options.env = { ...process.env, WS_LOG: join(root, "calls.jsonl") };
  }
  if (arm === "mcp") {
    options.disallowedTools = BUILTIN_FILE_TOOLS;
    options.allowedTools = MCP_TOOLS;
  } else if (arm === "builtins") {
    options.allowedTools = ["Read", "Write", "Edit", "Bash"];
  } else {
    options.allowedTools = [...MCP_TOOLS, "Read", "Write", "Edit", "Bash"];
  }

  const prompt = arm === "mcp"
    ? `${PROMPT}\n\nThis session has no built-in file or shell tools. The ${NAME} tools are the only way to reach the filesystem; paths are relative to the workspace root.`
    : PROMPT;

  const calls: Record<string, number> = {};
  let turns = 0;
  let subtype = "none";
  let usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  const started = Date.now();
  for await (const m of query({ prompt, options } as never) as AsyncIterable<any>) {
    if (transcript) appendFileSync(transcript, JSON.stringify(m) + "\n");
    if (m.type === "assistant") {
      for (const b of m.message.content ?? []) if (b.type === "tool_use") calls[b.name] = (calls[b.name] ?? 0) + 1;
    }
    if (m.type === "result") {
      subtype = m.subtype;
      turns = m.num_turns ?? 0;
      for (const u of Object.values<any>(m.modelUsage ?? {})) {
        usage = {
          input: usage.input + (u.inputTokens ?? 0),
          output: usage.output + (u.outputTokens ?? 0),
          cacheRead: usage.cacheRead + (u.cacheReadInputTokens ?? 0),
          cacheWrite: usage.cacheWrite + (u.cacheCreationInputTokens ?? 0),
        };
      }
    }
  }
  const ms = Date.now() - started;
  // What the workspace server itself clocked for each call, so the MCP hop can
  // be separated from the model's thinking time.
  const logPath = join(root, "calls.jsonl");
  const serverMs = existsSync(logPath)
    ? readFileSync(logPath, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l).ms).filter((n: unknown) => typeof n === "number")
    : [];
  const { ok, checks } = verify(root);
  rmSync(root, { recursive: true, force: true });
  const result: Run = {
    arm,
    rep,
    ms,
    turns,
    calls,
    usage,
    serverMs,
    ok,
    failed: Object.entries(checks).filter(([, v]) => !v).map(([k]) => k),
    subtype,
  };
  console.log(JSON.stringify(result));
  return result;
}

function mean(ns: number[]) {
  return Math.round(ns.reduce((a, b) => a + b, 0) / ns.length);
}

async function main() {
  const dir = join(REPO, "spikes/mcp/transcripts");
  mkdirSync(dir, { recursive: true });
  const transcript = join(dir, "mcp-only.jsonl");
  writeFileSync(transcript, "");

  const runs: Run[] = [];
  for (const arm of ["mcp", "builtins", "both"]) {
    for (let rep = 1; rep <= REPS; rep++) {
      runs.push(await run(arm, rep, arm === "mcp" && rep === 1 ? transcript : undefined));
    }
  }

  console.log("\n| arm | completed | tool calls | turns | input | output | cache read | cache write | wall clock |");
  console.log("|---|---|---|---|---|---|---|---|---|");
  for (const arm of ["mcp", "builtins", "both"]) {
    const rs = runs.filter((r) => r.arm === arm);
    const totalCalls = rs.map((r) => Object.values(r.calls).reduce((a, b) => a + b, 0));
    console.log(
      `| \`${arm}\` | ${rs.filter((r) => r.ok).length}/${rs.length} | ${mean(totalCalls)} | ${mean(rs.map((r) => r.turns))} | ` +
        `${mean(rs.map((r) => r.usage.input))} | ${mean(rs.map((r) => r.usage.output))} | ${mean(rs.map((r) => r.usage.cacheRead))} | ` +
        `${mean(rs.map((r) => r.usage.cacheWrite))} | ${(mean(rs.map((r) => r.ms)) / 1000).toFixed(1)} s |`,
    );
  }
  const byTool: Record<string, Record<string, number>> = {};
  for (const r of runs) {
    byTool[r.arm] ??= {};
    for (const [k, v] of Object.entries(r.calls)) byTool[r.arm][k] = (byTool[r.arm][k] ?? 0) + v;
  }
  const allServerMs = runs.flatMap((r) => r.serverMs);
  console.log(`\nworkspace server call latency, ms: n=${allServerMs.length} min=${Math.min(...allServerMs)} max=${Math.max(...allServerMs)} mean=${mean(allServerMs)}`);
  console.log("\ntool calls by name, summed over reps:", JSON.stringify(byTool, null, 1));
  writeFileSync(join(dir, "arms.json"), JSON.stringify(runs, null, 1) + "\n");
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
