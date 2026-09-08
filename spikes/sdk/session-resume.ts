// Q1: can a caller-chosen session id be created in one process and resumed in
// another, with the model still holding the first turn's content?
//
// Run: pnpm exec tsx spikes/sdk/session-resume.ts
// The driver spawns two child processes; each runs one turn and exits.

import { query } from "@anthropic-ai/claude-agent-sdk";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";

const CODEWORD = "TANGERINE-4417";

async function turn(prompt: string, opts: Record<string, unknown>) {
  const q = query({ prompt, options: { allowedTools: [], maxTurns: 2, ...opts } as never });
  for await (const m of q) {
    if (m.type === "result") {
      console.log(JSON.stringify({
        pid: process.pid,
        subtype: m.subtype,
        session_id: m.session_id,
        result: m.subtype === "success" ? m.result : (m as { errors?: string[] }).errors,
      }));
    }
  }
}

const mode = process.argv[2];
if (mode === "first") {
  await turn(`Remember this codeword: ${CODEWORD}. Reply with just: OK`, { sessionId: process.argv[3] });
} else if (mode === "second") {
  await turn("What codeword did I give you earlier in this conversation? Reply with the codeword only.", { resume: process.argv[3] });
} else if (mode === "elsewhere") {
  // Same session id, resumed from a different working directory: transcripts
  // are filed per project directory, so this asks whether cwd is part of the key.
  await turn("What codeword did I give you earlier in this conversation? Reply with the codeword only.", { resume: process.argv[3], cwd: "/tmp" });
} else {
  const id = randomUUID();
  console.log(`driver pid=${process.pid} caller-chosen session id=${id}`);
  const tsx = new URL("../../node_modules/.bin/tsx", import.meta.url).pathname;
  const self = new URL(import.meta.url).pathname;
  for (const phase of ["first", "second", "elsewhere"]) {
    console.log(`--- ${phase} process ---`);
    const r = spawnSync(tsx, [self, phase, id], { stdio: "inherit" });
    if (r.status !== 0) process.exit(r.status ?? 1);
  }
}
