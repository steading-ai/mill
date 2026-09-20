# Operator tools

What to run when a run misbehaves on the host, and what each answer means.
The same tools are one client with two framings: the CLI for a shell, `mill
mcp` for a session, exactly as `dg` and `dg mcp` are ([architecture.md](architecture.md)
§"One binary, two faces"). An operator in a Claude Code session and a thicket
agent asked to look are reading the same fields.

Two rules hold across all of them.

**Read tools return ledger data, never transcript content.** The ledger holds
evidence, sessions' identifiers, and usage — not what a model said
([ledger.md](ledger.md) §"What the ledger is and is not"). The one place a
session's own output surfaces is `mill logs`, which reads a file the redactor
already wrote.

**`run` is not among them.** It executes in the calling process and is local
to the mill's host and account. A session elsewhere that wants work done
files a dagger node ([prd.md](prd.md) R-55).

Every tool takes `--socket`; the readers take `--view`; the two `workspace`
subcommands take `--workspace-dir`, since the files they answer about are on
this host and not in the ledger. The `image` and `cache` subcommands take no
socket at all: they answer about subvolumes, lineages and a registry on this
host, and nothing in the ledger knows any of them. `doctor`, `cache` and
`image` read the configuration file (§Configuration) for the settings a
deployment keeps there, `--config` naming another.

| Tool | What it answers |
|---|---|
| `status` | What is live, and which step each run is on |
| `inspect <run>` | Every attempt at every step, and the evidence that closed it |
| `explain <run>` | Where one run is, and what it is waiting on |
| `logs <run>` | What the session on a step said, redacted |
| `history <run>` | What changed about a run, and when |
| `cost` | What a window of runs billed |
| `workflow list\|show` | What a run on this project would follow |
| `workflow push\|pull` | Store a workflow directory as a version; write a stored version back out |
| `workspace show <run>` | Where a run's files are, and what state they are in |
| `doctor` | Whether this host could work at all |
| `cancel <run>` | Stop a run's current step cleanly, leaving it resumable |
| `rerun <run> --from <step>` | Redo from a step, keeping the generation and the sessions |
| `workspace clean` | Remove the checkouts of runs that have ended |
| `image ls` | Which image subvolumes this host holds, from what, and when last booted |
| `image prune` | Evict image subvolumes: expired, then least recently booted under the watermarks |
| `image build-base` | Build the base image machines boot from, and with `--push` publish it |
| `cache ls` | Which cache lineages this worker holds, their generations, size and last restore |
| `cache prune` | Evict: expired lineages and images, then least recently used under the free-space watermarks |

A run is named by its node reference — `mill#18` — or by the run id `status`
prints. Anything carrying a `#` is a node reference; anything else is a run
id. A node reference resolves to that node's most recent run in the ledger,
so the reference an operator already has in front of them is enough; a
reference the ledger holds no run for is told so — *the ledger holds no run
for node mill#99: the mill has never worked it* — rather than answered with
silence.

## Views

The readers size their answer with `--view`, as dagger's tools do, so an
agent that starts narrow does not fill its context with a backlog.

| View | What it is |
|---|---|
| `refs` | Identifiers alone: what to ask about next |
| `summary` | The fields worth reading at a glance. The default |
| `full` | Everything the ledger holds, in the wire's own spelling |

## status

Runs by state, with the step each is on, how long it has been going, what it
has cost, and the gate a waiting one is blocked on. With no `--status`
filter this is every run that has not ended.

A run listed here whose node dagger has since closed — dropped, or completed
by a person — does not stay: the worker's sweep fails it within a sweep
interval (`--sweep-interval`, fifteen minutes by default) of noticing, and
`history` then shows the `failed` event with the reason
(`docs/ledger.md` §Sweep).

```
$ mill status
NODE       STATUS   STEP       ATTEMPT  ELAPSED  TOKENS       GATE
millcli#1  running  implement  1        0s       184213/9820  -
```

`TOKENS` is input/output, summed over models. Counts, never currency: the
ledger records tokens and a rate table is applied by whoever wants money.

`MACHINE` is the machine a run on the `machine` backend runs in, as the
ledger's workspace row names it (`ledger.md` §workspaces); `-` for a local
run. `--view full` adds `IMAGE`, the derived digest it booted, `VETH`, and
`CACHES`, each lineage as `<scope>/<name>@<restored>` with `->` and the
generation it promoted once the run has finished.

A workflow row the store cannot decode — one a mill of another version
wrote, with a field this one's schema does not have — does not stop the
listing. The run on it is still listed, with no `STEP`, because the step is
derived from the definition; and after the table one line on standard error
names the row and the field:

```
warning: workflow 01K4… does not decode and was left out: field "model": proto: (line 1:2): unknown field "model"
```

The row is reported and nothing else: not deleted, not repaired. `workflow
list` says the same of a row it leaves out, and a worker starting on the
`machine` backend, whose reconciliation reads the same listing, logs the same
line and starts. The remedy is the operator's; for the development database
it is `make db-reset` ([runbook.md](runbook.md)).

## inspect

One run in full: the workflow version it follows and every attempt in order,
with the evidence that closed it, how its process ended, and what it cost.

```
$ mill inspect millcli#1
millcli#1 run 01M20W6CBS8NX5QRG9XPYTD21P generation 1
  status    running
  workflow  quick-fix@fb2764fd70fd
  step      implement attempt 1
  branch    mill/millcli-1
  tokens    184213/9820

STEP            ATTEMPT  STATUS   EXIT  EVIDENCE     VALUE
select          1        closed         structured   workflow=quick-fix
gather-context  1        closed         artifact     sha256:3f3f3f3f3f3f3f…
implement       1        running        unspecified  -
```

A run on the `machine` backend shows its machine under the header, from
the ledger's workspace row (`ledger.md` §workspaces): the name and unit, the
derived image digest it booted, the host side of its network link, and one
`cache` line per lineage with the generation the run's copy was restored
from and the one it promoted — `-` until the run has finished, and for a copy
the run discarded. A local run shows none of these.

```
  branch    mill/millcli-1
  machine   mill-01m20w6cbs8nx5qrg9xpytd21p (mill-machine@01m20w6cbs8nx5qrg9xpytd21p.service)
  image     sha256:3c9a43f2…
  veth      ve-mill-01m20w
  cache     global/mise restored 3 promoted 4
  cache     repo/9f1c…/go-build restored 0 promoted -
```

`VALUE` is the claim the mill verified — a SHA, a URL, an artifact digest, a
gate node's id — and not a description of it. `select` is the ledger's record
of which workflow was chosen and why; no workflow declares it.

`--view full` adds each attempt's kind, session and `TOKENS`: the adapter's report
as `status` shows it, and, for an attempt whose session ran behind the
credential gateway, the gateway's own count beside it as `gateway:in/out`
(§cost, `docs/sandbox.md` §Credentials).

A step that closed on a verdict shows the decision, and any blocking finding
inline as `criterion: summary` — a blocking finding is what the reviewer held
against the work, and reading it should not cost a second command. Notes are
not shown: they decided nothing.

```
review-plan     1        closed         verdict      approve
review          1        failed
review          2        closed         verdict      approve (AC-2: the assertion is thin)
```

A `revise` closes nothing, so the attempt it failed has no evidence and no
value: that is the empty row above. The verdict behind it is on the node in
dagger, as a text artifact named `<step>-<generation>.<attempt>.md` carrying the decision,
every finding, and the id of the session that reviewed
([domain-model.md](domain-model.md) §Verdict). The same findings are the next
attempt's prompt, and `mill logs --step review` is the session that wrote
them.

## explain

Where a run is, why that step is open, what would close it, how many attempts
it has used, and what it is waiting on. This is the tool to reach for when
asked why a run is stuck.

```
$ mill explain millcli#1
millcli#1 run 01M20W6CBS8NX5QRG9XPYTD21P is running
  step      implement (skill) running
  evidence  artifact would close it
  attempts  1 of 3 retries
  waiting   step implement to produce artifact evidence
```

A run that is `waiting` or `parked` is blocked on a dagger gate, and only a
person can answer it. `explain` names the gate node and its ask, which is
what somebody needs in order to go and answer it:

```
$ mill explain millcli#1
millcli#1 run 01M20W6CBS8NX5QRG9XPYTD21P is waiting
  step      merge (gate) running
  evidence  status would close it
  attempts  1 of 1 retries
  gate      millcli#2
  ask       Merge !412 for millcli#1?
  waiting   a human to answer gate millcli#2
```

Resolving the gate's reference and title needs dagger: `--dagger-socket` and
`--project`, or `$DG_SOCKET` and `$DG_PROJECT`. Without them the gate's id is
printed and nothing else is lost.

## logs

The per-step-run log: the session's events, one summary line each, as the
worker wrote them through the redactor
([architecture.md](architecture.md) §Observability). With no `--step` this is
the step the run is on, or — for a run that has finished, and so is on
none — the last step it attempted. `--follow` tails the file and returns when
the attempt leaves `running`, so a session watching a step is told the step
ended rather than guessing from silence.

```
$ mill logs mill#18 --step implement --follow
2026-09-08T16:04:08Z text reading the work order
2026-09-08T16:04:11Z tool Read call=toolu_017
2026-09-08T16:04:11Z result call=toolu_017 ok out=package cli assembles the cobra command tree…
2026-09-08T16:05:52Z usage claude-sonnet-4-5 in=184213 out=9820
```

The redaction happened at write time and there is no unredacted copy: a value
a session registered with the workspace's `redact` verb was masked before it
reached the file. Full tool output is never written; a summary line per
result is.

Every kind of step writes one. A **check** step has no session and no events,
so its file is what its command wrote: the exit status, then stdout when the
check's contract keeps it, then stderr — which is where a check says why it
failed.

```
$ mill logs mill#18 --step clean-tree
2026-09-08T16:11:02Z check clean-tree exit status=1
2026-09-08T16:11:02Z check clean-tree stderr begin bytes=29
the tree is dirty: README.md
2026-09-08T16:11:02Z check clean-tree stderr end
2026-09-08T16:11:02Z check clean-tree failed: exited 1
```

A **skill** step whose harness refused or crashed carries the harness's own
standard error under a `harness stderr` banner — for the claude adapter, the
sidecar's, bounded to the last 64 KiB. A sidecar that dies before writing its
terminal record leaves its stack trace there and nowhere else, and the
failure's own message is that stream's last twenty lines rather than the one
line a crash ends on.

Logs live under `--log-dir`, one file per attempt, defaulting to
`$XDG_STATE_HOME/mill/logs`. A worker started without one keeps no logs and
this tool has nothing to read.

## history

The run's audit trail, oldest first: what changed, when, and under which
worker. The record of a run that has already ended, when `inspect` has been
superseded by later attempts. A run the sweep ended carries a `failed`
event whose `reason` says why — `node dropped underneath the run`, or
`node completed outside the run` — or a `done` one when the node's
completion was the run's own and the store had missed it.

```
$ mill history millcli#1
WHEN                  OP              AFTER
2026-09-08T16:04:08Z  create_run      generation=1 lease_id=01HZZZ… status=selecting
2026-09-08T16:04:08Z  start_step_run  attempt=1 kind=skill step_name=select
2026-09-08T16:04:08Z  close_step_run  auto_approved=false evidence_kind=structured status=closed
2026-09-08T16:04:08Z  start_step_run  attempt=1 kind=skill step_name=gather-context
2026-09-08T16:04:08Z  close_step_run  auto_approved=false evidence_kind=artifact status=closed
2026-09-08T16:04:08Z  start_step_run  attempt=1 kind=skill step_name=implement
```

`--view full` adds the step run, the worker, and the fields as they were.

## cost

One window of usage rolled up four ways, because "what did this cost" is
asked of a run, of a workflow, of a step and of a model. `--since` takes a
window such as `24h` or `7d`.

```
$ mill cost --since 7d
RUN        SOURCE   INPUT   OUTPUT  CACHE READ  CACHE WRITE
millcli#1  sdk      184213  9820    1204331     0
millcli#1  gateway  184301  9820    1204331     0

WORKFLOW                SOURCE   INPUT   OUTPUT  CACHE READ  CACHE WRITE
quick-fix@fb2764fd70fd  sdk      184213  9820    1204331     0
quick-fix@fb2764fd70fd  gateway  184301  9820    1204331     0

STEP       SOURCE   INPUT   OUTPUT  CACHE READ  CACHE WRITE
implement  sdk      184213  9820    1204331     0
implement  gateway  184301  9820    1204331     0

MODEL                       SOURCE   INPUT   OUTPUT  CACHE READ  CACHE WRITE
claude-sonnet-4-5           sdk      184213  9820    1204331     0
claude-sonnet-4-5-20250929  gateway  184301  9820    1204331     0
```

SOURCE is who counted: `sdk` is the adapter's report of what the harness
billed, and `gateway` is the credential gateway's own count of the upstream's
`usage` block on every response it forwarded, taken from outside the session
(`docs/sandbox.md` §Credentials). A run with no gateway in front of it has
`sdk` rows alone. The two are listed beside each other and never summed into
one: the adapter's rows are what `status` and `inspect` call the cost and
what is posted to dagger, and the gateway's are the check on them — the
gateway sees the CLI's side requests the SDK does not report, and names the
model by its resolved id.

The MODEL table is summed from the rows the STEP table renders — every step's
bucket carries its own totals per model and per source — so the two tables
agree by construction and one of them cannot come back empty under the other.

## workflow show

One stored version as a run would walk it: what closes each step, the skill
body it pins, what its workspace handle grants, how many times it may fail
before the breaker parks the run, and what a failure does. This is the
definition; `inspect` shows what a run made of it.

`mill workflow show <name>` is the newest version of that name; `<name>@<version>`
and `--version <version>` both name a particular one.

```
$ mill workflow show quick-fix
quick-fix org 1a91a9657d7eca08735284abc1ba22c4aa4a500dd65527b3aab9ad0cf78c8eff

STEP            KIND   EVIDENCE  SKILL                        CAPABILITIES     RETRIES  ON FAILURE        SESSION
gather-context  skill  artifact  gather-context@7f2f5a2074f3  read             2        park              implement
implement       skill  artifact  implement@6d0a3794a0cd       read,write,exec  3        park              implement
commit          skill  commit    commit@6c1aea33c155          write            2        park              implement
clean-tree      check  exit      -                            exec             3        revise:implement  -
pr              skill  url       pr@63bfc09f1e95              forge            2        park              implement
merge           gate   status    -                            -                1        park              -
```

The `SKILL` column is the name and digest of the body the harness will be
handed. A workflow version is the hash of the definition and a step pins its
skill by name and digest, so the version on the header line is the version
`workflow list` prints for the same push, and one changed byte of one skill
body is a new digest here and a new version there. A check step and a gate
step pin no skill and print `-`.

The same for the tier that plans first. A step's harness options follow the
steps, printed as the JSON they are stored as: mill interprets no key of one
and has nothing else to say about it, and what an operator wants here is what
the step will hand the harness.

```
$ mill workflow show feature
feature org 6415ee6fe8105d1764a4031dd70037b947145c505e81a01559b3f07ae4ba1ce2

STEP            KIND   EVIDENCE  SKILL                        CAPABILITIES     RETRIES  ON FAILURE        SESSION
gather-context  skill  artifact  gather-context@7f2f5a2074f3  read             2        park              implement
think           skill  artifact  think@2337c82ccc2c           read             2        park              implement
plan            skill  artifact  plan@f076e8dd3ef0            read,write       2        park              implement
review-plan     skill  verdict   review-plan@8c1b12c1d413     read             2        revise:plan       review
plan-approval   gate   gate      -                            -                1        park              -
implement       skill  artifact  implement@6d0a3794a0cd       read,write,exec  2        park              implement
simplify        skill  artifact  simplify@b8333c666bef        read,write,exec  2        park              implement
commit          skill  commit    commit@6c1aea33c155          write            2        park              implement
clean-tree      check  exit      -                            exec             3        revise:implement  -
pr              skill  url       pr@63bfc09f1e95              forge            2        park              implement
review          skill  verdict   review@68e660b64496          read             2        revise:implement  review
merge           gate   status    -                            -                1        park              -

STEP            HARNESS  OPTIONS
gather-context  claude   {"effort":"medium","model":"sonnet","tools":{"allow":["Glob","Grep","Read"]}}
think           claude   {"effort":"high","model":"opus","tools":{"allow":["Glob","Grep","Read"]}}
plan            claude   {"effort":"high","model":"opus","tools":{"allow":["Glob","Grep","Read","Write"]}}
review-plan     claude   {"effort":"high","model":"opus","tools":{"deny":["Write","Edit","Bash","NotebookEdit"]}}
implement       claude   {"effort":"high","model":"opus","tools":{"allow":["Bash","Edit","Glob","Grep","Read","Write"]}}
simplify        claude   {"effort":"medium","model":"opus","tools":{"allow":["Bash","Edit","Glob","Grep","Read","Write"]}}
commit          claude   {"effort":"medium","model":"sonnet"}
pr              claude   {"effort":"medium","model":"sonnet"}
review          claude   {"effort":"high","model":"opus","tools":{"deny":["Write","Edit","Bash","NotebookEdit"]}}
```

The two workflows share four skills: `feature`'s `gather-context`,
`implement`, `commit` and `pr` steps name `quick-fix/<skill>`, and the record
they pin is the one `quick-fix` pins — same name, same digest. Pushing the
second of them says so, one line per skill: `skill implement 6d0a3794a0cd
unchanged`.

## workflow list

The versions the store holds, newest of each name unless `--all-versions`;
`--name`, `--project` and `--scope` narrow it. A row the store cannot decode
is left out and named on standard error, as `status` names it.

```
$ mill workflow list
NAME       SCOPE  VERSION       STEPS  CREATED
quick-fix  org    1a91a9657d7e  7      2026-09-11 06:12
feature    org    6415ee6fe810  13     2026-09-11 06:12
```

## workflow push

Store a workflow directory — `workflow.yaml` beside `skills/` — as a
version, and every skill its steps pin. The version is the hash of the
definition, so pushing unchanged files stores nothing and names the version
already there; one changed byte of one skill body is a new digest and a new
version. `--scope org|project` overrides the definition's scope, `--project`
names the project when the scope is `project`. A definition the loader
refuses — a step with no `timeout`, a preparatory step with a check's
contract — is refused here, naming the step. Needs the `operator` role.

## workflow pull

Write a stored version back out as `workflow.yaml` and one `SKILL.md` per
skill step, in a directory `push` accepts unchanged; `--version`,
`--scope` and `--project` name which. A skill that carries scripts, assets
or references cannot be written back: the store keeps their digests, not
their bytes.

## doctor

Ten probes, one line of reason each. Every probe is reported; the command
exits non-zero when any of them failed. A probe that passed with something
still worth looking at is marked `warn` and does not fail the command.

```
$ mill doctor
pass  socket     /run/user/1000/mill/mill.sock answers; mill 0.0.0-dev
pass  schema     migration 18, current
pass  dagger     /run/dagger/dg.sock answers; this account is a principal on mill, 1 node(s) ready for an agent
fail  sidecar    sidecar/dist/main.js did not answer: claude: the sidecar answered nothing before it exited
fail  signing    no signing key under ~/.local/state/mill/workspaces/signing
pass  forge      https://github.com/octo-org/thing.git accepts the credential store's forge-token ($CREDENTIALS_DIRECTORY/forge-token) as octocat
pass  notify     contrib/notify/thicket-send.sh accepts a probe notice
pass  workspace  local: a clone under the mill's account
pass  space      local: no sandbox volume to watch
warn  gateway    https://api.anthropic.com answers a token count through the gateway with credential:774fdbe2; the credential was minted 2025-09-30, more than 11 months ago; a setup-token lasts a year — mint a new one
```

| Probe | What it checks |
|---|---|
| `socket` | `mill serve` answers on its unix socket, and which version |
| `schema` | The database is migrated to the schema this binary carries |
| `dagger` | Dagger answers, and this account resolves to a principal that may read the project |
| `sidecar` | The harness sidecar starts, parses one frame, and answers |
| `signing` | The commit signing key exists and some project's `allowed_signers` names it — a checkout's, or the project's repository directly when no checkout exists yet |
| `forge` | The forge accepts the credential the mill holds — the credential store's `forge-token`, else `$MILL_FORGE_TOKEN` (§Forges) — through the adapter a run would deliver with. The line names where the credential came from. On GitHub it names the account the token authenticates as, and a token that has expired, or lacks one of the permissions §Forges lists, fails naming the credential and what it lacks. A `forge` setting that names no adapter fails here too |
| `notify` | The configured notify command runs and accepts a probe notice |
| `workspace` | Which workspace backend a run works in, and for `machine` whether the sandbox root is btrfs |
| `space` | For `machine`, free space on the sandbox volume against the eviction watermarks (`cache-high-watermark`, `cache-low-watermark`): pass above the low one, warn between, fail below the high one — eviction runs at every promotion and the host is short of space |
| `gateway` | The credential gateway's upstream answers one token count through it with the configured credential, and the credential's age against `model_credential_minted` |

`sidecar`, `signing` and `forge` ask the worker behind `--socket` to run
themselves, over the same `WorkerService` a run's own store connects
through, and report the worker's own verdict verbatim: this is what makes
`mill doctor` on a client host — a Mac reaching a Linux worker through a
forwarded socket, say — report the worker's sidecar, signing key and forge
credential, not a search of files that were never going to be on the
client's disk (docs/architecture.md §"One binary, two faces"). A `mill
serve` with no `--work` in it, or an older one, mounts no route for that
service; against one of those, the three probes fall back to the file search
below, run on the host `doctor` itself is on. `doctor` run on the worker's
own host asks the same RPC first and gets the same answer a remote client
would, which is the more authoritative one: it reads `--work`'s actual
configuration rather than `doctor`'s own flags, which may not agree with it.
Either way the reported line does not say which path answered it: the two
are meant to agree, since the RPC runs the same checks below in the worker's
own process.

The probes take the worker's own flags for what they check — `--dagger-socket`
and `--project` for dagger, `--sidecar` and `--env-mode` for the sidecar,
`--workspace-dir`, `--identity` and `--name` for the signing key, `--project`
and `--forge` for the forge — the repository is the dagger project's
`vcs_url`, the adapter is the file's `forge` or the host's — `--notify-command`, `--workspace` and `--sandbox-root`, and the
gateway's — so a host is probed as its worker is configured, or as a flag
says to try it. The deployment's configuration file (§Configuration) fills
the flags the command line left alone for the settings a deployment keeps
there and nowhere else: `sidecar`, `env_mode` and `workspace-dir`, so a
packaged host's installed sidecar and state directory are what is probed;
`workspace` and `sandbox-root`; `notify_command`; and the gateway's. The
sockets resolve as they do for every face — `--socket` and
`--dagger-socket`, then `$MILL_SOCKET` and `$DG_SOCKET`, then the file's
`socket` and `dagger-socket`, then the default — so a host whose file names
the paths is probed with no flag and no variable.

The sidecar probe sends a `cancel` request on stdin. Cancellation reaches a
running sidecar as SIGTERM, so a cancel on stdin is a frame the sidecar
understands and refuses by contract: it never starts a session, never reaches
the Claude API, and bills nothing. Its refusal is the answer the probe wants.

The signing probe looks for the key in an `allowed_signers` file of a
project checked out under `--workspace-dir`. A fresh host has a key —
homestead makes it at provisioning — and no checkout yet, nothing for that
scan to find; when the forge `--project` names implements the contents API
(GitHub today, not GitLab), the probe asks the project's repository
directly for the same file before failing, so such a host is green before
its first run. Neither a checkout nor a repository naming the key is a
plain failure, same as today.

The notify probe runs the deployment's `--notify-command` — the file's
`notify_command`, when no flag names one — with `{"kind":"probe"}` on its
standard input and nothing else: no gate, no node, no run. A notifier that
routes on `kind` can tell a health check from a person's decision and stay
quiet, and the shipped example does. A host with no notify command configured
passes and says so: filing a gate that reaches nobody is a supported choice,
because the answer is written in dagger either way (docs/architecture.md
§"Gate steps").

The workspace probe reports the kind in effect — `--workspace`, else the
file's `workspace`, else `local` — the same way a run chooses it
(docs/architecture.md §"The workspace"). Under `local` there is nothing to
probe beyond naming it. Under `machine` it asks what filesystem the sandbox
root is on — `--sandbox-root`, else the file's `sandbox-root`, else
`/var/lib/mill`, or `/home/agent/mill` when `systemd-detect-virt --container`
says the worker is itself inside a machine (docs/sandbox.md §Self-hosting;
`run`, `work` and `cache ls` default the same way) — and fails naming the
filesystem it found when that is not btrfs, because ephemeral roots, cache
lineages and eviction are all subvolume operations (docs/sandbox.md
§Storage):

```
fail  workspace  machine: sandbox root /var/lib/mill is xfs, not btrfs (docs/sandbox.md §Storage)
```

That is the whole of the probe. What else a machine needs of the host is not
probed: the template unit and its polkit rule, the helper and its sudoers
rule, `/run/mill`, the proxy, Harbor and the base image subvolume. Each is
found by the first run that needs it — `Check` refuses a missing `/run/mill`
before a claim, an unstartable unit or an unreachable Harbor fails the run's
provision naming what it asked for, and a proxy that does not answer is the
toolchain step's finding (`docs/sandbox.md` §"Failure modes") — and by
homestead's own specs for the roles that provide them.

The gateway probe is the credential gateway of `docs/sandbox.md`
§Credentials, asked the way a session would ask it. When `gateway: true` is
configured — `--gateway`, else the file — the doctor loads the upstream
credential from `--model-credential-file` (the file's `model-credential-file`,
else `$CREDENTIALS_DIRECTORY/claude-oauth`; never the environment), builds
the gateway in process, mints a step token for the probe alone, and sends one
`POST /v1/messages/count_tokens` through it to `--gateway-upstream`. The
probe does not listen: `--gateway-listen`, the address a worker's gateway
binds, is accepted for the file's sake and not used. A token
count is answered without billing, so the probe costs nothing and proves the
whole path: the file is there, the gateway accepts a step token, the upstream
accepts the swapped credential. It fails naming the file when the credential
is absent, and naming the upstream's status when it refuses:

```
fail  gateway    gateway: no upstream credential: /run/credentials/mill.service/claude-oauth: open …: no such file or directory
fail  gateway    gateway: api.anthropic.com answered 401 to the probe: {"type":"error","error":{"type":"authentication_error",…
```

The request carries every header mill itself sets on a real session's
request too — `Authorization`, `Content-Type` and `anthropic-version` — and
no more. Headers a real session also carries, like `anthropic-beta` and
`user-agent`, are chosen by the Claude Agent SDK inside the sidecar process
at request time, not by anything mill holds, so the probe cannot mirror them
without hardcoding a guess that would drift, unnoticed, the moment the SDK
changes what it sends. A credential the upstream would only accept alongside
one of those headers can fail this probe and still pass a real run.

The credential is a `claude setup-token`, which lasts a year, and the CLI's
own three-day expiry warning applies only to `/login` credentials, so the
doctor is where the age is watched: `model_credential_minted: YYYY-MM-DD` in
the configuration file records when it was minted, and the probe warns from
eleven months on — and warns that it cannot check when the date is not
written down. The probe never prints the credential; the line names it by a
fingerprint of its hash, as the gateway's log does. A host with no gateway
configured passes and says so: its sessions run on the account's login.

**What the dagger probe cannot check.** Dagger's API exposes no whoami and its
principals carry no name ([dagger's domain model](https://github.com/ivy/dagger)
§Principal: a principal is an id, an org and a kind). A successful `Ready`
proves the socket is there and that this account resolves to a principal that
may read the project; that the principal is the one called `mill` is not a
question dagger will answer today. Closing that gap is dagger's to do.

## cancel

Ask the worker holding a run to stop the step it is on. This sets
`cancel_requested` and writes nothing else; the worker acts on it
([architecture.md](architecture.md) §Cancel).

```
$ mill cancel mill#18 --reason "the wrong branch"
cancel requested on mill#18 run 01M20W6CBS8NX5QRG9XPYTD21P at step implement; the worker stops at its next heartbeat
```

Within one heartbeat interval plus the step's grace period: the session is
stopped through the adapter, the attempt is recorded `aborted` with reason
`cancelled`, and the lease is released `preempted` with detail `cancel`.

A cancelled run is not a failed one. It keeps its workflow, its branch and
its status, so `mill run` on the same node resumes at that step with the next
attempt. `mill restart` is the verb that throws the work away instead.

## rerun

"The work up to here is fine, redo from this step." Every step from the one
named through the step the run is on is superseded — the same mechanism a
`revise` verdict uses — and the run is left resumable at the named step.

```
$ mill rerun mill#18 --from implement
rerun mill#18 run 01M23T3ZQA2ZE8MD2V9ZAEKPZA from implement through clean-tree
STEP        ATTEMPT  STATUS      REASON
implement   1        superseded  rerun
commit      1        superseded  rerun
clean-tree  1        superseded  rerun

mill#18 is resumable at implement in generation 1; the sessions and the branch are kept
```

**`rerun` keeps the generation and the sessions**: the work before the named
step stands, the branch is the same one, and the implementer's session
resumes with the transcript it already has.
**`restart` does not**: it opens a new generation with new session ids,
destroys the workspace, deletes the branch and closes the pull request
([domain-model.md](domain-model.md) §Restart).

The superseded attempts stay in the ledger with the evidence they produced;
they simply close nothing, which is what moves the run backwards
([ledger.md](ledger.md) Invariant 10). `mill history` shows one
`supersede_step_run` event per attempt, each with reason `rerun` and the uid
that asked.

A run that **failed** is reopened by the same act. Failure is terminal to the
run loop, but a rerun on one is a person overruling the ending, and a span
superseded on a run no claim can take over would strand the node; so the run
goes back to `running` holding no lease — the state Invariant 9 admits a
claim on — and the gate and the park reason of the ending it left are
cleared:

```
$ mill rerun mill#18 --from implement
rerun mill#18 run 01M23T3ZQA2ZE8MD2V9ZAEKPZA from implement through clean-tree
STEP        ATTEMPT  STATUS      REASON
implement   1        superseded  rerun
commit      1        superseded  rerun
clean-tree  1        superseded  rerun

mill#18 had ended failed and is live again

mill#18 is resumable at implement in generation 1; the sessions and the branch are kept
```

Two refusals. An `abandoned` run is refused: a later generation has taken the
node, so reviving this one would put two live runs on it
([ledger.md](ledger.md) Invariant 1), and `mill restart` is the verb for a
node in that state. A run a worker is still holding is refused naming the
holder, because superseding a span under a running step would have the worker
close a step the ledger has already put behind it — `mill cancel` it first.
And a `--from` naming a step the run has never attempted is refused naming
the ones it has:

```
$ mill rerun mill#18 --from pr
mill: rerun mill#18: failed_precondition: run 01M23T3ZQA2ZE8MD2V9ZAEKPZA: step
"pr": run has no attempt at that step; it has attempted gather-context,
implement, commit, clean-tree
```

Needs the `operator` role.

## workspace show

Where a run's files are on this host and what state they are in. A workspace
is destroyed when its run ends well and retained while the run is waiting or
parked, so that the worker that resumes it — or the human inspecting it —
finds the tree intact ([domain-model.md](domain-model.md) §Workspace).

```
$ mill workspace show mill#18
mill#18 run 01M23T405PB1AG13CAJBN37V0F
  path      /var/lib/mill/workspaces/01M23T405PB1AG13CAJBN37V0F/checkout
  branch    mill/mill-18
  head      7317601c9fcee24e64526c838dbcbac147a3deed
  tree      clean
  socket    /var/lib/mill/workspaces/01M23T405PB1AG13CAJBN37V0F/ws.sock (absent)
  retained  the run is parked for a human
```

`tree` is what `git status` says: `clean`, or `dirty (n changed)`, which is
the same question the `clean-tree` check asks. `socket` is present while a
worker holds the workspace open and absent once it has let go. `retained` is
the run's own status, which is the whole of the rule.

Nothing is opened to answer this: no socket is bound, no branch is checked
out, no signing key is made. This host's workspaces only — a run worked
elsewhere has nothing here to show.

## workspace clean

Remove the checkouts of runs that have ended: `done`, `failed`, or
`abandoned`. A run that is selecting, running, waiting or parked keeps its
workspace whatever its age, because that is what its resume attaches to.

```
$ mill workspace clean --dry-run
RUN                         NODE     STATUS  PATH                                      ACTION
01M23T408J5TXTB7FM2JJ0H3BE  mill#12  done    /var/lib/mill/workspaces/01M23T408J5T…3BE  would remove

$ mill workspace clean --older-than 7d
RUN                         NODE     STATUS  PATH                                      ACTION
01M23T408J5TXTB7FM2JJ0H3BE  mill#12  done    /var/lib/mill/workspaces/01M23T408J5T…3BE  removed
```

The ledger decides, not the directory. A directory whose run the ledger holds
no ended row for is left alone, because deciding it is rubbish needs a record
that says so; `--older-than` narrows further to runs that ended longer ago
than the window. `--dry-run` lists and removes nothing.

The mill's signing key lives above the run directories and survives a clean:
it is the account's, not any run's.

Needs the `operator` role.

## image ls

Which image subvolumes this host holds (`docs/sandbox.md` §Images): the
derived digest, the base it derives from, the packages over the base, when
it was built, and when a machine last booted it. Least recently used first,
which is the order `prune` takes them in.

```
$ mill image ls
DIGEST        BASE          PACKAGES                BUILT                 LAST BOOT
7d3a1f0c9b2e  base          -                       2026-09-04T03:12:09Z  2026-09-10T22:41:07Z
599b7919d9a1  7d3a1f0c9b2e  libpq-devel,vips-devel  2026-09-05T14:02:51Z  2026-09-11T06:35:20Z
```

`base` in the second column is the base's own subvolume, the one a run
boots when its repository declares no packages or its derived build failed.
The subvolumes live under `--machines-dir` (`/var/lib/machines`); the base is
named by `--image-registry` and `--image-repository` (`harbor.tailcbcc0e.ts.net`
and `mill/base`, or the file's `image-registry` and `image-repository`), which
every `image` subcommand takes.
`+nested` after a package list marks the nested variant (docs/sandbox.md
§"Derived images"): the same packages with `agent`'s sudo baked in, a
subvolume of its own.
The listing is the worker's records under `<sandbox-root>/images`, each
checked against the subvolume it names; a record whose subvolume is gone is
not shown. The subvolume directory is root's, so the check goes through
`sudo`; `--sudo none` on a host where it is not.

## image prune

Eviction with images as the only victims (`docs/sandbox.md` §Eviction):
remove the image subvolumes whose last boot — or build, when nothing has
booted them — is older than the expiry (30 days, or `--expiry`, or the
configuration's `image-expiry`); then, while free space on the sandbox
volume is below the high watermark, the least recently booted until it is
above the low one. Each removal is printed with its reason.

```
$ mill image prune --dry-run
DIGEST        PACKAGES     LAST BOOT             ACTION        REASON
599b7919d9a1  libpq-devel  2026-08-01T06:35:20Z  would remove  unused for 41d, past the expiry of 30d

$ mill image prune
DIGEST        PACKAGES     LAST BOOT             ACTION   REASON
599b7919d9a1  libpq-devel  2026-08-01T06:35:20Z  removed  unused for 41d, past the expiry of 30d
```

`--dry-run` lists what expiry would take; the watermark pass reads free
space and cannot be rehearsed. The watermarks are `--high-watermark` and
`--low-watermark`, as `cache prune` takes them. Each removal holds the image's build lock,
so a provision ensuring the same digest either finds the subvolume or
rebuilds it. A running machine's root is an ephemeral snapshot of its own
and is untouched by the source going. `mill cache prune` is the same
eviction over lineages and images both.

## image build-base

Build the base image from `images/base` — `--context` names another
directory holding a Containerfile — by `podman build` on this host, outside
any machine, the one place mill runs a container there; tag it
`<registry>/mill/base:<today>` and `:latest`, and with `--push` publish
both. The push presents the Harbor robot credential, one line of
`robot:secret` from `$CREDENTIALS_DIRECTORY/harbor-robot` or
`--robot-credential-file`, over podman's stdin and never its command line.

```
$ mill image build-base --push
pushed harbor.example/mill/base:2026-09-11
pushed harbor.example/mill/base:latest
digest sha256:7d3a1f0c9b2e…
```

`contrib/systemd/mill-image-build.timer` runs it weekly. The worker
resolves `:latest` to a digest at each provision, so a pushed base takes
effect on the next run; `base-image` in the configuration file — a tag or
a digest — is the rollback.

## cache ls

Which cache lineages this worker holds (`docs/sandbox.md` §Caching): the
scope — `global`, or `repo/<repo-id>` — the name, the generations on disk,
the latest generation's `du -s` estimate at promotion, when a run last
restored it, and a flag: `over-cap` when the latest estimate exceeds the
scope's cap, `reset` when the last promotion emptied a global lineage for
exceeding its cap. Global lineages first, then by repository and name.

```
$ mill cache ls
SCOPE                              NAME      GENERATIONS  LATEST SIZE  LAST RESTORED         FLAGS
global                             go-mod    3,4          1.2G         2026-09-11T06:35:20Z  -
global                             mise      2,3          640M         2026-09-11T06:35:20Z  -
repo/gitlab.com-ivy-mill-1a2b3c4d  go-build  7,8          2.7G         2026-09-11T06:35:20Z  -
```

The listing is the directory tree under `<sandbox-root>/cache` and each
lineage's `manifest.json`, never `btrfs subvolume list`, which needs root.
The caps are `--global-cap` and `--repo-cap`, or `cache-global-cap`,
`cache-global-caps` (per global lineage name) and `cache-repo-cap` in the
configuration file; the global ones are what a promotion is measured
against, the repo one is only the flag — `over-cap` on a repo lineage is
advisory, and nothing acts on it. `evicted` marks a repo lineage eviction
emptied; the flag clears at its next promotion.

## cache prune

Eviction (`docs/sandbox.md` §Eviction), the pass the worker runs at start
and — the watermarks alone — after each promotion, for
`contrib/systemd/mill-cache-prune.timer` to run daily. Two passes, in
order:

1. **Expiry.** Every repo-scoped lineage not restored within the expiry
   (`--expiry`, or `cache-expiry`, 30 days) and every image subvolume not
   booted within it (`--image-expiry`, or `image-expiry`, else the same)
   is deleted whatever the free space.
2. **Watermarks.** When free space on the sandbox volume — `statfs`, the
   truth; never the manifests' size estimates — is below the high
   watermark, repo-scoped lineages and images are deleted least recently
   used first until free space is above the low watermark. After each
   deletion the volume is synced and free space re-read, so eviction
   stops as soon as it can. The watermarks are `--high-watermark` and
   `--low-watermark`, or `cache-high-watermark` and `cache-low-watermark`
   in the configuration file — a share of the volume such as `10%` or a
   size such as `50G`; `10%` and `20%` unless set. Eviction starts below
   the high one and stops above the low one, so the low watermark is the
   larger amount of free space.

Global lineages are bounded by their caps and never evicted. A run in
flight is never affected: its copies are snapshots of their own, and one
whose lineage was evicted promotes into a fresh lineage.

```
$ mill cache prune
KIND     NAME                                                  LAST USED             PASS       REASON
lineage  repo/gitlab.com-ivy-oldrepo-9f8e7d6c/go-build gen-3,4  2026-08-01T06:35:20Z  expiry     unused for 41d, past the expiry of 30d
image    599b7919d9a1                                          2026-08-02T11:02:44Z  expiry     unused for 40d, past the expiry of 30d
lineage  repo/gitlab.com-ivy-mill-1a2b3c4d/go-build gen-7,8    2026-09-01T06:35:20Z  watermark  free space 9.1G below the high watermark 10% (12G of 120G); least recently used
free 26.4G of 120G (high watermark 12G, low 24G)
```

Nothing to evict prints `nothing to evict under <sandbox-root>/cache` and
the free-space line. The image side reads `--machines-dir` and goes through
`sudo`, as `image prune` does; `--sudo none` on a host where the image
directory is the caller's.
An eviction that fails part way prints what it deleted before the error.

## Configuration

A worker's settings are the long tail of a systemd `ExecStart` line, so they
may live in a file: `$MILL_CONFIG`, else `$XDG_CONFIG_HOME/mill/config.yaml`,
else `~/.config/mill/config.yaml`, else — when the account has none and the
package installed one — `/etc/mill/config.yaml`, or whatever `--config`
names. Its keys are
the long flag names, with the exceptions the table spells — `env_mode`,
`notify_command`, `nested_repos`, `dagger_checkout` and
`model_credential_minted` are written the way the deployment that sets them
writes them. A flag always wins: the file is consulted only for a flag the
command line left alone. A file that is not there is not an error; a file
named by `--config` or `$MILL_CONFIG` and not there is.

**A key the table does not list is ignored**, silently: the file is decoded
into the settings the binary knows and nothing else is looked at. So
`nested-repos`, `dagger-checkout` or `model-credential-minted` with hyphens
set nothing and say nothing — the underscore is the whole spelling of those
three — and `mill doctor`'s gateway probe warning that
`model_credential_minted` *is not set in the configuration file* under a
file that names the date is the first sign of a misspelt key.

Which command reads which key is a matter of which flags it declares, since
a key fills the flag of the same name, and of whether it reads the file at
all. The mechanism is one: `bindWorkFlags` declares the worker's flags —
`--config` and `--socket` among them — on `work` and on `serve` alike, and
`applyConfig` fills in every one the command line left alone. `run` declares
no `--config`: it reads the file for the two socket keys and nothing else.
A key with no flag of its own — the machine backend's — is the file's alone,
and is read whenever the file is.

**`socket` and `dagger-socket` reach every command that dials.** The
readers, `doctor`, `run`, `cancel`, `rerun`, `restart`, `workspace`,
`workflow`, `mcp`, `work` and `serve` resolve each socket through one
resolver, in one order: the flag, then the variable (`$MILL_SOCKET`,
`$DG_SOCKET`), then the file's key, then the default
(`$XDG_RUNTIME_DIR/mill/mill.sock` or `/run/mill/mill.sock`, and
`/run/dagger/dg.sock`). A command with no `--config` of its own reads the
file at the default location. The file sits *below* the environment on
purpose: a deployment that sets `MILL_SOCKET` on a unit or a plugin
registration keeps reaching the service it named whatever a file on the host
says, while a host whose file names the paths — a Mac reaching a daemon
through forwarded sockets — needs no variable at all. The table below shows
the two keys under `run` for that reason; the commands it does not have a
column for honour them the same way.

The table is hand-kept, and a spec in `internal/cli` renders the same table
from the commands' declared flags and asserts the two are the same, so a
flag added or removed without this table changing is a failing spec.

| Key | `run` | `work` | `serve --work` |
|---|---|---|---|
| `socket` | yes | yes | yes |
| `dagger-socket` | yes | yes | yes |
| `project` | | yes | yes |
| `label` | | yes | yes |
| `concurrency` | | yes | yes |
| `workflow` | | yes | yes |
| `tier` | | yes | yes |
| `workflow-dir` | | yes | yes |
| `sidecar` | | yes | yes |
| `origin` | | yes | yes |
| `ref` | | yes | yes |
| `forge` | | yes | yes |
| `identity` | | yes | yes |
| `name` | | yes | yes |
| `workspace-dir` | | yes | yes |
| `workspace` | | yes | yes |
| `sandbox-root` | | yes | yes |
| `machines-dir` | | yes | yes |
| `image-registry` | | yes | yes |
| `image-repository` | | yes | yes |
| `base-image` | | yes | yes |
| `image-build-budget` | | yes | yes |
| `image-expiry` | | yes | yes |
| `cache-global-cap` | | yes | yes |
| `cache-global-caps` | | yes | yes |
| `cache-repo-cap` | | yes | yes |
| `cache-expiry` | | yes | yes |
| `cache-high-watermark` | | yes | yes |
| `cache-low-watermark` | | yes | yes |
| `skill-dir` | | yes | yes |
| `sidecar-dir` | | yes | yes |
| `workflow-copy-dir` | | yes | yes |
| `run-state-dir` | | yes | yes |
| `proxy` | | yes | yes |
| `no-proxy` | | yes | yes |
| `nested_repos` | | yes | yes |
| `dagger_checkout` | | yes | yes |
| `scratch-namespace` | | yes | yes |
| `scratch-budget` | | yes | yes |
| `log-dir` | | yes | yes |
| `env_mode` | | yes | yes |
| `notify_command` | | yes | yes |
| `gateway` | | yes | yes |
| `gateway-listen` | | yes | yes |
| `gateway-upstream` | | yes | yes |
| `model-credential-file` | | yes | yes |
| `work` | | | |
| `model_credential_minted` | | | |

`work` is parsed and read by nothing: the worker in a `serve` process is
turned on by `--work` and only by it. `model_credential_minted` is read by
`mill doctor` and `mill mcp`'s doctor tool, not by a worker. Other commands
read the file for their own keys — `doctor`, `cache` and `image` for the
sandbox and cache settings, `mcp` for the credential's age — and are outside
this table.


## Forges

A run delivers to the repository its dagger project names in `vcs_url`, and
the forge is read off that URL's host: `github.com` is GitHub, and any other
host is a GitLab, because a self-hosted GitLab has no name the mill could
recognise. `forge` in the configuration file — `--forge` on `run`, `work`
and `doctor` — names the adapter outright, `github` or `gitlab`, for a
repository whose host says the wrong thing. A value that names neither stops
the worker at startup:

```
mill: --forge: forge: "gitbucket" is not a forge this mill has an adapter for: github, gitlab
```

The credential is the same pair on either forge, never a flag, so it
reaches no process listing. The token is read from the credential store
first and the environment second:

| Source | What it is |
|---|---|
| `$CREDENTIALS_DIRECTORY/forge-token` | The token, as systemd delivers it: `ImportCredential=forge-token` on the unit finds `/etc/credstore/forge-token` (or the encrypted `/etc/credstore.encrypted/forge-token`), and `LoadCredential=forge-token:<path>` names a file outright, beside the gateway's `claude-oauth` (`docs/sandbox.md` §Credentials). The token then enters no environment at all, which is what the store exists for. A file that is delivered and empty, or cannot be read, is an error rather than a fall-through |
| `MILL_FORGE_TOKEN` | The token, when the store holds no `forge-token`: a laptop exports it, and `/etc/mill/env` may still carry it. Every API call and every git command the mill makes against the forge is authenticated with the token, minted per request through the credential helper `internal/forge` installs, so it appears in neither a remote URL nor argv |
| `MILL_FORGE_USER` | The username git is given beside the token. GitLab wants a placeholder and the default `mill` is one; GitHub ignores it |

`mill doctor`'s `forge` probe says which of the two the token came from.

### GitHub

GitHub is one host, so nothing about it is configured but the token. The
mill delivers as the person: a GitHub App needs an organisation
administrator to install it, and the people this serves are engineers who
are not administrators of the organisations they work in. The token is
either of:

- **The OAuth token the `gh` CLI holds.** `gh auth token` prints it after
  `gh auth login`, and it is sufficient: its `repo` scope covers every
  permission below. `MILL_FORGE_TOKEN="$(gh auth token)" mill doctor` is
  the whole of the setup on a laptop.
- **A fine-grained personal access token**, made at
  https://github.com/settings/personal-access-tokens for the repositories
  the mill delivers to, with exactly these permissions:

| Permission | Access | Used for |
|---|---|---|
| Contents | read and write | clone the repository, push the run's branch, delete it after the merge |
| Pull requests | read and write | open the pull request, comment on it, merge it or close it |
| Checks | read | read the check runs on the run's head |
| Commit statuses | read | read the commit statuses on the run's head |
| Metadata | read | read the repository's default branch and merge settings; GitHub grants it with any other |

The table is the list `internal/forge/github` probes against, and a spec
holds the two to the same words. A run that changes a file under
`.github/workflows/` needs **Workflows: read and write** as well; nothing
else does, so it is not in the list.

`mill doctor`'s `forge` probe reads, with the token, everything a delivery
reads — the account, the repository, its default branch, the checks and
statuses on that branch's head, its pull requests — and reports the account
it authenticated as and where the token came from. A token GitHub no longer
honours fails the probe naming the token's source and the remedy; a token
that lacks one of the reads fails naming the permission as the token page
spells it. A write is proven
only by writing, and the push is the first write a run makes, before any
session is spent.

A pull request merges by the method the repository allows, in the order
GitHub's own button offers them — a merge commit, else a squash, else a
rebase — and a merge GitHub declines for the repository's own reasons is
reported in GitHub's words and not retried.

### GitLab

A personal, group or project access token with the `api` scope, or an OAuth
token, on the self-hosted GitLab the project's URL names.

## Identity

Identity on the store's socket is the peer uid, mapped by configuration to a
role ([architecture.md](architecture.md) §"Identity and credentials"). There
is no token scheme and nothing in a request body says who sent it.

| Role | May |
|---|---|
| the service's own uid | Everything, including writing the record of a run |
| `operator` | Read and operate: `cancel`, `rerun`, `restart`, `workflow push`, `workspace clean` |
| `reader` | Read |

A `reader` calling `cancel` or `rerun` is refused naming the role; a uid the
map does not name is refused as unauthenticated. `mill mcp` inherits whatever
the account it runs as holds: what a session may do is decided by the unix
account, not by anything the session says.

`workspace clean` removes directories and asks the store nothing, so there is
no write for the role map to refuse. `mill mcp` asks `WhoAmI` — a read, which
every role holds — and applies the same answer: a `reader` calling
`workspace_clean` is refused naming the role, exactly as it is refused
`rerun`. `workspace show` and `workflow show` are reads and are not.

## mcp

The same tools as a stdio MCP server, one JSON-RPC message per line, for a
Claude Code session or a thicket agent. Registered the way dagger's is:

```json
{
  "mcpServers": {
    "mill": { "command": "mill", "args": ["mcp"] }
  }
}
```

The tool names are the CLI's verbs, with the two-word ones spelled with an
underscore: `workflow_list`, `workflow_show`, `workspace_show`,
`workspace_clean`. Arguments are the flags:
`{"run": "mill#18", "view": "full"}`. A misspelt argument is reported rather
than ignored.

`doctor` is among them, read from the deployment's configuration file for
the settings that have no argument (`model_credential_minted`, the
watermarks), so a session asked why a host is sick reads the same ten lines.
`cache` and `image` are not: they answer about subvolumes on this host's
volume and go through `sudo` for the image directory, which is nothing a
session should drive; an operator runs them from a shell or lets the timers
(`contrib/systemd/mill-cache-prune.timer`, `contrib/systemd/mill-image-build.timer`)
run them. `run`, `restart`, `workflow push` and `workflow pull` are absent
for the reasons `mill mcp --help` gives.

`initialize` returns instructions telling a session what mill is, how a run is
named, what the views mean, and that a gate is a person's to answer. Reading
them is enough to use the rest.

The socket resolves as it does for every other face: `--socket`, then
`$MILL_SOCKET`, then the configuration file's `socket`, then
`$XDG_RUNTIME_DIR/mill/mill.sock`, then `/run/mill/mill.sock`
(§Configuration). The environment variable is what a plugin has, since a
registration is a command line fixed at install time and not one an operator
edits per host; the file is what a host has when nothing sets a variable.

`mill mcp --describe` prints the tool list as JSON — name, description and
input schema, exactly what `tools/list` answers — and connects to nothing. It
is how the plugin's reference document is checked against the server it
documents.

```
$ mill mcp --describe | jq -r '.tools[].name'
status
inspect
explain
…
```

## When a tool cannot start

Two of mill's paths bind a unix socket, and a unix socket path has a hard
limit the kernel enforces: `sun_path` holds 108 bytes on Linux, including its
terminator, so 107 is the longest path that can be bound. Over it, `bind`
answers `invalid argument`, which names neither the path nor the length nor
the limit.

Both are checked before anything else happens, and the refusal names all
three.

| Path | Checked by | Before |
|---|---|---|
| `mill serve --socket` | `mill serve` | The migrations and the bind |
| `<workspace-dir>/<run>/ws.sock` | `mill run`, `mill work` | The claim, the lease and the clone |

```
$ mill serve --socket /home/mill/a/very/long/path/.../mill.sock
mill serve: --socket: socket: /home/mill/…/mill.sock is 121 bytes long, and a
unix socket path can be at most 107 (the kernel's sun_path holds 108 bytes
including its terminator): use a shorter path
```

The workspace one is the expensive one to find late: its path is derived from
`--workspace-dir` and the run's id, so without the check it fails on the bind
after the node has been claimed and its repository cloned.

## The plugin

`plugin/mill` is how a session picks these tools up the way it picks up
dagger's ([prd.md](prd.md) R-56). It carries three things:

| Path | What it is |
|---|---|
| `plugin/mill/.mcp.json` | the registration: a `mill` server, run as `mill mcp` over stdio |
| `plugin/mill/skills/mill/SKILL.md` | when to reach for which tool, the role rules, and the rule that a session wanting work done files a dagger node |
| `plugin/mill/skills/mill/references/tools.md` | every tool with its arguments and an example response |

The reference is checked against the binary rather than maintained beside it:
a spec compares the `## <tool>` sections with `mill mcp --describe` and fails
when they disagree about which tools exist.

### Into a Claude Code session

```sh
claude plugin marketplace add ./plugin       # from a checkout of this repo
claude plugin install mill@mill
```

Restart the session, or start a new one, and `/mcp` lists the `mill` server.
While working on the plugin itself, `claude --plugin-dir ./plugin/mill` reads
the tree directly and skips the install cache, which is keyed by the version
in the two manifests. `plugin/mill/README.md` has the rest.

The registration passes no `--socket`, so a session reaching a service
anywhere but the default path is started with `MILL_SOCKET` in its
environment. It is also what a session's role follows from: the server runs as
whatever account started the session, and the store maps that uid.

### Into thicket's agent provisioning

A thicket agent is an account on a host, declared in thicket's roster —
[`agents.yaml`](https://github.com/ivy/thicket) at the repository root, whose
schema is `packages/roster/src/schema.ts`. That file is what an operator
edits, and it fixes the two things that decide whether mill's tools are
reachable: `user`, the unix account whose uid the role map must name, and
`workspaces`, the directories a conversation binds to.

The roster has no field for a plugin. Thicket's executor passes the SDK its
own toolbelt server and leaves `settingSources` unset, so a session loads the
filesystem settings of the account it runs as: installing the plugin into that
account — `claude plugin install mill@mill` as the roster's `user`, or a
`.mcp.json` in the bound workspace — is what puts the tools in front of the
agent today. Carrying a plugin list in the roster is thicket's to add, and
this repository does not edit thicket.

Two things to get right on the host, both administrative rather than
configurable from here:

- The agent's uid must be in mill's role map, or every call is refused as
  unauthenticated. `operator` if it should be able to `cancel`, `rerun` or
  `workspace clean`; `reader` otherwise.
- `MILL_SOCKET` must be in the agent's environment when the service is not on
  the default path. A thicket agent runs as its own account, so
  `$XDG_RUNTIME_DIR` is its own, not the mill's.

An agent that reaches the tools should still not be asked to run work: `run`
is not exposed, and a session that wants something built files a dagger node
([prd.md](prd.md) R-55).
