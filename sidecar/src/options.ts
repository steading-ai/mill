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

// PERMISSION_MODES are the modes a step may name. `auto` — the model
// classifier — is the default, and the whole reason this option exists: an
// unattended session under the SDK's `default` mode is prompted for
// permission and nothing answers, so the first end-to-end run stopped at
// `git add` with "This command requires approval".
//
// `bypassPermissions` is deliberately absent. The spike recommended it
// (docs/spikes/sdk-sessions.md, and its addendum of 2026-09-09), and it is
// refused here until the workspace is a sandbox (docs/roadmap.md §O4):
// today a session runs as the mill's own account on the mill's own host, and
// a mode that says yes to everything has nothing but the tool policy between
// it and that host. The classifier answers; `tools.deny` and the workspace's
// socket are what enforce.
export const PERMISSION_MODES = ["auto", "default", "acceptEdits", "dontAsk", "plan"] as const;

export type PermissionMode = (typeof PERMISSION_MODES)[number];

// DEFAULT_PERMISSION_MODE is what a step that names none runs under.
export const DEFAULT_PERMISSION_MODE: PermissionMode = "auto";

// BYPASS is the mode this harness refuses, named so the refusal can say so.
const BYPASS = "bypassPermissions";

// Env is the step's carve-out from the environment its session's tools see.
// `passthrough` copies named variables from the sidecar's own environment,
// which under `--env-mode strict` is the floor plus what the same key told
// the adapter to carry that far; `set` sets literals. Neither can reach a
// deny-listed variable: the adapter refuses the step before the sidecar is
// spawned, so a name on the deny list never arrives here with a value.
export interface Env {
  passthrough?: string[];
  set?: Record<string, string>;
}

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
  permissionMode?: PermissionMode;
  env?: Env;
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
      case "permission_mode":
        options.permissionMode = permissionModeAt(key, value);
        break;
      case "env":
        options.env = envAt(key, value);
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

function permissionModeAt(key: string, value: JsonValue): PermissionMode {
  const named = stringAt(key, value);
  if (named === BYPASS) {
    throw new PolicyError(
      `${key} ${BYPASS} is refused until the workspace is a sandbox (docs/roadmap.md §O4)`,
    );
  }
  const mode = PERMISSION_MODES.find((known) => known === named);
  if (mode === undefined) {
    throw new PolicyError(`${key} ${JSON.stringify(named)} is not one of ${PERMISSION_MODES.join(", ")}`);
  }
  return mode;
}

function envAt(key: string, value: JsonValue): Env {
  const mapping = mappingAt(key, value);
  const env: Env = {};
  for (const [name, entry] of Object.entries(mapping)) {
    switch (name) {
      case "passthrough":
        env.passthrough = stringsAt(`${key}.${name}`, entry);
        break;
      case "set":
        env.set = literalsAt(`${key}.${name}`, entry);
        break;
      default:
        throw new PolicyError(`${key}.${name} is not an option this harness has`);
    }
  }
  return env;
}

function literalsAt(key: string, value: JsonValue): Record<string, string> {
  const mapping = mappingAt(key, value);
  const literals: Record<string, string> = {};
  for (const [name, entry] of Object.entries(mapping)) {
    literals[name] = stringAt(`${key}.${name}`, entry);
  }
  return literals;
}

function mappingAt(key: string, value: JsonValue | undefined): { [k: string]: JsonValue } {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new PolicyError(`${key} must be a mapping, and is ${typeName(value)}`);
  }
  return value;
}

function toolsAt(key: string, value: JsonValue): Tools {
  const tools: Tools = {};
  for (const [name, list] of Object.entries(mappingAt(key, value))) {
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
