// The sidecar: one request on stdin, its events and one terminal record on
// stdout, exit 0.
//
// The framing is newline-delimited protojson, one harness.v1 message per
// line, and the process is spawned per step: it reads a single Start or
// Resume, runs that session, and ends. Cancellation is not a request — it
// arrives as SIGTERM, because a process that is mid-turn is not reading its
// stdin.
//
// Exactly one Terminal leaves this process, on every path. A malformed
// frame, options that cannot be applied, a session the harness refuses to
// start, a cancellation that the harness never acknowledges: each is a
// Terminal whose exit says which, so the worker never has to infer a step's
// outcome from a stream that stopped. The exit code is 0 whenever the
// sidecar itself worked — the session's own outcome is in the record, not in
// the process's status.

import { create, fromJson, toJsonString } from "@bufbuild/protobuf";
import { pathToFileURL } from "node:url";

import {
  ExitReason,
  RequestSchema,
  ResponseSchema,
  TerminalSchema,
  type Event,
  type Response,
  type Terminal,
} from "../gen/harness/v1/harness_pb.js";
import { describe, neverRan } from "./exit.js";
import { loadQuery, type QueryFn } from "./sdk.js";
import { PolicyError } from "./options.js";
import { buildOptions, runSession, turnOf, type Turn } from "./session.js";
import { UsageAccumulator } from "./usage.js";

// GRACE_MS_VAR names the environment variable the worker sets to say how
// long the sidecar may take to stop cleanly after SIGTERM. It is on the
// environment rather than on the wire because Cancel reaches a running
// sidecar as a signal, which carries nothing.
export const GRACE_MS_VAR = "MILL_SIDECAR_GRACE_MS";

// DEFAULT_GRACE_MS is the grace window when the worker names none. After it
// elapses the sidecar reports the cancellation it was asked for and exits,
// whether or not the harness has acknowledged the abort.
export const DEFAULT_GRACE_MS = 5000;

// Io is everything the sidecar touches outside itself, named so a spec can
// hand it something else.
export interface Io {
  stdin: AsyncIterable<string | Uint8Array>;
  stdout: { write(chunk: string, callback: () => void): unknown };
  env: Record<string, string | undefined>;
  // onCancel registers the handler that runs when the worker asks the
  // session to stop.
  onCancel: (handler: () => void) => void;
  // uid is the invoking user, and pid this process. The breaker needs both:
  // the first is half of where the harness writes a background task's
  // output, the second is the root of the process tree it kills
  // (src/tasks.ts).
  uid: () => number;
  pid: number;
  // loadQuery resolves the harness. The default is the Claude Agent SDK.
  loadQuery: () => Promise<QueryFn>;
}

// readFirstLine returns the first newline-delimited frame on a stream, or
// everything before its end when it carries no newline. It stops reading at
// the newline: the sidecar handles one request, and whatever follows it on
// the pipe belongs to nobody.
export async function readFirstLine(stream: AsyncIterable<string | Uint8Array>): Promise<string> {
  let buffered = "";
  for await (const chunk of stream) {
    buffered += typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf8");
    const newline = buffered.indexOf("\n");
    if (newline >= 0) {
      return buffered.slice(0, newline);
    }
  }
  return buffered;
}

// requestIdOf recovers the request id from a frame that could not be
// understood, so even a refusal is correlated when the id was readable.
function requestIdOf(line: string): string {
  try {
    const parsed: unknown = JSON.parse(line);
    if (typeof parsed === "object" && parsed !== null && "id" in parsed && typeof parsed.id === "string") {
      return parsed.id;
    }
  } catch {
    // A frame that is not JSON has no id to recover.
  }
  return "";
}

function refusal(requestId: string, reason: ExitReason, message: string): Terminal {
  return create(TerminalSchema, { requestId, finalMessage: message, exit: neverRan(reason) });
}

function graceMs(env: Record<string, string | undefined>): number {
  const raw = env[GRACE_MS_VAR];
  const parsed = raw === undefined ? Number.NaN : Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_GRACE_MS;
}

// run reads one request, runs the session it asks for, and writes the
// session's events and terminal record. The number it returns is the
// process's exit status.
export async function run(io: Io): Promise<number> {
  const write = async (body: Response["body"]): Promise<void> => {
    const line = `${toJsonString(ResponseSchema, create(ResponseSchema, { body }), { useProtoFieldName: true })}\n`;
    await new Promise<void>((resolve) => {
      io.stdout.write(line, resolve);
    });
  };
  const writeTerminal = (terminal: Terminal): Promise<void> => write({ case: "terminal", value: terminal });

  const line = await readFirstLine(io.stdin);

  let turn: Turn;
  try {
    turn = turnOf(fromJson(RequestSchema, JSON.parse(line) as never));
  } catch (err) {
    await writeTerminal(refusal(requestIdOf(line), ExitReason.REFUSED, describe(err)));
    return 0;
  }

  const controller = new AbortController();
  let cancelled = false;

  let options;
  try {
    options = buildOptions(turn, io.env, controller);
  } catch (err) {
    const reason = err instanceof PolicyError ? ExitReason.POLICY : ExitReason.REFUSED;
    await writeTerminal(refusal(turn.requestId, reason, describe(err)));
    return 0;
  }

  // The events are queued rather than awaited: a turn streams from the
  // harness and must not block on the worker's pipe mid-message. They are
  // drained in order before the terminal record is written, which is what
  // makes the terminal the last line of the request.
  const pending: Promise<void>[] = [];
  const emit = (event: Event): void => {
    pending.push(write({ case: "event", value: event }));
  };

  const accumulated = new UsageAccumulator();
  const query = await io.loadQuery();

  // A turn in flight holds nothing of the sidecar's own: stdin is read to
  // its first newline and closed, and the harness's child process belongs to
  // the harness. Without a handle of its own Node would treat a session that
  // is merely waiting as a finished program, so the sidecar keeps one until
  // it has a terminal record to write.
  const inFlight = setInterval(() => {}, 1 << 30);

  // SIGTERM aborts the turn and starts the grace window. The harness
  // answers an abort by throwing, which runSession turns into a cancelled
  // terminal; if it does not answer within the window, the sidecar reports
  // the cancellation itself, with whatever usage the turn had streamed.
  let expire: (value: "expired") => void = () => {};
  const expired = new Promise<"expired">((resolve) => {
    expire = resolve;
  });
  io.onCancel(() => {
    if (cancelled) {
      return;
    }
    cancelled = true;
    controller.abort();
    setTimeout(() => {
      expire("expired");
    }, graceMs(io.env)).unref();
  });

  const outcome = await Promise.race([
    runSession({
      query,
      turn,
      options,
      emit,
      accumulated,
      cancelled: () => cancelled,
      env: io.env,
      uid: io.uid(),
      pid: io.pid,
    }),
    expired,
  ]);

  const terminal =
    outcome === "expired"
      ? create(TerminalSchema, {
          requestId: turn.requestId,
          finalMessage: "the session did not stop within the grace window",
          usage: accumulated.total(),
          exit: neverRan(ExitReason.CANCELLED),
        })
      : outcome;

  clearInterval(inFlight);
  await Promise.all(pending);
  await writeTerminal(terminal);
  return 0;
}

// silenceBrokenPipe keeps a worker that has already gone away from ending
// the sidecar in a stack trace. There is nothing left to report the failure
// to, so the process simply stops.
export function silenceBrokenPipe(stream: { on(event: "error", handler: (err: NodeJS.ErrnoException) => void): unknown }): void {
  stream.on("error", (err) => {
    if (err.code !== "EPIPE") {
      throw err;
    }
    process.exit(0);
  });
}

// bootstrap runs the sidecar against the real process and the real harness.
export async function bootstrap(): Promise<void> {
  process.stdin.setEncoding("utf8");
  silenceBrokenPipe(process.stdout);
  const status = await run({
    stdin: process.stdin,
    stdout: process.stdout,
    env: process.env,
    onCancel: (handler) => {
      process.on("SIGTERM", handler);
      process.on("SIGINT", handler);
    },
    uid: () => process.getuid?.() ?? 0,
    pid: process.pid,
    loadQuery,
  });
  // An unstopped harness child would otherwise hold the event loop open past
  // the terminal record the worker is waiting for.
  process.exit(status);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await bootstrap();
}
