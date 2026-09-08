// Reads one JSON-encoded harness.v1.Terminal on stdin, parses it with the
// generated TypeScript type, re-serializes it, and writes the result to
// stdout. The Go side of the wire drives this to prove that a Terminal it
// constructed survives the trip through the sidecar's types unchanged.
//
// Run with `node sidecar/spec/terminal_roundtrip.ts`; node strips the types.

import { fromJson, toJson } from "@bufbuild/protobuf";
import { readFileSync } from "node:fs";
import { TerminalSchema } from "../gen/harness/v1/harness_pb.js";

const input = readFileSync(0, "utf8");
const terminal = fromJson(TerminalSchema, JSON.parse(input));

// Touch the per-model usage map through the generated types rather than the
// JSON, so a map that failed to type would fail here and not merely pass a
// string through.
for (const [model, counts] of Object.entries(terminal.usage?.byModel ?? {})) {
  if (typeof model !== "string" || typeof counts.input !== "bigint") {
    throw new Error(`usage entry ${model} is not typed as a model to token counts`);
  }
}

process.stdout.write(JSON.stringify(toJson(TerminalSchema, terminal)));
