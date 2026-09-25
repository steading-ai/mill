# Orchestrator spec review — race conditions, correctness, resume

Reviewed: `Domain Model.md`, `Architecture.md`, `Ledger.md` against `Orchestrator (PRD).md`, `2026-09-07 - PRD Review Responses.md`, and skift's `docs/domain-model.md`, `docs/architecture.md`, `proto/skift/v1/service.proto`, `skift.proto`, and the domain/store Go source (`internal/domain/patch.go`, `lease.go`, `limits.go`, `usage.go`) where the docs' prose needed checking against the actual contract.

Findings are ordered most severe first.

---

## 1. No heartbeat anywhere in the design — leases will expire mid-step, routinely

**Severity: critical.** *Domain Model.md, Architecture.md, Ledger.md — no section.*

None of the three specs mention `Heartbeat` at all (`grep -i heartbeat` across all three returns nothing), despite the PRD requiring it explicitly: "R-1 ... Heartbeat the lease; release it with a reason on failure." Skift's default lease TTL is 15 minutes and the server-enforced ceiling is 1 hour (`internal/domain/lease.go:30`, `internal/domain/limits.go:61`). A single skill step — `implement`, a `revise` round, a CI-watch loop — routinely runs longer than 15 minutes and can exceed an hour.

**Failing sequence:**
1. Worker A claims node N, lease L1 (TTL 15m), starts the `implement` skill step, session running for 20 minutes.
2. L1 expires at minute 15. Skift's reaper (`skift-reap.timer`) fires the expiry event and N returns to the ready set.
3. Worker B (or Worker A itself on its next unrelated poll cycle) calls `ClaimNext`, gets N under a fresh lease L2, calls `RunByNode`, finds `status = running`, and per the Resume algorithm's own comment treats this as "a previous worker died; its lease expired" — even though Worker A's session is still actively running at minute 20.
4. Worker B calls `AbortOpenStepRuns` (flips A's in-flight `step_runs` row to `aborted` with no liveness check beyond the expired lease) and `SetLease(run, L2, self)`, then reattaches the **same** local workspace directory (or clones a fresh one on another host) and starts a **new attempt of `implement`, resuming the same deterministic session id** (UUIDv5 of `context_id, "implement"`), because session ids are derived from the run, not the lease.
5. Worker A, unaware L1 is gone (nothing in its loop checks lease liveness mid-step), keeps running: still writing to the same git checkout (if local/same host), or later tries to close its step with `store.Close(sr, evidence)` for a `step_runs` row that Worker B has since marked `aborted`, or pushes a commit to the same branch Worker B's session is also committing to.

This is exactly the "two workers both believe they own the run" scenario, and it is not a rare race — at 15-minute default TTL it will happen on essentially any non-trivial `implement` or `review` step. `AbortOpenStepRuns` is unconditionally destructive (it does not, and cannot, verify Worker A is actually dead), and nothing bounds Worker A's continued activity against the workspace, the git branch, or the ledger.

**Fix:** Add an explicit heartbeat loop to the worker's per-step execution (a goroutine that calls `skift.Heartbeat(lease_id, ttl)` on an interval well under the TTL, for the duration of every step, in both `Ledger.md`'s Resume pseudocode and `Architecture.md`'s "Steps" execution description). State the invariant that a step's wall-clock `timeout` must be enforced independently of, and shorter than, the lease TTL so the worker can always release/heartbeat before expiry. Until this exists, treat "lease expired but the run's ledger status is still `running`" as an ambiguous case the design has not solved, not as the normal crash-recovery path it's currently described as.

---

## 2. `revise` to an earlier step does not re-run, and does not invalidate, the steps between it and the failure

**Severity: critical.** *Domain Model.md §Step, §Evidence; Ledger.md §`current_step` derivation, §Resume; PRD R-22, D3 in Responses.*

`current_step` is defined as "the first step in `workflow.definition.steps` whose latest `step_runs` row has no closing evidence" (Ledger.md, `runs` table note; repeated in Domain Model.md §Evidence: "a run's `current_step` is its first open step"). Evidence is insert-only and never invalidated (Ledger.md Invariant 6, Domain Model.md §Evidence: "Immutable once recorded").

**Failing sequence**, using the `feature` workflow's implied step order (`implement → simplify → commit → pr → review`, one session through `pr`, `review` separate, per Responses B6):
1. Pass 1 completes normally: `implement`, `simplify`, `commit`, `pr` all get closing evidence (a structured output, an exit, a commit SHA, a PR URL respectively).
2. `review` runs and returns `verdict.decision = revise`. Its `on_failure: revise implement` fires (Domain Model.md §Step, `on_failure` field). The Resume loop sets `step = implement` and continues with the finding as the next prompt.
3. `implement`'s session is resumed (same deterministic session id) and produces new code. A new `step_runs` attempt for `implement` is started and, assuming the step's evidence kind is satisfied, closed with new evidence.
4. The for-loop's next iteration calls `run.current_step()` again. By definition this walks the workflow's step list in order and returns **the first step whose latest row has no evidence**. `implement`'s latest row now has evidence (step 3), so the derivation moves on to `simplify` — whose latest `step_runs` row is still the **pass-1** row, which **still has closing evidence** (evidence is immutable and was never invalidated). So does `commit`'s and `pr`'s. `current_step()` therefore returns `review` again, having silently skipped re-running `simplify`, `commit`, and `pr` on the revised code.

Concretely: the new implementation is never re-simplified, the new changes are never committed (the `commit` step's evidence — a SHA on the branch — is the pass-1 SHA, which does not contain the revision), and the `pr` step's evidence (a URL) still points at the pass-1 PR/head commit. `review` re-runs against a workspace containing uncommitted changes with no corresponding evidence trail, while the ledger's record of "what closed this run" (the `commit` and `pr` evidence rows) is now false. This is not a rare case — it is what happens on the **second** `revise implement` cycle in any run, i.e., the ordinary path once a reviewer asks for a second round of changes to something already committed once.

This directly contradicts the prose in Ledger.md's Resume section ("Resume is the same code as a first run... a fresh run's `current_step()` is simply the first step") and PRD principle A8 ("a step is complete when the orchestrator has verified a fact... An agent saying 'done'... is not evidence and never closes a step") — here a step is being treated as closed by *stale* evidence from a superseded attempt.

**Fix:** Define what a `revise <step>` targeting an earlier step does to the steps between the target and the point of failure. The two coherent options: (a) `revise <step>` implicitly re-opens every step from `<step>` through the failing step by writing a new "aborted"/"superseded" marker (or simply requiring current_step derivation to consider the failing step's step_name, not just the plain min-over-list scan, so that any step positioned between the revise target and the reviewer is forced to re-run); or (b) make `current_step()` derivation step-name aware of "generation": tag each step_run with the workflow-step-list index of the *latest revise cycle* it belongs to, and require monotonically non-decreasing generation across the ordered step list, so a lower-generation closed step downstream of a higher-generation reopened step is treated as open again. Either way, the current single-sentence definition of `current_step` in Ledger.md needs a second clause covering this, and Domain Model.md's `on_failure: revise <step>` needs to state explicitly whether intermediate steps re-run.

---

## 3. Gate resume path is unspecified — resuming a `waiting` run at the gate step re-files the gate instead of consuming its result

**Severity: critical.** *Architecture.md §Gate steps; Ledger.md §Resume; Domain Model.md §Run states, §Gate.*

Domain Model.md says: "`waiting` — a gate node exists and blocks this node... When the gate completes, the node becomes ready again, `ClaimNext` returns it, and the worker that gets it resumes this run." But nothing describes *how* resume distinguishes "the current step is a gate step we already filed and which has now completed" from "the current step is a gate step we haven't touched yet."

The only description of executing a gate step is in Architecture.md §Gate steps: "Build the gate node from the step's `ask` template... Evaluate `auto`... Otherwise, in one skift call, create the gate, link it to block this node, and release the lease with reason `gate`." This is the entire specification of gate execution — there is no branch for "a gate for this step already exists (`run.gate_node_id` is set); read its result instead of filing a new one."

Trace it through Ledger.md's Resume pseudocode:
```
for step in run.workflow.steps starting at run.current_step():
    sr := store.StartStepRun(run, step)
    ...
    result := execute(step, sr, ws)
```
`current_step()` is "the first step whose latest `step_runs` row has no closing evidence" (Ledger.md). The gate step's evidence (`kind: gate`, "verified how: the gate node's status is `done`") is only recorded *after* the gate completes — but nothing in the Resume algorithm shows *when or how* that check happens. On resume, `current_step()` still points at the gate step (no evidence yet), so the loop calls `store.StartStepRun` (a **new attempt**, since attempts are contiguous per `(run_id, step_name)` — Invariant 7) and `execute()` for a gate step, which per Architecture.md's only description of that code path **creates a second gate** rather than reading the first one's completion.

**Failing sequence:**
1. `implement` closes; `plan-approval` gate step evaluates `auto` false, files gate G1 (blocking N), run → `waiting`, `gate_node_id = G1`.
2. A human completes G1 (approves). N becomes ready (no more open/blocking children); `ClaimNext` returns it.
3. Worker calls `RunByNode`, finds `status = waiting`, proceeds per the `waiting`/`parked` branch of the assert (no `AbortOpenStepRuns` needed since nothing was left `running`), sets the new lease, and computes `current_step()`. The `plan-approval` step's only `step_runs` row is still open (no `gate` evidence was ever written for it — nothing in the docs specifies a routine that, on resume, checks G1's status and writes the evidence row before falling into the execute loop). So `current_step()` returns `plan-approval` again.
4. The loop starts a **second attempt** of the `plan-approval` step and calls `execute()`, which (per the only spec of gate execution) evaluates `auto` and, if false, **files a second gate G2** identical to G1, even though a human already approved G1 a moment ago.

**Fix:** Ledger.md's Resume pseudocode needs an explicit branch, checked before the generic step-kind dispatch, for "current step is a gate and `run.gate_node_id` is set": fetch the gate node, and if its status is `done`, write the `gate` evidence for the existing `step_runs` row (closing it, no new attempt) using the gate's result; if `open` (should not happen, since `waiting` implies the node had to become ready, which required the gate to close, but a defensive check costs nothing); if the gate was denied, follow R-22's failure path. Architecture.md §Gate steps needs the equivalent "on resume of a step already gated" paragraph next to the "evaluate `auto`" one.

---

## 4. Finish-sequence ordering: `store.Done` before `skift.Complete`, and `ReportUsage` called after the lease it needs is already gone

**Severity: critical.** *Ledger.md §Resume, final line; cross-checked against `proto/skift/v1/service.proto` and `internal/domain/usage.go`.*

The Resume pseudocode's terminal line is:
```
store.Done(run); skift.Complete(node, lease, result, artifacts); skift.ReportUsage(lease)
ws.Destroy()
```

Two independent bugs here, both concrete:

**(a) `store.Done(run)` runs before the skift write it depends on.** If the process dies (or the skift call fails/times out) after `store.Done` commits but before `skift.Complete` (`ApplyPatch` with `complete_node`) succeeds, the ledger now says the run is `done` — a *terminal* state per Domain Model.md's Run states — while skift's node is still `open`, still (briefly) leased, and will return to the ready set once that lease expires. The next worker to claim it calls `RunByNode`, finds a run whose `status` is `done`. Ledger.md's Resume algorithm only asserts `run.status in (waiting, parked, running)` for an existing run — `done` is not in that set, and nothing describes what happens next. Because `runs.node_id` is unique (Invariant 1) and a workflow is set exactly once (Invariant 2), the orchestrator cannot create a second run for this node either. The node is now permanently unclaimable and unfinishable: every future `ClaimNext` on it will re-hit the same broken assertion. This is a stuck-node deadlock caused directly by writing the irreversible local state before the external system has durably confirmed the corresponding fact.

**(b) `skift.ReportUsage(lease)` is called after `skift.Complete`, but `Complete` already released that lease.** `service.proto`'s `ReleaseRequest` comment states outright: *"Completing a node releases its lease as part of the patch; this is for every other way an attempt ends."* Confirmed in the domain implementation: `releaseLease` inside a patch transaction is only ever invoked from `internal/domain/complete.go`, i.e., completing a node atomically releases its lease as part of that same `ApplyPatch` call — no separate `Release` call is made or needed for the `done` path. `ReportUsage`'s own doc comment requires "the node's current lease... it must be the node's current lease and the caller's own," and `internal/domain/usage.go`'s `ErrUsageNeedsLease` / the requirement that "an actual is attributed to an attempt" both confirm usage reporting needs a **live** lease. By the time `skift.Complete` has returned in the pseudocode's stated order, the lease is no longer live — so this final `skift.ReportUsage(lease)` call, exactly as sequenced, will be rejected every single time a run finishes normally. This isn't a race, it's a deterministic ordering bug in the pseudocode as written.

Also note: `skift.Complete` as a named call does not exist in skift's API — completion is `ApplyPatch` with a `CompleteNodeOp` (`service.proto:34-64`, `CompleteNodeOp` at line ~293). This is a small vocabulary mismatch worth fixing in the spec regardless, because it's what obscures bug (b): thinking of "Complete" as its own RPC hides that it's a patch that auto-releases the lease as a side effect.

**Fix:** Reorder to: `skift.ReportUsage(lease)` (while the lease is still live) → `skift.Complete(node, result, artifacts)` (an `ApplyPatch` carrying the lease id, which atomically closes the node **and** releases the lease — do not call `Release` separately for this path) → `store.Done(run)` (only after skift confirms) → `ws.Destroy()`. State explicitly that `Complete` releases the lease as a side effect of the same call, so no separate release step exists for the `done` path, unlike gate/park/crash. Because `ApplyPatch` takes a client-supplied idempotent `request_id`, a crash between `ReportUsage`/`Complete` and `store.Done` can safely retry the skift call with the same request id and get the original result back — but only if `store.Done` is what's missing, i.e., only if it's written last.

---

## 5. Store-side lease fencing is asserted in prose but never specified as an atomic mechanism

**Severity: high.** *Ledger.md §Resume, last bullet; §Invariants (no matching entry).*

The Resume section's closing claim is load-bearing: "A stale worker cannot corrupt the record. Its skift writes are fenced out; its store writes are rejected because the run's `lease_id` no longer matches the one it presents. The rule: the worker sends its lease id on every store write too, and the store compares." This is the *only* place this rule appears. It is not one of the ten numbered Invariants, and no table (`runs`, `step_runs`, `evidence`, `step_tokens`) specifies a column, constraint, or transactional check that would actually reject a mismatched write.

Contrast with how skift enforces this for its own writes: `internal/domain/patch.go`'s `fence()` function takes an explicit row lock (`SELECT ... FOR UPDATE`) on the node, re-reads which lease currently holds it in a **separate statement after the lock** (with a comment explaining exactly why: "a row that is only locked, not updated, is not re-read once the lock is granted... this would miss a lease a claim committed while the lock was waited for"), and only then compares against the caller's presented lease id — all inside the same transaction as the write it's guarding. That is a carefully-reasoned, concrete mechanism. Nothing comparable is specified for the orchestrator's own store.

Without it, the specific race the Resume section claims is handled is not actually closed: e.g., two `UPDATE step_runs SET status = 'closed' ...` statements from an old and a new worker, issued against the same row without a `WHERE (SELECT lease_id FROM runs WHERE id = $1) = $2` guard evaluated atomically with the write, can interleave under READ COMMITTED exactly the way skift's own `heldBySQL` comment warns about for patches.

**Fix:** Ledger.md needs an explicit mechanism, mirroring skift's: every store write that mutates `step_runs`/`evidence`/`step_tokens` takes a row lock on the `runs` row (or does a single `UPDATE ... WHERE lease_id = $presented` and checks the row count), inside the same transaction as the write, and this needs to be its own numbered Invariant, not a sentence in the Resume narrative.

---

## 6. Gate filing is not "one skift call," and the two specs disagree on write order

**Severity: high.** *Architecture.md §Gate steps ("in one skift call, create the gate, link it to block this node, and release the lease"); Ledger.md §Resume ("`gate filed: store.MarkWaiting(run, gate); release lease; return`"); Domain Model.md §Run states ("The lease was released atomically with filing the gate, per skift").*

Skift's own domain model says filing a gate and releasing the lease **should** be atomic ("Filing a gate and releasing the lease is one atomic operation. Doing it in two calls can strand a lease on a node whose graph has already changed" — `skift/docs/domain-model.md`), but the actual contract does not provide a combined call. `ApplyPatch` (which creates the gate node and the blocking edge) and `Release` (which ends the lease with `reason: gate`) are two separate RPCs on two separate services (`GraphService.ApplyPatch`, `WorkService.Release`). Skift's own acceptance test confirms this is genuinely two calls: `tests/acceptance/acceptance_test.go:176-198` — *"when it files a user-assigned gate and releases in one operation... gate is a patch under the lease, and the release follows it"* — i.e., `ApplyPatch` then `Release`, sequentially, not one call. (`Complete` is the one operation that gets true atomicity, because `complete_node` releases the lease as a side effect of the same patch — see finding 4. There is no equivalent for gate filing.)

So Architecture.md's "in one skift call" is factually wrong against skift's own implementation, and this matters for exactly the crash-window question the domain model itself raises: if the process dies **between** the `ApplyPatch` (gate node created, edge added — the node is now graph-blocked) and the `Release` call, the lease sits live until it naturally expires (harmless: the node stays non-ready regardless, because the gate edge already blocks it independent of the lease). That part self-heals. But:

The two specs also disagree on where the **store** write falls relative to the two skift calls. Architecture.md's order is: create gate + link → release lease → *then* "Mark the run `waiting`" (store last). Ledger.md's Resume pseudocode order is: `store.MarkWaiting(run, gate)` → `release lease` (store write in the middle, before the skift release). Neither order addresses the actual danger, which is a crash between the `ApplyPatch` that creates the gate and *any* store write recording `gate_node_id` — see finding 3: a crash there means the store never learns the gate exists, and resume (whenever it eventually happens, since the run's `status` is still `running` at that point) will re-run the gate step from scratch, filing a **second**, orphaned gate that duplicates the first.

**Fix:** State plainly that gate filing is two skift calls (`ApplyPatch` then `Release`), not one, and specify the crash-safe order across all three writes: `ApplyPatch` (create gate, get its node id back) → `store.MarkWaiting(run, gate_node_id)` (durably record the gate id *before* releasing the lease, so a crash here at worst leaves the lease alive a little longer, which is harmless, rather than leaving the store ignorant of a gate that already exists) → `Release(lease, reason=gate)`. Make Architecture.md and Ledger.md agree on this order explicitly.

---

## 7. `shutdown` is not a valid `ReleaseReason`

**Severity: high** (as a spec/implementation contradiction that will fail at the first `SIGTERM`). *Architecture.md §Failure and stop ("release every lease with reason `shutdown`"); PRD R-50 ("leases are released with reason `shutdown`").*

Skift's `ReleaseReason` enum (`skift.proto:87-104`) has exactly seven values: `UNSPECIFIED, COMPLETE, GATE, CRASH, BUDGET, EXPIRED, PREEMPTED`. There is no `SHUTDOWN`. Both the PRD (R-50) and Architecture.md's Stop section call for releasing with reason `shutdown`, which is not a value the `Release` RPC accepts. As written, the orchestrator's shutdown path will send an invalid enum value on every `SIGTERM`.

**Fix:** Pick the closest existing value and say so explicitly — `PREEMPTED` reads correctly ("the lease was taken away by policy or by an operator," which is what an operator-initiated `SIGTERM` is), with the free-text `detail` field carrying "orchestrator shutdown" for the human-readable distinction from an actual operator preemption. Update Architecture.md §Failure and stop and flag PRD R-50 as needing the same correction.

---

## 8. Auto-approval predicate references `tier`, which does not exist when workflow selection used the project default

**Severity: high.** *Domain Model.md §Gate ("`auto`: ... e.g. `verdict.decision == approve && tier <= feature`"); §Workflow §Selection.*

Workflow selection is, in order: project default, else assessment (Domain Model.md §Selection; confirmed by Responses B15/C4: "project default in the orchestrator's store → the tier assessment"). `tier` is part of the assessment step's structured output (`{workflow, tier, signals, reasoning}`), recorded as evidence only when the assessment step actually ran. When a project default supplies the workflow, the assessment step is skipped entirely — there is no `tier` evidence anywhere in the run.

The example auto-approval predicate `verdict.decision == approve && tier <= feature` is evaluated by the gate step against "prior evidence" (Domain Model.md §Gate: "a predicate over prior evidence"). For any run that took the project-default path, this predicate has no `tier` to read. Nothing specifies whether that's an evaluation error (park the run pointlessly), a false default (never auto-approve — silently downgrades every project-default run to always gating on a human, defeating the point of a project default for routine work), or a true default (silently auto-approve regardless of tier, which is the dangerous direction).

**Fix:** Either (a) always run a lightweight tier classification even when the workflow itself is fixed by project default (decoupling "which workflow" from "what tier is this for auto-approval purposes"), or (b) define a per-project default tier that stands in for the assessment's `tier` output when no assessment ran, and state which of these two the design intends. Whatever is chosen, add it as a field on the project-default record (Domain Model.md doesn't currently have a project-default entity to hang it on either — this is worth a table).

---

## 9. Usage lost on crash is unacknowledged, and the Resume pseudocode never posts usage at the gate/park boundaries its own table promises

**Severity: medium-high.** *Ledger.md §`usage` table, §Resume, §What goes to skift.*

The `usage` table's note says: "Per-lease totals are posted to skift through `ReportUsage` when the run reaches a boundary — a gate, a park, or `done` — so skift's per-attempt attribution stays honest." The "What goes to skift, and when" table repeats this: "Per-lease usage | Gate, park, or `Complete` | `ReportUsage`, kind `actual`."

But the Resume pseudocode only shows a `skift.ReportUsage(lease)` call on the `done` path (and, per finding 4, in the wrong position even there). The `gate filed` branch (`store.MarkWaiting(run, gate); release lease; return`) and the `park` branch (`store.Park(run, reason); comment; file gate; release lease; return`) have **no** `ReportUsage` call at all. This is an internal contradiction: Ledger.md's own table states usage must post at all three boundaries; its own algorithm only does it at one, and does that one incorrectly.

Separately and worse: if the worker crashes **mid-step**, with no boundary reached at all, that lease's accumulated usage is recorded in the orchestrator's own `usage` table (per-turn, as the adapter reports — this part is durable and local) but is **never** forwarded to skift, because `ReportUsage` requires that lease to still be current/live, and by the time anyone notices the crash the lease has expired and a new one exists for the resumed attempt. There is no stated mechanism to post a dead lease's already-recorded local usage to skift after the fact (and skift's contract may not even allow it — `ReportUsage` requires "the node's current lease and the caller's own"). This directly undercuts M.5 ("Usage and cost per node are in skift") for every run that ever crashes mid-step, which — combined with finding 1's near-certainty of lease expiry under real step durations — will be a routine occurrence, not an edge case.

**Fix:** Add explicit `ReportUsage` calls to the gate and park branches of the Resume pseudocode, positioned before the corresponding `Release` call (per finding 6's fix, since `Release` — like `Complete` — ends the lease, `ReportUsage` must precede it there too). For the mid-crash case, either accept and document the gap explicitly (skift's per-node total will undercount abandoned attempts; the orchestrator's own ledger remains the complete record), or specify a reconciliation job that reads `usage` rows attributed to `step_runs` in the `aborted` state and posts them to skift under the *new* lease with a note that they're carried over from an abandoned attempt (if skift's contract permits that at all — it currently does not appear to, since usage is scoped to the lease it was earned under).

---

## 10. Cross-host resume of a `waiting`/`parked` run may have no branch to recreate the workspace from

**Severity: medium.** *Domain Model.md §Workspace ("a resuming worker on another host recreates it from the run's branch"); Architecture.md §Deployment.*

Workspace state on `waiting`/`parked` is "retained... a resuming worker on another host recreates it from the run's branch" (Domain Model.md §Workspace). This assumes the branch is fetchable from the forge. But `git.push` only happens as part of the `forge` capability, exercised by the `pr` step, which in the `feature` workflow comes *after* `implement`/`simplify`/`commit` (Responses B6's session ordering: gather-context → think → plan → implement → simplify → commit → pr). The `plan-approval` gate — one of the "first workflows" gates named in Domain Model.md §Gate — fires **before** `implement`, i.e., before any commit exists and long before `pr` has pushed anything.

**Failing sequence:** a run reaches the `plan-approval` gate and goes `waiting` with a workspace that exists only as a local clone-plus-uncommitted-plan-artifact on the original worker's host (nothing has been committed or pushed yet). The original host becomes unavailable before the gate resolves (routine in a multi-worker deployment: R-2/R-3 call for concurrency and multiple projects, and O2 explicitly targets multiple hosts). When the gate completes and a *different* worker claims the resumed run, "recreates it from the run's branch" has nothing to fetch — the branch does not exist on the forge yet. The spec doesn't say what happens: reattach fails, and there's no described fallback (re-run from the node body and prior artifacts loses the plan artifact's uncommitted working-tree state, if any was expected to survive; note the plan itself is stored as a skift artifact per the Result table, so the *plan text* does survive, but any workspace-local state does not).

**Fix:** State explicitly that a workspace with no evidence of a `push` cannot be recreated cross-host, and choose one of: (a) require every gate before the first push to be answerable only by the same worker/host that filed it (an affinity requirement, mirroring skift's `agent_ref` placement-affinity idea, which would need to be enforced by the orchestrator's own scheduling since skift doesn't route), or (b) make the workflow structurally always commit-and-push before any gate that can leave the run `waiting` for an unbounded human-timescale duration, so the branch always exists by the time a workspace might need remote recreation.

---

## 11. `attempts` is overloaded between two different counters, and the circuit-breaker guard doesn't say which one it means

**Severity: medium.** *Domain Model.md §Run ("Derived, never stored: ... `attempts` (count of skift leases on the node)"); Ledger.md §`step_runs` ("`attempt` — 1-based per `(run_id, step_name)`"); Ledger.md §Resume ("`if step.on_failure == revise and attempts < step.retries`").*

Two distinct things are both called "attempts": the run-level derived `attempts` (how many skift leases the node has ever had — i.e., how many times a worker has claimed/re-claimed this node, driven by crashes and lease expiry, see finding 1) and the step-level `step_runs.attempt` (how many times *one named step* has been tried, driven by `revise` loops). `retries` is a per-step field ("How many `revise` returns before `park`. Default 3" — Domain Model.md §Step). The circuit breaker's job is to bound a `review ↔ implement` revise loop; that requires counting **step-level** attempts for the step whose `on_failure` fired (or possibly the target step — the spec doesn't say which either), not the run-level lease count, which is driven by an unrelated axis (worker crashes).

The Resume pseudocode's guard, `attempts < step.retries`, uses the bare identifier `attempts` without saying which of the two it binds to. If implemented against the run-level derived value (the one the Domain Model actually names `attempts`), the circuit breaker would trip based on how many times the *node* was re-claimed across crashes, not on how many times the review/implement loop actually cycled — the wrong axis entirely, and one that would either never trip during a real revise storm (if the worker doesn't crash) or trip spuriously after unrelated infrastructure hiccups.

**Fix:** Rename one of the two. Suggest keeping `runs.attempts` (skift leases) as-is and renaming the per-step counter referenced in the circuit-breaker guard to `step_runs.attempt` explicitly in the pseudocode (`step_runs.attempt(run_id, step.name) < step.retries`), and state which step's name the counter is keyed on when `on_failure: revise <target>` fires on a *different* step than the one being capped (is it `review`'s own attempt count, or `implement`'s? — the natural reading is the failing step's, i.e. `review`, since `retries` is a field of the step whose `on_failure` fired, but this should be stated, not inferred).

---

## 12. The owner's own open question about restarting a run is unresolved, and current design gives no answer

**Severity: medium.** *Domain Model.md §`runs.node_id` inline comment: "Question: What if a run goes sideways and we want to just start over from scratch?"*

This is the owner's own inline `` comments, not this later domain-model draft's). The closest the spec comes is Domain Model.md §Run: "There is no 'restart from scratch' — that is a new node," which describes skift's semantics but not a supported orchestrator-level operation for it: dropping the old node and creating a fresh one loses the original node's edges, comments, and backlink history (anything that pointed at the old node's number now points at an abandoned node), and nothing in these specs describes a "re-point predecessors/children at the new node" helper, an "abandon and relink" workflow, or even an operator CLI verb for it.

**Fix:** Either explicitly scope this out of MVP with a one-line note in Domain Model.md replacing the open `%%` (e.g., "out of scope for MVP: operators drop the node and manually relink; a `orchestrator restart <node>` verb that automates drop+relink+create is a candidate for O3"), or specify the mechanism now, since it interacts with `runs.node_id UNIQUE` (finding-adjacent: this constraint is exactly what forecloses "restart the same run" and forces "abandon and recreate").

---

## 13. Architecture.md overstates reviewer read-only enforcement relative to the accepted decision in Responses C3

**Severity: low.** *Architecture.md §Executing a step ("the SDK enforces it, the model doesn't get a vote") and §The workspace ("the tool policy and the capability set are then two views of one thing"); Responses C3 ("In MVP it isn't, honestly").*

Responses C3 is candid that MVP read-only enforcement for the reviewer is "the adapter's tool policy... the model's honor plus the SDK's enforcement... real read-only is... the O4 workspace implementation." Architecture.md's phrasing — "the tool policy and the capability set are then two views of one thing" — reads as though the MCP-server-exposed workspace verbs (`fs.write` gated by the step's capability set) provide a second, independent enforcement layer alongside the SDK's tool allowlist. But Domain Model.md §Capability set says plainly: "MVP enforces `read`/`write`/`exec` through the adapter's tool policy" — i.e., in MVP there is only the one mechanism (the SDK allowlist), not two independent "views." If the reviewer's session is allowed `Bash` for running tests, nothing described stops `Bash` from writing to the checkout (e.g. `echo x > file`), since the workspace does not gate `exec` against the `write` capability until the container-based O4 implementation. This isn't a new finding beyond what Responses C3 already accepted, but Architecture.md's wording should match C3's more accurate description rather than imply a redundancy that doesn't exist yet, so a future reader of Architecture.md alone doesn't over-trust MVP's guarantee.

**Fix:** Replace "two views of one thing" with language matching C3: in MVP, the tool-policy allowlist is the only enforcement (and must in practice exclude `Bash`, not just `Edit`/`Write`, from the reviewer's policy, since `Bash` is a write-capable escape hatch); the capability-set check on the workspace socket becomes the second, independent layer only once the O4 container/read-only-mount implementation lands.

---

## Checked and found sound

- **Skift's own fencing.** `internal/domain/patch.go`'s `fence()`/`heldBySQL` pattern (row lock, re-read in a separate statement, compare to the presented lease) is correctly relied upon by the specs for every *skift* write; the gap identified in finding 5 is specifically about the orchestrator's own store, not a misunderstanding of skift's mechanism.
- **Gate node topology.** The gate is created both as a child of the run's node *and* linked with an explicit `blocks` edge into it (confirmed against skift's acceptance test, `tests/acceptance/acceptance_test.go:40-57,176-204`). Both skift's "no open children" and "no `blocks` edge from an open source" readiness rules independently hold the run's node non-ready while the gate is open, and both release it once the gate is `done` — no contradiction between the mechanisms.
- **Gate disposal rules.** "The orchestrator never claims the gate, never completes it, never disposes of it" (Domain Model.md §Gate) matches skift's actual default policy exactly ("An agent cannot dispose of the gate it filed... forbids a non-user principal that filed a gate from dropping, completing, reassigning, reclassifying, or unlinking it" — `skift/docs/domain-model.md` §Gates, §Default policy).
- **`ReportUsage` principal restriction.** Skift refuses agent principals on `ReportUsage` and expects the orchestrator (a `service` principal) to be the caller ("It is the orchestrator's call... an agent principal is refused" — `service.proto`), matching Domain Model.md §Principal and PRD M.5/R-39 without contradiction.
- **Evidence-kind vocabulary.** The eight evidence kinds (`commit, exit, verdict, structured, url, status, artifact, gate`) are listed identically, with identical "verified how" semantics, in both Domain Model.md §Evidence and Ledger.md §`evidence` — no drift between the two documents on this point.
- **Auto-merge / squash-merge evidence.** Domain Model.md's `merge commit` artifact ("The merge commit | `reference`... the commit on the default branch") correctly reflects Responses B2's decision ("the evidence for `merge` is the merge commit SHA on the default branch from the forge API, never the branch ref") — resolved consistently across documents.
- **One run per node vs. skift's node lifecycle.** "A second lease on the same node resumes the same run at its current step... a fresh slate is a new node" (Domain Model.md §Run) is a correct restatement of skift's own rule ("Every attempt shares the node's context id, so a retry resumes the prior session; a fresh slate is a new node" — skift domain-model.md §Lease). No contradiction, modulo the still-open "how do we ever intentionally restart" question in finding 12.
- **Credential ladder.** Step token → derived per-step credentials (git signing key held by the workspace, forge/registry tokens minted per step and dying with it) is internally consistent between Domain Model.md §Step token, §Principal, and Architecture.md §Identity and credentials, and matches the PRD's A9 principle and the buildkite-agent precedent cited in Responses C12.
