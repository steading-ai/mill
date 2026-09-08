// Q2: is `outputFormat: { type: "json_schema" }` a constraint or a suggestion?
// Ten independent sessions, one non-trivial schema (nested object, enum,
// required array), conformance counted.
//
// Run: pnpm exec tsx spikes/sdk/structured-output.ts

import { query } from "@anthropic-ai/claude-agent-sdk";

const schema = {
  type: "object",
  additionalProperties: false,
  required: ["verdict", "findings", "summary"],
  properties: {
    verdict: { type: "string", enum: ["approve", "revise", "needs_human"] },
    findings: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["criterion", "severity", "note"],
        properties: {
          criterion: { type: "string" },
          severity: { type: "string", enum: ["blocker", "major", "minor"] },
          note: { type: "string" },
        },
      },
    },
    summary: {
      type: "object",
      additionalProperties: false,
      required: ["headline", "confidence"],
      properties: { headline: { type: "string" }, confidence: { type: "number" } },
    },
  },
} as const;

// Enough of JSON Schema to judge this one schema. Not a library.
function validate(node: any, value: unknown, path = "$"): string[] {
  const errs: string[] = [];
  if (node.type === "object") {
    if (typeof value !== "object" || value === null || Array.isArray(value)) return [`${path}: not an object`];
    const v = value as Record<string, unknown>;
    for (const k of node.required ?? []) if (!(k in v)) errs.push(`${path}.${k}: missing`);
    for (const k of Object.keys(v)) {
      if (!(k in node.properties)) { if (node.additionalProperties === false) errs.push(`${path}.${k}: additional property`); continue; }
      errs.push(...validate(node.properties[k], v[k], `${path}.${k}`));
    }
  } else if (node.type === "array") {
    if (!Array.isArray(value)) return [`${path}: not an array`];
    value.forEach((v, i) => errs.push(...validate(node.items, v, `${path}[${i}]`)));
  } else if (node.type === "string") {
    if (typeof value !== "string") errs.push(`${path}: not a string`);
    else if (node.enum && !node.enum.includes(value)) errs.push(`${path}: ${JSON.stringify(value)} not in enum`);
  } else if (node.type === "number") {
    if (typeof value !== "number") errs.push(`${path}: not a number`);
  }
  return errs;
}

const prompt = `You are reviewing a change against two acceptance criteria.
AC-1: "the CLI prints the version". AC-2: "the CLI exits non-zero on an unknown flag".
The change prints the version but exits 0 on unknown flags.
Report your verdict, one finding per failing criterion, and a summary.`;

let conforming = 0;
const runs: unknown[] = [];
for (let i = 1; i <= 10; i++) {
  const q = query({
    prompt,
    options: {
      allowedTools: [],
      maxTurns: 2,
      outputFormat: { type: "json_schema", schema: schema as never },
    },
  });
  let record: any = { run: i };
  for await (const m of q) {
    if (m.type === "result") {
      record.subtype = m.subtype;
      const so = (m as { structured_output?: unknown }).structured_output;
      record.present = so !== undefined;
      record.errors = so === undefined ? ["structured_output absent"] : validate(schema, so);
      record.value = so;
    }
  }
  if (record.present && record.errors.length === 0) conforming++;
  runs.push(record);
  console.log(JSON.stringify({ run: i, subtype: record.subtype, present: record.present, errors: record.errors }));
}
console.log(JSON.stringify({ conforming, of: 10 }));
console.log("sample:", JSON.stringify((runs[0] as any).value, null, 2));
