// stdio ↔ unix socket, the shape of bridge a sidecar would run if the workspace
// listens on a socket and the SDK only speaks stdio. Equivalent to
// `socat STDIO UNIX-CONNECT:<sock>`, which this host does not have.
//
// Run: pnpm exec tsx spikes/mcp/socket-bridge.ts <socket-path>

import { connect } from "node:net";

const socket = connect(process.argv[2]);
socket.on("error", (e) => {
  console.error(String(e));
  process.exit(1);
});
process.stdin.pipe(socket);
socket.pipe(process.stdout);
socket.on("close", () => process.exit(0));
