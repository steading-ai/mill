---
name: plan
description: Turn the thinking into an implementation plan a reviewer can check and an implementer can follow — steps, files, and the test that proves each one. Writes the plan into the repository.
---

# Plan

Write the implementation plan. It is reviewed before any code is written and
committed with the change, so it is a document for people, not a note to
yourself.

This step **writes one file and nothing else**: the plan. No code, no
configuration, no commit.

You are running unattended. Where a decision has no obvious answer, take the
smallest option that satisfies the work order and record it under
*Decisions*.

## Where the plan goes

The mill names the file and the run's facts under *The file this step closes
on*, below this skill. Write the plan there, opening with YAML frontmatter
built from those facts:

```markdown
---
run: <the run id>
node: <the node reference>
generation: <the generation>
workflow: <the workflow>
date: <the date>
---

# <the node's title>
```

That file is what closes this step: the mill reads it back and attaches its
content to the node. Do not report the plan in your summary instead — a
summary closes nothing here.

## Instructions

### 1. Choose the approach

From the previous step's note. Say which one, in a sentence, and why the
others were not chosen. A reviewer who disagrees needs to see what you
rejected.

### 2. Break it into steps

Each step is one coherent change: what it touches, what it does, and — this
is the part that gets skipped — **the test that proves it**. A step whose
proof is "it compiles" is not a step, it is a hope.

Order them so the tree is working after each one. A plan whose middle is
broken cannot be reviewed, resumed, or abandoned halfway.

### 3. Name the files

Every file the change creates, edits, or deletes, with one line on what
happens to it. A file you are unsure about belongs in the list with the
uncertainty stated, not left out.

### 4. Say how it is proven

The commands the implementer runs, and what passing looks like. Use the
project's own commands, the ones its documentation names.

### 5. Say what could go wrong

The risks that would send this back, and what you would do about each. Name
anything the plan depends on that does not exist yet — a missing dependency
is the plan's problem, not the implementer's surprise.

## The plan

```markdown
## Approach

The chosen approach in a paragraph, and what was rejected.

## Steps

1. **<what changes>** — files, what happens in them, and the test that
   proves it.

## Files

| Path | What happens |
|---|---|

## Proof

The commands, and what passing looks like.

## Risks

What could send this back, and the answer to each.

## Decisions

What was chosen where the work order was silent.
```
