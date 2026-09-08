// How a session ended, in the wire's terms.
//
// `ProcessExit.status = -1` means the session never ran and `reason` says
// why; any other status is one the session actually reached. Keeping those
// apart is what lets the worker's retry policy do the right thing, so every
// path out of a turn comes through this file rather than building a
// ProcessExit inline.

import { create } from "@bufbuild/protobuf";

import { ExitReason, ProcessExitSchema, type ProcessExit } from "../gen/harness/v1/harness_pb.js";

// NEVER_RAN is the status that says no session reached a result.
export const NEVER_RAN = -1;

// neverRan is the exit for a session that was not started, or was stopped
// before it could produce one: `reason` carries the whole story.
export function neverRan(reason: ExitReason): ProcessExit {
  return create(ProcessExitSchema, { status: NEVER_RAN, reason });
}

// ran is the exit for a session that produced a result. `status` is 0 when
// the turn succeeded and non-zero when it failed, and `reason` stays unset:
// a session that ran needs no reason beyond its status.
export function ran(status: number, signal = 0): ProcessExit {
  return create(ProcessExitSchema, { status, signal, reason: ExitReason.UNSPECIFIED });
}

// SdkOutcome is the part of the SDK's terminal result that decides the exit.
// The SDK emits exactly one result message per turn; `subtype` is "success"
// when the turn completed and names the failure otherwise, and `is_error`
// marks a successful-shaped result that carries an API error instead of an
// answer.
export interface SdkOutcome {
  subtype: string;
  is_error?: boolean;
}

// fromResult maps the SDK's result message to an exit. Anything but a clean
// success is a session that ran and failed, which is a different thing from
// a session that never ran.
export function fromResult(result: SdkOutcome): ProcessExit {
  if (result.subtype === "success" && result.is_error !== true) {
    return ran(0);
  }
  return ran(1);
}

// ABORT_MESSAGE is what the SDK's iterator throws when the abort signal
// fires: cancellation is an exception, not a terminal result, so the
// sidecar recognises it and synthesises the record the wire needs.
// Observed in docs/spikes/sdk-sessions.md §Q5.
export const ABORT_MESSAGE = "Claude Code process aborted by user";

// isAbort says whether a thrown value is the SDK's cancellation. `aborted`
// is the sidecar's own record of having asked for one, which is the
// authority; the message is the fallback for an abort raised by anything
// else holding the signal.
export function isAbort(err: unknown, aborted: boolean): boolean {
  if (aborted) {
    return true;
  }
  if (err instanceof Error) {
    return err.name === "AbortError" || err.message.includes(ABORT_MESSAGE);
  }
  return false;
}

// fromError maps a thrown turn to an exit. A throw before the SDK produced
// any message is a session that never started — the SDK rejecting a
// malformed session id lands here — so it is `refused`. A throw after the
// session was already talking is a session that ran and failed.
export function fromError(err: unknown, sawMessage: boolean, aborted: boolean): ProcessExit {
  if (isAbort(err, aborted)) {
    return neverRan(ExitReason.CANCELLED);
  }
  return sawMessage ? ran(1) : neverRan(ExitReason.REFUSED);
}

// describe renders a thrown value for the terminal's final message. The
// worker's log wants a sentence, not a stack.
export function describe(err: unknown): string {
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}
