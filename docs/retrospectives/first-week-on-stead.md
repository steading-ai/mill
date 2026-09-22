# Retrospective: the first week on stead

**Date:** 2026-09-22
**Scope:** the week from the subscription reset on Sunday 2026-09-20 04:00 PDT to the cap on Tuesday morning: the factory moved from the server to a laptop guest under stead, the worker was opened to every project, and mill, stead, dagger and steading were built by mill under a person's watch. One voice, Claude's, written from the record: mill's ledger (`mill cost`, `mill inspect`, the store), dagger's nodes, the guest journal, GitHub's merged pull requests, and the server's ledger for the week before.

## What was built

51 pull requests merged across four repositories, 52 work nodes done, from a cold start on Sunday.

| | Merged PRs | Nodes done | Open at the cap |
|---|---|---|---|
| mill | 33 | 34 | 6 |
| steading | 9 | 9 | 12 |
| stead | 7 | 7 | 0 |
| dagger | 2 | 2 | 0 |

On top of the 52, mill filed 24 park gates a person answered. Every merge went through plan review, implementation review and the auto merge gate. One merged with red checks (below).

Monday night alone, between the laptop sleeping at 00:03Z and the cap: twelve nodes merged; five bugs found in mill from its own logs and four of them fixed and deployed the same night; GHCR publishing stood up for mill and dagger so steading's CI can obtain both without a credential of its own; the browser end-to-end suite landed on steading.

**The code.** 913 files, +67,958 / −6,556 lines, by what the lines are:

| Class | Added | Removed |
|---|---|---|
| Source | 18,108 | 3,180 |
| Tests | 15,014 | 568 |
| Generated (proto, buf) | 20,408 | 2,601 |
| Plan documents | 12,761 | 0 |
| Docs | 843 | 150 |
| Lockfiles | 824 | 57 |

About 33,000 written lines, 45% of them tests. Roughly 650 written lines and 340k output tokens per merged pull request; about 530 output tokens per line that survived review, the rest spent on plans, reviews and revise rounds.

**The tokens**, gateway-counted:

| | Input | Output | Cache read | Cache write |
|---|---|---|---|---|
| Sonnet | 2.64M | 15.36M | 1,860M | 122M |
| Opus | 0.26M | 2.09M | 212M | 17M |
| total | 2.90M | 17.45M | 2,072M | 139M |

By UTC day: Sunday 2.0M output, Monday 12.1M, Tuesday 3.3M by the cap. Opus is reviews only and is 12% of output. At published API rates the equivalent bill is about $2,050, over half of it cache reads and writes; mill's rate table has no entry for these models, so the ledger shows a dash and this is arithmetic, not the ledger.

**The funnel** on mill's own feature workflow, 17 runs: think never retried; plan 32 attempts with 11 revise rounds from plan review; implement 31 attempts; review sent 8 of 15 back once. Implement generated 1.73M output tokens, plan 0.87M, simplify 0.43M, the two reviews 0.65M together. Attempt counts include restarts and the outage, so the true revise rate is lower than the raw attempts.

## Against the week before

The server's ledger for the previous week, nothing having run there since Sunday:

| | Last week (server) | This week (guest) |
|---|---|---|
| Runs | 26 | 54 |
| Output tokens | 4.24M | 17.45M |
| Opus share of output | 80% | 12% |
| Cache read | 417M | 2,072M |
| Cache write | 38M | 139M |
| Output per run | 163k | 323k |
| Cache read per run | 16M | 38M |
| API-rate equivalent | ~$1,130 | ~$2,050 |
| Per run | ~$43 | ~$38 |

The premise going in was that this week burned the cap faster on less output with Opus. The record says otherwise on every count: last week was Opus for the big steps and Sonnet for the small ones; this week produced twice the runs and four times the output tokens; and it cost about 1.8 times as much in cap terms. Per run, the switch to Sonnet bought almost nothing, $43 then against $38 now, because the cap counts cache reads and those went up five times.

Of 1,216 step attempts this week, 420 were redone work: 92 aborted (87 of them by worker restarts), 61 failed, 267 superseded by revise and catch-up rounds. That 35% is where the week went.

## Round 1

Claude's account, then Ivy's comments in `%%` blocks, verbatim.

### What went wrong

In rough order of cost.

- **The deploy loop.** Every merge to mill main redeploys the worker through the follower, immediately, with sessions mid-flight. mill merged 33 times; each restart aborted up to two attempts and each resume re-read the whole session context and rewrote the cache. Last week mill merged four times. mill#44, merged in the last hour, stops new claims from jumping ahead of the aborted runs; nothing yet drains a run before a redeploy or waits for idle.

%%
I thought it was great that we *got* a deploy loop running. I noticed far less trips against the auto mode classifier. Last week on the homelab, we saw significantly more hand-holding. I ran a Fable model to supervise and subscribe to events. I was pleased to find that it was unsatisfied with the breakage and took it upon itself to file new Dagger nodes to make fixes. We had the deploy loop set up so it didn't have to unblock Mill nearly as much, nor did it require any oversight or manual approval from me.

We had a lot more work going through Mill and at a concurrency of *two* parallel workers. I think as a result, the naive deploy approach introduced a lot of churn. Nodes sat on the backlog in unfinished states and I think we paid significantly more for cold cache tokens than we would have if they'd run uninterrupted.

Seems to me like a drain-deploy approach would be better. The goal is to never have an idle software factory.
%%

- **Every failure of the upstream was handled by burning retries.** Opus answered 529 for fifteen minutes on Monday night and 500 for a while after; the runner read each as malformed verdict output and re-asked at once, spending three retries in thirty seconds and then the revise rounds that come after them. mill#34 parked three times on one outage; steading#6 once. mill#57 fixed the 5xx case that night. A 429 from the subscription cap is a 4xx and would have walked the whole ready set into park gates at two runs a minute (mill#63, open); the ready nodes were held by hand instead.

%%
In addition to fixing Mill so it doesn't fail nodes unncessarily, we should probably implement some backoff here and be good Internet citizens.
%%

- **The sleep.** The laptop slept an hour past the lease TTL. mill#31's reclaim-on-fence saved one run and not the other, the reason for the failure was lost behind the step's `context canceled` (mill#55), and the run it saved recorded no usage afterwards because the gateway's recorder held the old lease (mill#58). Both fixed the same night. The pattern underneath: the error that mattered was swallowed at the moment it mattered, and the diagnosis needed a person with `psql`.

%%
Not a person, an agent. I barely touched anything but I *did* compact the supervising agent's context a couple times.

Before I forget: We should rethink the supervision approach. It's working well and I think we're almost at a point where we should start thinking about it as a feature and making this more efficient (reminder for myself: Jev as a way to monitor nodes and escalate to a supervisor agent).
%%

- **The merge gate merged with red checks.** steading#6's review approved, the gate merged the pull request into main, and only then read the forge's checks on the head it had just merged, found them failing, and sent the run back to implement on a branch whose pull request was closed. The gate merges first and asks after, and no repository has branch protection, so nothing stood between a session and main. mill#62, p0, merged as the last run before the cap and deployed itself through the same gate; the gate now reads the head's checks before it merges.

%%
Yay! New edge case! I've been unhappy that we aren't running any form of CI/CD. This was a good step in the right direction. I expect we'll have more to improve here.
%%

- **A design question baked into a node.** steading#6 said "pick one and record it" about how the suite obtains the dagger and mill binaries. The implementer correctly refused to wire an unattended fetch from private repositories with no credential story, but the only way it could say so was to fail: three implement attempts and four reviews before a person decided in five minutes (images on GHCR, pulled with the workflow's own token). The decision then went on the node as comments, and the comments never reached a session: mill's prompt (`internal/run/execute.go`, `prompt`) carries the node's body, the results of the nodes it depends on, the artifact brief and a failure's feedback, and not the node's comments. What the next attempt saw was the one-line result I gave the park gate when I completed it, because the gate blocked the node and so counted as an input. Detail below under "The plan as the contract".

%%
We might want to rethink Dagger and how it supplies context to agents. I've been feeling some pain points and I'm not sure if we've covered some/all of this:
- Context supplied has "inputs" but I don't know if comments are included.
- In this case, did the agent just misunderstand how it should answer? I thought we had a tool to allow agents to mark a node as needing a decision from a human/supervisor?
  - Reminder to myself: Write more about my thoughts about agent supervisors and team leads.
- I recall that the supervisor took the implementing agent's suggestion as fact and didn't really reason about the solution much. It presented me with a choice to mint a personal access token or make a GitHub App. I proposed that we might be able to do with just publishing images to GHCR. Might be worth considering that prompting to future supervisors / team leads should try to come up with ideas, understand underlying requirements, and evaluate a best fit then present me with a menu of options.
%%

- **Plans cited interfaces that do not exist.** A `dg create` flag, an output flag on mill, sidecar unit tests, a Vitest script. Review caught every one, at a round each.

%%
I suspect we only know of the hallucinations the reviewer *caught*. There's likely to be more. I'm suspecting we need to include documentation in our workflow:
- Diataxis or another documentation framework.
- Cleaner organization of work docs vs. end-user docs.
- A better approach to CLI design (I did some research on this earlier in the week -- Datadog's pup CLI approach to it seems like a model example).
%%

- **First-draft tests that do not bite.** Key-determinism tests restating the query library, a usage test that passes with the fix removed, a retry discount with no covering spec. The single most common review finding; review caught every one.

%%
Same here. We might not have caught everything. We won't know until we spot regressions:
- I wonder if we should think ahead here on ways to collect data and get answers about the efficacy / change-failure rate?
- I've been thinking we should have a step focused on writing effective test cases up front. I'm on the fence about TDD and I think a Gherkin-like approach is better (or at least, BDD-style tests).
- I wonder if code coverage is a good signal to agents? I wonder where we sit *now*.
- Digital twins seem like a real need. A lot of stuff just can't be tested without giving the agents credentials or testing in production.
%%

- **Cross-repository catch-up churn.** Four repositories landing into each other all night: every catch-up merge re-runs simplify, commit, clean-tree, pr and review. mill#42, mill#57, steading#7 and steading#6 each paid a full re-review to a neighbour's merge.

%%
The parallelism presents a real problem. It's good that we exercised it and found gaps in our merge process (or lack thereof), but nodes crossed over into the same parts of the codebase more frequently than I expected. Some ideas:
- Classification of nodes to the subsystems they touch. This could unlock smarter scheduling by running nodes that touch the same subsystem(s) one at a time.
  - Then again, epics sort of cluster around similar subsystems. For greenfield work, it might just be unavoidable. We'll have to look at this closer.
- Decompose nodes *more* (skill tweak or a project manager agent). In prior weeks, agents have gasped at how small the tasks were and proposed changes to the `/dagger:dagger` skill to scope them larger. I don't have data here but I strongly suspect it resulted in larger changesets, bigger conflicts, more cache read tokens, and slower loops.
- Perhaps a project manager agent could own the responsibilities of ensure nodes have clear decisions made (or block on human nodes), are classified by subsystem(s), and are of reasonable size/scope?
%%

- **The watcher's own bill.** The session that watched all this spent $204 and 480M cache reads to generate 0.73M tokens, mostly on wake-ups that said one line. The ratio, 650 to 1, is worse than mill's.

%%
Yeah, it was terrible, but it helped us bootstrap the system and get it running smoothly. Worth it, in my opinion. I think we could go another week or two like this.
%%

### Gaps that remain

- **A "needs a decision" outcome.** A step should be able to ask a question and wait, distinct from failing. Today the closest thing is exhausting retries and parking, which costs the rounds first and asks second.

%%
Ah, there's my answer. We *don't* have a way to let agents flag a node as needing clarification. I recall punting this because I was worried agents would use it as an escape hatch constantly and leave me with everything parked on what color a button should be.
%%

- **Deploy safety.** Drain in-flight runs before a redeploy, or redeploy only when idle; that is stead's follower. Branch protection with required checks on all four repositories, so the gate is the second line and not the only one.

%%
Yes to draining and branch protection. Also:
- What happens when a main branch is broken? Who will spot and fix it? We probably want to auto-create Dagger nodes.
  - The supervisor mentioned a merge queue? I've thought they were kind of slow, but maybe there's something here. Another role that comes to mind: SRE/QA. I don't think an agent running in Mill will be able to inspect their work in production. Maybe they could hand off details to a thicket agent that's able to inspect and retrigger the Mill agents or file a new Dagger node?
    - There's other considerations here to avoid race conditions. Note to self: level up my distributed systems expertise.

Unrelated:
- We need to stay on top of Dependabot and other security/maintenance alerts. These should just create Dagger nodes, honestly.
  - Same for application errors.
%%

- **A worker-level breaker and cap awareness.** Stop claiming while the upstream refuses; honour a reset time; report "waiting on the upstream since" in `mill status` (mill#63).

%%
Agreed.
%%

- **Merge pacing.** A merge queue, or concurrency one per project, would have saved four review rounds this week.

%%
I want to hear more about this.
%%

- **The plan as the contract.** A comment after approval should reopen the plan, or a resume should offer re-plan. Otherwise a person and the session talk past each other. The case, from steading#6's run `01M3342MQ83SBHJBT51SFGM2FM` and dagger's event log:
  - 04:50:56Z: the first comment, "How the suite obtains `dg` and `mill` — decided", naming `ghcr.io/steading-ai/<product>:<sha>` and a versions file with both the commit and the version string. 04:5xZ: park gate steading#22 completed with a one-line result summarising it.
  - 05:23:37Z: implement attempt 4 starts. Its prompt (step log, line 126) carries the gate's one-line result as an input; the comment's body is not in it. The attempt builds the structure the result names — versions file, guard, `make services`, a GHCR step in CI — and commits `66421d7` at 05:36Z pulling `ghcr.io/ivy/<product>:<version-string>`: wrong owner, wrong tag.
  - 05:36:50Z: review attempt 6 blocks on the `+` in the tag and notes the `ghcr.io/ivy` images are 403, without naming the owner as wrong.
  - 05:40:25Z: implement attempt 5 starts, and at 05:41:21Z ends, having swapped `+` for `-` in the tag (`f7b45d9`). At 05:41:29Z, eight seconds after it ended, the second comment lands naming the owner and the SHA tag again.
  - 05:44Z–05:54Z: review 7 approves, the merge gate catches up (`77e8d40`, attempt 6), review 9 approves, and the gate merges with red checks (mill#62). Implement attempt 7, 05:54:47Z, sees only the failed-checks feedback, fixes the owner from `buf.gen.yaml`'s own pins and keeps the `-` tag (`8deb799`, on a branch already merged).
  - So no attempt ever read either comment. Whether a session should read comments is a workflow question; that they are not in the prompt at all is the fact.

%%
I want to take a closer look at this case. I need a reference/citation here to the node(s) and run(s) where this happened.
%%

- **Proof the implementer can run before shipping.** "Run `--help` before citing a flag" and "remove the change and watch the test fail" are both checkable inside the implement step; today review is where they get checked.

%%
Is this a prompt/skill improvement? I could definitely see an agent defaulting to "by the way, you should run XYZ," instead of doing it on its own. Another thing that's costing us (I've seen it with agents I drive personally, but haven't looked through runs here to confirm it's in Mill):
- We should probably run the test suite ahead of time. I wonder if we can feasibly run the same GitHub Actions workflow in the sandbox as a pre-step?
  - We'd feed in details about the test suite so the agent is aware and doesn't spend cycles on, "I think that test failed *before* I started my work [stashes changes, runs test suite to find that the agent *did* in fact break something]".
  - I might be over-engineering. Maybe we just feed in status checks from the HEAD it branched off of at startup? Similar to how I recall us handling mise tool installs.
%%

- **Cost visibility.** No USD in the ledger for these models; no redone-work number beside the funnel; the journal buries the worker's own lines under one line per gateway request; the gateway logs nothing for a non-2xx upstream response beyond "no usage on the response".

%%
Oof, I think I have some notes from when I was brainstorming a redesign of this. On the subject of redesigns:
- Projects have no priority, even though I was wanting the web UI to take priority. That's me reaching for a solution before defining my problem. It could be that there are better ways to handle scheduling.
- I've been wondering about cross-repo nodes and epics. I had a thought earlier that maybe an epic or some other node group should live *outside* of a project. In this case, my reasoning about projects is starting to map projects and repos one-to-one, but agents don't always behave this way. In this case, I was trying to:
  - Make a web UI Steading which needed:
    - Improvements to Mill's run tracking / observability / event streaming
    - Improvements to Dagger along the same lines.
    - Frontend development to convert the clickable prototype into a working product.
    - Deployment into the Stead VM.
    - Infrastructure (mainly GitHub repo setup, tokens, GitHub Actions workflow setup, GHCR permissions).
      - Note to self: Set up Terraform and let an agent drive more of this with some additional human oversight.
%%

- **Sandbox posture.** Sessions in the guest run with passwordless sudo and the forge credential is one hop away. Fine for dogfooding; not the flat provision every other repository is meant to get.

%%
Yeah... I was under the understanding that we sandboxed this in a system container and put all of this in a VM. Not as worrisome until we got into giving it more credentials to reach GitHub. I shrugged my shoulders a bit out of impatience to get this running. We'll need to assess and define a threat model soon.
%%

- **The human surface.** Everything a person must answer is a node body reached through the `dg` CLI, and knowing which ones needed a person meant an agent watching all night. That is what steading is for, and this week is the argument for it.

%%
100%. I don't think I scoped push notifications into it yet, but I found myself wishing a thicket agent would ping me on Slack or that I'd get a push notification from Steading to jump in and answer a question. I also found myself wanting summaries throughout the day and a way to dig into specific nodes and quickly understand them. Instead, I did stuff manually or asked the supervisor. It wasn't efficient.
%%

### What I would do differently

- **Hold the gate through an outage.** I completed one park gate into a live outage and burned three attempts. The second time I waited for a real request to succeed first; that should have been the rule from the first.
- **Put the decision where the prompt reads.** On steading#6 I left the decision as comments under the node, and no session ever saw them. The prompt carries the node's body and the results of what blocks it; I should have edited the body, or written the whole contract into the gate's result rather than a one-line summary of it.

%%
This is going to trip up a lot of agents. I think we have history for node edits? Otherwise, I suspect that changing `dg` to share better hints and output comments would make this better.
%%

- **Watch in batches.** Per-event wake-ups over a 34-hour session cost more than the reporting was worth. A half-hour digest, with a push only for a gate, would have carried the same information for a tenth of the reads.

%%
I'm not sure. I think maybe we could look at [Jev](https://docs.typesafe.ai/introduction.md) and see if we can make a smarter monitoring tool built into dagger/mill that wakes up the supervisor when something noteworthy happens in the system.
%%

- **File the follow-ups as the run ends, not after.** mill#62 and steading#23 were filed inside minutes of the bad merge and that was right; mill#63 could have been filed the moment mill#57's plan said "a 4xx takes the ordinary path".

### Nodes this week filed from the record

mill#52 (done), mill#53, mill#54, mill#55 (done), mill#57 (done), mill#58 (done), mill#61 (done), mill#62 (done), mill#63; stead#6 (done), stead#7 (done); dagger#2 (done); steading#23. Gates answered: mill#48–51, mill#56, mill#59, mill#60, steading#21, steading#22; steading#24 dropped.

%%
My closing thoughts:
- We blew through my Claude Max 20x subscription in less than 48 hours. It's tempting to get another subscription or look at model routing but it's also nice to have some downtime to think, plan, and prioritize.
  - I might not have spent my tokens wisely.
    - For example: Do I even need a web UI or do I need to invest in making the CLI/MCP more effective?
      - Giving hints in tool outputs (contextual onboarding / JIT learning for agents), adopting a CLI schema (similar to pup CLI), and investing into the skills might make this system easier to use.
      - I considered investing into a TUI instead but I think a web UI is more marketable as an open source project. Also, an interface that works effectively on mobile is valuable. At any rate, the service architecture seems to be holding up and anything we build on the backend for desktop will work just as well elsewhere.
- I need to spend some time assessing the architecture and code quality and addressing it before it turns into a slop factory.
  - On a localized scale:
    - golangci-lint was helpful in the past for:
      - Enforcing use of Ginkgo/Gomega
      - Blocking dependencies on sqlc and instead going through proper APIs/packages
        - Mandating architecture layers and blocking dependencies outside of those areas
    - Code coverage reporting and possibly enforcement
  - If tech debt accrues faster than I pay it off, this is going to slow down a lot. We're in a greenfield/feature-development/proving phase and there's likely lots that needs to be rethought or tossed.
- I don't think we've done anything about cross-repo research.
- Knowledge retrieval is also on the backburner. [kb](https://github.com/ivy/kb) is a prototype that still has an architecture worth keeping. It might make sense to revisit it in a few more weeks.
- Dagger doesn't really have a way to "stage" work. The moment it's in the system, Mill picks it up. In hindsight, I think this makes me hesitant to add to it frequently. I think for the system to realize its full potential, it needs to track *my* tasks too. Another reason for a web UI. That means some thought needs to be put into how I navigate the backlog and view the tasks for *me*.
- At some point, I'd really like to resume an agent from a dagger node. Basically, having a way to reference which sessions ran on a node and having a way to pick them back up or fork them to ask questions. I think it'd be helpful for both agents and I to have a way to interrogate a transcript against a running agent rather than deciphering it ourselves. I'm not sure though. It could just be theater and lead the agent to hallucinate a rationale for its decisions.
  - The knowledge retrieval system might fit in here. I started it with Markdown in mind, but making it agnostic to data sources could prove valuable.
%%

## Round 2

Claude's follow-ups to Round 1, then a proposal for the next iteration.

### Corrections after reading the comments

- **Comments never reach a session.** The "half-followed" claim in Round 1 was wrong, and the "plan as the contract" bullet above now carries the timeline. mill's `prompt` (`internal/run/execute.go`) passes a step's session the node's body, the results of the nodes that block it, the artifact brief and a failure's feedback; not the node's comments. Implement attempt 4 saw the one-line result of the completed park gate, because the gate blocked the node and so arrived as an input, and built the structure that line named with guessed values. Attempt 5 ended eight seconds before the second comment landed. Attempt 7 saw only the failed-checks feedback. Nothing read either comment. The lever is in the prompt, or in a skill that runs `dg node show` (which does return comments), not in the sessions.
- **There is no decision tool.** `hold` is a user's verb; a session has comment, complete and release. The implementer on steading#6 did the only honest thing available, which was to fail with a full explanation.
- **The supervisor took the implementer's frame.** Round 1's menu was "how does CI get a credential" because that was the question the session handed over. The requirement in its own words was "a workflow on repository A reads a build of private repository B", and GitHub already answers that with a package's Actions access. A supervisor should state the requirement before pricing anything.

### Answers to the open questions

- **Merge pacing.** The cost was not the merges, it was the re-reviews: every catch-up re-ran simplify through review because the branch moved. Three options, cheapest first: (1) catch-up re-runs checks and not reviews when the overlap is lockfiles and generated files only, which was half of this week's catch-ups; (2) one run per project in the merge phase, a lock the merge gate takes so branches land one at a time and the second catches up once against a stable main; (3) GitHub's merge queue, which is (1) done on the forge side. Subsystem classification and smaller nodes attack the same thing from the front, and smaller nodes have this week's data behind them.
- **Broken main.** A node filed by mill's sweep when main's checks go red, since the sweep already reconciles against the forge. The same hook fits Dependabot and application errors.
- **Resuming a session from a node.** The ledger already records the session id per attempt (`session_started`), and the activity files hold the transcript, so the reference exists. Resuming the actor to ask why is the theatre risk named in Round 1: a resumed session will produce a rationale whether or not one existed. The cheaper and more honest form is a fresh agent given the transcript as data, asked what the record shows, which is what the supervisor did by hand all week with `mill logs` and the activity files. That is a retrieval problem, and a data-source-agnostic `kb` fits it exactly.
- **Supervision as a feature.** What the supervisor did was a polling monitor plus judgment, at $204 for the week. What it wants is dagger and mill emitting the noteworthy events (park, fence, red main, outage, cap) to something that wakes a supervisor with the context attached, so the judgment is bought without the polling. Jev is the right shape for the wake-up; the summaries and the escalation are the same job made cheap.

### Everything mentioned, bucketed

1. **Flow and scheduling.** Drain in-flight runs before a redeploy. Catch-up without re-review; a per-project merge lock; a merge queue. Subsystem classification for scheduling. Smaller nodes, or a project-manager agent that scopes and classifies. Project priority, or a better statement of the scheduling problem. Epics that live outside a project for cross-repository work. A way to stage work in dagger so it is not claimed the moment it exists, and for dagger to hold a person's tasks too.
2. **Resilience.** A worker-level breaker and cap awareness with backoff (mill#63). A node filed when main goes red, and for Dependabot and application errors. The sleep fixes (done). Race conditions once more than one agent can act on production.
3. **Quality.** A decision-request outcome with a per-run budget. Comments or a re-plan in the prompt path. A documentation framework (Diataxis) and a CLI schema (pup) so plans cite real interfaces. A tests-first or BDD step; coverage as a signal; change-failure-rate data; digital twins for what cannot be tested without credentials. The test baseline at branch time (checks from the HEAD branched from) so sessions do not re-diagnose pre-existing failures. golangci-lint for architecture layers; a code-quality pass before debt outruns the work.
4. **Observability and cost.** USD in the ledger for these models; a redone-work number beside the funnel; the journal's gateway noise; the gateway's silence on non-2xx; the supervisor's own bill.
5. **Supervision and the human surface.** Supervision as a feature on events (Jev). Push notifications and daily summaries. Drill-down into a node and its runs. Resume or fork a session from a node, or interrogate its transcript. Knowledge retrieval (kb) made source-agnostic. Hints in `dg` tool output and node edit history. Whether the CLI and MCP, rather than a web UI, are the better investment.
6. **Security and infrastructure.** A threat model for the guest now that it holds credentials. Branch protection with required checks. Terraform for GitHub repositories, tokens, workflows and package permissions. CI/CD as a discipline rather than one workflow.

### The 20% for the next iteration

Judged by what the record says cost the most: 35% of attempts were redone work (87 restart aborts, 267 superseded rows), one node lost four hours to a question nobody could ask, and one failure mode would have parked the entire ready set. Five items, in order, each small enough for a node or two:

1. **Drain before deploy** (bucket 1; stead's follower). Stop claiming, let in-flight runs finish or reach a step boundary, then restart. Removes the 87 aborts and the resumes behind them, which is the single largest share of this week's cache reads. One `mill drain` verb and one wait in the follower.
2. **Catch-up without re-review, and one merge at a time per project** (bucket 1; mill's merge gate). Removes most of the 267 superseded rows. The check-only path already exists for the tree (mill#52); the lock is a few lines in the gate.
3. **A decision request that parks the run with the question and the session's own best answer, budgeted at one per run, and the answer written into the prompt path** (bucket 3; mill and dagger). Turns a four-round loop into one gate and closes the comments gap at the same time, since the answer arrives as an input.
4. **The worker-level breaker for 429 and 5xx, honouring the reset, plus a red-main node from the sweep** (bucket 2; mill#63 and one more). Cheap, and it protects everything else from the two failure modes that take the whole factory down.
5. **Redone-work and USD in `mill cost`** (bucket 4). So next week's retrospective can say whether 1 through 4 worked, in the same units as this one.

Deferred on purpose: the documentation framework, coverage, twins, the web UI's push and summaries, subsystem classification, Terraform, session resume, kb. Each is real; none moves this week's numbers as far as the five above, and several depend on them (supervision on events needs the events; classification needs smaller nodes first).
