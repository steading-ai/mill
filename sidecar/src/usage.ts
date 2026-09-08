// What a turn cost, keyed by model.
//
// Two sources, and they are not interchangeable. The SDK's result message
// carries `modelUsage`, a per-model total for every model the query pipeline
// billed — main loop, subagents, sidechains, compaction — and that is what
// the terminal record reports. Its sibling `usage` covers the main loop
// alone and silently omits an auxiliary model's bill, so nothing here reads
// it (docs/spikes/sdk-sessions.md §Q4).
//
// The other source is the per-turn usage on each assistant message. It is
// what the streamed Usage events carry, and it is the only thing left when a
// turn is cancelled: an aborted iterator throws instead of producing a
// result, so its `modelUsage` never arrives (§Q5).

import { create } from "@bufbuild/protobuf";

import { TokenCountsSchema, UsageSchema, type TokenCounts, type Usage } from "../gen/harness/v1/harness_pb.js";

// ModelTotals is the SDK's per-model entry. `thinkingTokens` is already
// counted inside `outputTokens` and is deliberately not added again.
export interface ModelTotals {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
}

// TurnTokens is the usage block on one assistant message, in the Messages
// API's spelling.
export interface TurnTokens {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_input_tokens?: number | null;
  cache_creation_input_tokens?: number | null;
}

function counts(input: number, output: number, cacheRead: number, cacheWrite: number): TokenCounts {
  return create(TokenCountsSchema, {
    input: BigInt(input),
    output: BigInt(output),
    cacheRead: BigInt(cacheRead),
    cacheWrite: BigInt(cacheWrite),
  });
}

function n(value: number | null | undefined): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

// fromModelUsage renders the SDK's per-model totals as the wire's Usage. It
// is a translation and not a sum: the SDK's numbers are authoritative, so a
// model it billed appears with the counts it reported and nothing else.
export function fromModelUsage(modelUsage: Record<string, ModelTotals> | undefined): Usage {
  const usage = create(UsageSchema, {});
  for (const [model, totals] of Object.entries(modelUsage ?? {})) {
    usage.byModel[model] = counts(
      n(totals.inputTokens),
      n(totals.outputTokens),
      n(totals.cacheReadInputTokens),
      n(totals.cacheCreationInputTokens),
    );
  }
  return usage;
}

// fromTurn renders one assistant message's usage as the wire's Usage, keyed
// by the model that produced it.
export function fromTurn(model: string, tokens: TurnTokens | undefined): Usage {
  const usage = create(UsageSchema, {});
  if (tokens === undefined) {
    return usage;
  }
  usage.byModel[model] = counts(
    n(tokens.input_tokens),
    n(tokens.output_tokens),
    n(tokens.cache_read_input_tokens),
    n(tokens.cache_creation_input_tokens),
  );
  return usage;
}

// UsageAccumulator sums the per-turn usage of a session as it streams. It
// exists for the cancelled turn, whose tokens are otherwise lost: on a clean
// end the terminal reports the SDK's own totals instead.
export class UsageAccumulator {
  readonly #byModel = new Map<string, { input: bigint; output: bigint; cacheRead: bigint; cacheWrite: bigint }>();

  // add folds one turn's usage into the running total.
  add(usage: Usage): void {
    for (const [model, c] of Object.entries(usage.byModel)) {
      const running = this.#byModel.get(model) ?? { input: 0n, output: 0n, cacheRead: 0n, cacheWrite: 0n };
      running.input += c.input;
      running.output += c.output;
      running.cacheRead += c.cacheRead;
      running.cacheWrite += c.cacheWrite;
      this.#byModel.set(model, running);
    }
  }

  // total is every model billed so far, with its counts by kind.
  total(): Usage {
    const usage = create(UsageSchema, {});
    for (const [model, running] of this.#byModel) {
      usage.byModel[model] = create(TokenCountsSchema, { ...running });
    }
    return usage;
  }
}
