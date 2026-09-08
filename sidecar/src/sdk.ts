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

import type { Options, SDKMessage } from "@anthropic-ai/claude-agent-sdk";

export type { Options, SDKMessage };

// QueryFn is one turn: a prompt and the options it runs under, in; the
// session's messages, out. The real `query` returns a Query, which is an
// AsyncGenerator with control methods the sidecar does not use.
export type QueryFn = (params: {
  prompt: string;
  options: Options;
}) => AsyncIterable<SDKMessage>;

// loadQuery resolves the real SDK. Passed as a parameter rather than
// imported at the top of main so a spec can substitute a fake.
export async function loadQuery(): Promise<QueryFn> {
  const { query } = await import("@anthropic-ai/claude-agent-sdk");
  return query;
}
