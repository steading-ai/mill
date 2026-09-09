# Runbook — the acceptance suite

`tests/acceptance` drives the whole mill against real dependencies: the
deployed dagger, a real GitLab, the real sidecar, and a real Claude account.
It is the executable form of O0's done-when — a quick-fix node delivered by
`mill run`, with every step's evidence in the ledger ([prd.md](prd.md) §8).

Every other suite in this tree proves a part against a fake. This one proves
the whole against nothing fake at all, because the facts it asserts —
a merge landing on a default branch, a signature verifying, a worker
noticing it has been fenced — are exactly the facts a fake would have to
invent.

It costs money and spends merge requests. `make test` never runs it;
`make test-acceptance` does.

```sh
make test-acceptance
```

## What it needs

The suite refuses to start when anything below is missing, and names
everything that is missing in one message rather than failing four minutes
in. If it starts, it has what it needs.

### Variables

| Variable | What it is |
|---|---|
| `MILL_ACCEPTANCE` | Set to anything to turn the suite on. Unset, every spec skips. `make test-acceptance` sets it. |
| `MILL_ACCEPTANCE_FORGE_URL` | The GitLab the scratch projects are made on, such as `https://gitlab.example.test`. |
| `MILL_ACCEPTANCE_FORGE_TOKEN` | A personal access token with `api` scope, owned by the account the mill delivers as, able to create and delete projects in the scratch namespace. |
| `MILL_ACCEPTANCE_FORGE_NAMESPACE` | The group scratch projects are created in, by numeric id or by full path. Nothing but the suite should live in it. |
| `MILL_ACCEPTANCE_FORGE_USER` | The GitLab username the token belongs to. It is both the basic-auth username git is given and the identity the suite expects to see on `merged_by`. Defaults to `mill`. |
| `MILL_ACCEPTANCE_DATABASE_URL` | PostgreSQL for the `mill serve` the suite starts. `make db-up` and the `DATABASE_URL` from `.env` will do on a development host. Migrations are applied by `mill serve` itself. |
| `MILL_ACCEPTANCE_DAGGER_SOCKET` | The dagger to file nodes in. Defaults to `$DG_SOCKET`, then `/run/dagger/dg.sock`. |
| `MILL_ACCEPTANCE_SECOND_DAGGER_CONFIG` | Optional. A `dg --config` file for a second dagger principal, used by the fencing scenario to take a node's lease. Without it the second claim is made by the same principal under a second agent ref, which fences the running worker just the same. |
| — | The sessions run on the account's own Claude Code login, `~/.claude/.credentials.json`. `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` are on the harness adapter's deny list and never reach a sidecar, so setting one does nothing; the preflight checks for the login instead. |

`MILL_FORGE_TOKEN` and `MILL_FORGE_USER` are what `mill run` itself reads;
the suite sets them on the subprocess from `MILL_ACCEPTANCE_FORGE_TOKEN` and
`MILL_ACCEPTANCE_FORGE_USER`, so nothing needs to be exported twice.

### On the host

- `dg`, `git`, `node` and `go` on `PATH`.
- A Claude Code login for the account running the suite, under `$HOME`.
- `sidecar/dist/main.js` built. `make test-acceptance` builds it; by hand it
  is `make sidecar-build`.
- Something listening on the dagger socket.
- Podman is *not* needed: the suite uses the database the environment names
  rather than testcontainers.

The `mill` binary is built by the suite from this checkout. It never reaches
for one that happens to be installed.

## What it does

Each scenario stages a fixture and then drives it:

1. A scratch GitLab project in the scratch namespace, seeded with the
   fixture's files, merge method `merge` and squash disabled.
2. A scratch dagger project, and the fixture's node filed in it.
3. One `mill run` per attempt, as a subprocess, with the scratch project as
   its `--vcs-url` and a workspace root of the scenario's own. The
   environment mode is the default, `inherit`, which is what a person at a
   shell gets; a deployment runs `--env-mode strict`, as
   `contrib/systemd/mill.service` does (docs/architecture.md §"Skill steps:
   the harness adapter").

Three scenarios run, in parallel:

- **One node, delivered.** The happy path, end to end: the README's one word,
  the node `done` with a merge-commit artifact on the default branch, six
  closed step runs each with the evidence its step names, the merge by the
  mill's forge identity, the commit verified with `git verify-commit` against
  the mill's `allowed_signers`, and the run's cost in dagger's usage rollup.
- **The same node twice is the same run.** The first run is killed during
  `implement` — the ledger, not the log, says when — and a second is
  invoked. It reattaches rather than cloning, `implement` shows attempt 1
  `aborted` and attempt 2 `closed` in the same session, and the node is
  delivered anyway.
- **Fenced out, and finished by the next worker.** A run in flight has its
  lease released and the node claimed by a second worker. The running `mill
  run` exits `75`, and a fresh run finishes the node with no second merge
  request on the branch.

The whole suite fits inside fifteen minutes on the host because the
scenarios run concurrently; its wall clock is its slowest scenario, not
their sum.

## What it leaves behind

Cleanup is registered before the thing that needs cleaning up exists, so a
failed or interrupted scenario still tears down what it made.

| Made | Cleaned up | How |
|---|---|---|
| Scratch GitLab project | Deleted | `DELETE /projects/:id` at the end of the scenario |
| Scratch dagger project | Archived | `dg project archive`. Dagger has no delete: archiving makes a project read-only and takes every node in it out of every ready set, which is as gone as dagger goes. |
| Workspaces, clones, the built binary | Removed | Temporary directories the suite owns |
| `mill serve` | Killed | Its process group, at the end of the suite |
| Rows in `MILL_ACCEPTANCE_DATABASE_URL` | **Kept** | The ledger is the record of the run and is worth reading after a failure |

A `SIGKILL` to the suite itself skips all of it. To clean up by hand:

```sh
# Scratch projects on the forge.
curl -sH "PRIVATE-TOKEN: $MILL_ACCEPTANCE_FORGE_TOKEN" \
  "$MILL_ACCEPTANCE_FORGE_URL/api/v4/groups/$MILL_ACCEPTANCE_FORGE_NAMESPACE/projects?search=mill-acceptance"

# Scratch projects in dagger.
dg project list | jq '.projects[] | select(.slug | startswith("mill-acceptance"))'
dg project archive <slug>

# Whatever a killed run left on this host.
rm -rf /tmp/mill-acceptance-*
```

Nothing is named without a nonce, so a leftover from an earlier run never
collides with a later one.

## When it fails

- **The preflight refuses.** Read the list; every line names a variable, a
  path or a tool. Nothing has been spent.
- **A step run is `failed` in the ledger.** `mill run`'s own output is in the
  spec's report. `Inspect` is the fuller story: the attempt, its exit, and
  the evidence that did not arrive.
- **The merge is refused.** A protected branch or a required approval on the
  scratch namespace. Repository protections are the repository's job
  ([core-principles.md](core-principles.md) §1) — the fix is the namespace's
  settings, not the suite.
- **`git verify-commit` fails.** The signature is over the merge commit's
  second parent. A scratch project whose merge method is not `merge`, or
  whose squash option is not `never`, replaces that commit with one GitLab
  wrote; the suite pins both at creation, so a failure here means the
  namespace overrode them.

## Adding a scenario

Adding a scenario is adding a fixture. `tests/acceptance/fixtures_test.go`
holds one: the files a scratch project is seeded with, and the node's title
and body. `fixture.stage` does the rest, and a spec that wants a repository
already carrying the wrong code — O1's seeded-bad-implementation test — is a
second fixture and nothing else.
