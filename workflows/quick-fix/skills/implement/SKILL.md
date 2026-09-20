---
name: implement
description: Make the change one node asks for, keeping it small, tested, and consistent with the code around it. The working step of a quick fix.
---

# Implement

Make the change. One node's worth, no more.

You are running unattended. There is no one to ask and nothing to approve
your plan. Where a decision has no obvious answer, take the smallest option
that satisfies the work order, and say what you chose and why in your
summary.

## What is in scope

The work order, and nothing else. Specifically **not** in scope:

- Refactoring code the node does not name.
- Fixing unrelated bugs, typos, or lint you notice on the way. Note them in
  the summary; leave the code alone.
- Reformatting, reordering imports, or renaming beyond what the change
  needs.

A quick fix that grows into a redesign is the wrong tier. If that is what
this turns out to be, stop, leave the tree clean, and say so: it is worth a
human's attention, and the run will reach one.

## Instructions

### 1. Work from the context you were given

The previous step's summary names the files and how they work today. Read
them again before editing — the summary is a map, not the territory.

### 2. Make the change

- Follow the patterns already in the file over your own preferences.
- Prefer the standard library and what the project already depends on. Add a
  dependency only if the work order names it.
- Fix the cause, not the symptom. A workaround that hides the problem is
  worse than the bug.
- Keep the diff readable. Every hunk should be explainable in one sentence.

### 3. Add or update the tests

A behaviour change with no test is not done. Put the test where the
project's tests already live, in the style they already use. If the project
has no tests at all, say so in the summary rather than inventing a
framework.

### 4. Prove it

Run the project's own build, test, and lint commands — the ones its
documentation names, not ones you invent. Read the output. A command that
fails is not "flaky" until you have looked at why it failed.

Repeat until they pass. If a failure is genuinely unrelated to your change,
say so in the summary with the evidence.

### 5. Leave the tree ready to commit

Do not commit; the next step does. Delete scratch files, temporary scripts,
and debug output — but a plan under `docs/plans/` is not scratch: it is
committed with the change. A check after the commit fails the run if the
tree is dirty, and it sends the work back to the step that commits.

### 6. Summarize

End with:

- **What changed** — per file, one line each.
- **Why** — the reasoning behind anything non-obvious.
- **Proof** — the commands you ran and what they printed.
- **Decisions** — what you chose where the work order was silent.
- **Left alone** — anything you noticed and deliberately did not touch.
