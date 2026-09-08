// A plain MCP client against the workspace server over stdio: does it speak the
// protocol, do the three tools work, and is the root confinement real?
//
// Run: pnpm exec tsx spikes/mcp/smoke.ts

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

async function main() {
  const root = mkdtempSync(join(tmpdir(), "ws-smoke-"));
  writeFileSync(join(root, "a.txt"), "hello\n");
  const c = new Client({ name: "smoke", version: "0.0.0" });
  await c.connect(
    new StdioClientTransport({ command: "pnpm", args: ["exec", "tsx", "spikes/mcp/workspace-server.ts", "stdio", root] }),
  );
  console.log("tools:", (await c.listTools()).tools.map((t) => t.name));
  for (const call of [
    { name: "fs_read", arguments: { path: "a.txt" } },
    { name: "fs_write", arguments: { path: "b.txt", content: "x" } },
    { name: "exec", arguments: { command: "ls", args: ["-1"] } },
    { name: "fs_read", arguments: { path: "../../../etc/passwd" } },
  ]) {
    const r: any = await c.callTool(call as never);
    console.log(call.name, JSON.stringify({ isError: r.isError ?? false, text: r.content?.[0]?.text }));
  }
  await c.close();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
