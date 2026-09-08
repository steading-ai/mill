# Core Principles

Decision filters for this project. Every principle has teeth — it should be possible to violate it, and doing so should feel wrong. Each was a decision first; it is here because it kept coming up.

## 1. Repo protections are the repo's job
Auto-merge always. If a change needs more care than the mill's own checks, the repository enforces it — CI gating merge, required reviews, branch protection. The mill never second-guesses a forge that let it merge. If nothing blocks, hitting the default branch is done.

## 2. One node's worth of state per run
The mill never holds more than the current node's state in memory or ledger. Decomposition is a workflow that files child nodes in dagger and ends; the mill never keeps a sub-plan. This is the rule that stops it growing a planner or a shadow backlog.

## 3. Every spawned process goes through the workspace
`Workspace.Exec` is the single chokepoint for anything a step runs — it sets cwd, uid, and environment. `exec.Command` is forbidden outside the workspace package (depguard/forbidigo, as dagger does). Checks are `Workspace.Exec` calls with a contract. This is what makes the local → container → remote swap a config change.

## 4. Escalation has a price
There is no `needs-human` verdict. A reviewer may `approve` or `revise`. A human is reached one way: the circuit breaker — retry cap exhausted, last findings as a comment, dagger gate. Asking a human costs three real attempts.

## 5. One way to create work
The workflow that decomposes a node uses the same skill a thicket agent uses to file work from a conversation. There is one path into dagger, and the mill is a client of it like everyone else.

## 6. The skill is portable; the record is ours
A workflow step is an agent skill (name, description, body, scripts, assets — the part that survives switching harnesses) plus an mill record beside it (output schema, ordering, the gate check, harness translation, provenance, scope). Nothing mill-specific goes in `SKILL.md` frontmatter beyond a flat breadcrumb.

## 7. Routing may be judgment; sequencing must not be
A description-driven router may *choose* a workflow. Within a workflow, order and gates are explicit metadata. The spec's own docs say activation-by-description is probabilistic; nothing that must happen relies on it.

## 8. Evidence closes steps, agents don't
A step is complete when the mill has verified a fact outside the session: a commit SHA that resolves, a test process that exited 0, a verdict that parsed and said `approve`, a PR URL the forge returns, a merge commit on the default branch, a healthcheck that answered. The workflow names the evidence each step must produce; the mill checks it; nothing else advances the run. An agent saying "done," a transcript that reads well, or a task list marked complete is not evidence and never closes a step. This is the principle every other one serves: it is what makes a workflow enforced rather than described, what makes resume possible, and what makes a run's record trustworthy after the fact.

## 9. A session never sees a credential it could keep
A step is the thing that runs a session and carries a capability set, so a step is the unit a credential is scoped to. The store issues a token per step; that token is the only credential the session holds; every derived credential — git, forge, registry — is minted against it and dies with the step. The session's environment contains nothing worth stealing. Buildkite arrives at a similar ladder by scoping to the job; ours follows from the domain model, where the lease belongs to the run and the capability set to the step.

---
