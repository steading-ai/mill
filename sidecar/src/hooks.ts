// The one thing a session may not do with its shell.
//
// A wait written as `until ! pgrep -f "ginkgo"; do sleep 5; done` never
// ends: the shell running the loop carries the pattern on its own command
// line, so `pgrep -f` matches it and the condition is true forever. It cost
// the reference harness two sessions, each ended by nothing but the step's
// hour. There is a tool for the wait — a backgrounded task wakes the session
// when it finishes, and TaskOutput reads it in the meantime — so the loop is
// refused before it runs and the refusal says what to use instead.
//
// Backgrounding stays allowed, and so does `pgrep` outside a loop: the
// failure is the polling, not the command.

// POLL_LOOP matches a shell loop whose condition polls with `pgrep -f`,
// either spelling: the keyword before the poll, or the poll before the `do`
// that opens the body.
const POLL_LOOP = /\b(?:until|while)\b[^\n]*\bpgrep\s+-f|\bpgrep\s+-f[^\n]*;\s*do\b/;

// POLL_LOOP_REASON is what the session is told instead. It names TaskOutput,
// because a refusal that does not say what to do next is answered with the
// same command spelled differently.
export const POLL_LOOP_REASON =
  "A loop that polls with `pgrep -f` matches the shell running the loop and " +
  "never ends. Wait on a background task with TaskOutput, or end your turn: " +
  "the task's completion wakes you.";

// pollsWithPgrep reports whether a command is that wait.
export function pollsWithPgrep(command: string): boolean {
  return POLL_LOOP.test(command);
}

// PreToolUse is the hook decision the harness reads back, in its own shape.
interface PreToolUseDecision {
  hookSpecificOutput?: {
    hookEventName: "PreToolUse";
    permissionDecision: "deny";
    permissionDecisionReason: string;
  };
}

// denyPollLoops is the PreToolUse hook the sidecar registers on `Bash`.
// Anything that is not the self-matching wait is passed through untouched:
// the hook decides nothing it was not written to decide.
export function denyPollLoops(input: unknown): PreToolUseDecision {
  const command = commandOf(input);
  if (!pollsWithPgrep(command)) {
    return {};
  }
  return {
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: POLL_LOOP_REASON,
    },
  };
}

function commandOf(input: unknown): string {
  if (typeof input !== "object" || input === null || !("tool_input" in input)) {
    return "";
  }
  const toolInput = input.tool_input;
  if (typeof toolInput !== "object" || toolInput === null || !("command" in toolInput)) {
    return "";
  }
  return typeof toolInput.command === "string" ? toolInput.command : "";
}
