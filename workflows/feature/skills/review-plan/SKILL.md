---
name: review-plan
description: Check an implementation plan against the node's contract for gaps, wrong assumptions, and missing dependencies, and emit a structured verdict. The gate before any code is written.
---

# Review Plan

Judge the plan, not the planner. A plan is cheap to fix and expensive to
follow, so this is the last cheap moment.

You are running unattended and you decide alone: `approve` or `revise`.

## What you are judging against

The node's acceptance criteria and scenarios, the design documents the work
order names, and the code as it actually is. Nothing else. A plan you would
have written differently but that satisfies the contract is an `approve`.

## What to look for

- **Gaps** — an acceptance criterion or scenario no step satisfies.
- **Wrong assumptions** — the plan says the code does something; read the
  code and find out whether it does. This is where the real findings are.
- **Missing dependencies** — the plan needs something that does not exist,
  belongs to another node, or was never built.
- **Unprovable steps** — a step whose test would not fail if the step were
  skipped.
- **Scope** — work the node did not ask for, or work it asked for that the
  plan quietly dropped.
- **Order** — a step that cannot be done before the one it follows.

Verify the load-bearing claims. Open the files the plan names. A finding you
did not check is a guess, and a guess sent back as blocking costs a whole
attempt.

## A revision

When the plan comes back to you after a `revise`, the mill says so under
*What changed since your last verdict*: which attempt, where the file is,
and whether it changed. Read the plan again before you judge it — your
findings are in your context, the revised plan is not until you open it.
Judge the revision against the findings that sent it back and the same
criteria as before. A blocking finding you did not raise the first time must
be a gap the revision introduced; one you could have raised then is not
blocking now, however true it is.

## The verdict

Answer with the structured verdict the mill asks you for, and nothing else:

- `decision` — `approve` or `revise`.
- `findings[].criterion` — the acceptance criterion or scenario the finding
  is against, by its label. A finding you cannot tie to one is recorded as a
  note, so tie it.
- `findings[].severity` — `blocking` if the plan cannot be followed to a
  correct result without fixing it, `note` otherwise.
- `findings[].summary` — what is wrong, in one sentence.
- `findings[].location` — where, as `path:line` or the plan's own heading.

`revise` needs at least one blocking finding; `revise` with none is a
definition error and is recorded as `approve`. Do not hedge: a plan that is
good enough to follow is approved with notes.
