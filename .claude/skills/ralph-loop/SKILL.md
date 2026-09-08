---
name: ralph-loop
description: Use when authoring, validating, or running this repo's autonomous build loop — filing work orders as nodes in the deployed dagger service, decomposing an epic into a task DAG agents can execute unattended, or driving harness/loop.py. Encodes how to size nodes, write bodies for an agent with no context, and keep humans off the critical path.
argument-hint: "[status | author | add <what> | validate | run --epic '#N' | diagnose [node]]"
allowed-tools:
  - Read
  - Glob
  - Grep
  - Bash(dg ready:*)
  - Bash(dg explain:*)
  - Bash(dg node:*)
  - Bash(dg search:*)
  - Bash(dg project:*)
  - Bash(harness/.venv/bin/python harness/loop.py --dry-run:*)
  - Bash(harness/.venv/bin/python harness/loop.py --report:*)
  - Bash(git status:*)
  - Bash(git log:*)
  - Bash(git diff:*)
---

# Ralph Loop

**Autonomy:** model-invocable · authors, validates, and diagnoses without asking · launching a run is the one act needing an explicit instruction, because it runs for hours, spends plan budget, and pushes commits

## Arguments

```
$ARGUMENTS
```

## Instructions

Route on the first word. Empty means `status`.

The backlog is the deployed dagger service, project `dagger`. The harness works one **epic** — a grouping node whose open children it labels `epic-<N>` and claims by that label. `backlog.toml` is the first build's work orders and nothing reads it.

### `author` · `add <what>`

File nodes in dagger. Read `WORK-ORDER.md` in this directory before writing a body; it has the anatomy, a worked example, and sizing. Create the epic with `dg create`, its children with `dg create` and `dg link`, or file the whole graph at once with `dg patch apply` so aliases resolve inside one transaction. Nodes carry `acceptance_criteria` and Gherkin `scenarios`; the reviewer judges exactly those.

Decompose by **dependency and verifiability**, never by granularity. A node is one coherent deliverable a fresh agent can finish and a reviewer can judge in a single pass. Every agent pays a large fixed cost re-reading the design before it writes anything, so twice as many nodes costs nearly twice as much for the same code.

Four rules, each of which cost a wasted run to learn:

1. **Nobody waits on a human.** A node with `assignee_kind = "user"` is a gate: the harness never claims it, and everything behind it stalls until a person completes it. If the work needs a decision, *make the decision now* and write it into the body. A gate is legal only as a terminal leaf that blocks nothing, or as the one ask a person genuinely has to answer.
2. **A body is self-contained.** The agent opens with an empty context window and sees only this repo and that body, plus the results of the nodes that block it (`dg node inputs`). Name the documents to read, the files to create, and the traps. Never write "as we discussed" or "per the above."
3. **A deliverable never sits in a protected path.** `PROTECTED` in `harness/loop.py` lists them: `backlog.toml`, `harness/`, `scripts/`, `.claude/`. A work order whose output lives there cannot be satisfied and burns every attempt. `docs/` and `plugin/` are not protected.
4. **Grouping nodes carry no contract.** A node with children and no acceptance criteria is bookkeeping the harness closes itself when dagger makes it ready. Give a node children or a contract, never both.

### `validate`

Dagger rejects cycles and dangling references at patch time, so structural errors surface when you file. What it cannot check is whether the graph will *move*:

```
dg node list -l epic-N        what is in scope
dg ready                      what is claimable now
dg explain '#N'               why a node is or is not ready
```

Look for nodes that can never become ready, gates that block work, missing criteria or scenarios, and deliverables in protected paths. Every finding is a runtime failure you are paying to discover early. Fix all of them in dagger before spending a run.

### `run --epic '#N' [flags]`

Validate first, state the node count and rough cost from the calibration table in `WORK-ORDER.md`, then:

```
harness/.venv/bin/python harness/loop.py --epic '#N'
```

`--max` caps nodes this run, `--attempts` review attempts per node (default 3), `--budget` dollars, `--no-push` commits without pushing. Launch only on an explicit instruction to run. If the ask is ambiguous, confirm — the single place in this skill to stop. The run command is deliberately absent from this skill's allowed tools, so in default mode it prompts; the two read-only harness forms do not. **While a run is live, do not commit to the working tree.** The harness records HEAD before each agent; a foreign commit diverges history and breaks every later push.

### `status` · `diagnose [node]`

`harness/.venv/bin/python harness/loop.py --epic '#N' --dry-run` for what is claimable under the epic, and `--report` for per-node tokens and cost from `harness/state.json`. `dg explain` for why a node is structurally not ready; `dg node show` for one work order.

A node that exhausted its attempts carries the last review as a comment (`dg node show`), has traded `epic-N` for `harness-blocked`, and keeps its feedback in `harness/state.json` so a relaunch resumes with it. Transcripts are in `harness/logs/<number>-<title>.log`. Read the feedback before the code. Fix the **work order** when the contract was wrong or unsatisfiable (`dg update`); fix the code only when the contract was right and the work missed it. Put the scope label back to retry.

## After a run

A green suite is not proof. Drive the system by hand as the principal your vision describes. The first run here passed its entire suite while agents could not file work at all, because the acceptance suite filed as a user and only ever claimed as the agent. Ten minutes of dogfooding found what 43 reviewed nodes did not.
