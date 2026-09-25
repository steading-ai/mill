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
dg inputs '#18'         # results handed on by the nodes it depends on
dg ready                # what is claimable now
dg explain '#18'        # why a node is or is not ready
```

The work order names the documents that govern it. Read them before writing code; they are the authority when they disagree with existing code.

Your contract is the node's **Acceptance criteria** and **Scenarios**. Every criterion must hold, and every Gherkin scenario must exist as an executable Ginkgo spec whose assertions match its Then clauses. A reviewer checks exactly this and nothing else.

**If you changed a proto contract, say so in your final message** — which file, which message, which field, and whether the change is breaking. `make lint` does not check compatibility, so nothing else will tell the reviewer; `make lint-contract` is the report, and `proto/README.md` says when breaking the contract is allowed. Your final message is the node's result and what the commit message is written from, so a contract change you do not name is a contract change nobody reviewed.

**Do not modify** `harness/`, `.claude/`, `scripts/`, or `backlog.toml`. That is the machinery running you; the harness reverts changes there and counts them as a review failure. `docs/` is not protected, because work orders name documents as deliverables — but a work order is the only reason to touch a design document. If the design is wrong, say so in your final message and carry on. Do not commit; the harness owns history.

## Repo map

```
cmd/mill/             binary entrypoint; one binary, two faces (service, worker)
internal/cli/         the cobra command tree: serve, work, run, restart, workflow, image, cache, doctor, mcp, the readers, version
internal/store/       persistence and its migrations; only internal/domain may import it
internal/dagger/      the client for dagger's dagger.v1 API, and the refusals mill owes a gate
internal/run/         the loop: claim, heartbeat, steps, evidence, finish. docs/ledger.md §Resume in Go
internal/harness/     the adapter seam: the Harness interface, the claude sidecar driver, a scripted fake
internal/workflow/    the loader: a workflow directory in, a versioned definition and its skills out
internal/workspace/   the process chokepoint. os/exec is forbidden everywhere else
internal/workspace/machine  the machine backend: a run's systemd-nspawn machine through mill-machine@, its driver seam, images, cache lineages, eviction, reconciliation; docs/sandbox.md
internal/gateway/     the credential gateway: the session's step token in, the subscription credential out, usage counted from outside; docs/sandbox.md §Credentials
internal/skill/       the mill skill and the sidecar copy the worker mounts read-only into every machine; docs/sandbox.md §"Skill mount"
internal/testsupport/ the testcontainers PostgreSQL fixture db-labelled specs use, and a real `dg serve` for the run loop
internal/version/     the version injected at link time by the Makefile
proto/                the contracts: mill.v1 the store API, harness.v1 the sidecar wire; README.md is the compatibility policy and says where skift.v1 comes from
gen/                  generated Go, committed. buf owns it; `make generate` writes it
vendor/               every Go dependency, committed by `go mod vendor`; skift's generated skift.v1 client comes from here, pinned to a commit in go.mod
sidecar/              the TypeScript harness adapter; sidecar/gen is harness.v1 in TypeScript
workflows/            the workflow definitions this repo seeds the store with; `mill workflow push` loads them. mill-quick-fix and mill-feature are the mill project's own: the org definitions plus the live tier as a check step
contrib/systemd/      the user units a deployment runs: mill.service, the image-build and cache-prune timers; docs/architecture.md §Deployment
images/               the base image machines boot from: images/base is the Containerfile, the dnf shim and what the export must restore; docs/sandbox.md §Images
tests/acceptance/     the suite that drives the whole mill against real dependencies; docs/runbook.md
docs/                 the design. Authoritative
docs/sandbox.md       the per-run sandbox: one systemd-nspawn machine per run, its images, caches, network and credentials; the O2 milestone's authority
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
- **The sandbox a run works in** — `docs/sandbox.md`: the machine, its image and `mill.toml`, caches and eviction, network and the credential gateway, privilege, reconciliation, what the host provides. `docs/sandbox-scenarios.md` is its behaviour; ADR-018 its decision.
- **The tools an operator has** — `docs/tools.md`: what each read tool answers, what `doctor` probes, `cache` and `image`, and how a session reaches them over `mill mcp`.
- **How to run the acceptance suite** — `docs/runbook.md`: what it needs, what it leaves behind, how to clean up.
- **Prior art and reviews** — `docs/research/`.

## How to work here

Canonical commands live in `Makefile`; tool versions in `mise.toml`. The only system package assumed is podman, which testcontainers and the development database use.

```bash
mise install            # bootstrap every tool
make build              # ./mill
make test               # all specs
make test-unit          # specs needing no database
make test-db            # specs needing PostgreSQL, via testcontainers
make test-acceptance    # the whole mill against a real dagger, GitLab and Claude account
make lint               # go vet and golangci-lint
make lint-contract      # buf breaking against main: a report, not a gate
make check              # the release gate: lint, the contract report, every spec
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

Contract compatibility is reported, not gated: `make lint` does not run `buf breaking`, so a deliberate proto change commits. `make lint-contract` runs the report, and `hk`'s `pre-push` hook does too. `proto/README.md` says when breaking the contract is allowed.

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
