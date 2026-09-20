# Runbook — the acceptance suite

`tests/acceptance` drives the whole mill against real dependencies: a real
dagger, a real GitLab, the real sidecar, and a real Claude account.
It is the executable form of O0's done-when — a quick-fix node delivered by
`mill run`, with every step's evidence in the ledger ([prd.md](prd.md) §8) —
and of O1's: a `feature` node the run sized for itself, delivered through a
plan gate a person answered, and a seeded bad implementation the pipeline
refuses (M.2, R-25).

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
| `MILL_ACCEPTANCE_DAGGER_SOCKET` | Optional. An external dagger to file nodes in, for the deliberate case of exercising the deployed service. Unset, the suite starts a dagger of its own and files there; see [Which dagger](#which-dagger). `$DG_SOCKET` is not consulted: it names the operator's dagger, which the suite never files in unless told to. |
| `MILL_ACCEPTANCE_SECOND_DAGGER_CONFIG` | Optional, and read only with `MILL_ACCEPTANCE_DAGGER_SOCKET`. A `dg --config` file for a second principal of that dagger, used by the fencing scenario to take a node's lease. Without it the second claim is made by the same principal under a second agent ref, which fences the running worker just the same. |
| — | The sessions run on the account's own Claude Code login, `~/.claude/.credentials.json`. `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` are on the harness adapter's deny list and never reach a sidecar, so setting one does nothing; the preflight checks for the login instead. |

`MILL_FORGE_TOKEN` and `MILL_FORGE_USER` are what `mill run` itself reads;
the suite sets them on the subprocess from `MILL_ACCEPTANCE_FORGE_TOKEN` and
`MILL_ACCEPTANCE_FORGE_USER`, so nothing needs to be exported twice.

On the homestead host these variables come from `~/.config/mill/acceptance.env`
— written by the `mill_forge` Ansible role, mode `0600` — so a suite run there
is `set -a; . ~/.config/mill/acceptance.env; set +a` and then
`make test-acceptance`.

### On the host

- `dg`, `git`, `node` and `go` on `PATH`.
- A Claude Code login for the account running the suite, under `$HOME`.
- `sidecar/dist/main.js` built. `make test-acceptance` builds it; by hand it
  is `make sidecar-build`.
- For the suite's own dagger, the default: podman, for the PostgreSQL its
  database lives in; `goose`, for dagger's migrations; and dagger's checkout
  at `~/src/github.com/ivy/dagger` or wherever `MILL_DAGGER_REPO` points,
  which is where the migrations are read from. `dg` itself is the one on
  `PATH`. These are what `internal/testsupport` asks for, and the preflight
  names whichever is missing.
- With `MILL_ACCEPTANCE_DAGGER_SOCKET` set, something listening on it, and
  none of the above: the ledger's database is the one the environment
  names, so nothing needs a container.

The `mill` binary is built by the suite from this checkout. It never reaches
for one that happens to be installed.

## Which dagger

By default the suite files into a dagger it started for itself: one
instance for the whole suite, `dg serve` from the `dg` on `PATH` against a
database of its own in a testcontainers PostgreSQL, on a socket in a
temporary directory, seeded with one principal — the account running the
suite — and torn down when the suite ends. It is the same fixture the run
loop's own specs use (`internal/testsupport/dagger.go`), for the same
reason: fencing, leases and patches only behave like production against
the real service, and the real service is cheap to start.

The default is the disposable one because dagger has no delete. A project
is archived, never removed, so every scratch project the suite ever made in
the operator's dagger would be there for good, and a suite killed before
its cleanup — a `SIGKILL`, a lost session, a host going down — would leave
an unarchived one in the graph the operator and the harness read. A suite
that misbehaves would be writing into that graph too. Against its own
instance the suite can be killed at any point and the deployed graph is
exactly as it was: the leftover is a row in a database the container takes
with it. Two scenarios assert this — *The suite owns the dagger it files
into* and *A killed suite leaves the operator's graph alone* — by reading
the deployed dagger's project list before and after filing.

`MILL_ACCEPTANCE_DAGGER_SOCKET` points the suite at an external dagger
instead, for the deliberate case of exercising the deployed service — its
version, its policy, its principals. Everything the suite does is the same;
only the socket changes, and with it who cleans up: the suite archives its
scratch projects at the end of each scenario, and a killed run leaves
whichever it had not reached. The fencing scenario's second principal,
`MILL_ACCEPTANCE_SECOND_DAGGER_CONFIG`, is a principal of that dagger and
is read only in this mode; the suite's own dagger has one principal, and
the scenario claims as it under a second agent ref.

## What it does

Each scenario stages a fixture and then drives it:

1. A repository seeded with the fixture's files: a scratch GitLab project in
   the scratch namespace, merge method `merge` and squash disabled — or, for
   a fixture that never reaches a forge, a bare repository in a temporary
   directory ([Without a forge](#without-a-forge)).
2. A scratch dagger project in the suite's own dagger — or in the one
   `MILL_ACCEPTANCE_DAGGER_SOCKET` named — and the fixture's node filed in
   it.
3. One `mill run` per attempt, as a subprocess, with that repository as the
   dagger project's `vcs_url` and its `--origin`, and a workspace root and
   log directory of the scenario's own. The environment mode is the default, `inherit`, which is
   what a person at a shell gets; a deployment runs `--env-mode strict`, as
   `contrib/systemd/mill.service` does (docs/architecture.md §"Skill steps:
   the harness adapter").

Nine scenarios run, in parallel. Five of them deliver to a forge and skip
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
  the same session, and the node is delivered anyway.
- **A worker fenced out writes nothing.** A run in flight has its lease taken
  and the node claimed by a second worker. The running `mill run` exits `75`,
  and a fresh run finishes the node with no second merge request on the
  branch. A fenced worker notices at its next heartbeat, and the scenario
  gives it a short one.
- **One node, planned and delivered.** The `feature` workflow end to end:
  every step through `pr` closed on the evidence its definition names, the
  plan in the branch under `docs/plans` naming the run, the reviewer in a
  session of its own, and a pull request for the run's branch.
- **A feature is delivered through the gate.** The milestone's whole: a node
  with no workflow named, sized by the run itself, planned, stopped at a
  gate a person answers, and merged. See
  [A feature is delivered through the gate](#a-feature-is-delivered-through-the-gate).

Four need no forge and run on every host that can start the suite:

- **The reviewer says no.** The seeded bad implementation, rejected on the
  record. It uses the forge when there is one, and a bare repository on this
  host when there is not. See
  [The reviewer says no](#the-reviewer-says-no).
- **The suite is honest about what it could not run.** The skip machinery
  itself: a host with no forge is asked for no forge credentials, a
  forge-dependent scenario's skip names `MILL_ACCEPTANCE_FORGE_URL` and
  points at this document, and the reviewer scenario stages without
  reaching for a forge at all. It costs no session and no merge request.
- **The suite owns the dagger it files into** and **A killed suite leaves
  the operator's graph alone.** The suite's own discipline: with no socket
  named, a scratch project lands in the dagger the suite started, and the
  deployed dagger's project list is the same before and after — including
  for a project whose archive never runs, which is what a kill leaves.
  Both skip when `MILL_ACCEPTANCE_DAGGER_SOCKET` is set, because filing
  there was the operator's decision. They cost no session and no merge
  request.

### What it takes

Scenarios run concurrently, so the suite's wall clock is its slowest
scenario and not their sum. The numbers below are observed on the
development host on 2026-09-09, `ginkgo -p` across fifteen processes,
against the homestead GitLab and one Claude account.

| Scenario | Wall clock | Cost |
|---|---|---|
| One node, delivered | 2 min 20 s | $0.37 |
| A killed worker's node is resumed by the next run | 1 min 58 s | $0.36 |
| A worker fenced out writes nothing | 2 min 00 s | $0.37 |
| One node, planned and delivered | 4 min 28 s | $1.09 |
| The reviewer says no | 6 min 33 s | $2.44 |
| A feature is delivered through the gate | 9 min 11 s over its two passes | $2.22 |
| The suite is honest about what it could not run | under a second | nothing |
| The suite owns the dagger it files into | under a second | nothing |
| A killed suite leaves the operator's graph alone | under a second | nothing |
| **The whole suite** | **9–10 min** | **$6.85** |

The suite's own dagger adds its container's start to the setup, before any
scenario runs: a few seconds with the image pulled, up to the fixture's
three-minute allowance without it.

So the suite fits inside thirty minutes on this host with room to spare.
Its wall clock is *A feature is delivered through the gate*, which is the
longest path by a margin — nine sessions over two passes, five of them
Opus — and every other scenario finishes while it is still running: the
other six together took 6 min 34 s. What would put the suite over is a
retry: the reviewer's scenario and the feature gate's are the two that can
go round again, and a second round costs each of them roughly what the
first did. `make test-acceptance` allows ninety
minutes, which is that worst case and not the expectation.

The dollars are derived, not recorded. The ledger holds token counts and
says so — `mill cost` is *counts, never currency* — so the figures above
apply the published per-model rates to what `mill cost --project <scratch
project>` reports for the run: input, output, cache read at a tenth of
input, cache write at a quarter over it. The sessions themselves run on the
account's Claude Code login, so this is the list-price equivalent of the
work and not an invoice. Recompute it the same way after a change to the
models a workflow pins; the shape of the bill is Opus output and Sonnet
cache reads, and nothing else comes close.

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
failed or interrupted scenario still tears down what it made. What a kill
skips, the worker sweeps: everything a suite makes outside its own process
is named `mill-acceptance-<scenario>-<nonce>`, and a machine worker's
reconciliation pass at start takes what carries that name, is older than the
suite's budget (90 minutes, the suite's own timeout; `scratch-budget` in the
worker's configuration) and belongs to no live run
([sandbox.md](sandbox.md) §"External state", §"Reconciliation on resume").
The pass is recorded in the ledger's `reconciliations` table and printed as
the worker starts.

| Made | Cleaned up | How |
|---|---|---|
| Scratch GitLab project | Deleted | `DELETE /projects/:id` at the end of the scenario; a stale one, by the worker's sweep of `scratch-namespace` through the forge adapter |
| The suite's own dagger | Removed | `dg serve` killed, its database dropped and its socket's directory removed at the end of the suite; the PostgreSQL container terminated after them. Nothing of it is on the host afterwards. |
| Scratch dagger project | Archived | `dg project archive` at the end of the scenario; a stale one, by the worker's sweep of the dagger it is configured for. In the suite's own dagger this is a courtesy, since the instance goes with the suite. Against an external dagger it is the only cleanup there is: dagger has no delete, and archiving — read-only, every node out of every ready set — is as gone as dagger goes. The sweep calls delete when the service offers it, archives when it does not, and its report says which. |
| A live suite's machines | Stopped, run directory removed | Every machine is `mill-<run-id>`, the `mill run` or `mill work` subprocess's own; a delivered run destroys its machine and removes `/var/lib/mill/runs/<id>` and `/run/mill/<id>` itself, the scenarios that kill a run sweep theirs at teardown, and the worker's reconciliation takes the rest: a `mill-*` machine with no live run is an orphan, whatever made it |
| Promoted cache generations | **Kept** | `gen-<n>` under `/var/lib/mill/cache/<scope>/<name>`, earned by an evidence-closed step and the point of the warm scenario; eviction takes repo lineages by expiry and by watermark, `mill cache prune` on demand |
| Derived image subvolumes | **Kept** | `/var/lib/machines/mill-image-<digest>`, one per package list the fixtures declare; image expiry takes them, `mill image prune` on demand |
| Workspaces, clones, step logs, bare local origins, the built binary | Removed | Temporary directories the suite owns |
| `mill serve` | Killed | Its process group, at the end of the suite |
| Rows in `MILL_ACCEPTANCE_DATABASE_URL` | **Kept** | The ledger is the record of the run and is worth reading after a failure |
| Workflow versions a scenario derived | **Kept**, at project scope | A workflow is immutable and `mill workflow` has no unpush, so a derived definition is pushed into the scenario's own dagger project — where the next suite run's assessment cannot see it. The suite refuses to push one anywhere else: `stageWorkflow` fails the scenario that asks for org scope |

A `SIGKILL` to the suite itself skips all of it, with one exception that
is the point of the suite's own dagger: its scratch projects are in a
database of a container testcontainers' reaper removes a few minutes after
the suite's process is gone. Nothing a killed run made reaches the
operator's dagger; `dg project list` there is what it was before the run.
What a kill does leave on this host is two orphaned servers — the suite's
`dg serve` and its `mill serve`, each in a process group of its own so that
a scenario's kill of a `mill run` cannot reach them — and the temporary
directories. Scratch projects on the forge, and in an external dagger, wait
for the next worker start; to clean up by hand before then:

```sh
# Scratch projects on the forge.
curl -sH "PRIVATE-TOKEN: $MILL_ACCEPTANCE_FORGE_TOKEN" \
  "$MILL_ACCEPTANCE_FORGE_URL/api/v4/groups/$MILL_ACCEPTANCE_FORGE_NAMESPACE/projects?search=mill-acceptance"

# Scratch projects in an external dagger, when MILL_ACCEPTANCE_DAGGER_SOCKET
# pointed the run at one. The suite's own dagger needs nothing here.
dg --socket "$MILL_ACCEPTANCE_DAGGER_SOCKET" project list \
  | jq '.projects[] | select(.slug | startswith("mill-acceptance"))'
dg --socket "$MILL_ACCEPTANCE_DAGGER_SOCKET" project archive <slug>

# The servers a killed run orphaned, then whatever else it left on this
# host: workspaces, the binary, and the suite's dagger socket directory.
pkill -f 'dg serve --socket /tmp/mill-dagger-'
pkill -f 'mill serve --socket /tmp/mill-acceptance-svc-'
rm -rf /tmp/mill-acceptance-* /tmp/mill-dagger-*

# Workflows at org scope, which is what every assessment chooses between.
mill workflow list --socket <socket>
```

Nothing is named without a nonce, so a leftover from an earlier run never
collides with a later one — with one exception, which is workflows. They are
not named with a nonce, because a scenario that derives one is deriving a
definition a deployment could plausibly have, and the assessment picks
between them *by name*. So a derived definition goes in at project scope,
where only the scenario that pushed it can resolve it, and the suite fails
any scenario that tries to push one at org scope (*A suite's attempt to push
a workflow at org scope fails the scenario that attempted it*). An org-scope
definition this tree's `workflows/` does not hold is a leftover from before
that guard, and there is no verb to remove it: the store is a deployment's,
and dropping the row is the operator's own `DELETE FROM workflows WHERE name
= … AND scope = 'org'`. *A feature is delivered through the gate* asserts
the org scope is clean before it spends a session, so the failure names the
leftover rather than the wrong workflow it caused to be chosen.

## When it fails

- **The preflight refuses.** Read the list; every line names a variable, a
  path or a tool. Nothing has been spent.
- **A scenario refuses before it starts, naming a workflow.** The org scope
  of `MILL_ACCEPTANCE_DATABASE_URL` holds a definition this tree does not,
  left by an older suite run, and the assessment would have been free to
  choose it instead of `feature`. See
  [What it leaves behind](#what-it-leaves-behind). Nothing has been spent.
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
- **A run fails outside the suite, on a live node, and the node is not
  reclaimed.** Selection ending in a bad config (an unknown workflow, a
  missing or malformed `build-workflow` definition, an assessment that
  exhausted its retries) or the run's machine going missing (a lost
  sandbox) leaves the run `failed` and holds the node in dagger under the
  failure's own reason — `dg node show <node>` says why. Holding is
  independent of a node's open/closed status, so `mill restart <node>` (a
  fresh generation) or `mill rerun <run> --from <step>` (retrying this one)
  do not by themselves make the node claimable again; a person clears the
  hold first, with `dg unhold <node> --reason ...`.

## A feature is delivered through the gate

O1's done-when, executable ([prd.md](prd.md) §8 O1,
[roadmap.md](roadmap.md) §O1): a feature node the reference harness would
have worked, delivered by mill, with a plan gate answered by a person.

**The fixture** is a Go module with one subcommand dispatched by hand, and a
node asking for a second subcommand, for somewhere to put the third, and for
tests — three acceptance criteria and two scenarios. That shape is what the
assessment's own skill calls a feature before it has opened a file: a new
subcommand, new behaviour that has to be tested, and a design decision left
open twice over — what the registry looks like, and how words are split. It
is sized so `implement` is one session: a small registry, one function, one
test.

Neither the program nor the node hands the change to the assessment
pre-sized, and that is load-bearing rather than stylistic. An earlier
fixture shipped a `subcommands` map and a note saying to touch two named
files; the assessment read that as an entry in a table somebody else had
written and answered `quick-fix` twice running, with the two files and the
existing `lines` template as its signals. A work order that has done its own
sizing is a work order the assessment will agree with.

**Nothing names a workflow.** `mill run` is invoked with neither `--workflow`
nor `--tier`, which is what a project with no default looks like, so the run
runs `build-workflow`'s `assess` as its step 0 and the assessment names
`feature` ([domain-model.md](domain-model.md) §Selection). The spec asserts
the selection's basis is `assessment`, the workflow is `feature` and the
tier is `feature`, and puts the whole assessment in its report.

**The gate is not automatic.** `feature`'s `plan-approval` auto-approves an
approved plan at this tier, which is the right default and the wrong thing
to test with, so the scenario pushes `feature` **into its own dagger
project** with the `auto` key removed and nothing else changed. A project
definition shadows an org one of the same name, so the run resolves
`feature` to that definition and every scenario beside it still resolves the
real thing. Everything else — the skills, the models, the retries, the ask —
is `workflows/feature/workflow.yaml` byte for byte, read at run time.

**Then the two passes.** The first ends when the gate is filed: `mill run`
exits `0`, the run is `waiting`, and one gate node hangs off the fixture's
node with the plan's first forty lines quoted in its ask. The suite asserts
the notify command was handed it — `--notify-command` points at a script
that keeps the notice, and the spec decodes the JSON and checks the kind,
the gate, the node and the run — then completes the gate with `dg complete`,
under the operator's own configuration and with no lease, which is what a
person answering in dagger does. The second pass is `mill run` again: it
resumes at the gate, reads the answer, and delivers.

**What is asserted at the end**: the node is `done`; the merge-commit
artifact is a SHA on the default branch; the reviewer's last verdict on the
node is `approve`; every step of the run — `assess` and `select` included —
closed on the evidence its definition names; `mill inspect` shows no closed
attempt with `unspecified` evidence; and dagger's usage rollup carries at
least three models. The assessment, the gate and its notify payload, the
verdict and the merge SHA all go into the spec's report.

**Answering the gate from Slack** is the person's part of `prove-o1` and not
this suite's: the suite answers in dagger, which is where the answer is
written either way.

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

**The workflow it runs**, and the repository, depend on the host. With a
forge, it is `feature` itself against a scratch project, so a run that
answers the reviewer's no goes all the way to a merge on the default branch
and the scenario asserts that too.

Without a forge, `feature` opens the pull request before the reviewer reads
the change, so a run of it parks at `pr` and the reviewer never speaks. The
scenario then derives its definition from `workflows/feature/workflow.yaml`
at run time and drops the two steps whose evidence only a forge can state —
`pr`, whose evidence is a URL, and the `merge` gate, whose evidence is a
status — keeping every other step, skill, model, retry and `on_failure`
exactly as `feature` writes them. It is pushed under the name
`feature-no-forge` so a scenario running beside it still selects the real
thing, and the spec asserts the run never reached `pr`.

**Cost.** Two sessions, and up to eight Opus steps across them: the
implementing session's think, plan, implement and simplify, and the
reviewer's review-plan and review, plus a further implement and review for
each round the reviewer sends back. One round is six and a half minutes and
$2.44 ([What it takes](#what-it-takes)); it is the most expensive scenario
in the suite, and a second round roughly doubles it.

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
the scenarios that end in a merge request skip — naming that variable and
this document, which *The suite is honest about what it could not run*
asserts — and *The reviewer says no* runs against a bare git repository in a
temporary directory, which `mill run --origin` points a workspace at. It is
a real repository cloned by real git; the only thing missing is the API no
step in that scenario calls.

The dagger project still has to name a repository — a run resolves its
project's forge after the claim and before the clone, so a project with none
fails before it has spent a session — and the suite creates the scratch
project with a placeholder pointing at a host that does not exist. The
scenario asserts for itself that the run never reached `pr`, which is what
makes the placeholder safe.

Half a forge is refused: a URL with no token or no namespace fails the
preflight rather than skipping, because that is a configuration mistake and
not a choice.

## The live tier

Most of the suite proves mill against a real forge and a real Claude account
but on the mill's own host account — a clone under `~/.local/state/mill`, the
session on the account's login. The **live tier** proves the per-run sandbox
of [sandbox.md](sandbox.md): the same node delivered inside a
`systemd-nspawn` machine — cloned, imaged, cached, booted, worked by a
session that runs as an unprivileged uid in its own network namespace,
tested, merged — with the properties the design promises asserted from
*outside* the machine. It is `tests/acceptance/sandbox_test.go`, labelled
`live`, and it is what the bootstrap gate (`prove-sandbox`) runs.

**Where it runs.** On `core`, by hand, as below; and inside every machine a
node of mill's own is worked in, as the `live-tier` check step of the mill
project's workflows (`workflows/mill-quick-fix`, `workflows/mill-feature`),
which runs `MILL_SANDBOX_HOST=1 mise exec -- make test` on the committed
tree after `clean-tree`. Inside a machine the specs that need the host —
the template unit, the host's podman, the subscription credential, a forge
— skip naming what they need, and the ones written for the inside run
against the machine's ephemeral root ([sandbox.md](sandbox.md)
§Self-hosting, "What the tier is inside a machine"). `mill logs <run>
--step live-tier` has ginkgo's report; the step's exit is its evidence.

**What it needs.** `core`, prepared by homestead's roles
([sandbox.md](sandbox.md) §"What the host provides"): `mill_storage` for the
btrfs volume at `/var/lib/mill`; `mill_machine` for the template unit, the
root-owned helper at `/usr/libexec/mill-machine`, its policy and its polkit
and sudoers rules; `mill_egress` for the nftables table and squid on the
`mill-host` link; `mill_selinux` for the file context; and `mill` for
`/run/mill`, the base image in Harbor (`mill-image.timer`, or
`systemctl --user start mill-image.service` once) and `/var/lib/mill/dagger`
— a clone of dagger's `main`, which nothing pins: mill's `go.mod` has no
dagger dependency and dagger has no tags, so a dagger change can fail the
fixture inside a machine before mill changes at all, and a pin is a roadmap
item ([roadmap.md](roadmap.md) §"What mill needs from the neighbours"). The
configuration file the deployed worker reads is the one `mill doctor`
reads: keys in their long-flag spelling, and `nested_repos`,
`dagger_checkout` and `model_credential_minted` with underscores — a
hyphenated one sets nothing ([tools.md](tools.md) §Configuration). On top
of the ordinary suite's variables:

| Variable | What it is |
|---|---|
| `MILL_SANDBOX_HOST` | Set to anything to turn the live tier on. Unset, every `live` spec skips with the message that names its cost: *set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine*. `make test` never sets it, so the tier skips there and on any laptop — silently, because without `MILL_ACCEPTANCE` the whole suite skips at `BeforeSuite` first; the live specs' own message shows under `MILL_ACCEPTANCE=1` with this unset. |
| `MILL_SANDBOX_MODEL_CREDENTIAL` | The `claude setup-token` file the credential gateway presents upstream for a machine's sessions. No login enters a machine, so a machine run has no account credential of its own; supply the setup-token the deployed worker loads from its vault ([sandbox.md](sandbox.md) §Credentials). Unset, the live tier skips before booting anything. |

The service the tier's `mill run` and `mill work` write to is the ordinary
suite's, on `MILL_ACCEPTANCE_DATABASE_URL` — on `core`, `make db-up`'s
development database. That database outlives every suite run, and a
worker's reconciliation reads every live run it holds: a run killed by an
earlier tier run is a lost sandbox finding (harmless), and a workflow row a
mill of another version stored is one the listing cannot decode: the worker
logs the row and the field and starts without it, and `mill status` warns
the same way ([tools.md](tools.md) §status). The row stays until something
removes it, and `make db-reset` is the answer; it is the throwaway
development database and nothing else.

The live tier **refuses** to file into the deployed dagger. It boots real
machines and drives real `mill run` subprocesses, and `mill#49` already showed
a test process becoming a worker against the operator's graph; so a
`MILL_ACCEPTANCE_DAGGER_SOCKET` — or a `DG_SOCKET` — that names
`/run/dagger/dg.sock` fails the tier before anything boots, naming the socket
and telling you to unset it and let the suite start its own throwaway dagger
from `internal/testsupport` ([Which dagger](#which-dagger)). The live tier
runs on that same disposable dagger `mill#51` gave the suite.

**Run it one scenario at a time.** The first host run is debugged scenario by
scenario, because a machine that will not boot, an image that will not build,
or a proxy that will not answer is a different repair each time. Each scenario
is `--focus`able by its name:

```sh
set -a; . ~/.config/mill/acceptance.env; set +a
export MILL_SANDBOX_HOST=1 MILL_SANDBOX_MODEL_CREDENTIAL=~/.config/mill/credentials/claude-oauth
make sidecar-build
MILL_ACCEPTANCE=1 ginkgo --label-filter=sandbox \
  --focus 'One node, delivered, inside a machine' ./tests/acceptance
```

The scenarios: *One node, delivered, inside a machine* (the whole delivery,
its every step's evidence in the ledger, and the ledger's workspace row
recording the machine, the derived digest, the veth and the lineages it
restored and promoted, with the promoted generation found on the volume);
*The db suite runs inside a machine* (`make test-db` inside a machine bound
to a mill checkout, against the machine's own podman socket, once the run's
own toolchain step has installed what `mise.toml` pins; no container
outliving the step); *The boundaries hold* (the uid, the unreachable home,
the proxy the only way out, no login in the machine, nothing outliving its
step, `machinectl list` empty after, the gateway's log naming the step token
and never its value); *The second run is warm* (the go-build lineage
restored from a promoted generation and the build step faster off the
ledger's durations); *The live tier refuses the deployed dagger* (the
refusal, which needs no host); and *A killed suite leaves no machine, run
directory, or project after reconciliation* (a run killed mid-flight, the
lease taken back, and a `mill work --workspace machine` started against the
suite's own service and dagger, whose startup reconciliation sweeps what the
kill left).

Every network assertion runs from *inside* the machine, through the helper,
never from the host with a source-interface trick: squid refuses host
processes by design (`homestead#40`), so a host probe is denied whatever
the machine's policy is. The fixture's `mise.toml` pins an exact go release
rather than a minor (`1.25.14`, not `1.25`): a partial pin makes mise list
the releases upstream before it settles on one — a `git ls-remote` of
golang/go from inside the machine that the toolchain step then pays on
every run — while an exact release already in the worker's global `mise`
lineage costs the step no lookup at all. Its build cache is warmed by a
mise `postinstall` hook (`go vet` over a package that pulls in `net/http`),
which runs on every `mise install`, so the toolchain step is what the warm
scenario times.

**What it creates, and what it leaves behind.** One machine per run, named
`mill-<run-id>` (every machine here is `mill run`'s or the worker's; the
`mill-acceptance-<scenario>-<nonce>` name is the scratch project's and the
dagger project's, which is what the sweep by name matches), on the btrfs
volume; a run directory under `/var/lib/mill/runs`; cache copies restored
from the worker's lineages; a derived image with `btrfs-progs` for the db
suite's machine; and, for the delivering scenarios, a scratch project and a
merge on it as the ordinary suite makes. It leaves behind **nothing but
promoted cache generations** — the `gen-<n>` snapshots an evidence-closed
step earns, which are the point of the warm run and which eviction reclaims
under its watermarks ([sandbox.md](sandbox.md) §Eviction) — and the derived
image, which image expiry takes. Everything else is swept **by name**: a
`mill-*` machine with no live run, its run directory, and stale scratch
projects on the forge and in dagger are taken by the next worker's
reconciliation pass ([sandbox.md](sandbox.md) §"External state",
§Reconciliation), which the killed scenario exercises on purpose. The two
scenarios that kill their own run — the db suite's, and the boundaries when
it fails — sweep it themselves at teardown, because a killed run is live in
the ledger with no worker to resume it: `mill-machine terminate` through the
helper, every subvolume under the run directory deleted, the directory and
the socket directory removed. To do the same by hand after a suite killed
from outside: `sudo /usr/libexec/mill-machine terminate mill-<id>`, then
under `/var/lib/mill/runs/<id>` a `btrfs property set -f <copy> ro false`
and `btrfs subvolume delete` of each `cache/*` and `candidate/*` as the
worker's own account, `rm -r` the directory and `/run/mill/<id>`; the
scratch projects go the way [What it leaves behind](#what-it-leaves-behind)
describes.

**Its wall clock and cost the first time.** The first run on a cold host pays
for the base image: `mill image build-base --push` is minutes of `dnf`
(homestead's `mill-image` timer does it weekly, and the role's first apply
does it once), and the first machine's derived build and cache lineages
start empty, so *One node, delivered, inside a machine* is dominated by the
toolchain install (`mise install` of the Go toolchain through the proxy)
rather than the session. Budget the base build separately — it is a
prerequisite, not a scenario. On `core` with the base built and the global
`mise` lineage holding the go release, measured on 2026-09-11: *One node,
delivered, inside a machine* ran in 70 s end to end (the machine ready
5 s after the claim, the toolchain step 3.9 s cold, the four sessions and
the merge the rest); *The boundaries hold* 82 s; *The second run is warm*
142 s for its two deliveries, the toolchain step 3.9 s cold against 0.22 s
warm. *The db suite runs inside a machine* is the long one: `make test-db`
compiles the module cold inside the machine and pulls PostgreSQL through
Harbor's proxy for every `db` suite. The session cost is the ordinary
suite's: roughly a quick-fix's `$0.37` per delivering scenario, and the
warm scenario delivers twice. Recompute the base build's wall clock the
same way after a change to `images/base`; the shape of the first-run bill
is `dnf` and a cold toolchain, and nothing else comes close.

## Adding a scenario

Adding a scenario is adding a fixture. `tests/acceptance/fixtures_test.go`
holds one: the files a scratch project is seeded with, and the node's title
and body. `fixture.stage` does the rest. A fixture that sets `Local`
is staged as a bare repository on this host instead of a project on the
forge, which is how the seeded bad implementation runs where there is no
GitLab.
