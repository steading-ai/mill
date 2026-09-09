---
name: mill
description: Use when a question is about a mill run — what is running, why a run is stuck, what a step cost, what evidence closed it, whether this host could work at all. Covers the mill MCP tools and the `mill` CLI, what each answers, and which of them a session is allowed to call.
when_to_use: mill status, mill inspect, mill explain, mill logs, mill cost, mill doctor, why is the run stuck, which step is it on, what evidence closed the step, a parked run, a waiting run, a mill workspace, a workflow version. This is the mill workflow runner over a dagger work graph.
allowed-tools:
  - Bash(mill status:*)
  - Bash(mill inspect:*)
  - Bash(mill explain:*)
  - Bash(mill logs:*)
  - Bash(mill history:*)
  - Bash(mill cost:*)
  - Bash(mill workflow list:*)
  - Bash(mill workflow show:*)
  - Bash(mill workspace show:*)
  - Bash(mill doctor:*)
  - Bash(mill version:*)
  - mcp__plugin_mill_mill__status
  - mcp__plugin_mill_mill__inspect
  - mcp__plugin_mill_mill__explain
  - mcp__plugin_mill_mill__logs
  - mcp__plugin_mill_mill__history
  - mcp__plugin_mill_mill__cost
  - mcp__plugin_mill_mill__workflow_list
  - mcp__plugin_mill_mill__workflow_show
  - mcp__plugin_mill_mill__workspace_show
  - mcp__plugin_mill_mill__doctor
---

# Mill

**Autonomy:** model-invocable · reads a run unprompted · `cancel`, `rerun` and
`workspace_clean` are the session's own calls, never pre-authorized here

Mill is a workflow runner over dagger's work graph. It claims a ready node,
drives each step as a headless coding-agent session or a deterministic check,
closes a step only on evidence it verified, files a dagger gate when a person
must decide, and completes the node. A **run** is one dagger node being worked.

## Two rules before anything else

**Want work done? File a dagger node.** `run` is not an MCP tool and will not
be: it executes in the calling process, on the mill's own host and account. A
session that wants something built files it in dagger — the dagger skill is
how — and a mill worker picks it up. Do not try to start a run.

**The role decides, and nothing you say changes it.** Identity on mill's
socket is the peer uid of the process this server runs as, mapped by
configuration to a role. Every tool here reads except three: `cancel`,
`rerun` and `workspace_clean` change something, and a `reader` uid is refused
naming the role. Ask the operator; do not work around it.

| Role | May |
|---|---|
| the mill's own uid | Everything |
| `operator` | Read, and `cancel`, `rerun`, `workspace_clean` |
| `reader` | Read |

## Reading a run

A run is named by its dagger node reference — `mill#18` — or by the run id
`status` prints. A node reference resolves to that node's most recent run.

`status` → `explain` → `inspect` → `logs`.

| Tool | Answers |
|---|---|
| `status` | What is live, which step each is on, elapsed, cost, the gate a waiting run sits on. Start here |
| `explain` | Where one run is and **what it is waiting on**. Reach for this when asked why a run is stuck |
| `inspect` | Every attempt at every step, in order, with the evidence that closed it and how its process ended |
| `logs` | One attempt's log: the session's events, one summary line each, as the redactor wrote them |
| `history` | The audit trail — what changed, when, under which worker. The record of a run that has ended |
| `cost` | A window of usage rolled up by run, workflow, step and model. Counts, never currency |
| `doctor` | Whether this host could work at all: the socket, the schema, dagger, the sidecar, the signing key, the forge credential, the notifier |

The readers take `view`: `refs` is identifiers, `summary` the default, `full`
everything the ledger holds. Start narrow.

**Nothing here returns transcript content.** The ledger holds evidence,
identifiers and usage — not what a model said. `logs` is the one place a
session's own output surfaces, and it was redacted at write time.

## A stuck run is usually a person's turn

A run that is `waiting` or `parked` is blocked on a **dagger gate**, and only
a person completes a gate. `explain` names it and gives its title. Say what
you think should happen — comment on the gate in dagger if you can reach
it — and stop. Do not try to unblock the run yourself.

`parked` means the circuit breaker tripped: a step used its retries, or the
mill refused to guess. `explain` carries `parked_reason`.

## Changing something

- `cancel` asks the worker holding a run to stop the step it is on. The run is
  not failed: it keeps its workflow and branch and the next claim resumes there.
- `rerun` redoes a run from a named step, keeping the generation, the branch
  and the sessions. A failed run is reopened by it; an abandoned one is not.
  `restart`, which replaces all three, is CLI-only and not offered here.
- `workspace_clean` removes the checkouts of runs that have ended. `dry_run`
  first.

## References

- [references/tools.md](references/tools.md) — every tool with its arguments
  and an example response, checked against `mill mcp --describe`
