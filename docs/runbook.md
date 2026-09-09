# Runbook — the acceptance suite

`tests/acceptance` drives the whole mill against real dependencies: the
deployed dagger, a real GitLab, the real sidecar, and a real Claude account.
It is the executable form of O0's done-when — a quick-fix node delivered by
`mill run`, with every step's evidence in the ledger ([prd.md](prd.md) §8) —
and of O1's: a `feature` node planned, and a seeded bad implementation the
pipeline refuses (M.2, R-25).

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
| `MILL_ACCEPTANCE_FORGE_URL` | The GitLab the scratch projects are made on, such as `https://gitlab.example.test`. Optional: unset, the scenarios that deliver to a forge skip and the rest run against a bare repository on disk. See [Without a forge](#without-a-forge). |
| `MILL_ACCEPTANCE_FORGE_TOKEN` | Required when the forge URL is set. A personal access token with `api` scope, owned by the account the mill delivers as, able to create and delete projects in the scratch namespace. |
| `MILL_ACCEPTANCE_FORGE_NAMESPACE` | Required when the forge URL is set. The group scratch projects are created in, by numeric id or by full path. Nothing but the suite should live in it. |
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

1. A repository seeded with the fixture's files: a scratch GitLab project in
   the scratch namespace, merge method `merge` and squash disabled — or, for
   a fixture that never reaches a forge, a bare repository in a temporary
   directory ([Without a forge](#without-a-forge)).
2. A scratch dagger project, and the fixture's node filed in it.
3. One `mill run` per attempt, as a subprocess, with that repository as its
   `--vcs-url` and `--origin`, and a workspace root and log directory of the
   scenario's own. The environment mode is the default, `inherit`, which is
   what a person at a shell gets; a deployment runs `--env-mode strict`, as
   `contrib/systemd/mill.service` does (docs/architecture.md §"Skill steps:
   the harness adapter").

Five scenarios run, in parallel. Four of them deliver to a forge and skip
when there is none:

- **One node, delivered.** The happy path, end to end: the README's one word,
  the node `done` with a merge-commit artifact on the default branch, six
  closed step runs each with the evidence its step names, the merge by the
  mill's forge identity, the commit verified with `git verify-commit` against
  the mill's `allowed_signers`, and the run's cost in dagger's usage rollup.
- **A killed worker's node is resumed by the next run.** The first run is
  killed during `implement` — the ledger, not the log, says when — its lease
  taken from it, and a second run invoked. The second reattaches rather than
  cloning, `implement` shows attempt 1 `aborted` and attempt 2 `closed` in
  the same session, and the node is delivered anyway. It takes about a minute
  and a half and one quick-fix run's tokens: 1.7k in, 4.0k out, 532k read
  from cache, 38k written to it.
- **A worker fenced out writes nothing.** A run in flight has its lease taken
  and the node claimed by a second worker. The running `mill run` exits `75`,
  and a fresh run finishes the node with no second merge request on the
  branch. It takes about three minutes — a fenced worker notices at its next
  heartbeat, and the scenario gives it a short one — and one quick-fix run's
  tokens: 0.05k in, 5.3k out, 694k read from cache, 37k written to it.
- **One node, planned and delivered.** The `feature` workflow end to end:
  every step through `pr` closed on the evidence its definition names, the
  plan in the branch under `docs/plans` naming the run, the reviewer in a
  session of its own, and a pull request for the run's branch.

The fifth needs no forge and runs on every host that can start the suite:

- **The reviewer says no.** The seeded bad implementation, rejected on the
  record. See [The reviewer says no](#the-reviewer-says-no).

The forge scenarios fit inside fifteen minutes on the host because they run
concurrently; the suite's wall clock is its slowest scenario, not their sum.
*The reviewer says no* is the slowest, at up to seventy-five minutes.

The two lease scenarios take the lease away rather than waiting it out,
because killing a worker releases nothing and dagger hands a node to nobody
while its lease is live: the next claim would be refused until the TTL had
passed, which is longer than either scenario's patience. The reason is
`preempted` — an operator taking a lease from a worker that is not coming
back — and it is the only one dagger accepts here: `expired` is the server's
own, set by its expiry timer and refused from a client. Nothing about the
resume turns on it: the next worker reads the ledger, not the release reason
([ledger.md](ledger.md) §Resume).

## What it leaves behind

Cleanup is registered before the thing that needs cleaning up exists, so a
failed or interrupted scenario still tears down what it made.

| Made | Cleaned up | How |
|---|---|---|
| Scratch GitLab project | Deleted | `DELETE /projects/:id` at the end of the scenario |
| Scratch dagger project | Archived | `dg project archive`. Dagger has no delete: archiving makes a project read-only and takes every node in it out of every ready set, which is as gone as dagger goes. |
| Workspaces, clones, step logs, bare local origins, the built binary | Removed | Temporary directories the suite owns |
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
- **`commit` fails with "was committed by … not by …".** The `commit` skill
  has the session run `git commit` in its own shell, and the session's git
  reads the host account's `~/.gitconfig` (docs/architecture.md
  §"Skill steps: the harness adapter", the environment floor). The `commit`
  verifier compares the result against the identity `mill run` was told to
  commit under, which defaults to `mill <user@host>`. On a host whose git
  identity is anything else, every `commit` step fails three times and the
  run parks. This is a gap in the commit path and not in the suite: a
  session's git also cannot reach the mill's signing key, so a commit it
  makes is unsigned and *One node, delivered*'s `git verify-commit`
  assertion cannot hold either. Until it is closed, run the suite on a host
  whose git identity is the one the mill commits under.
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

## The reviewer says no

The milestone's own proof: a change that compiles, lints, passes its tests
and violates a stated acceptance criterion, and a pipeline that rejects it
([prd.md](prd.md) M.2, R-25). Fifty-nine approvals in a row proved nothing;
one refusal on the record proves the reviewer reads.

The fixture is a Go package with no conversion in it and a node with two
criteria: `Fahrenheit(100)` returns `212`, and a test named `TestFahrenheit`
proves it. The implementation the scenario seeds returns `180` and ships a
`TestFahrenheit` that asserts `180`. So `gofmt`, `go vet` and `go test`
are all clean, AC-2 holds, and AC-1 is plainly false. The violation is blunt
on purpose: a criterion a reviewer could argue about would prove nothing
either way.

**What "rejected" means here.** The scenario asserts the reviewer's first
verdict is `revise`, that a blocking finding names AC-1, and that the
finding reached the implementer — read out of the ledger's step log for the
attempt the revise created, not out of the session. What happens next is the
model's business: the run may deliver on a later `approve`, or spend the
review step's retries and park with the findings in a gate. Both are the
pipeline rejecting the change. Only one ending is a failure, and it is the
one where the seeded implementation was approved.

The reviewer's findings go into the spec's report verbatim, so a
retrospective can quote the no.

**The workflow it runs.** `feature` opens the pull request before the
reviewer reads the change, so a run of it on a host with no forge parks at
`pr` and the reviewer never speaks. The scenario derives its definition from
`workflows/feature/workflow.yaml` at run time and drops the two steps whose
evidence only a forge can state — `pr`, whose evidence is a URL, and the
`merge` gate, whose evidence is a status — keeping every other step, skill,
model, retry and `on_failure` exactly as `feature` writes them. It is pushed
under the name `feature-no-forge` so a scenario running beside it still
selects the real thing.

**Cost.** Two sessions, and up to eight Opus steps across them: the
implementing session's think, plan, implement and simplify, and the
reviewer's review-plan and review, plus a further implement and review for
each round the reviewer sends back. Budget seventy-five minutes and a few
dollars. It is the most expensive scenario in the suite.

## The scripted implementer

The seeded implementation is a prompt, not a patch:

```sh
mill run '#7' --skill-override implement=tests/acceptance/testdata/skills/implement
```

`--skill-override <step>=<dir>` runs a step on a skill read from a directory
instead of the one its workflow pinned in the store. The directory is a skill
directory exactly as `mill workflow push` accepts one, so the seam cannot run
something the loader would reject, and every other thing about the step — its
session, its model, its tools, its evidence — is the workflow's own.

It is a test seam and nothing else. `mill run` refuses it unless
`MILL_ACCEPTANCE` is set, before it claims anything, because a run driven
with a prompt the store does not hold produces evidence nothing can be traced
back to a pushed workflow. Nothing in a deployment sets that variable, and
`mill work` has no such flag at all.

## Without a forge

With `MILL_ACCEPTANCE_FORGE_URL` unset, the suite runs in local-origin mode:
the scenarios that end in a merge request skip, and *The reviewer says no*
runs against a bare git repository in a temporary directory, which
`mill run --origin` points a workspace at. It is a real repository cloned by
real git; the only thing missing is the API no step in that scenario calls.

`--vcs-url` is still required — the project's forge is resolved before
anything is claimed, so a project with no repository fails before it has
spent a session — and the suite passes a placeholder pointing at a host that
does not exist. The scenario asserts for itself that the run never reached
`pr`, which is what makes the placeholder safe.

Half a forge is refused: a URL with no token or no namespace fails the
preflight rather than skipping, because that is a configuration mistake and
not a choice.

## Adding a scenario

Adding a scenario is adding a fixture. `tests/acceptance/fixtures_test.go`
holds one: the files a scratch project is seeded with, and the node's title
and body. `fixture.stage` does the rest. A fixture that sets `Local`
is staged as a bare repository on this host instead of a project on the
forge, which is how the seeded bad implementation runs where there is no
GitLab.
