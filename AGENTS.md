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

## Repo map

```
cmd/mill/             binary entrypoint; one binary, two faces (service, worker)
internal/cli/         the cobra command tree: serve, work, run, restart, version
internal/store/       persistence and its migrations; only internal/domain may import it
internal/workspace/   the process chokepoint. os/exec is forbidden everywhere else
internal/testsupport/ the testcontainers PostgreSQL fixture db-labelled specs use
internal/version/     the version injected at link time by the Makefile
proto/                the two contracts: mill.v1 the store API, harness.v1 the sidecar wire
gen/                  generated Go, committed. buf owns it; `make generate` writes it
sidecar/              the TypeScript harness adapter; sidecar/gen is harness.v1 in TypeScript
docs/                 the design. Authoritative
harness/              the reference agent loop that works one milestone of the backlog
compose.yaml          PostgreSQL for local development; .env.example seeds .env
```

## Where to look first

- **What mill is and is not** — `docs/prd.md` §1–§3, then `docs/core-principles.md`. The principles have teeth.
- **The model you are implementing** — `docs/domain-model.md`. Run, workflow, step, session, evidence, gate, workspace, principal.
- **How the pieces fit** — `docs/architecture.md`: two faces, the request path, the harness adapter, the workspace socket, identity, failure.
- **The store and the resume algorithm** — `docs/ledger.md`. The pseudocode there is the specification of the run loop.
- **Where this is going** — `docs/roadmap.md`. O0 is the walking skeleton; everything past it is parked there with its provenance.
- **The contract mill consumes** — dagger's `docs/domain-model.md`, checked out on this host under `~/src/github.com/ivy/dagger`.
- **Prior art and reviews** — `docs/research/`.

## How to work here

Canonical commands live in `Makefile`; tool versions in `mise.toml`. The only system package assumed is podman, which testcontainers and the development database use.

```bash
mise install            # bootstrap every tool
make build              # ./mill
make test               # all specs
make test-unit          # specs needing no database
make test-db            # specs needing PostgreSQL, via testcontainers
make lint               # go vet and golangci-lint
make generate           # code generators; commit the output
make db-up              # local PostgreSQL for interactive work
make db-migrate         # apply migrations to it
make db-down            # stop it
```

Before you finish, this must pass:

```bash
make lint && make test
```

`hk check` runs the same gate a commit does: `make lint`, then `make test-unit`.

Specs are Ginkgo and Gomega, in the package the code lives in, and a scenario's text is its spec name. Specs that touch PostgreSQL take a database from `internal/testsupport` and must carry the `db` label; the suite starts one container and clones a migrated template per spec.

Two lint rules carry design decisions rather than style: `depguard` stops anything but `internal/domain` importing `internal/store`, and `forbidigo` keeps `os/exec` out of everything but `internal/workspace` — the single chokepoint every spawned process goes through — and `testing` out of everything but `*_suite_test.go`. If one fires, the fix is the code, not the config.

## Change hygiene

- Do the node you were given. If a dependency is missing, report it rather than building it.
- Add specs with the code. A criterion with no spec is not done.
- Follow the patterns already in the tree over your own preferences; dagger's repository is the style reference.
- No repo-wide reformatting, and no dependency the work order did not name.
- Never commit a secret. `.env` is ignored; `.env.example` is the template.
- Generated code is committed. Run `make generate` and include the result.

## Running the build loop

The loop works one milestone: a grouping node whose open children it labels and then claims.

```bash
harness/.venv/bin/python harness/loop.py --epic '#1' --dry-run   # what is ready under the milestone
harness/.venv/bin/python harness/loop.py --epic '#1'             # run until it drains
```

An implementer builds a node, a reviewer judges it against its acceptance criteria and scenarios only, and a pass is committed and pushed. See `harness/README.md`.
