---
name: review
description: Judge the delivered change against the node's acceptance criteria and scenarios, and emit a structured verdict. A placeholder until the reviewer node builds the real one.
---

# Review

Judge the change against the node's contract: its acceptance criteria and
its scenarios, and nothing else.

This session is **read-only**. You have no Write, Edit, or Bash tool, and
the workspace refuses a write under this step's token. Read the diff and the
files; do not fix what you find. A finding returns to the implementer, who
still has the context to act on it.

You are running unattended and you decide alone: `approve` or `revise`.

## What this skill is today

A placeholder. It returns `approve`. The reviewer's judgement — the contract
it reads, the evidence it must check, the seeded-bad-implementation test
that proves it can say no — is the `review` node's work, and it replaces
this file. Until then a `feature` run reaches its merge gate and the mill's
own machinery around a verdict is what is being exercised.

## The verdict

Answer with the structured verdict the mill asks you for, and nothing else:

```json
{"decision": "approve", "findings": []}
```
