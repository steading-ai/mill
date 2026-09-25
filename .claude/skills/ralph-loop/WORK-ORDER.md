# Work orders

Reference for `/ralph-loop author`. A node in `backlog.toml` is a work order:
everything an agent needs, given that it has never seen this repo and will
never see this conversation.

## Anatomy

Five sections, in this order. Use a TOML literal string (`'''`) for the body so
nothing needs escaping.

| Section | Holds | Why |
|---|---|---|
| `## Context` | What this is, and which documents govern it | The agent has no history. Name the authority so it reads that instead of guessing. |
| `## Deliverables` | Concrete paths to create or change | Removes the question "where does this go", which agents otherwise answer inconsistently. |
| `## Acceptance criteria` | Checkboxes, each independently verifiable | This *is* the contract. The reviewer walks it item by item and fails anything it cannot verify. |
| `## Scenarios` | A ```gherkin block | Each becomes an executable spec whose name is the scenario. This is what makes the suite readable and the review tractable. |
| `## Notes` | Traps, prior art, deliberate non-goals | Optional. Where you put the thing you would otherwise explain in chat. |

Write criteria a reviewer can check without judgment calls. "Handles errors
gracefully" is unverifiable; "a patch exceeding the ceiling is rejected before
any row is written, and the error names the limit" is not.

Write scenarios as behaviour, not implementation. The Then clause is what the
spec asserts, so vague Then clauses produce vague specs.

## Worked example

```toml
[[nodes]]
number = 17
parent = 15
blocked_by = [14]
priority = "p0"
title = "Cycle detection over the combined wait relation"
body = '''
## Context

See the Acyclicity section of docs/domain-model.md. This is the invariant the
whole system rests on, and checking the edge table alone is not enough.

## Deliverables

- `internal/domain/cycles.go` — recursive CTE over the union of `blocks` edges
  and the implicit parent-waits-for-child relation
- Parent acyclicity enforced in its own right

## Acceptance criteria

- [ ] Detection runs over the resulting graph of a whole patch, not per operation
- [ ] The error names the cycle path in node references
- [ ] A parent chain that loops is rejected
- [ ] Detection is bounded by the connected component, not the project

## Scenarios

```gherkin
Scenario: A cycle formed only within a patch is rejected
  Given an empty project
  When one patch creates nodes A, B and C and edges A to B, B to C, C to A
  Then the whole patch is rejected and no node is created

Scenario: Containment and dependency deadlock is rejected
  Given node 2 has parent node 1
  When a patch adds an edge from node 1 to node 2
  Then the patch is rejected
```
'''
```

## Sizing

Right-size against two opposing costs. Too large and the agent runs out of
context or the reviewer cannot verify it in one pass. Too small and the fixed
cost dominates: every node pays roughly 1.8M tokens re-reading the design
before it writes a line, so granularity is not free.

Signals a node is **too large**: more than about eight acceptance criteria, more
than about six scenarios, deliverables spanning more than about three packages,
or a title containing "and".

Signals a node is **too small**: you cannot write a scenario for it, or its only
criterion restates its title. Fold it into its neighbour.

## Calibration

From the first full run: 43 nodes, 93.2M tokens, $221.53 at list prices, about
8.5 hours at concurrency one, zero review failures. Per node: $5.15 and 2.2M
tokens on average, with a median of 28 implementer turns. Roughly 92% of tokens
were cache reads. All of it is reproducible from `harness/state.json`.

| Node | Turns | Cost |
|---|---|---|
| Go module and Makefile | 12 | $1.32 |
| Migrations: nodes, edges, leases | 21 | $3.20 |
| Connect server over a unix socket | 21 | $4.35 |
| skift CLI | 47 | $8.91 |
| Concurrency and failure suite | 60 | $11.12 |
| Embedded OPA and default policy | 79 | $13.02 |
| Handlers and wire mapping | 111 | $17.33 |

Cost tracks turns, and turns track how much of the system the node has to hold
at once. Handlers touched every domain type; the Makefile touched one file.
Per-node history lives in `harness/state.json` in skift's own `Usage` shape,
which is the calibration data for estimating the next backlog.
