// The Claude Agent SDK, behind the narrowest surface the sidecar uses.
//
// The sidecar drives exactly one SDK entry point: `query()`. Naming that
// shape here does two things. It keeps the rest of the sidecar from reaching
// into the SDK's several hundred exported types, and it gives the specs a
// seam: a fake `QueryFn` is a function returning an async iterable, so the
// framing, the exit mapping and the usage accumulation are testable without
// a credential or a network.
//
// The import is dynamic on purpose. A malformed frame is answered before the
// SDK is ever loaded, so a sidecar spawned with a broken request neither
// pays for the module nor depends on it being installable.

import type { Options, SDKMessage, SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";

export type { Options, SDKMessage, SDKUserMessage };

// Conversation is a session in flight: the harness's messages, and the one
// control method the sidecar uses. `stopTask` is optional because it is the
// polite half of the breaker — a harness that does not offer it is still
// stopped, by killing the processes it left behind.
export type Conversation = AsyncIterable<SDKMessage> & {
  stopTask?: (taskId: string) => Promise<void>;
};

// QueryFn is one session: a stream of user messages and the options it runs
// under, in; the session's messages, out.
//
// The prompt is a stream and not a string because the sidecar has a second
// thing to say. A session that ends its turn with a background task still
// running is waiting to be woken, and the sidecar asks it about a task that
// has gone quiet (tasks.ts) — which the harness only accepts in streaming
// input mode.
export type QueryFn = (params: {
  prompt: AsyncIterable<SDKUserMessage>;
  options: Options;
}) => Conversation;

// loadQuery resolves the real SDK. Passed as a parameter rather than
// imported at the top of main so a spec can substitute a fake.
export async function loadQuery(): Promise<QueryFn> {
  const { query } = await import("@anthropic-ai/claude-agent-sdk");
  return query;
}
