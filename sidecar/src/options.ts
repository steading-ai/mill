// The sidecar's own schema for the options a step hands it.
//
// A step's `harness.claude` block travels from the workflow to here
// untouched: the loader checks only that it is a mapping, the store keeps it
// as a document, and the adapter passes it through, because what a knob
// means is this side's to say (docs/domain-model.md §Step). This file is
// where it is said. Adding a knob is a change here and a change in a
// workflow, and the mill is not rebuilt for it.
//
// Everything not named below is refused. A document is written by hand and a
// misspelled key that were merely ignored would be a step running under
// settings its author did not ask for — the silent-drop failure the strict
// loader exists to prevent, one hop further along. The refusal is a
// PolicyError, so main answers it with EXIT_REASON_POLICY and never loads
// the SDK.

import type { JsonObject, JsonValue } from "@bufbuild/protobuf";

import { ExitReason } from "../gen/harness/v1/harness_pb.js";

// PolicyError is a request whose options the sidecar cannot apply: the
// session is never started under them rather than started under something
// else.
export class PolicyError extends Error {
  readonly reason = ExitReason.POLICY;
}

// EFFORT_LEVELS are the values the SDK accepts for `effort`. The document's
// value is a free string on the wire, so one outside this set is a setting
// the sidecar cannot apply rather than one it may guess at.
export const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

export type EffortLevel = (typeof EFFORT_LEVELS)[number];

// Tools is the step's tool policy. The two lists are not one mechanism with
// a sign: `allow` permits, leaving an excluded tool offered and refused at
// the permission layer, while `deny` removes the tool from the session
// (docs/spikes/sdk-sessions.md §Q3).
export interface Tools {
  allow?: string[];
  deny?: string[];
}

// HarnessOptions is the whole of what this harness understands today.
export interface HarnessOptions {
  model?: string;
  effort?: EffortLevel;
  tools?: Tools;
}

// parseOptions validates a step's options document and returns it typed. It
// throws a PolicyError naming the offending key, because the author of a
// workflow reading the ledger needs to know which line to fix.
export function parseOptions(document: JsonObject | undefined): HarnessOptions {
  if (document === undefined) {
    return {};
  }
  const options: HarnessOptions = {};
  for (const [key, value] of Object.entries(document)) {
    switch (key) {
      case "model":
        options.model = stringAt(key, value);
        break;
      case "effort":
        options.effort = effortAt(key, value);
        break;
      case "tools":
        options.tools = toolsAt(key, value);
        break;
      default:
        throw new PolicyError(`${key} is not an option this harness has`);
    }
  }
  return options;
}

function stringAt(key: string, value: JsonValue): string {
  if (typeof value !== "string") {
    throw new PolicyError(`${key} must be a string, and is ${typeName(value)}`);
  }
  return value;
}

function effortAt(key: string, value: JsonValue): EffortLevel {
  const effort = stringAt(key, value);
  const level = EFFORT_LEVELS.find((known) => known === effort);
  if (level === undefined) {
    throw new PolicyError(`${key} ${JSON.stringify(effort)} is not one of ${EFFORT_LEVELS.join(", ")}`);
  }
  return level;
}

function toolsAt(key: string, value: JsonValue): Tools {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new PolicyError(`${key} must be a mapping, and is ${typeName(value)}`);
  }
  const tools: Tools = {};
  for (const [name, list] of Object.entries(value)) {
    switch (name) {
      case "allow":
        tools.allow = stringsAt(`${key}.${name}`, list);
        break;
      case "deny":
        tools.deny = stringsAt(`${key}.${name}`, list);
        break;
      default:
        throw new PolicyError(`${key}.${name} is not an option this harness has`);
    }
  }
  return tools;
}

function stringsAt(key: string, value: JsonValue | undefined): string[] {
  if (!Array.isArray(value)) {
    throw new PolicyError(`${key} must be a list of tool names, and is ${typeName(value)}`);
  }
  return value.map((entry, index) => stringAt(`${key}[${index}]`, entry));
}

// typeName says what the document carried where something else was wanted,
// in the words a JSON document is written in.
function typeName(value: JsonValue | undefined): string {
  if (value === undefined) {
    return "absent";
  }
  if (value === null) {
    return "null";
  }
  return Array.isArray(value) ? "a list" : typeof value === "object" ? "a mapping" : `a ${typeof value}`;
}
