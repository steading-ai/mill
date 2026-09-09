// The wait, the heartbeat and the breaker.
//
// A session may background a command and end its turn to be woken when it
// finishes, so a turn's result message is not the end of the session: the
// sidecar keeps reading until a turn ends with no task still running
// (docs/spikes/sdk-sessions.md). That wait has no bound of its own, and
// twice on the reference harness the thing being waited on was a shell loop
// that could never end — `until ! pgrep -f "ginkgo"; do sleep 5; done`,
// whose own shell matches the pattern — with the step's hour the only thing
// that ended it.
//
// So the wait is watched. Every HEARTBEAT_MS the sidecar looks at each
// silent task's output file. Output that grew is work and earns silence.
// Output that did not grow earns one turn asking the session whether the
// task is still needed, counted as a strike. HEARTBEAT_STRIKES strikes trip
// the breaker: the session's child processes are killed, the last working
// turn's words are taken as the step's result, and the session ends with
// EXIT_REASON_STALLED so the ledger says which of the two happened.
//
// The constants are the reference harness's (harness/loop.py). Keep them
// the same unless a spec shows a reason.

import { readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { kill } from "node:process";

// HEARTBEAT_MS is how long a silent task is left alone between looks.
export const HEARTBEAT_MS = 180_000;

// HEARTBEAT_STRIKES is how many consecutive silent looks trip the breaker.
// The last one does not ask: it kills.
export const HEARTBEAT_STRIKES = 3;

// BASH_TIMEOUT_MS is the foreground cap the session's shell runs under. The
// harness backgrounds a foreground command that runs past it, and a
// backgrounded test run is what taught sessions to poll. Ten minutes: this
// repository's suite takes half a minute warm and a little over one cold.
export const BASH_TIMEOUT_MS = "600000";

// BASH_TIMEOUT_VAR is the name the harness reads that cap under.
export const BASH_TIMEOUT_VAR = "BASH_DEFAULT_TIMEOUT_MS";

// HEARTBEAT_MS_VAR shortens the wait between looks. The worker sets none —
// the constant above is the contract — and it exists because a spec cannot
// wait three minutes to watch a breaker trip.
export const HEARTBEAT_MS_VAR = "MILL_SIDECAR_HEARTBEAT_MS";

// heartbeatMs is the wait between looks: the constant, or the override when
// the environment carries a usable one.
export function heartbeatMs(env: Record<string, string | undefined>): number {
  const raw = env[HEARTBEAT_MS_VAR];
  const parsed = raw === undefined ? Number.NaN : Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : HEARTBEAT_MS;
}

// heartbeatPrompt is the one turn a silent task earns. It says how long the
// wait has been, names the tasks, offers both answers, and says what happens
// if nothing changes — a session that means to keep waiting has to say what
// it is waiting for, and one that is done has a tool to stop with.
export function heartbeatPrompt(minutes: number, tasks: string[], strike: number, strikes: number): string {
  return (
    `You ended your turn ${minutes} minutes ago with a background task still running and ` +
    `it has produced no output since: ${tasks.join("; ")}. Is your work done, or is that task ` +
    `still needed? If you are done, stop the task (TaskStop) and say so in one line. ` +
    `If it is needed, say what it is doing and wait on it with TaskOutput, not a shell ` +
    `loop. This is heartbeat ${strike} of ${strikes}; after the last one the task is killed ` +
    `and your last summary is taken as your result.`
  );
}

// TERMINAL_TASK_STATUSES are the `task_updated` statuses that mean a task is
// over. A task that reaches one is no longer something the session is
// waiting to be woken for.
const TERMINAL_TASK_STATUSES = new Set(["completed", "failed", "killed"]);

// taskOutputPath is where the harness writes a background task's output.
//
// The harness names the file itself and reports it only once the task has
// settled (`task_notification.output_file`), which is too late to watch it
// grow, so the path is reconstructed from the same three things the harness
// builds it out of: the invoking uid, the session's working directory, and
// the session and task ids. A layout that moves is a file that is never
// found, which reads as a silent task — the sidecar asks, and a session
// doing real work says so.
export function taskOutputPath(uid: number, cwd: string, sessionId: string, taskId: string): string {
  const slug = cwd.replaceAll("/", "-").replaceAll(".", "-");
  return join(`/tmp/claude-${uid}`, slug, sessionId, "tasks", `${taskId}.output`);
}

// outputSize is how much a task has written, or -1 when there is no file to
// read. Absent and empty are deliberately the same size, so a task that
// never writes anything is silent from its first look.
function outputSize(path: string): number {
  try {
    return statSync(path).size;
  } catch {
    return -1;
  }
}

// Task is one background task the harness said it would wake the session
// for.
interface Task {
  description: string;
  // seen is the task's output size at the last look.
  seen: number;
}

// Look is what one look at the silent tasks found.
export type Look =
  | { case: "working" }
  | { case: "silent"; tasks: string[] };

// Tasks is the set of background tasks a session is waiting on, fed the
// harness's own task messages.
export class Tasks {
  private readonly active = new Map<string, Task>();
  private sessionId = "";

  constructor(
    private readonly uid: number,
    private readonly cwd: string,
  ) {}

  // observe folds one harness message into the set. It answers whether the
  // message was one of the task bookkeeping messages, which is how the
  // caller knows a message it does not otherwise render still mattered.
  observe(message: { type?: string; subtype?: string; [key: string]: unknown }): boolean {
    if (message.type !== "system") {
      return false;
    }
    const sessionId = typeof message.session_id === "string" ? message.session_id : "";
    if (sessionId !== "" && this.sessionId === "") {
      this.sessionId = sessionId;
    }
    const taskId = typeof message.task_id === "string" ? message.task_id : "";
    switch (message.subtype) {
      case "task_started": {
        const description = typeof message.description === "string" ? message.description : taskId;
        this.active.set(taskId, { description, seen: outputSize(this.pathOf(taskId)) });
        return true;
      }
      case "task_notification":
        this.active.delete(taskId);
        return true;
      case "task_updated": {
        const patch = message.patch;
        const status =
          typeof patch === "object" && patch !== null && "status" in patch && typeof patch.status === "string"
            ? patch.status
            : "";
        if (TERMINAL_TASK_STATUSES.has(status)) {
          this.active.delete(taskId);
        }
        return true;
      }
      default:
        return false;
    }
  }

  // waiting reports whether a turn that has just ended leaves the session
  // waiting to be woken.
  waiting(): boolean {
    return this.active.size > 0;
  }

  // ids are the tasks still running, for the breaker to stop.
  ids(): string[] {
    return [...this.active.keys()];
  }

  // clear forgets every task, which is what the breaker leaves behind.
  clear(): void {
    this.active.clear();
  }

  // look reads every active task's output once and says whether any of them
  // is still producing. A task that grew resets nothing but its own record:
  // strikes are the caller's, and they count consecutive looks in which
  // nothing at all moved.
  look(): Look {
    const silent: string[] = [];
    let working = false;
    for (const [taskId, task] of this.active) {
      const size = outputSize(this.pathOf(taskId));
      if (size > task.seen) {
        working = true;
      } else {
        silent.push(task.description);
      }
      task.seen = size;
    }
    return working ? { case: "working" } : { case: "silent", tasks: silent };
  }

  private pathOf(taskId: string): string {
    return taskOutputPath(this.uid, this.cwd, this.sessionId, taskId);
  }
}

// children is every process under pid, deepest first, read from /proc. A
// platform without /proc reports none, which leaves the breaker with
// nothing to kill and the abort to do the work.
export function children(pid: number): number[] {
  let listed: string;
  try {
    listed = readFileSync(`/proc/${pid}/task/${pid}/children`, "utf8");
  } catch {
    return [];
  }
  const out: number[] = [];
  for (const entry of listed.split(/\s+/)) {
    const child = Number.parseInt(entry, 10);
    if (!Number.isFinite(child)) {
      continue;
    }
    out.push(...children(child), child);
  }
  return out;
}

// killChildren SIGTERMs every process under pid, deepest first.
//
// Nothing is spared, because the breaker is the end of the session: the
// harness's own child process goes with the task it would not stop, and the
// terminal record the worker is waiting for is already in hand. A process
// that has already gone is not an error.
export function killChildren(pid: number): number[] {
  const killed: number[] = [];
  for (const child of children(pid)) {
    try {
      kill(child, "SIGTERM");
      killed.push(child);
    } catch {
      continue;
    }
  }
  return killed;
}
