# AGENTS.md

## What this repo is

Mill is a workflow runner over dagger's work graph. It claims a ready node, selects a workflow, drives each step as a headless coding-agent session or a deterministic check, closes steps only on evidence it verified, files dagger gates for human decisions, and completes the node with a result and provenance. It stores nothing but the record of its own runs.

- **Design is frontloaded and authoritative.** `docs/` settles the decisions; code follows it, not the other way round. When code and a document disagree, the document wins and the disagreement is a finding to report.
- **The repo builds itself.** Work is filed as nodes in the deployed dagger service (project `mill`) and executed by agents through `harness/loop.py`, one milestone at a time.
- **Mill runs as the operator.** It is the operator's dagger principal; dagger applies no gate veto to it. Never claiming, completing, dropping, reclassifying, or unlinking a gate is mill's own discipline — see `docs/domain-model.md` §Principal.

## If you were dispatched to work a node

The harness hands you the work order — the node's body plus the results of every node that blocks it. To see it again, or to look around:

```bash
dg node show '#18'      # the full work order for one node
dg node inputs '#18'    # results handed on by the nodes it depends on
dg ready                # what is claimable now
dg explain '#18'        # why a node is or is not ready
```

The work order names the documents that govern it. Read them before writing code; they are the authority when they disagree with existing code.

Your contract is the node's **Acceptance criteria** and **Scenarios**. Every criterion must hold, and every Gherkin scenario must exist as an executable Ginkgo spec whose assertions match its Then clauses. A reviewer checks exactly this and nothing else.

**Do not modify** `harness/`, `.claude/`, `scripts/`, or `backlog.toml`. That is the machinery running you; the harness reverts changes there and counts them as a review failure. `docs/` is not protected, because work orders name documents as deliverables — but a work order is the only reason to touch a design document. If the design is wrong, say so in your final message and carry on. Do not commit; the harness owns history.

## Where to look first

- **What mill is and is not** — `docs/prd.md` §1–§3, then `docs/core-principles.md`. The principles have teeth.
- **The model you are implementing** — `docs/domain-model.md`. Run, workflow, step, session, evidence, gate, workspace, principal.
- **How the pieces fit** — `docs/architecture.md`: two faces, the request path, the harness adapter, the workspace socket, identity, failure.
- **The store and the resume algorithm** — `docs/ledger.md`. The pseudocode there is the specification of the run loop.
- **Where this is going** — `docs/roadmap.md`. O0 is the walking skeleton; everything past it is parked there with its provenance.
- **The contract mill consumes** — dagger's `docs/domain-model.md`, checked out on this host under `~/src/github.com/ivy/dagger`.
- **Prior art and reviews** — `docs/research/`.

## How to work here

The `scaffold` node establishes `Makefile`, `mise.toml` pins, and the test harness; until it lands there is nothing to build. After it: `mise install`, `make build`, `make test`, `make lint`. Specs are Ginkgo and Gomega. Two lint rules carry design decisions: `depguard` keeps `internal/store` behind `internal/domain`, and `forbidigo` forbids `os/exec` outside `internal/workspace` — the single chokepoint every spawned process goes through.

## Change hygiene

- Do the node you were given. If a dependency is missing, report it rather than building it.
- Add specs with the code. A criterion with no spec is not done.
- Follow the patterns already in the tree over your own preferences; dagger's repository is the style reference.
- No repo-wide reformatting, and no dependency the work order did not name.
- Never commit a secret.
- Generated code is committed. Run `make generate` and include the result.
