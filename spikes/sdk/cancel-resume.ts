// Q5: can a session be cancelled mid-tool-call, and resumed afterwards?
// Turn 1 is aborted while a `sleep 120` Bash call is in flight; the abort's
// terminal shape is recorded, then a second process resumes the same session id.
//
// Run: pnpm exec tsx spikes/sdk/cancel-resume.ts

import { query } from "@anthropic-ai/claude-agent-sdk";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const transcript = (id: string) =>
  join(homedir(), ".claude", "projects", process.cwd().replace(/[/.]/g, "-"), `${id}.jsonl`);

async function cancelled(id: string) {
  const ac = new AbortController();
  const q = query({
    prompt: "The codeword is PERSIMMON-9. Run this shell command in the foreground, with a 200000 ms timeout: python3 -c \"import time; time.sleep(120)\" — then say DONE.",
    options: {
      sessionId: id,
      allowedTools: ["Bash"],
      permissionMode: "bypassPermissions",
      dangerouslySkipPermissions: true,
      abortController: ac,
      maxTurns: 4,
    } as never,
  });
  const seen: string[] = [];
  let aborting = false;
  try {
    for await (const m of q) {
      seen.push(m.type);
      if (m.type === "assistant") {
        for (const b of m.message.content as any[]) {
          if (b.type === "tool_use") {
            console.log(JSON.stringify({ event: "tool_use in flight", tool: b.name, input: b.input }));
            if (!aborting) {
              aborting = true;
              setTimeout(() => {
                console.log("tool process before abort:", spawnSync("pgrep", ["-a", "-x", "python3"], { encoding: "utf8" }).stdout.trim() || "none");
                console.log("aborting");
                ac.abort();
              }, 5000);
            }
          }
        }
      }
      if (m.type === "result") {
        console.log(JSON.stringify({ event: "result", subtype: m.subtype, is_error: m.is_error, terminal_reason: (m as any).terminal_reason, errors: (m as any).errors, modelUsage: Object.keys(m.modelUsage) }));
      }
    }
    console.log(JSON.stringify({ event: "iterator ended normally", messages: seen }));
  } catch (e) {
    console.log(JSON.stringify({ event: "iterator threw", name: (e as Error).name, message: (e as Error).message, messages: seen }));
  }
  try {
    console.log(JSON.stringify({ event: "transcript after abort", ...tail(id, 3) }, null, 2));
  } catch (e) {
    console.log(JSON.stringify({ event: "transcript unreadable", message: (e as Error).message }));
  }
}

function tail(id: string, n: number) {
  const lines = readFileSync(transcript(id), "utf8").trim().split("\n");
  return {
    lines: lines.length,
    tail: lines.slice(-n).map((l) => {
      const e = JSON.parse(l);
      const content = e.message?.content;
      const kinds = Array.isArray(content) ? content.map((c: any) => c.type + (c.name ? `:${c.name}` : "")) : typeof content;
      return { type: e.type, subtype: e.subtype, kinds, text: JSON.stringify(content ?? null).slice(0, 220) };
    }),
  };
}

async function resumed(id: string) {
  const q = query({
    prompt: "Without running anything: what codeword did I give you, and what shell command were you running when we were interrupted?",
    options: { resume: id, allowedTools: [], maxTurns: 2 } as never,
  });
  for await (const m of q) {
    if (m.type === "result") {
      console.log(JSON.stringify({ pid: process.pid, subtype: m.subtype, session_id: m.session_id, result: m.subtype === "success" ? m.result : (m as any).errors }));
    }
  }
  // How did the resumed session reconcile the tool_use the abort left dangling?
  const pairs = readFileSync(transcript(id), "utf8").trim().split("\n").flatMap((l) => {
    const e = JSON.parse(l);
    const c = e.message?.content;
    return Array.isArray(c) ? c.filter((b: any) => b.type === "tool_use" || b.type === "tool_result").map((b: any) => JSON.stringify(b).slice(0, 240)) : [];
  });
  console.log(JSON.stringify({ event: "tool blocks in transcript after resume", pairs }, null, 2));
}

const mode = process.argv[2];
if (mode === "cancel") await cancelled(process.argv[3]);
else if (mode === "resume") await resumed(process.argv[3]);
else {
  const id = randomUUID();
  console.log(`session id=${id}`);
  const tsx = new URL("../../node_modules/.bin/tsx", import.meta.url).pathname;
  const self = new URL(import.meta.url).pathname;
  for (const phase of ["cancel", "resume"]) {
    console.log(`--- ${phase} process ---`);
    const r = spawnSync(tsx, [self, phase, id], { stdio: "inherit" });
    console.log(`--- ${phase} process exited status=${r.status} signal=${r.signal} ---`);
  }
  console.log("stray tool processes after cancel:", spawnSync("pgrep", ["-a", "-x", "python3"], { encoding: "utf8" }).stdout.trim() || "none");
}
