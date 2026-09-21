---
name: simplify
description: Apply reuse, simplification, efficiency, and altitude cleanups to the code this run changed, and nothing else. Quality only — it does not hunt for bugs.
---

# Simplify

Clean up what this run wrote, in the tree, before it is committed. Apply the
fixes; do not report them.

You are running unattended. There is no one to approve a cleanup: make the
ones that are clearly right and leave the ones that are arguable, saying so
in your summary.

## Scope

**The diff this run made, and nothing else.**

```bash
git status --porcelain
git diff
```

Code the run did not touch is out of scope however much it deserves
attention. Note it; leave it alone.

This is not a bug hunt. Behaviour must be identical when you are done. If
you find a bug, say so in the summary and leave it — a correctness fix here
would ride in under a cleanup's name.

## What to look for

- **Reuse** — something in this repository already does this. Find it before
  writing a second one; a helper duplicated is a helper that will diverge.
- **Simplification** — a branch that cannot be taken, a variable used once, a
  layer of indirection with one implementation, an abstraction earning
  nothing.
- **Efficiency** — work repeated in a loop, a whole file read to answer a
  question about one line. Only where it is plainly wasteful; do not trade
  clarity for micro-optimisation.
- **Altitude** — a function doing two jobs, a comment explaining code that
  should explain itself, an error message that does not say what to do next.
  Comments state current facts, not the history of the change.

## Instructions

1. Read the diff, whole. A cleanup chosen hunk by hunk misses the
   duplication between hunks.
2. Make the changes. Keep each one explainable in a sentence.
3. Run the project's build, tests, and lint — its own commands. They must
   pass exactly as they did before you started.
4. Leave the tree ready to commit: no scratch files, no debug output. The
   next step commits, and a check after it fails the run if the tree is
   dirty.

## Summarize

End with:

- **Applied** — each cleanup, one line, with the file.
- **Left alone** — what you saw and deliberately did not change, and why.
- **Proof** — the commands you ran and what they printed.
