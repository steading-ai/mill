# `/work-on` skill analysis

Source: `~/src/github.com/ivy/dotfiles/home/dot_claude/skills/work-on/` (`SKILL.md.tmpl`, `README.md`, `TIERS.md`, `EPIC-WORKFLOW.md`). Rendered copy differs only in line 5 (`model: opus` vs the `bedrock-model` chezmoi template). Sub-skills read: checkout, gather-context (+INDEXES.md), think, plan, review-plan, share-plan, commit, pr, reflect, agents-md. Doctrine: `write-skill/AUTONOMY.md`, `docs/adrs/007-*.md`, `docs/skill-autonomy-migration.md`.

## 1. Workflow selection

Selection is **entirely model-read prose**. Step 0 (`SKILL.md.tmpl:61-70`): fetch the issue with `gh issue view ... --json number,title,body,labels,comments,assignees,milestone`, then "Read the TIERS.md decision matrix. Assess the issue's complexity tier based on signals."

Inputs (`TIERS.md:9-19`) are a weighted-signal table — labels (type, readiness, scope), body length, comment count, linked issues, file references, "should we X or Y?" design questions, sub-tasks/checkboxes/"Phase 1/2/3". Weights are words ("High/Medium/Low"); nothing is scored numerically. Tie-break: "If torn between two tiers, pick the higher one" (`TIERS.md:79`); non-agent-ready labels force `/gather-context` + `/think` regardless (`:80`).

Five tiers (`TIERS.md:23-75`): Quick Fix (single file, exact description) · Small (1-3 files, no design) · Medium (4-10 files, tradeoffs) · Large (cross-cutting, parallel workstreams) · Epic (multi-PR/session). What changes between tiers is which sub-skills get inserted: Small adds `/gather-context`; Medium adds `/think`, `/plan`, `/review-plan`, `/share-plan`, `/simplify`, `/agents-md`; Large adds `TeamCreate` and `/reflect`; Epic replaces the pipeline with decomposition. Tier can be revised mid-run (`SKILL.md.tmpl:232`). Nothing structured produces or records the tier — it lives only in the transcript.

## 2. The steps

Legend: **D** deterministic, **M** model-judgement, **H** human-checkpoint. Column shows what actually happens inside the sub-skill.

| Tier | Step | Invokes | Kind |
|---|---|---|---|
| all | 0. fetch + assess tier | `gh issue view` (D) then TIERS.md (M) | D→M |
| all | 1. propose workflow, "proceed immediately... do not wait" (`:157`) | prose | M |
| all | 2. build task list | `TaskCreate` with `addBlockedBy` (`:159-190`) | M (D artifact) |
| Quick | checkout → edit → `/commit` → `/pr` | git/gh | D/M/M/M |
| Small | + `/gather-context` (light) | read-only, index queries, 1 Explore agent | M |
| Medium | `/checkout` | derive branch from issue, `git fetch`, `git checkout -b` | D; H only on dirty tree (`checkout:65-75`) |
| | `/gather-context` (full) | codebase-memory/qmd + ≤3 Explore agents → synthesized summary | M |
| | `/think — <decisions>` | reads `docs/vision.md`, `docs/core-principles.md`; "ask minimum questions (0-3)" (`think:61`); may invoke `/adr` | M, H-ish |
| | `/plan` | `EnterPlanMode` → task graph → `ExitPlanMode` "await approval" (`plan:175`) → `TaskCreate` append | M then **H** |
| | `/review-plan` | `context: fork`, `agent: reviewer`, Approve/Revise verdict (`review-plan:6-7,103-107`) | M (isolated) |
| | `/share-plan #N` | `gh issue edit N --body-file` | D (write) |
| | implement per plan | edits, agents | M |
| | `/simplify` | built-in quality gate, "mandatory for Medium+" (`:55`) | M |
| | `/commit` | `git commit`; "never `git add -A`" (`commit:38`) | D-ish, M message |
| | `/agents-md` gate | judgement per Doc Refresh bar (`:130-147`) | M |
| | `/pr` | `git push -u`, `gh pr create --body-file` | D |
| Large | Medium 1-6, then `TeamCreate`, multiple `/commit`, `/simplify`, `/agents-md`, `/pr`, `/reflect` | `/reflect` runs `ccstat`, files issues, commits fixes | M / **H** (reflect is "Interactive" in `:210`) |
| Epic | `/gather-context` → `/think` (decomposition) → slice (`EPIC-WORKFLOW.md:16-34`) → choose tracking (sub-issues vs `TaskCreate` vs hybrid, `:36-50`) → per-unit tier workflow → rebase/verify between units → close issues → `/reflect` | mostly M; `gh issue create/close` D | M with D writes |

Step 3 execution loop (`:192-199`): `TaskUpdate` in_progress → execute → completed → `TaskList`. Step 5 close-out (`:236-240`) checks tasks completed and "Closes #N" in PR body.

## 3. Guardrails, or their absence

Every guardrail is prose addressed to the model. Nothing runs a check.

- **Mandatory-step language is the only enforcement.** `:53-57`: "Never skip the tier assessment", "Listed workflow steps are mandatory", "`/simplify` is a quality gate, not polish... Never skip it". Commit `8d422a8` explains why: "Agents were dropping /simplify because the name implies optional polish." Step 4 (`:228-231`) protects `/review-plan`, `/simplify`, `/pr` from deletion — again prose.
- **The task list is the closest thing to a runbook, and it is self-policed.** `:194`: "After each skill invocation, call `TaskList` and verify the workflow tasks still exist. If any were removed, re-create them." PR #324's own reviewer note: "The anti-clobber rule in Step 3 is guidance to the LLM, not a code enforcement mechanism — sub-skills run in the same context window."
- **A referenced check does not exist.** `:54` says "Check 'Available skills' in pre-computed context", but the pre-computed block (`:41-44`) contains only branch and dirty-worktree. The model is told to consult data that is never injected.
- **No test-run step anywhere.** Neither `SKILL.md` nor any tier lists "run the test suite". `/plan` only probes `test -d test/` (`plan:39`); `/review-plan` flags "No verification step after changes" as a *gap in the plan*, not something the workflow runs. `/pr` never checks CI.
- **No verification that `/review-plan` returned Approve** before implementation begins; the verdict is text in the transcript.
- **Silent-skip points:** `/simplify` (documented history), `/agents-md` (judgement gate that can be closed with a one-line rationale, `:190`), `/share-plan`, `/reflect` ("If `/reflect` is available and tier is Large or Epic", `:238`), the whole `/think` step on a mis-tiered issue, and the tier assessment itself if the model jumps to editing.
- Only structural gates: `allowed-tools` (`:8-25`) — `/work-on` cannot itself run `git push`, `gh pr merge`, or edit files except via delegated skills; the Autonomy line `:30` says "has no merge capability". Per ADR 007 this is "defense-in-depth, never a guarantee" under `permissions.defaultMode: auto`. The two real hooks (`~/.claude/settings.json`) are unrelated: a PreToolUse Bash hook (installer guard + rtk rewrite) and a SessionStart beads primer.

## 4. Interactive-only parts

| Point | Where | Essential judgement or policy? |
|---|---|---|
| Dirty worktree / merge conflicts | `checkout:69-75` "Inform user and wait" | Policy: orchestrator starts from a clean sandbox clone; conflict → fail node. |
| `/think` clarifying questions | `think:61,127` 0-3 questions; `:210` "Interactive" | Partly essential. The question *content* is judgement; whether to block is policy. Route to approval surface with a default + timeout, or require the issue to be `agent-ready` so questions become "Revise issue" outcomes. |
| `/plan` `ExitPlanMode` approval | `plan:175`; README "last checkpoint before the agent goes autonomous" | The one checkpoint the docs call load-bearing (README `:11-19`). Can be a gate: post plan to issue via `/share-plan`, wait for label/approval, or auto-approve when `/review-plan` returns Approve and tier ≤ Medium. |
| `/review-plan` Revise loop | `review-plan:107` | Not human — but unbounded; needs a retry cap (AUTONOMY.md "Unbounded iteration"). |
| Escape hatch | `:213` "flag the user and wait for guidance" on test failures, missing deps, ambiguity | Policy: node → `blocked` state with reason. |
| `/pr` asks | `pr:13,40,49,171,200` base branch, template choice, staged changes, empty diff | All policy/defaults. |
| `/agents-md` | `agents-md:146-150` opens `tmux split-window "$EDITOR AGENTS.md"` | Pure interactive side effect; drop or replace with PR-diff review. |
| `/reflect` | `:210` "Interactive"; `reflect:164` "Present it, then execute in the same turn" | Already autonomous post-ADR 007; only "unclear which repo → ask" remains. |
| `/grill-me` | Not invoked by `/work-on`; not in `allowed-tools`. | n/a — interactive by design ("Ask the questions one at a time"). |
| README `:27` "Waits for your confirmation before starting" | contradicts `SKILL.md:157` "do not wait" | Stale doc; the skill does not wait. |

`docs/skill-autonomy-migration.md:98` states it directly: "Medium+ tiers therefore park before a PR exists; Quick Fix and Small do not. Parallel unattended runs are viable today only at those two tiers."

## 5. State

- **Git**: branch `feat|fix|chore/<n>-<kebab>` (`checkout:61-63`), optionally `EnterWorktree`; commits via `/commit`; push only in `/pr`.
- **Claude Code task list** (`TaskCreate/TaskList/TaskUpdate`) — in-session only; the runbook and dependency graph die with the session.
- **Plan file**: `/plan` writes via plan mode; `/review-plan` looks in `~/.claude/plans/` most-recent `.md` or "recent conversation context" (`review-plan:29-31`) — i.e. "the newest plan file" with no binding to the issue.
- **GitHub issue**: `/share-plan` rewrites the body (`share-plan:114-117`), preserving Problem/Goal; epics create/close sub-issues. PR body embeds `<!-- claude-session: ${CLAUDE_SESSION_ID} -->` (`pr:93-96`) for transcript lookup.
- **Temp files**: `/tmp/plan-body.md`, `/tmp/pr-body.md` — fixed paths, collide under parallelism.
- **Beads** is primed at SessionStart by hook but `/work-on` never writes to it; it is not in `allowed-tools`.
- **Interruption/resume**: nothing. No step records "tier=Medium, completed through /review-plan". A resumed session must re-derive everything from git state and the transcript. The tier, the Approve verdict, and which gates ran are unrecoverable.

## 6. Extensibility points

Plug-in today:
- **Per-project docs**: `/think` reads `docs/vision.md` and `docs/core-principles.md` (`think:27-35`); `/gather-context` reads `CLAUDE.md` for constraints (`:147`); `/plan` probes `CLAUDE.md` and `test/`; `/review-plan` checks conventions against `CLAUDE.md`; `/agents-md` maintains `AGENTS.md`.
- **PR templates**: `pr:32-37` six template paths.
- **Label taxonomy**: `TIERS.md:7` `gh label list` — the only project-specific input to tier selection, and it is interpreted, not mapped.
- **Skill availability**: "degrades gracefully" (README `:55`) — but see §3, the availability list is not actually injected.
- **Agent types**: `plan:118-122` "Project-specific agents — check pre-computed context" (also not injected).
- **Model routing**: `bedrock-model` chezmoi template per skill tier (opus/sonnet).

Hardcoded: the five tiers and their step lists; GitHub/`gh` as the only tracker (no Linear, no skift, no beads); the sub-skill names; task-naming rules; the Doc Refresh heuristic; `/tmp` paths; the assumption of one issue → one branch → one PR (epics excepted). No per-task-type workflows (docs-only, dependency bump, incident) — issue #323 wants `/doc` added, #286 wants `/checkout` to assign + move status; both would be more prose in the same file.

## 7. Delivery gap

"Deliver" = **PR opened**. `:30` "drives the whole workflow autonomously — invoking it authorizes every delegated step through PR · has no merge capability"; `:209` `/pr` "Opens PR for review"; Step 5 verifies "Closes #N" text. README `:3` says "open to merged PR" but the skill never merges, never waits for CI, never reads bot/review comments, never rebases on green, never deploys. `pr:152` has no `--draft`. The only post-PR activity is `/reflect`. Epics add "check if earlier PRs have been merged — rebase if needed" (`EPIC-WORKFLOW.md:68`), again by hand. The intended end state per `migration.md:107`: "parallel `/work-on` runs open draft PRs, churn to green builds and passing bots unattended, and the human flips to ready and merges" — blocked on "Do the bots engage with draft PRs?"

## 8. Map to an orchestrator

**Deterministic gates the Go orchestrator enforces** (never the model):
- Preconditions: clean sandbox clone at default HEAD; branch name derived from node; `/tmp` paths replaced by per-run dirs.
- Tier/workflow selection: keep the model's *assessment* but make it emit structured output (`{tier, signals, reasoning}`) that the orchestrator validates against a pluggable `Workflow` definition (ordered steps, required gates, per-tier) — the extensibility point Ivy wants. Label-driven overrides (`workflow: docs`) short-circuit the model.
- Step ledger: the orchestrator owns the runbook (the current `TaskCreate` list), marks steps done from *evidence* — a commit SHA exists, `review-plan` output parsed for `Approve`, `simplify` ran (exit + diff), tests ran (exit code), `gh pr view` returns a URL — instead of the model self-reporting `TaskUpdate completed`. This is the missing "did it happen" check.
- Retry caps on Revise loops and CI churn.
- Post-PR: poll checks, ingest bot comments as data, rebase — the delivery half that doesn't exist today.

**Headless Claude Code sessions** (one per step, fresh context, artifacts in/out): `/gather-context` (already read-only), `/think` in non-interactive mode (decision + open questions as output, not chat), `/plan` with `ExitPlanMode` replaced by writing the plan artifact, `/review-plan` (already forked), implement, `/simplify`, `/commit`, `/pr --draft`. Each step's `allowed-tools` becomes the sandbox capability set — the structural gate ADR 007 prefers.

**Approval surface**: plan approval for Medium+ (or auto when Approve + tier ≤ Medium — a policy knob), `/think` open questions that cannot be resolved from the codebase, blocker escalations, ready-for-review/merge. Each is a node state in skift, not a chat pause.

**Single-run mode** lives at the orchestrator CLI: `orchestrator run --issue N --tier medium --approve=auto --sandbox=local` executes one node end-to-end with the same step ledger, streaming each headless session's output, and stops at the same gates — the same code path the daemon uses when it claims from skift, minus the claim. That is also the test harness for new workflow definitions.

## 9. Why it's unreliable — per the docs

- **`795b192`/#284**: "blob task names... sub-skill invocations were clobbering the task list mid-workflow. This caused skipped steps, wrong sequencing, and lost workflow tasks." Fix was prose + a self-check; reviewer note admits no hard prevention.
- **`8d422a8`**: "Agents were dropping /simplify because the name implies optional polish."
- **#320** `/plan` replaced the task list; **#283** `/plan` "doesn't reliably enter plan mode or pass arguments through"; **#322** "The agent pauses between phases waiting for 'continue' despite `/work-on` instructions saying 'drive autonomously'" — fixed by adding a `/execute` skill "primarily a system reminder to have a bias toward action".
- **ADR 007 consequences**: "the `**Autonomy:**` line is a convention with no schema enforcement"; "activation-awareness is model self-report. A skill can widen its own authority"; omitted tools do not prompt under `auto`.
- **`migration.md:98`**: Medium+ parks at `/think`/`ExitPlanMode`; only Quick Fix/Small run unattended. Tier 4 "loop-hazard surgery" is explicitly unstarted.
- **#344**: commit signing disabled because 1Password Touch ID blocks unattended commits.
- **#113** (closed, but the origin story): "Babysit and approve every tool call... Test suite and many tools can't be trusted not to modify the host. No isolation/sandboxing."
- Doc drift: README `:27` "Waits for your confirmation" vs SKILL `:157`; `:54` references pre-computed "Available skills" that is never computed; `reflect` "no skips" vs `:238` conditional.
