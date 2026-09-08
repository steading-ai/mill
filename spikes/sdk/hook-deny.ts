// Q6: can a PreToolUse hook refuse a tool call before it runs, and does the
// refusal reach the model as a message it can act on?
//
// Run: pnpm exec tsx spikes/sdk/hook-deny.ts

import { query } from "@anthropic-ai/claude-agent-sdk";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const dir = mkdtempSync(join(tmpdir(), "spike-hook-"));
const allowed = join(dir, "allowed.txt");
const denied = join(dir, "denied.txt");
const hookCalls: unknown[] = [];

const q = query({
  prompt:
    `Write the file ${denied} with contents X. If that fails, write ${allowed} with contents Y instead, ` +
    `and in your final message quote verbatim the reason the first write failed.`,
  options: {
    cwd: dir,
    allowedTools: ["Write"],
    permissionMode: "bypassPermissions",
    dangerouslySkipPermissions: true,
    maxTurns: 8,
    hooks: {
      PreToolUse: [
        {
          hooks: [
            async (input: any) => {
              const path = input.tool_input?.file_path ?? "";
              const deny = path.endsWith("denied.txt");
              hookCalls.push({ tool: input.tool_name, path, decision: deny ? "deny" : "allow" });
              return {
                continue: true,
                hookSpecificOutput: {
                  hookEventName: "PreToolUse",
                  permissionDecision: deny ? "deny" : "allow",
                  permissionDecisionReason: deny
                    ? "spike-hook: writing denied.txt is forbidden by the step's policy"
                    : "spike-hook: allowed",
                },
              } as never;
            },
          ],
        },
      ],
    },
  } as never,
});

for await (const m of q) {
  if (m.type === "user") {
    for (const b of ((m.message.content as any[]) ?? [])) {
      if (b.type === "tool_result") {
        console.log(JSON.stringify({ event: "tool_result reaching the model", is_error: b.is_error, content: b.content }));
      }
    }
  }
  if (m.type === "result") {
    console.log(JSON.stringify({
      hook_invocations: hookCalls,
      denied_file_written: existsSync(denied),
      allowed_file_written: existsSync(allowed),
      permission_denials: (m as any).permission_denials,
      final: m.subtype === "success" ? m.result : m.subtype,
    }, null, 2));
  }
}
rmSync(dir, { recursive: true, force: true });
