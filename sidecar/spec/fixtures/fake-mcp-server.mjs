#!/usr/bin/env node
// A minimal stdio MCP server: initialize, tools/list (one tool) and one
// tools/call, framed as newline-delimited JSON-RPC — the same framing the
// SDK's own stdio transport uses (spikes/mcp/workspace-server.ts's
// LineTransport frames a socket the same way). Exists to prove a mount
// without the real Playwright server, which is stead's to provide
// (mill#19 Notes).

import { createInterface } from "node:readline";

const rl = createInterface({ input: process.stdin, terminal: false });

function reply(id, result) {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id, result }) + "\n");
}

rl.on("line", (line) => {
  if (line.trim() === "") {
    return;
  }
  const message = JSON.parse(line);
  switch (message.method) {
    case "initialize":
      reply(message.id, {
        protocolVersion: "2024-11-05",
        capabilities: { tools: {} },
        serverInfo: { name: "fake", version: "0.0.0" },
      });
      return;
    case "tools/list":
      reply(message.id, {
        tools: [
          {
            name: "fake_tool",
            description: "Echoes its argument.",
            inputSchema: { type: "object", properties: { echo: { type: "string" } } },
          },
        ],
      });
      return;
    case "tools/call":
      reply(message.id, {
        content: [{ type: "text", text: `echoed: ${message.params?.arguments?.echo ?? ""}` }],
      });
      return;
    default:
      // A notification (no id) or a method this fixture does not answer.
      return;
  }
});
