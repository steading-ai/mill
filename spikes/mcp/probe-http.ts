// Why does an http MCP server fail where a stdio one connects? Ask the CLI:
// mcpServerStatus() carries the error string the init message omits.
// Run: pnpm exec tsx spikes/mcp/probe-http.ts <url>
import { query } from "@anthropic-ai/claude-agent-sdk";

async function main() {
  const url = process.argv[2] ?? "http://127.0.0.1:8931/mcp";
  async function* input() {
    yield { type: "user" as const, message: { role: "user" as const, content: "Say READY and stop." }, parent_tool_use_id: null, session_id: "" };
    await new Promise(() => {}); // hold the input stream open so the CLI stays up
  }
  const q = query({
    prompt: input(),
    options: {
      maxTurns: 1,
      settingSources: [],
      strictMcpConfig: true,
      mcpServers: { mill_workspace: { type: "http", url, alwaysLoad: true } },
    } as never,
  });
  for await (const m of q as AsyncIterable<any>) {
    if (m.type === "system" && m.subtype === "init") {
      await new Promise((r) => setTimeout(r, 6000));
      console.log(JSON.stringify(await q.mcpServerStatus(), null, 1));
      break;
    }
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
