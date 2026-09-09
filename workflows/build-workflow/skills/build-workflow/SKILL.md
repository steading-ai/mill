---
name: build-workflow
description: Size a node and name the workflow that should deliver it — quick-fix or feature — emitting the assessment as structured output with the signals it rests on. The first step of a run in a project with no default.
---

# Build Workflow

Size the node in front of you and name the workflow that should deliver it.
Nothing else. You are not gathering context for the change, not reading the
whole repository, and not writing anything: the workflow you name has a
first step that does all of that, in its own session, with its own budget.

This step is **read-only** and short. You have Glob, Grep and Read, the
workspace refuses a write under this step's token, and you have ten minutes.
Spend the smallest part of them that answers the question.

You are running unattended and you decide alone. There is nobody to ask and
nothing to escalate to.

## What you are given

- The node's work order — its title, body, acceptance criteria and
  scenarios — and the results of the nodes it depends on.
- A checkout of the repository at its default branch, in your working
  directory.
- The workflows in scope, listed for you by the mill. **You must name one of
  them, spelled exactly as it is listed.** A name that is not in that list
  fails the run at the selection step; the mill will not invent a workflow
  to match your answer.

## The tiers

Two, and the line between them is whether the change needs a plan:

- **`quick-fix`** — one file, or a few lines across two, with no design
  question in it. A typo, a message, a bounds check, a version bump, a
  missing nil guard. The change is obvious once you have found the code.
- **`feature`** — anything that needs a plan before it is written: several
  packages touched, an interface changed, a new command or subcommand, a
  schema or a wire format altered, a decision about *how* that a reviewer
  could reasonably disagree with. New behaviour that has to be tested is a
  feature.

**When you are uncertain, say `feature`.** The tiers are not symmetric: a
feature run over a one-line change costs a plan nobody needed, and a
quick-fix run over a change that needed a plan ships an unreviewed design.
The expensive path is the safe one.

## How to decide

1. Read the work order. What outcome is being asked for?
2. Find the code it touches. Glob and Grep for the symbols, files and
   commands the node names; read enough of what you find to know how many
   files the change lands in.
3. Note the **signals** — the observable facts your answer rests on. Each
   one is a fact somebody else could check, not an impression:
   - the files or packages the ask touches, by path;
   - whether tests exist for them, and whether the ask needs new ones;
   - whether the ask names a design change: a new interface, a new command,
     a changed contract, a document under `docs/` as a deliverable;
   - how many acceptance criteria and scenarios the node carries.
4. Pick the tier the signals support, then name the workflow in scope that
   stands for it.

Stop as soon as the signals settle the question. A node whose body says "add
a subcommand, with tests" is a `feature` before you have opened a file.

## Your answer

Emit structured output and nothing else:

- `workflow` — the name of a workflow in scope, spelled as listed.
- `tier` — `quick-fix` or `feature`.
- `signals` — at least one observable fact, each a short sentence. These are
  the evidence for the decision and they are recorded on the node; write
  them so somebody reading the run later can check them.
- `reasoning` — one short paragraph tying the signals to the tier.

Your prose is not read. The structured output is the selection's evidence.
