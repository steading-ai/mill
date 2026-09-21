---
name: think
description: Explore a feature-sized problem and the ways it could be solved before any plan is written. Produces a note about the problem, not a design.
---

# Think

Understand the problem and the space of answers to it. Write nothing but a
note: no code, no files, no branches, no plan.

This step is **read-only**. The plan comes next, and it is a different step
on purpose — a plan written while the problem is still being understood is a
plan for the first idea that arrived.

You are running unattended. There is no one to ask. Where this skill would
have asked a question, it states the default instead; take the default and
record the assumption in your note.

## Instructions

### 1. State the problem in your own words

From the work order and the context the previous step gathered. If your
sentence and the work order's disagree, the work order wins and the
disagreement is worth naming: it usually means something is being assumed
that nobody wrote down.

### 2. Find the constraints that are already there

The design documents the work order names, the patterns in the code the
change touches, the tests that pin current behaviour, and anything the
repository's own instructions require. Practise Chesterton's Fence: the code
that looks pointless is where a constraint is hiding.

### 3. Put up at least two approaches

For each: what it would change, what it would cost, what it would make
harder later, and what would have to be true for it to be the right one.
One approach is not exploration; it is the first idea with a paragraph
around it.

Include the smallest thing that could work, even when you are sure it is too
small. It is the baseline everything else is measured against.

### 4. Name what you do not know

The questions whose answers would change the choice, and how each could be
answered — a file to read, a command to run, an experiment. Answer the cheap
ones now.

### 5. Note

End with the note, and nothing else:

- **Problem** — what is actually being asked for.
- **Constraints** — what the design, the code, and the tests already fix.
- **Approaches** — each with its cost and what it forecloses.
- **Leaning** — which one, and what would change your mind.
- **Unknowns** — what you could not settle, and how it could be settled.
- **Assumptions** — every question this skill would have asked a human, with
  the default you took.
