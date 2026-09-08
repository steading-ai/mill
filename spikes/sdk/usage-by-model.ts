// Q4: does the terminal result report usage per model, including auxiliary
// models billed on the same call? One Opus turn that delegates to a Haiku
// subagent; the result's `usage` and `modelUsage` are both printed.
//
// Run: pnpm exec tsx spikes/sdk/usage-by-model.ts

import { query } from "@anthropic-ai/claude-agent-sdk";

const q = query({
  prompt:
    "Use the Task tool with subagent_type 'probe' to ask a subagent for a one-word colour. " +
    "Then reply with just that word.",
  options: {
    model: "opus",
    allowedTools: ["Task"],
    maxTurns: 6,
    agents: {
      probe: {
        description: "Answers one trivial question. Used to force a second model onto the bill.",
        prompt: "Answer in one word. Use no tools.",
        model: "haiku",
        tools: [],
      },
    },
  } as never,
});

for await (const m of q) {
  if (m.type === "result") {
    console.log(JSON.stringify({
      subtype: m.subtype,
      result: m.subtype === "success" ? m.result : undefined,
      num_turns: m.num_turns,
      total_cost_usd: m.total_cost_usd,
      usage_main_loop_only: m.usage,
      modelUsage: m.modelUsage,
    }, null, 2));
  }
}
