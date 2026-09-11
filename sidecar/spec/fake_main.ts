// The sidecar, wired to the fake harness.
//
// Identical to `dist/main.js` in every respect but the harness it loads, so
// a spec that drives this process is testing the sidecar's own framing,
// exits and usage rather than the SDK's.

import { run, silenceBrokenPipe } from "../src/main.js";
import { fakeQuery } from "./fake_sdk.js";

process.stdin.setEncoding("utf8");
silenceBrokenPipe(process.stdout);

const status = await run({
  stdin: process.stdin,
  stdout: process.stdout,
  stderr: process.stderr,
  env: process.env,
  onCancel: (handler) => {
    process.on("SIGTERM", handler);
    process.on("SIGINT", handler);
  },
  uid: () => process.getuid?.() ?? 0,
  pid: process.pid,
  loadQuery: async () => fakeQuery(process.env),
});

process.exit(status);
