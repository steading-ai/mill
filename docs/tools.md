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

Every tool takes `--socket`; the readers take `--view`.

| Tool | What it answers |
|---|---|
| `status` | What is live, and which step each run is on |
| `inspect <run>` | Every attempt at every step, and the evidence that closed it |
| `explain <run>` | Where one run is, and what it is waiting on |
| `logs <run>` | What the session on a step said, redacted |
| `history <run>` | What changed about a run, and when |
| `cost` | What a window of runs billed |
| `workflow list\|show` | What a run on this project would follow |
| `doctor` | Whether this host could work at all |
| `cancel <run>` | Stop a run's current step cleanly, leaving it resumable |

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

```
$ mill status
NODE       STATUS   STEP       ATTEMPT  ELAPSED  TOKENS       GATE
millcli#1  running  implement  1        0s       184213/9820  -
```

`TOKENS` is input/output, summed over models. Counts, never currency: the
ledger records tokens and a rate table is applied by whoever wants money.

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

`VALUE` is the claim the mill verified — a SHA, a URL, an artifact digest, a
gate node's id — and not a description of it. `select` is the ledger's record
of which workflow was chosen and why; no workflow declares it.

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
dagger, as a text artifact named `<step>-<attempt>.md` carrying the decision,
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
superseded by later attempts.

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
RUN        INPUT   OUTPUT  CACHE READ  CACHE WRITE
millcli#1  184213  9820    1204331     0

WORKFLOW                INPUT   OUTPUT  CACHE READ  CACHE WRITE
quick-fix@fb2764fd70fd  184213  9820    1204331     0

STEP       INPUT   OUTPUT  CACHE READ  CACHE WRITE
implement  184213  9820    1204331     0

MODEL              INPUT   OUTPUT  CACHE READ  CACHE WRITE
claude-sonnet-4-5  184213  9820    1204331     0
```

The MODEL table is summed from the rows the STEP table renders — every step's
bucket carries its own totals per model — so the two tables agree by
construction and one of them cannot come back empty under the other.

## workflow show

One stored version as a run would walk it: what closes each step, what its
workspace handle grants, how many times it may fail before the breaker parks
the run, and what a failure does. This is the definition; `inspect` shows
what a run made of it.

```
$ mill workflow show quick-fix
quick-fix org fb2764fd70fda30764b59d00d34532c9195b668cb4c6ad446b23b2a44445ee23

STEP            KIND   EVIDENCE  CAPABILITIES     RETRIES  ON FAILURE        SESSION
gather-context  skill  artifact  read             2        park              implement
implement       skill  artifact  read,write,exec  3        park              implement
commit          skill  commit    write            2        park              implement
clean-tree      check  exit      exec             3        revise:implement  -
pr              skill  url       forge            2        park              implement
merge           gate   status    -                1        park              -
```

The same for the tier that plans first. A step's harness options follow the
steps, printed as the JSON they are stored as: mill interprets no key of one
and has nothing else to say about it, and what an operator wants here is what
the step will hand the harness.

```
$ mill workflow show feature
feature org c6bd94330f49e6edba3c50c3cfa6d9b6b162a0caae1d9a1c278b0da574b69a8c

STEP            KIND   EVIDENCE  CAPABILITIES     RETRIES  ON FAILURE        SESSION
gather-context  skill  artifact  read             2        park              implement
think           skill  artifact  read             2        park              implement
plan            skill  artifact  read,write       2        park              implement
review-plan     skill  verdict   read             2        revise:plan       review
plan-approval   gate   gate      -                1        park              -
implement       skill  artifact  read,write,exec  2        park              implement
simplify        skill  artifact  read,write,exec  2        park              implement
commit          skill  commit    write            2        park              implement
clean-tree      check  exit      exec             3        revise:implement  -
pr              skill  url       forge            2        park              implement
review          skill  verdict   read             2        revise:implement  review
merge           gate   status    -                1        park              -

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

## doctor

Seven probes, one line of reason each. Every probe is reported; the command
exits non-zero when any of them failed.

```
$ mill doctor
pass  socket   /run/user/1000/mill/mill.sock answers; mill 0.0.0-dev
pass  schema   migration 14, current
pass  dagger   /run/dagger/dg.sock answers; this account is a principal on mill, 1 node(s) ready for an agent
fail  sidecar  sidecar/dist/main.js did not answer: claude: the sidecar answered nothing before it exited
fail  signing  no signing key under ~/.local/state/mill/workspaces/signing
fail  forge    no repository to probe: pass --vcs-url or set MILL_VCS_URL
pass  notify   contrib/notify/thicket-send.sh accepts a probe notice
```

| Probe | What it checks |
|---|---|
| `socket` | `mill serve` answers on its unix socket, and which version |
| `schema` | The database is migrated to the schema this binary carries |
| `dagger` | Dagger answers, and this account resolves to a principal that may read the project |
| `sidecar` | The harness sidecar starts, parses one frame, and answers |
| `signing` | The commit signing key exists and some project's `allowed_signers` names it |
| `forge` | The forge accepts the credential |
| `notify` | The configured notify command runs and accepts a probe notice |

The sidecar probe sends a `cancel` request on stdin. Cancellation reaches a
running sidecar as SIGTERM, so a cancel on stdin is a frame the sidecar
understands and refuses by contract: it never starts a session, never reaches
the Claude API, and bills nothing. Its refusal is the answer the probe wants.

The notify probe runs the deployment's `--notify-command` — the file's
`notify_command`, when no flag names one — with `{"kind":"probe"}` on its
standard input and nothing else: no gate, no node, no run. A notifier that
routes on `kind` can tell a health check from a person's decision and stay
quiet, and the shipped example does. A host with no notify command configured
passes and says so: filing a gate that reaches nobody is a supported choice,
because the answer is written in dagger either way (docs/architecture.md
§"Gate steps").

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

## Identity

Identity on the store's socket is the peer uid, mapped by configuration to a
role ([architecture.md](architecture.md) §"Identity and credentials"). There
is no token scheme and nothing in a request body says who sent it.

| Role | May |
|---|---|
| the service's own uid | Everything, including writing the record of a run |
| `operator` | Read and operate: `cancel`, `restart`, `workflow push` |
| `reader` | Read |

A `reader` calling `cancel` is refused naming the role; a uid the map does not
name is refused as unauthenticated. `mill mcp` inherits whatever the account
it runs as holds: what a session may do is decided by the unix account, not by
anything the session says.

## mill mcp

The same tools as a stdio MCP server, one JSON-RPC message per line, for a
Claude Code session or a thicket agent. Registered the way dagger's is:

```json
{
  "mcpServers": {
    "mill": { "command": "mill", "args": ["mcp"] }
  }
}
```

The tool names are the CLI's verbs, with `workflow list` and `workflow show`
spelled `workflow_list` and `workflow_show`. Arguments are the flags:
`{"run": "mill#18", "view": "full"}`. A misspelt argument is reported rather
than ignored.

`initialize` returns instructions telling a session what mill is, how a run is
named, what the views mean, and that a gate is a person's to answer. Reading
them is enough to use the rest.

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

## Not built yet

`rerun <run> --from <step>`, `workspace show|clean <run>`, and the
`plugin/mill` Claude Code plugin are O1 ([roadmap.md](roadmap.md)).
