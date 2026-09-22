---
name: retrospective
description: Use when asked to write up a week or a milestone of the factory — progress, tokens, code, what went wrong, gaps — as docs/retrospectives/<slug>.md, and to carry it through numbered rounds of the person's %% comments. Every number comes from the ledger, dagger, and the forge, never from memory.
argument-hint: "[<slug> [since <date-or-window>] | round <N> <slug>]"
disable-model-invocation: true
allowed-tools:
  - Read
  - Grep
  - Glob
  - Write(docs/retrospectives/**)
  - Edit(docs/retrospectives/**)
  - Bash(../cli/stead shell -- mill cost:*)
  - Bash(../cli/stead shell -- mill inspect:*)
  - Bash(../cli/stead shell -- mill history:*)
  - Bash(../cli/stead shell -- mill logs:*)
  - Bash(../cli/stead shell -- mill doctor:*)
  - Bash(dg node show:*)
  - Bash(dg node list:*)
  - Bash(dg project list:*)
  - Bash(gh pr list:*)
  - Bash(gh pr view:*)
  - Bash(gh run list:*)
  - Bash(git log:*)
  - Bash(git status:*)
  - Bash(git diff:*)
  - Bash(git fetch:*)
  - Bash(ccstat:*)
---

# Retrospective

**Autonomy:** human-only · gathers, computes and writes the document without asking · shows the person nothing is committed: it never commits, never pushes, never edits a `%%` block · corrects its own earlier claims in a later round rather than defending them

## Arguments

```
$ARGUMENTS
```

## Instructions

The document is `docs/retrospectives/<slug>.md` in this repository, shaped like `first-week-on-stead.md`. Data recipes: QUERIES.md.

1. **Window.** `since` is the subscription reset (Sunday 04:00 PDT) unless given. State it in the scope line, in UTC too.
2. **First round** (no `round`): gather before writing, all of it:
   - progress: merged PRs per repository and nodes done per project, gates answered separately;
   - code: lines added and removed by class (source, tests, generated, plan docs, docs, lockfiles), from every merged PR's files;
   - tokens: `mill cost` gateway totals, by model, by UTC day, and the step funnel of the main workflow; the API-rate equivalent is arithmetic and is labelled as such;
   - the week before, from the server's ledger when reachable, in the same table;
   - waste: step runs by status, aborts by reason, from the ledger.
   Then write: title, date, scope; What was built; Against the week before; `## Round 1` with What went wrong (ranked by cost, each with a run id or node), Gaps that remain, What I would do differently, Nodes filed. Every claim about a run cites its id or the node; every number names its source.
3. **Later round** (`round N`): read every `%%` block since the last round and leave them byte-for-byte. Append `## Round N` with: corrections first, where the record contradicts an earlier claim of yours, with the trace; answers to each open question; everything mentioned so far, bucketed; a proposal for the next iteration chosen by the record's costs (the few items that remove most of the waste), each mapped to a bucket and small enough for a node or two, and what is deferred and why.
4. **Voice.** One voice, the agent's, from the record. No hostnames from the person's infrastructure beyond what earlier retrospectives already name. Tables for numbers, prose for judgment, headers only for the sections above.
5. **Hand back** with the path and what needs the person: comments in `%%` blocks, then `/retrospective round N+1 <slug>`.

## Examples

```
/retrospective first-week-on-stead                 → gather, write, Round 1
/retrospective o3 since 2026-09-27                 → a milestone, explicit window
/retrospective round 2 first-week-on-stead         → read the %% comments, append Round 2
```
