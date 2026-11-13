# Mill

The piece of the software factory that consumes skift tickets and delivers running software. Scoped the way skift was scoped: *just* a workflow runner — it claims a ready node, selects a workflow, drives gated steps with evidence, and reports a result with provenance. It does not plan, hold a backlog, or converse.

Design first, code second. Read [docs/prd.md](docs/prd.md), then [docs/domain-model.md](docs/domain-model.md), [docs/architecture.md](docs/architecture.md), and [docs/ledger.md](docs/ledger.md). [docs/core-principles.md](docs/core-principles.md) are the decision filters. [docs/roadmap.md](docs/roadmap.md) is where this is going.

## Status

Early, and running where it was built: mill delivers work on its own repository, inside the sandbox [docs/sandbox.md](docs/sandbox.md) describes. The `quick-fix` and `feature` workflows ship with it. [docs/roadmap.md](docs/roadmap.md) says what each milestone makes possible.

The repository's work is held in skift (project `mill`) and worked one node at a time. The reference harness that built the first milestones is still here: see [harness/README.md](harness/README.md) and `/ralph-loop`.

```
harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run   # what is ready under the milestone
harness/.venv/bin/python harness/loop.py --epic '#1'             # run until it drains
```

`mise install` creates the venv; `SKIFT_PROJECT=mill` is set by `mise.toml`.
