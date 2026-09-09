# Domain Model

The model behind the mill's store and API. This document is the input to the proto contract; the proto is authoritative once it exists. It composes with `dagger/docs/domain-model.md` and never restates it: where dagger already names a thing — node, lease, gate, artifact, usage, principal, context id — the mill uses dagger's word and dagger's rules.

The mill holds exactly one kind of state dagger does not: **what happened inside a run** — which workflow, which step, what evidence, which session. Everything a human or another agent would want to *see* goes to dagger as a result or artifact. Everything only resume needs stays here.

## Run

One node, worked through one workflow, by this mill. The unit a worker takes and the unit a workspace belongs to.

| Field | Notes |
|---|---|
| `id` | ULID, internal |
| `node_id`, `node_ref` | The dagger node. `node_ref` is the display key (`dagger#18`) |
| `generation` | 1 for the first run on a node; incremented by `restart`. `(node_id, generation)` is unique; at most one run per node is live. See [Restart](#restart) |
| `context_id` | Dagger's context id for the node, copied at creation. Every session in the run derives from it and the generation |
| `workflow_id` | The immutable workflow version this run follows. Fixed at selection; never changes mid-run |
| `status` | `selecting`, `running`, `waiting`, `parked`, `done`, `failed`, `abandoned`. See [Run states](#run-states) |
| `lease_id` | The current dagger lease, or null while `waiting` or `parked`. The fencing token on every dagger write |
| `worker_ref` | Where the current attempt runs. Free string; a hostname and account today |
| `parked_reason` | Set with `parked`: `retries`, `budget`, `denied`, `timeout`, `refused` |
| `gate_node_id` | Set with `waiting`: the gate this run filed and is blocked on |
| `cancel_requested` | Set by `cancel`; the worker reads it at every heartbeat and stops the step cleanly, releasing with `preempted`. Cleared on resume |
| `created_at`, `updated_at` | |

Derived, never stored: `current_step` (the first step whose latest step run is not `closed` — see [Evidence](#evidence) for what supersedes a closed one), `cost` (sum over step runs), `leases` (count of dagger leases the node has held under this run; distinct from a step's `attempt`).

**One live run per node.** A second lease on the same node resumes the same run at its current step — dagger's rule that "every attempt shares the node's context id, so a retry resumes the prior session" carries over. Starting over is a deliberate, human-initiated act with its own generation; see [Restart](#restart).

### Run states

```
selecting ──▶ running ──▶ done
                │  ▲
                │  └──── waiting   (gate filed; lease released with reason `gate`)
                │  ▲
                │  └──── parked    (circuit breaker; lease released with reason `budget` or `crash`)
                │
                ├──────▶ failed    (terminal; gate denied, or the node was dropped)
                └──────▶ abandoned (terminal; a human restarted the node — a new generation exists)
```

`abandoned` is reachable from every state a run can still be live in — `selecting`, `running`, `waiting`, `parked` — because [Restart](#restart) applies to the live run whatever it is doing, and a run waiting on a human is the ordinary thing to restart. `parked` returns to `running` the same way `waiting` does: the gate is answered, the node becomes ready, and the next claim resumes the run at the step it stopped on.

- `selecting` — claimed, no workflow yet. The assessment step runs here.
- `running` — a lease is held and a step is executing or about to.
- `waiting` — a gate node exists and blocks this node. The run holds no lease and no worker. When the gate completes, the node becomes ready again, `ClaimNext` returns it, and the worker that gets it resumes this run: it finds `gate_node_id` set and the current step a gate step, reads the gate's result, and closes that step with `gate` evidence — it does not file the gate again.
- `parked` — the circuit breaker fired. A comment with the last findings is on the node, a gate asks a human what to do, and the lease is released. Structurally identical to `waiting`; distinguished so `status` can say why.
- `done` — the terminal step closed with evidence and `Complete` was accepted by dagger.
- `failed` — a gate was denied, or the node was dropped underneath the run. Terminal. The result on the node says why.
- `abandoned` — a human restarted the node. Terminal. The run's record stays; the new generation is a different run.

### Restart

When a run has gone sideways and the answer is "start over," the node should not have to be re-filed and re-linked. `mill restart <node>` — from the CLI, or as the answer to a park gate:

1. If a lease is held, release it with reason `preempted` and detail `restart`. Cancel any running step.
2. Mark the run `abandoned`. Its step runs, evidence, and usage stay as the record of what went wrong.
3. Destroy the workspace. Delete the run's branch on the forge if it was pushed; close the PR if one was opened, with a comment naming the restart.
4. Create a run with `generation + 1`. Session ids derive from `(context_id, generation, name)`, so every session in the new run is one the harness has never seen.

Never automatic. The circuit breaker parks; a human restarts. A step that fails repeatedly must cost a human decision, not a generation.

## Workflow

A named, immutable, versioned definition of how a class of node gets worked. Data, not code. Lives in the store; seeded from files in the repo; pushed and pulled by the CLI.

| Field | Notes |
|---|---|
| `id` | ULID |
| `name` | `quick-fix`, `feature`, `docs`. Unique within a scope |
| `version` | Content hash of the definition. Two definitions with the same hash are the same version |
| `scope` | `org` or `project`, plus the project when `project`. Project overrides org; a shadowing definition logs a warning at load, mirroring the agent-skills precedence rule |
| `steps` | Ordered list. See [Step](#step) |
| `source` | Where this version came from: repo, commit, path — or `cli-push` with the pushing principal |
| `created_at` | |

A workflow is never edited. A change is a new version; runs in flight keep the version they started with. This is what lets a ledger entry say exactly which definition produced it.

### feature

The tier that plans before it writes: the `/work-on` skill's feature tier, with each of its judgement calls made a step that closes on evidence and each of its pauses a gate. `workflows/feature/` is the definition; this is what it says.

| Step | Kind | Evidence | Session | Capabilities | On failure |
|---|---|---|---|---|---|
| `gather-context` | skill | artifact | implement | read | park |
| `think` | skill | artifact | implement | read | park |
| `plan` | skill | artifact (`docs/plans/<date>-<node-slug>.md`) | implement | read, write | park |
| `review-plan` | skill | verdict | review | read | revise `plan` |
| `plan-approval` | gate | gate | — | — | park |
| `implement` | skill | artifact | implement | read, write, exec | park |
| `simplify` | skill | artifact | implement | read, write, exec | park |
| `commit` | skill | commit | implement | write | park |
| `clean-tree` | check | exit | — | exec | revise `implement` |
| `pr` | skill | url | implement | forge | park |
| `review` | skill | verdict | review | read | revise `implement` |
| `merge` | gate | status | — | — | park |

Two sessions, as [Session](#session) describes: the implementer's carries everything from `gather-context` to `pr`, and the reviewer's carries both reviews. A reviewer sharing the planner's context would be reviewing its own work, and the reviewer holds no credential that can write — no `write` capability, and a tool policy that keeps the write tools out of the session at all.

`plan-approval` is auto-approved today and is a gate anyway, so that the shape of the run is the shape it keeps: giving the plan to a person is dropping the policy and nothing else. `merge` auto-approves on an approving verdict at this tier or below.

Four of the nine skills are `quick-fix`'s, referenced as `quick-fix/<name>`. A skill is the portable part and carries nothing workflow-specific, so two workflows share one record rather than two copies of it to drift; what differs between them — the model, the effort, the tool policy — is on the step.

### Selection

How a run gets its workflow, in order:

1. **Project default** — a project in the store may name a workflow *and the tier it stands for*. Used when set. The tier is what gate policies read when no assessment ran.
2. **Assessment** — otherwise the `/build-workflow` skill runs as the first step of the run, non-interactively, and emits `{workflow, tier, signals, reasoning}` as structured output. The named workflow must exist in scope; the output is recorded in the ledger as the selection's evidence.

Assessment is the norm. A project default is a person pinning a project to one workflow; without one, the run sizes the node for itself before it walks anything.

Either way, every run has a `tier` by the time its first gate is evaluated, and one `select` step run holds the choice: structured evidence carrying `workflow`, `version`, `tier` and `basis` — `project default` or `assessment` — and, when the assessment answered, its own `signals` and `reasoning` alongside them.

#### The assessment's schema

```json
{
  "type": "object",
  "required": ["workflow", "tier", "signals", "reasoning"],
  "properties": {
    "workflow": {"type": "string", "minLength": 1},
    "tier": {"type": "string", "enum": ["quick-fix", "feature"]},
    "signals": {"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}},
    "reasoning": {"type": "string", "minLength": 1}
  },
  "additionalProperties": false
}
```

`signals` are the observable facts the decision rests on — the files the node's ask touches, whether tests exist for them, whether the ask names a design change — and there is at least one: a decision resting on nothing is not one a person can check. The schema lives beside the skill, at `workflows/build-workflow/skills/build-workflow/assessment.schema.json`, and the step names it; output that does not validate fails the step as malformed, naming the field.

`tier` is the two tiers the workflows in this repository stand for. A change touching one file or a few lines with no design question is `quick-fix`; anything needing a plan, touching several packages, or changing an interface is `feature`. An uncertain assessment says `feature`: the more expensive path is the safe one.

The assessment runs in a read-only session of its own, and it does not gather context for the run — the selected workflow's first step does that. The names it may choose between are the workflows in scope, less `build-workflow` itself, handed to the session by the mill; a name that is not one of them fails the `select` step with the name and the scope in the message, and the run walks nothing.

### Project default

| Field | Notes |
|---|---|
| `project` | The dagger project |
| `workflow` | Name, resolved to the newest version in scope at run creation |
| `tier` | `quick-fix`, `small`, `feature`, `large`. What gate `auto` predicates compare against when the assessment did not run |

Dagger nodes carry no workflow label. Dagger never learns the word.

## Step

One unit of a workflow. Three kinds.

| Field | Notes |
|---|---|
| `name` | Unique within the workflow. The ledger's key for the step |
| `kind` | `skill`, `check`, or `gate` |
| `session` | Skill steps only. Names the session this step runs in; steps sharing a name resume the same session. See [Session](#session) |
| `skill` | Skill steps: the skill's name and content hash, resolved from the store at workflow load. A definition names it as `<name>`, or as `<workflow>/<name>` for a skill a sibling workflow's directory carries — one record, whichever workflow reached for it |
| `check` | Check steps: command path relative to the skill or workflow, exit-code semantics, expected stdout shape. See [Check](#check) |
| `gate` | Gate steps: the ask, and the auto-approve policy. See [Gate](#gate) |
| `evidence` | What closes this step. See [Evidence](#evidence) |
| `artifact` | Skill steps closing on `artifact`: the file in the checkout whose content is the artifact, as a template over the run — `docs/plans/{{.Date}}-{{.NodeSlug}}.md`. Absent, the artifact is the session's last message |
| `capabilities` | The capability set the step's workspace handle grants. See [Capability set](#capability-set) |
| `harness` | Per-harness **options documents**, keyed by harness name. The adapter reads its own entry and mill reads none of them |
| `on_failure` | What happens once `retries` are spent: `revise <step>` returns to a named earlier step in its session with the failure as the next prompt; `park` trips the breaker. Default `park`. See [Revise](#revise) |
| `retries` | How many times *this* step may fail and be tried again before `on_failure` is consulted. Counted on this step's own attempts. Default 3 |
| `timeout` | Wall clock for the step, enforced by the worker. Independent of the dagger lease, which the worker heartbeats for the step's whole duration; a step may run longer than a lease TTL |

Harness settings live on the **step**, not on the skill record. A skill is shared across workflows, so it cannot hold a setting that differs between them: the same reviewer is worth a cheaper model in a `docs` workflow than in `feature`. `mill.v1.HarnessSettings` and `harness.v1.Settings` carry the decision in the proto.

**Harness settings are the harness's.** A step's entry for a harness is an opaque document — the yaml block as written, stored as JSON, carried to the adapter unaltered — and mill interprets no key of it. Model, effort and tool policy are keys the `claude` harness happens to have, not fields of the step. The harness owns the schema and validates the document: an unknown key, or a value of the wrong type, is refused before the session starts, with `policy` as the exit reason and the key in the message, which the ledger records as a failed attempt. Adding a knob is a change to a harness and to the workflows that want it; mill is not rebuilt for one, and a knob mill has never heard of still reaches the harness that has. What mill does check is the *name*: a workflow naming a harness the worker has no adapter for is refused at run start, before any step runs.

```yaml
harness:
  claude:
    model: opus
    effort: high
    tools:
      deny: [Write, Edit, Bash, NotebookEdit]
```

`tools.deny` is the harness's half of a read-only step, and the [capability set](#capability-set) is mill's: the workspace refuses `fs.write` and `git.commit` under a step token whose capabilities lack `write`, whatever the session was offered. A reviewer step declares both — no `write` capability, and a `tools.deny` that keeps the write tools out of the session — so the model is not handed a tool it may not use and the boundary holds on the side that enforces it. Until the workspace socket is the session's own tool server, the SDK's file tools do not pass through it, which is why MVP needs both halves and why a `clean-tree` check follows a read-only step.

A **skill step** runs a skill as a headless session: the skill body is the prompt, the node's body and inputs are context, prior steps' artifacts are attached. It produces artifacts and, where the definition says so, structured output.

An artifact step's evidence is the session's own report, which is the right default: nothing else produced it. A step that names an `artifact` path instead closes on a *file* — the mill renders the path from the run, tells the session where to write, reads the file back through the workspace when the session ends, and attaches its content. `feature`'s plan step is what the field exists for: the plan is written into `docs/plans/`, so it is committed with the change and stays in the repository, and the same bytes are on the node so a person answering the plan gate reads the plan there. A file that is missing or empty does not close the step, and what the session said about it is not consulted.

A **check step** runs a command through the workspace and reads its exit code and stdout. It produces evidence directly.

A **gate step** files a dagger gate and releases the lease. It produces no evidence of its own; it closes when the gate node completes, and the gate's result is the evidence.

Steps run in order. There is no branching except `on_failure`. A workflow that needs a decision makes it in a skill step and records it as structured output the next step reads.

**Retries first, then `on_failure`.** A failed step is tried again in place while it has attempts left: a new attempt at the same step, in the same session, with what the last attempt failed on — the session's final message, or the check's stderr — as the next prompt. Nothing else moves; no other step is superseded. `retries: 3` is three retries after the first try, so the fourth failure is the first one with nowhere left to go, and only then is `on_failure` read: `revise <target>` supersedes the span back to the target, `park` or no `on_failure` at all trips the [circuit breaker](architecture.md#failure-and-stop). A step's `retries` and its `on_failure` are independent — `retries: 2` with no `on_failure` is three attempts and then a park, which is what a step that only ever fails on its own account wants. The `revise` is consulted once per set of retries: a step that fails again with its retries already spent parks rather than sending the work back a second time, so a target that cannot satisfy it does not loop forever.

One failure is not the step's own and is not retried in place: a reviewer's `revise` [verdict](#verdict). The step did its job — it judged — and the findings are addressed to the implementer, so asking the same reviewer again would be asking it to change its mind. A verdict goes to `on_failure` at once, and the step's `retries` bound how many review rounds a run gets.

### Revise

`on_failure: revise <target>` on step *F* reopens every step from *target* through *F* inclusive. Their latest step runs are marked `superseded`; their evidence rows remain, immutable, but no longer close anything. The run's `current_step` is therefore *target* again, and every step between *target* and *F* runs again on the revised work — a re-implemented change is re-simplified, re-committed, re-pushed, and re-reviewed. The alternative, in which `commit` and `pr` keep pass-one evidence while `implement` produced pass two, would make the ledger's record of what shipped false.

The counter is the failing step's: `review` fails with `revise implement`, and `review.retries` is what bounds it — the review rounds when the failure is a verdict, and `review`'s own attempts before the work goes back to `implement` at all when it is not. A `revise` whose target is not earlier in the workflow is a definition error caught at load.

### Gates and pushes

A gate step may leave a run `waiting` for as long as a human takes, and the worker that resumes it may be on another host. A workspace can only be recreated elsewhere from a branch the forge has. The loader therefore enforces: **any gate step that follows a step with the `write` capability must be preceded by a step whose evidence is `url` or a push** — the branch exists on the forge before the run can wait. One write does not arm the rule: a step whose `artifact` evidence is read from a file it wrote. That file's content is attached to the node, so a worker recreating the workspace elsewhere has it from the run's own evidence and nothing is stranded. This is what lets `feature` put its plan in the checkout and still gate on it before anything is pushed.

## Session

A resumable agent session inside a run. Named by the step definition, identified deterministically, owned by the run.

| Field | Notes |
|---|---|
| `run_id`, `name` | Composite key. `name` is the step's `session` value: `implement`, `review` |
| `id` | UUIDv5 of (`context_id`, `generation`, `name`). Deterministic, so a resumed worker computes the same id and the harness resumes the same transcript; a restart's new generation yields ids the harness has never seen |
| `harness` | `claude` today. Which adapter owns it |
| `adapter_ref` | Whatever the adapter needs to find the transcript: a config dir and cwd for Claude |
| `started_at`, `last_turn_at` | |

Sessions exist so that gather-context → think → plan → implement → simplify → commit → PR share one context window and its cache, while review runs in another. Feedback from a `revise` returns to the *implementing* session, so the reviewer's findings land where the work was done and the reviewer, on the next pass, still has its own prior findings. Two sessions per code workflow is the expected shape; the model permits more.

A session's transcript lives with the harness on the worker's host, never in the store. The store knows how to name it; only the adapter can resume it. This is dagger's "transcript resume vs. graph resume" distinction, one level down.

## Evidence

An externally verified fact that closes a step. Immutable once recorded.

| Field | Notes |
|---|---|
| `step_run_id` | The step run it closes |
| `kind` | `commit`, `exit`, `verdict`, `structured`, `url`, `status`, `artifact`, `gate` |
| `value` | The fact: a SHA, an exit code, a parsed verdict, a validated JSON document, a URL, a status string, an artifact digest, a gate node id |
| `verified_by` | Which check or which mill routine established it |
| `verified_at` | |

What each kind proves:

| Kind | Closes | Verified how |
|---|---|---|
| `commit` | commit steps | A commit made in this step: `git cat-file -e <sha>` in the workspace succeeds, the SHA is on the run's branch, it descends from the attempt's `base_commit` without being it, and the mill is its author and its committer |
| `exit` | check steps, build, test, lint | The process the workspace ran exited with the code the definition names |
| `verdict` | review steps | The session's final output parsed as a verdict and its `decision` is `approve` |
| `structured` | assessment, plan | The session's final output validated against the step's schema |
| `url` | PR steps | The forge returned the URL when asked for the PR by branch |
| `status` | CI steps | The forge reports the named check as `success` for the commit |
| `artifact` | any | An artifact with this digest was accepted by dagger |
| `gate` | gate steps | The gate node's status is `done` and its result is attached |

A step definition names one evidence kind. A step run with evidence is `closed`. A `revise` marks closed step runs `superseded`: the evidence row stays, immutable, but the step is open again and runs anew. A run's `current_step` is the first step whose latest step run is not `closed`. Nothing the session says, and nothing in a transcript, is evidence.

## Verdict

A review step's structured output. Parsed, never read.

```json
{
  "decision": "approve" | "revise",
  "findings": [
    { "criterion": "AC-2", "severity": "blocking" | "note", "summary": "...", "location": "path:line" }
  ]
}
```

There is no `needs-human` decision. A reviewer approves or revises; a human is reached only through the circuit breaker.

Output that does not match the schema is not a verdict, and neither is a `revise` with zero blocking findings: a reviewer that asks for changes and names none has asked for nothing. Both fail the step as **malformed**, naming the field that is wrong. A malformed verdict is the reviewing session's own failure — nothing was judged, so no other step's work is implicated — and the step is asked again rather than sending an implementer back on it; the run parks when the step's retries are spent.

`findings[].criterion` references the node's acceptance criteria by label. A reviewer that cannot tie a finding to a criterion is drifting from its contract, and the finding is recorded as `note`.

A verdict step's `output_schema` may be left out, and the mill then hands the reviewer this schema; a workflow that writes it down writes down this one, which the loader checks. A reviewer is asked for exactly the shape the verifier accepts.

The verdict is stored twice: as evidence here, and as a text artifact on the node's result in dagger named `<step>-<attempt>.md`, carrying the decision, the findings and the reviewing session's id, so a human reads it next to the work. Both a `revise` and an `approve` are attached: a `revise` closes no step, so the artifact and the next attempt's prompt are the whole of its record.

What the reviewer is judging is put in front of it by the mill: the diff since the base commit of the first attempt at the step a `revise` would return to. A reviewer has no shell (R-38), so a reviewer left to fetch its own diff would review the checkout without knowing which part of it this run wrote.

## Check

A deterministic command a step runs through the workspace.

| Field | Notes |
|---|---|
| `command` | Path relative to the skill directory or the workflow, plus arguments. Executed by `Workspace.Exec`, never directly |
| `exit` | Which codes mean pass. Default `0` |
| `stdout` | `none`, `text`, or `json` with a schema. JSON on stdout, diagnostics on stderr, per the agent-skills script convention |
| `feedback` | Whether a failure's stderr is returned to the session as the next prompt (`revise`) or parks the run |

The canonical example: a `clean-tree` check after `commit` whose stderr on failure is *"Workspace is dirty: stage, delete, or ignore uncommitted files"* and whose `on_failure` is `revise implement`.

Checks are what make the workflow enforced. A skill can *recommend* a script; only a check step makes the mill run it and act on the result.

## Gate

A human decision, expressed the only way dagger allows: a node with `assignee_kind: user` that blocks this run's node.

| Field | Notes |
|---|---|
| `ask` | The gate node's title and body, templated from the run: which node, which step, what is being asked, what the artifacts are |
| `auto` | A policy that closes the gate without a human: a predicate over prior evidence and the run's `tier`, e.g. `verdict.decision == approve && tier <= feature`. `tier` comes from the assessment or the project default, so it always exists. Recorded in the ledger as an auto-approval when it fires |
| `deny_result` | What the run records on the node if the human denies |

Filing a gate is two dagger calls — `ApplyPatch` creates the gate node and the edge that blocks this one; `Release` ends the lease with reason `gate` — with the store's record of the gate id written *between* them, so a crash at any point leaves either a lease that expires harmlessly or a gate the store already knows about, never a gate the store will file twice. The mill never claims the gate, never completes it, and never disposes of it — dagger's policy forbids all three for the filer, and the mill does not want the exception. It waits for the node to become ready again.

A gate the `auto` policy closes files nothing and reaches dagger not at all, so there is no gate node to read: the fact that closes the step is the policy that fired, recorded as `gate` evidence reading `auto:<predicate>` and as `auto_approved` on the attempt. A gate declaring `status` evidence closes on the forge instead, because approving it is what performs the merge.

On resume, a run whose current step is a gate step and whose `gate_node_id` is set reads the gate node: `done` closes the step with `gate` evidence carrying the gate's result; a denial fails the run with `deny_result`. A gate is filed once.

Plan approval, merge approval where a project has not opted into auto-merge, deploy approval, and the circuit breaker are the gates in the first workflows. Merge is auto-approved by default: repo protections are the repo's job.

## Workspace

What a step receives instead of a path and a keychain.

| Field | Notes |
|---|---|
| `id` | ULID |
| `run_id` | One workspace per run. Concurrent runs never share one |
| `kind` | `local` today; `container`, `remote` later behind the same interface |
| `ref` | The git ref checked out at creation: the project's default branch, or the run's branch on resume |
| `branch` | The run's working branch, derived from the node ref |
| `socket` | The unix socket a step's session reaches the workspace through |
| `token` | The step-scoped bearer for that socket. Rotated per step |
| `capabilities` | The effective capability set, from the current step |

A workspace is created fresh for a run — a clone at `ref`, a branch — and destroyed after `done` or `failed`. On `waiting` and `parked` it is retained so a resumed run and an inspecting human find it intact; a resuming worker on another host recreates it from the run's branch.

Steps talk to the workspace over its socket: read and write files, run commands, ask for git operations, ask for a forge credential. The socket is how a step is given capability without being given the host. The verbs and the transport are in [Architecture](architecture.md).

## Capability set

A named set of what a step may do through its workspace.

| Capability | Grants |
|---|---|
| `read` | Read files in the checkout |
| `write` | Write files, stage, commit on the run's branch |
| `exec` | Run commands in the checkout |
| `network` | Outbound network from commands |
| `forge` | A forge credential scoped to the project's repository: push the branch, open and query a PR, read CI status |
| `deploy` | Whatever the project's deploy step needs; defined per project |

MVP enforces `read`/`write`/`exec` through the adapter's tool policy — the only enforcement until containers — and grants `network` and `forge` by injecting scoped credentials per step. A reviewer that may run tests has `exec`, and `exec` can write; MVP does not prevent that, it **detects** it: a `clean-tree` check after every read-only step fails the step if the tree changed. Enforcement moves into the workspace implementation with containers. A step that needs a capability it lacks fails with a named reason the session sees — *"forge capability not granted to step `implement`"* — so a definition error is legible rather than mysterious.

## Step token

The credential ladder's bottom rung. Issued by the store per step run; the only credential a session ever holds.

| Field | Notes |
|---|---|
| `step_run_id` | What it is scoped to |
| `token` | Opaque bearer. Presented on the workspace socket |
| `capabilities` | Copied from the step at issue |
| `issued_at`, `revoked_at` | Revoked when the step run closes, whichever way |

Every derived credential — the forge token for a `push`, a registry pull — is minted by the workspace against a live step token and dies with it. A session's environment holds the socket path and this token, and nothing else.

## Result

What the mill gives dagger when a run reaches `done`. Shaped by dagger's `Complete`: a markdown body under the cap, plus artifacts.

The body is the implementing session's own summary. The artifacts, as dagger artifact parts:

| Artifact | Part kind |
|---|---|
| The plan | `text` |
| Each verdict | `text` |
| The PR | `reference`, scheme `git`, with the URL and the head commit |
| The merge commit | `reference`, scheme `git`, the commit on the default branch |
| Usage summary | `text` |

Every artifact's metadata carries `run_id` so a reader walks from the result back to the ledger.

Usage is posted to dagger through `ReportUsage` **after every step run closes or fails** — one record per `(lease, model)` — not only at the end. Dagger's contract says reporting again adds a record and the attempt's cost is the sum, and that each report renews the lease, so posting per step is both the intended shape and a heartbeat. A worker that dies loses at most the in-flight step's usage from dagger's view; the ledger has it per turn.

Predecessor results arrive in a run as the node's inputs. They are rendered into prompts as attributed, delimited data — a claim by that principal — never as instructions. This is dagger's rule and the mill is its first consumer that has to honour it.

## Principal

Mill runs as the operator's unix account on the host, and by dagger's transport-resolved identity it *is* the operator's `user` principal. Dagger designed for this: an agent under a person's uid is that person, with that person's authority; the trust zone is the delegation (ADR-006 §"A trust zone is the delegation mechanism", ADR-009). It is also the only principal kind whose `ReportUsage` dagger accepts today.

What follows from it, and what mill therefore owns:

| Fact | Mill's obligation |
|---|---|
| Dagger applies no gate veto to a user principal | Mill never claims, completes, drops, reclassifies, or unlinks a gate — including its own. Enforced in mill's code, tested, not delegated to policy |
| The principal cannot distinguish mill from the operator by hand | Every `ClaimNext` carries `harness: mill`, `agent_ref` naming the worker, `session_ref` naming the run; every leased write carries the lease. Every lease-less `ApplyPatch` uses a `request_id` prefixed `mill-<run>-`. "Everything mill did" is a join on those, and `mill history` performs it |
| Mill's writes are `human_touched` | The operator vouches for them by running mill. The ledger, not dagger, is where a reader learns a step was a machine's |
| A gate mill files and the operator answers is one principal completing it | Fine by design (ADR-009 §12). The ledger records that mill filed it and never completed it |
| Workers share the principal | They are distinguished by `worker_ref` on the run and by the fencing token, which is what makes a stale worker's late write fail correctly |

The reviewer runs under the same account with a read-only tool policy and a step token that grants no `write`; the ledger records which session produced which verdict.

Other principals reach the store over its socket by peer uid, mapped by configuration to `operator` (read and operate) or `reader` (read). Because mill and the operator share a uid, the role map matters for *other* uids — the zone agents. They never hold a lease and never write evidence; `cancel`, `rerun`, and `restart` are requests the worker honours, recorded as events with the requesting uid.

Moving mill to its own account is a later change and a larger one than it looks: a new `agent` principal in dagger, every gate veto beginning to apply, and a change to dagger's `ReportUsage` admission — an orchestrator principal kind or a stored delegation grant, which dagger's roadmap lists as deliberately unscheduled. It is the identity bet's work, not this project's.

## Open questions

Settled: a `workflow.yaml` is **one file per workflow**, sitting above a `skills/` directory rather than beside a single `SKILL.md`. A step references its skill by name and digest, so a skill shared across workflows is stored once and each workflow pins the version it ran. `internal/workflow` is the loader.

