// Can the SDK reach an MCP server over a unix socket, or only over stdio?
//
// The workspace server is started listening on a unix socket, then every URL
// spelling that could denote it is handed to the SDK as an `http` (and `sse`)
// server; then a stdio bridge (stdin/stdout piped onto the socket, the shape
// `socat STDIO UNIX-CONNECT:` would take) is tried against the same socket
// speaking HTTP and speaking line-delimited JSON-RPC. Each attempt reports what the CLI made of it: the `mcp_servers` and
// `mcp_server_errors` entries on the init message, and whether the server's
// tools appeared in the session's tool list. The controls at the end are the
// transports that are meant to work: a stdio child process, the in-process SDK
// server, and the same HTTP server on a TCP port.
//
// Run: pnpm exec tsx spikes/mcp/unix-socket.ts

import { query } from "@anthropic-ai/claude-agent-sdk";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeServer } from "./workspace-server.js";

const REPO = process.cwd();
const TSX = join(REPO, "node_modules/.bin/tsx");
const SERVER = join(REPO, "spikes/mcp/workspace-server.ts");
const BRIDGE = join(REPO, "spikes/mcp/socket-bridge.ts");
const NAME = "mill_workspace"; // "workspace" is refused: reserved_name

async function attempt(label: string, config: unknown) {
  const seen = { status: "no init message", error: undefined as unknown, tools: [] as string[] };
  try {
    const q = query({
      prompt: "Say READY and stop.",
      options: {
        cwd: REPO,
        maxTurns: 1,
        settingSources: [],
        strictMcpConfig: true,
        mcpServers: { [NAME]: config },
      } as never,
    });
    for await (const m of q as AsyncIterable<any>) {
      if (m.type === "system" && m.subtype === "init") {
        const entry = (m.mcp_servers ?? []).find((s: any) => s.name === NAME);
        seen.status = entry?.status ?? "absent";
        seen.error = entry?.error ?? (m.mcp_server_errors ?? []).find((e: any) => e.name === NAME);
        seen.tools = (m.tools ?? []).filter((t: string) => t.startsWith(`mcp__${NAME}__`));
      }
    }
  } catch (e: any) {
    seen.error = e.message;
  }
  console.log(JSON.stringify({ attempt: label, config: typeof config === "object" && (config as any).instance ? { type: "sdk", name: (config as any).name } : config, ...seen }));
}

async function main() {
  const root = mkdtempSync(join(tmpdir(), "ws-unix-"));
  const sock = join(root, "workspace.sock");
  writeFileSync(join(root, "a.txt"), "hello\n");

  const server = spawn(TSX, [SERVER, "unix", root, sock], { cwd: REPO, stdio: ["ignore", "inherit", "inherit"] });
  for (let i = 0; i < 100 && !existsSync(sock); i++) await new Promise((r) => setTimeout(r, 200));
  console.log(JSON.stringify({ socket: sock, exists: existsSync(sock) }));

  await attempt("http, curl's --unix-socket spelling", { type: "http", url: `http://unix:${sock}:/mcp`, alwaysLoad: true });
  await attempt("http, http+unix scheme", { type: "http", url: `http+unix://${encodeURIComponent(sock)}/mcp`, alwaysLoad: true });
  await attempt("http, unix scheme", { type: "http", url: `unix://${sock}:/mcp`, alwaysLoad: true });
  await attempt("http, bare socket path as url", { type: "http", url: sock, alwaysLoad: true });
  await attempt("sse, curl's --unix-socket spelling", { type: "sse", url: `http://unix:${sock}:/mcp`, alwaysLoad: true });
  await attempt("stdio bridge onto the http socket", { type: "stdio", command: TSX, args: [BRIDGE, sock], alwaysLoad: true });

  // The same socket speaking newline-delimited JSON-RPC instead of HTTP, which
  // is what a stdio bridge can actually carry.
  const jsonrpcSock = join(root, "workspace-jsonrpc.sock");
  const jsonrpc = spawn(TSX, [SERVER, "unix-jsonrpc", root, jsonrpcSock], { cwd: REPO, stdio: ["ignore", "inherit", "inherit"] });
  for (let i = 0; i < 100 && !existsSync(jsonrpcSock); i++) await new Promise((r) => setTimeout(r, 200));
  await attempt("stdio bridge onto a line-delimited JSON-RPC socket", { type: "stdio", command: TSX, args: [BRIDGE, jsonrpcSock], alwaysLoad: true });
  await attempt("http, curl spelling, against the JSON-RPC socket", { type: "http", url: `http://unix:${jsonrpcSock}:/mcp`, alwaysLoad: true });
  await attempt("stdio: the server as a child process", { type: "stdio", command: TSX, args: [SERVER, "stdio", root], alwaysLoad: true });
  await attempt("sdk: the server in the driver's own process", { type: "sdk", name: NAME, instance: makeServer(root) });

  const tcp = spawn(TSX, [SERVER, "http", root, "8931"], { cwd: REPO, stdio: ["ignore", "inherit", "inherit"] });
  await new Promise((r) => setTimeout(r, 3000));
  await attempt("http over a TCP port, as a control", { type: "http", url: "http://127.0.0.1:8931/mcp", alwaysLoad: true });

  tcp.kill();
  jsonrpc.kill();
  server.kill();
  rmSync(root, { recursive: true, force: true });
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
