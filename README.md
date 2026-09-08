# Mill

The piece of the software factory that consumes dagger tickets and delivers running software. Scoped the way dagger was scoped: *just* a workflow runner — it claims a ready node, selects a workflow, drives gated steps with evidence, and reports a result with provenance. It does not plan, hold a backlog, sandbox, or converse.

Design first, code second. Read [docs/prd.md](docs/prd.md), then [docs/domain-model.md](docs/domain-model.md), [docs/architecture.md](docs/architecture.md), and [docs/ledger.md](docs/ledger.md). [docs/core-principles.md](docs/core-principles.md) are the decision filters. [docs/roadmap.md](docs/roadmap.md) is where this is going.

## Status

Seeded. The design is written; the code is not. The repository is built by dagger's reference harness from the O0 backlog held in dagger (project `mill`), one node at a time: see [harness/README.md](harness/README.md) and `/ralph-loop`.

```
harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run   # what is ready under the milestone
harness/.venv/bin/python harness/loop.py --epic '#1'             # run until it drains
```

`mise install` creates the venv; `DG_PROJECT=mill` is set by `mise.toml`.
