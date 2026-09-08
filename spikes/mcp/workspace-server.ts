// The throwaway workspace MCP server: fs_read, fs_write and exec, confined to
// one root directory, exposed over whichever transport the caller asks for.
//
//   stdio:  pnpm exec tsx spikes/mcp/workspace-server.ts stdio <root>
//   unix:   pnpm exec tsx spikes/mcp/workspace-server.ts unix <root> <socket-path>
//   http:   pnpm exec tsx spikes/mcp/workspace-server.ts http <root> <port>
//   unix-jsonrpc:
//           pnpm exec tsx spikes/mcp/workspace-server.ts unix-jsonrpc <root> <socket-path>
//           the same MCP, framed the way stdio frames it, so a bridge can carry it
//
// It also exports makeServer() so a driver can mount the same tools in-process
// through the agent SDK's createSdkMcpServer.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { execFile } from "node:child_process";
import { appendFileSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { createServer } from "node:http";
import { createServer as createSocketServer, type Socket } from "node:net";
import { dirname, resolve } from "node:path";
import { z } from "zod";

// Every call is appended here so a driver can count calls and per-call latency
// server-side, independently of what the transcript says.
const log = process.env.WS_LOG;
function record(entry: Record<string, unknown>) {
  if (log) appendFileSync(log, JSON.stringify({ at: Date.now(), ...entry }) + "\n");
}

function confine(root: string, p: string): string {
  const abs = resolve(root, p);
  if (abs !== root && !abs.startsWith(root + "/")) throw new Error(`path escapes the workspace: ${p}`);
  return abs;
}

export function makeServer(root: string): McpServer {
  const server = new McpServer(
    { name: "workspace", version: "0.0.0" },
    {
      instructions:
        "The workspace. Every file read, file write and command in this session goes through these tools. " +
        "Paths are relative to the workspace root. There is no other filesystem.",
    },
  );

  server.registerTool(
    "fs_read",
    {
      description: "Read a file from the workspace. Returns its full contents.",
      inputSchema: { path: z.string().describe("path relative to the workspace root") },
      annotations: { readOnlyHint: true },
    },
    async ({ path }) => {
      const t = Date.now();
      try {
        const text = readFileSync(confine(root, path), "utf8");
        record({ tool: "fs_read", path, ms: Date.now() - t, ok: true });
        return { content: [{ type: "text", text }] };
      } catch (e: any) {
        record({ tool: "fs_read", path, ms: Date.now() - t, ok: false, error: e.message });
        return { content: [{ type: "text", text: e.message }], isError: true };
      }
    },
  );

  server.registerTool(
    "fs_write",
    {
      description: "Write a file in the workspace, creating or replacing it. Returns the byte count written.",
      inputSchema: {
        path: z.string().describe("path relative to the workspace root"),
        content: z.string().describe("the file's complete new contents"),
      },
    },
    async ({ path, content }) => {
      const t = Date.now();
      try {
        const abs = confine(root, path);
        mkdirSync(dirname(abs), { recursive: true });
        writeFileSync(abs, content);
        record({ tool: "fs_write", path, bytes: content.length, ms: Date.now() - t, ok: true });
        return { content: [{ type: "text", text: `wrote ${content.length} bytes to ${path}` }] };
      } catch (e: any) {
        record({ tool: "fs_write", path, ms: Date.now() - t, ok: false, error: e.message });
        return { content: [{ type: "text", text: e.message }], isError: true };
      }
    },
  );

  server.registerTool(
    "exec",
    {
      description:
        "Run a command in the workspace. cwd, uid and environment are set by the workspace; " +
        "the command is not interpreted by a shell. Returns exit code, stdout and stderr.",
      inputSchema: {
        command: z.string().describe("the program to run"),
        args: z.array(z.string()).optional().describe("its arguments"),
      },
    },
    async ({ command, args }) =>
      new Promise((done) => {
        const t = Date.now();
        execFile(command, args ?? [], { cwd: root, env: { PATH: process.env.PATH ?? "", HOME: root } }, (err, stdout, stderr) => {
          const code = err && typeof (err as any).code === "number" ? (err as any).code : err ? 1 : 0;
          record({ tool: "exec", command, args, ms: Date.now() - t, exit: code });
          done({
            content: [{ type: "text", text: JSON.stringify({ exit: code, stdout, stderr }) }],
            isError: code !== 0,
          });
        });
      }),
  );

  return server;
}

// The same newline-delimited JSON-RPC framing the stdio transport uses, over a
// connected socket, so a bridge such as `socat STDIO UNIX-CONNECT:<sock>` can
// carry MCP without an HTTP layer in between.
class LineTransport {
  onmessage?: (m: unknown) => void;
  onerror?: (e: Error) => void;
  onclose?: () => void;
  private buffer = "";
  constructor(private socket: Socket) {}
  async start() {
    this.socket.setEncoding("utf8");
    this.socket.on("data", (chunk: string) => {
      this.buffer += chunk;
      for (let nl = this.buffer.indexOf("\n"); nl !== -1; nl = this.buffer.indexOf("\n")) {
        const line = this.buffer.slice(0, nl).trim();
        this.buffer = this.buffer.slice(nl + 1);
        if (line) {
          try {
            this.onmessage?.(JSON.parse(line));
          } catch (e: any) {
            this.onerror?.(e);
          }
        }
      }
    });
    this.socket.on("close", () => this.onclose?.());
    this.socket.on("error", (e) => this.onerror?.(e));
  }
  async send(message: unknown) {
    this.socket.write(JSON.stringify(message) + "\n");
  }
  async close() {
    this.socket.end();
  }
}

async function main() {
  const [mode, rootArg, addr] = process.argv.slice(2);
  const root = resolve(rootArg);
  const server = makeServer(root);

  if (mode === "stdio") {
    await server.connect(new StdioServerTransport());
    return;
  }

  if (mode === "unix-jsonrpc") {
    createSocketServer((socket) => {
      const perConnection = makeServer(root);
      perConnection.connect(new LineTransport(socket) as never).catch((e) => record({ event: "connect failed", error: String(e) }));
    }).listen(addr, () => {
      record({ event: "listening", mode, addr });
      console.error(`workspace mcp listening ${mode} ${addr}`);
    });
    return;
  }

  // Both remote modes are the same MCP streamable-HTTP transport; only the
  // address the node:http server listens on differs. A fresh server and
  // transport per request: the CLI opens with a `server/discover` probe whose
  // method this MCP SDK does not know, and a shared stateless transport does
  // not survive it.
  const http = createServer((req, res) => {
    const perRequest = makeServer(root);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => {
      transport.close().catch(() => {});
      perRequest.close().catch(() => {});
    });
    perRequest
      .connect(transport)
      .then(() => transport.handleRequest(req, res))
      .catch((e) => {
        record({ event: "request failed", error: String(e) });
        if (!res.headersSent) res.statusCode = 500;
        res.end(String(e));
      });
  });
  http.listen(mode === "unix" ? addr : Number(addr), () => {
    record({ event: "listening", mode, addr });
    console.error(`workspace mcp listening ${mode} ${addr}`);
  });
}

if (process.argv[1]?.endsWith("workspace-server.ts")) {
  main().catch((e) => {
    console.error(e);
    process.exit(1);
  });
}
