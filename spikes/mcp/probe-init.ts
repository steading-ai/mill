// What the init message says about a stdio MCP server passed through the SDK.
// Run: pnpm exec tsx spikes/mcp/probe-init.ts
import { query } from "@anthropic-ai/claude-agent-sdk";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

async function main() {
  const root = mkdtempSync(join(tmpdir(), "ws-probe-"));
  const q = query({
    prompt: "Say READY and stop.",
    options: {
      cwd: root,
      maxTurns: 1,
      strictMcpConfig: true,
      settingSources: [],
      mcpServers: {
        mill_workspace: { type: "stdio", command: process.cwd() + "/node_modules/.bin/tsx", args: [process.cwd() + "/spikes/mcp/workspace-server.ts", "stdio", root], alwaysLoad: true },
      },
    } as never,
  });
  for await (const m of q as AsyncIterable<any>) {
    if (m.type === "system" && m.subtype === "init") console.log(JSON.stringify({ tools: m.tools, mcp_servers: m.mcp_servers, errors: m.mcp_server_errors }));
    if (m.type === "result") console.log(JSON.stringify({ subtype: m.subtype, result: m.result }));
  }
}
main().catch((e) => { console.error(e); process.exit(1); });
