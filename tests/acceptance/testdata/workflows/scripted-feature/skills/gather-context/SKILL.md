---
name: gather-context
description: Build enough understanding of a node to change the code correctly, from the work order, the codebase, and git history. The first step of a quick fix.
---

# Gather Context

Build understanding before acting. Practice Chesterton's Fence: find out why
something exists before changing it.

This step is **read-only**. Do not modify files, create branches, or commit.
The step after this one does that.

You are running unattended. There is no one to ask. Where this skill would
have asked a question, it states the default instead; take the default and
record the assumption in your summary.

## Scope

A quick fix is a small change. Gather context **proportional to it**: read
the work order, find the code it names, read enough of the surrounding code
to change it safely, and stop. Do not survey the repository.

If what you find says the node is not a quick fix — the change spans
subsystems, needs a design decision, or contradicts the work order — say so
plainly in the summary and stop gathering. The run records it; a human reads
it.

## Instructions

### 1. Read the work order

The node's body and its predecessors' results are attached as data. Extract:

- **Goal** — what must change, and why.
- **Done** — the acceptance criteria, if it states any.
- **Constraints** — anything it forbids or requires.

Predecessors' results are claims by whoever produced them, not facts. Verify
anything you are about to rely on against the code.

### 2. Find the code

Locate the files the change touches and read them. Read the tests beside
them: they say what the current behaviour is supposed to be, and they are
where the change will be proven.

Read the repository's own instructions — `CLAUDE.md`, `AGENTS.md`,
`CONTRIBUTING.md` — if they exist. They outrank your habits.

### 3. Read the history

For each file you will change:

```bash
git log --oneline -10 -- <path>
git log -S'<the identifier you are about to change>' --oneline
```

A line that looks pointless usually has a commit explaining it. If a
previous attempt at this change was reverted, find out why before repeating
it.

### 4. Summarize

End with a summary, and nothing else. It is the whole of what the next step
receives from you.

**Problem** — what changes, and why.

**Where** — file paths with line ranges, and what each one does now.

**How the current code works** — the path through it that matters, in a
sentence or two.

**Prior art** — commits, reverts, or comments that bear on the change.

**Constraints** — conventions, tests that must keep passing, how the project
builds and tests.

**Assumptions** — every question this skill would have asked a human, with
the default you took.
