package run_test

import (
	"context"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("Gates", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)
	})

	// asks is quick-fix with a merge gate no policy closes, which is every
	// gate a human is meant to see.
	asks := func(predicate string) string {
		return f.variant(ctx, "asked-fix", func(definition *millv1.Workflow) {
			stepOf(definition, "merge").GetGate().Auto = predicate
		})
	}

	Describe("A predicate that holds", func() {
		It("records an auto-approval in the ledger and files nothing in dagger", func() {
			Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

			var merge *millv1.StepRun
			for _, sr := range stepRuns(f.inspect(ctx), "merge") {
				merge = sr
			}
			Expect(merge.GetAutoApproved()).To(BeTrue(), "the ledger records the auto-approval")
			Expect(f.gatesOn(ctx)).To(BeEmpty(), "nothing was filed in dagger")
		})
	})

	Describe("A predicate that does not hold", func() {
		It("files exactly one gate blocking the node, records it, and releases the lease", func() {
			workflow := asks("verdict.decision == approve")
			client, watcher := f.watching()

			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.Dagger = client
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			gate := f.theGate(ctx)
			Expect(gate.GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_USER))
			Expect(gate.GetStatus()).To(Equal(skiftv1.Status_STATUS_OPEN))
			Expect(f.blockedBy(ctx)).To(ContainElement(gate.GetId()), "the gate blocks the node it was filed for")

			run := f.runOf(ctx)
			Expect(run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
			Expect(run.GetGateNodeId()).To(Equal(gate.GetId()))
			Expect(run.GetLeaseId()).To(BeEmpty())
			Expect(watcher.reasons()).To(ContainElement(skiftv1.ReleaseReason_RELEASE_REASON_GATE))
			Expect(f.leaseOf(ctx)).To(BeNil())
		})

		It("A gate is filed once", func() {
			// Given the plan-approval predicate fails: nothing in this run
			// has produced a plan, so the path is undefined and undefined
			// equals nothing.
			workflow := asks("plan.approved == true")
			store := &failingStore{StoreServiceClient: f.store}

			// When the process dies after ApplyPatch and before the store
			// write. The gate is in dagger; the ledger knows nothing of it.
			err := f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.Store = store
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			Expect(store.calls.Load()).To(Equal(int64(1)))
			filed := f.theGate(ctx)
			Expect(f.runOf(ctx).GetGateNodeId()).To(BeEmpty(), "the ledger missed the write")

			// And run is invoked again. A node an open gate blocks is not
			// claimable, so the rerun is the one that follows the human's
			// answer — which is exactly when a second gate would be filed.
			f.approveGate(ctx, filed.GetReference())
			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

			// Then the runner finds the gate by searching the node's
			// user-assigned children, records it, and files no second gate.
			Expect(f.gatesOn(ctx)).To(HaveLen(1))
			Expect(f.gatesOn(ctx)[0].GetId()).To(Equal(filed.GetId()))
			Expect(f.runOf(ctx).GetGateNodeId()).To(Equal(filed.GetId()))
		})
	})

	Describe("Resuming through a gate", func() {
		It("closes the gate step with the gate's result and continues, in one step run", func() {
			workflow := asks("verdict.decision == approve")
			runner := f.runner(func(o *run.Options) { o.Workflow = workflow })
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())
			gate := f.theGate(ctx)

			f.approveGate(ctx, gate.GetReference())
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())

			node, err := f.client.Node(ctx, f.nodeRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetStatus()).To(Equal(skiftv1.Status_STATUS_DONE))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))

			detail := f.inspect(ctx)
			Expect(f.gatesOn(ctx)).To(HaveLen(1), "the resume read the gate; it did not file another")
			Expect(stepRuns(detail, "merge")).To(HaveLen(1), "waiting for a human is not a second attempt")
			Expect(closedSteps(detail)).To(HaveKeyWithValue("merge", millv1.EvidenceKind_EVIDENCE_KIND_GATE))
			Expect(evidenceOf(detail, "merge").GetGate()).To(Equal(gate.GetId()))
		})

		It("fails the run with the step's deny_result when the gate is denied", func() {
			// A denial is a gate a person disposed of, and dagger's rule is
			// that dropping a gate never satisfies the edge out of it: the
			// node is never ready again, so the run that records the denial
			// has to be the one still holding the lease. That is single-run
			// mode, where the ask goes to the terminal.
			workflow := asks("verdict.decision == approve")
			person := &answerer{approve: false}

			Expect(f.runner(withAnswers(person), func(o *run.Options) {
				o.Workflow = workflow
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(person.answered()).To(HaveLen(1))
			Expect(person.answered()[0].Title).To(Equal("Merge " + f.nodeRef + "?"))
			Expect(f.theGate(ctx).GetStatus()).To(Equal(skiftv1.Status_STATUS_DROPPED))

			result, err := f.result(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.GetBody()).To(Equal(
				"Merge denied by a human; the branch and its pull request are left open."))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		})

		It("closes the step on a gate the person at the terminal approved", func() {
			workflow := asks("verdict.decision == approve")
			person := &answerer{approve: true}

			Expect(f.runner(withAnswers(person), func(o *run.Options) {
				o.Workflow = workflow
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(person.answered()).To(HaveLen(1))
			gate := f.theGate(ctx)
			Expect(gate.GetStatus()).To(Equal(skiftv1.Status_STATUS_DONE))

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "merge")).To(HaveLen(1))
			Expect(evidenceOf(detail, "merge").GetGate()).To(Equal(gate.GetId()))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		})

		// mill#30: resumeGate reopens the attempt it is about to close or
		// fail before either of those has a chance to run, so a failure in
		// between is this branch's own leak point, not one the rest of the
		// loop already covers.
		It("aborts the reopened attempt when the gate's evidence does not verify", func() {
			workflow := asks("verdict.decision == approve")
			logDir := GinkgoT().TempDir()
			runner := f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
			})
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())
			gate := f.theGate(ctx)

			// A gate done with nothing attached is not evidence `gate` can
			// verify (internal/evidence.verifyGate), the way a person who
			// completed it with no comment at the terminal would leave it.
			f.approveGateEmpty(ctx, gate.GetReference())
			Expect(runner.Execute(ctx, f.nodeRef)).To(HaveOccurred())
			f.assertWriterClosed(ctx, logDir, "merge")
		})

		It("aborts the reopened attempt when denyGate's own Complete fails", func() {
			workflow := asks("verdict.decision == approve")
			logDir := GinkgoT().TempDir()
			person := &answerer{approve: false}
			client := f.failingPatches(func(op *skiftv1.PatchOp) bool { return op.GetCompleteNode() != nil })

			err := f.runner(withAnswers(person), func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
				o.Dagger = client
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			f.assertWriterClosed(ctx, logDir, "merge")
		})
	})

	Describe("The gate's activity record", func() {
		var logDir string

		BeforeEach(func() {
			logDir = GinkgoT().TempDir()
		})

		It("files a gate with no attempt_ended, naming the filed node's reference", func() {
			workflow := asks("verdict.decision == approve")
			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			gate := f.theGate(ctx)
			sr := stepRuns(f.inspect(ctx), "merge")[0]
			activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "merge", sr.GetAttempt())
			Expect(err).NotTo(HaveOccurred())
			Expect(activities).NotTo(BeEmpty())
			Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(), "seq 1 opens the attempt")

			last := activities[len(activities)-1]
			filed := last.GetGateFiled()
			Expect(filed).NotTo(BeNil(), "the last record is the filing, not an end")
			Expect(filed.GetNodeRef()).To(Equal(gate.GetReference()))
			for _, a := range activities {
				Expect(a.GetAttemptEnded()).To(BeNil(), "filing leaves the attempt open")
			}
		})

		It("keeps one contiguous seq across a gate filed by one worker and resumed by another", func() {
			workflow := asks("verdict.decision == approve")
			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			gate := f.theGate(ctx)
			f.approveGate(ctx, gate.GetReference())

			// A second Runner instance stands in for a different worker
			// resuming the gate: reopenActivityWriter, not the writer the
			// first Runner held, is what recovers seq here.
			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			sr := stepRuns(f.inspect(ctx), "merge")[0]
			activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "merge", sr.GetAttempt())
			Expect(err).NotTo(HaveOccurred())
			Expect(activities).NotTo(BeEmpty())
			Expect(activities[0].GetAttemptStarted()).NotTo(BeNil())
			Expect(activities[len(activities)-1].GetAttemptEnded()).NotTo(BeNil())

			for i, a := range activities {
				Expect(a.GetSeq()).To(Equal(int64(i+1)), "seq is contiguous across the filing and the resume")
			}
		})

		It(`ends a denied gate's attempt with reason "gate denied"`, func() {
			workflow := asks("verdict.decision == approve")
			person := &answerer{approve: false}
			Expect(f.runner(withAnswers(person), func(o *run.Options) {
				o.Workflow = workflow
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			sr := stepRuns(f.inspect(ctx), "merge")[0]
			activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "merge", sr.GetAttempt())
			Expect(err).NotTo(HaveOccurred())
			Expect(activities).NotTo(BeEmpty())
			ended := activities[len(activities)-1].GetAttemptEnded()
			Expect(ended).NotTo(BeNil())
			Expect(ended.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
			Expect(ended.GetReason()).To(Equal("gate denied"))
		})
	})

	Describe("A predicate over the run's tier", func() {
		It("reads the tier the project default gave the run", func() {
			// The project default is what names the tier until an
			// assessment step exists to name one, and the run carries it
			// from selection onwards.
			workflow := asks("tier <= small")

			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.Tier = millv1.Tier_TIER_FEATURE
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_FEATURE))
			Expect(f.gatesOn(ctx)).To(HaveLen(1), "a feature is more than the policy allows")

			// The same predicate on a run the project default called a
			// quick fix closes without a human.
			other := f.node(ctx, "another line", "Change the README's second line.")
			Expect(f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.Tier = millv1.Tier_TIER_QUICK_FIX
			}).Execute(ctx, other)).To(Succeed())

			Expect(f.gatesOnNode(ctx, other)).To(BeEmpty())
			Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_QUICK_FIX))
		})
	})

	// mill#33: a branch whose base moved while the run was in flight is
	// caught up with the target before merging, rather than asking the
	// forge to merge a branch it will only refuse.
	Describe("Catching up with a moved target before merging", func() {
		// duringImplement wraps the implement turn's commit with a second
		// commit landed directly on the target, so the branch and the
		// target have each moved by the time `merge` runs its catch-up.
		duringImplement := func(name, content string) {
			scripts := f.quickFixScripts()
			scripts[1].Do = func(session harness.Session) {
				f.commits(session)
				f.seedTarget(name, content)
			}
			f.script(scripts...)
		}

		It("merges after catching up on a target that moved with no overlap", func() {
			duringImplement("from-elsewhere.txt", "another run's change\n")

			Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.mergeCalls()).To(Equal(1), "the forge is asked to merge once, after catching up locally")
			Expect(stepRuns(f.inspect(ctx), "merge")).To(HaveLen(1), "nothing here was a finding")
			// mill#62: the head that was actually merged was checked
			// immediately before, and never again once merged.
			Expect(f.forge.checksCalls()).To(Equal(1), "the merged head's checks were read once, before the merge")
		})

		It("a catch-up conflict is a finding for implement, not a merge attempt, and the run recovers", func() {
			const overlapFile = "session-2.txt"
			// The run's own implement turn writes this same path with this
			// same content (fixture.commits), so the overlap is on the
			// filename alone: git could auto-merge it, and mill#33's
			// acceptance criteria asks for that to still be a finding.
			duringImplement(overlapFile, "what the session changed\n")

			Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

			By("the merge gate's first attempt found the overlap and revised back to implement")
			detail := f.inspect(ctx)
			Expect(after(detail, "merge")).To(Equal("implement"))
			Expect(stepRuns(detail, "implement")).To(HaveLen(2), "the conflict cost implement an attempt")
			Expect(stepRuns(detail, "merge")).To(HaveLen(2), "merge ran again once caught up")
			Expect(stepRuns(detail, "merge")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))

			By("the finding names the file and never reached the forge")
			calls := f.harness.Calls()
			Expect(calls).To(HaveLen(7), "gather-context, implement x2, commit x2, pr x2")
			revised := calls[4].Prompt.Text
			Expect(revised).To(ContainSubstring("## What the last attempt failed on"))
			Expect(revised).To(ContainSubstring(overlapFile))
			Expect(revised).To(ContainSubstring("Catching up with the target branch"))
			Expect(revised).To(ContainSubstring("Do not run `git merge` or commit yourself"))

			By("and the run went on to merge without a human, spending exactly one forge merge")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.mergeCalls()).To(Equal(1), "never asked while the conflict stood, asked once it was resolved")
		})

		// mill#52: mill#2 and mill#26 each added a migration file with no
		// filename in common, so catch-up saw no overlap and merged clean —
		// and the merged tree still would not start. Nothing ran the
		// workflow's own checks against what catch-up actually produced
		// before the forge was asked to merge it.
		It("a clean catch-up merge whose tree fails a check is a finding, never a merge attempt", func() {
			const marker = "from-elsewhere.txt"
			workflow := f.variant(ctx, "checked-fix", func(definition *millv1.Workflow) {
				stepOf(definition, "clean-tree").GetCheck().Command = []string{
					"sh", "-c",
					`if grep -q leaked-secret ` + marker + ` 2>/dev/null; then ` +
						`echo "` + marker + ` carries a leaked secret" >&2; exit 1; fi`,
				}
			})
			duringImplement(marker, "leaked-secret\n")

			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			By("the merge gate's catch-up merged with no overlap, but the caught-up tree failed clean-tree")
			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "merge")).To(HaveLen(1), "the finding is the merge attempt's own, not a second one")
			Expect(stepRuns(detail, "merge")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
			Expect(after(detail, "merge")).To(Equal("implement"))

			By("the forge was never asked, and the finding names the check and what it found")
			Expect(f.forge.mergeCalls()).To(Equal(0), "the check failed before the forge was ever asked to merge")
			calls := f.harness.Calls()
			Expect(len(calls)).To(BeNumerically(">=", 5), "gather-context, implement, commit, pr, then implement again")
			revised := calls[4].Prompt.Text
			Expect(revised).To(ContainSubstring("Catching up with the target branch merged cleanly"))
			Expect(revised).To(ContainSubstring("clean-tree failed against the merged tree"))
			Expect(revised).To(ContainSubstring(marker + " carries a leaked secret"))
			Expect(revised).To(ContainSubstring("The merge is already staged on the branch"))
		})
	})

	// mill#42: a push that just landed can leave the forge still computing
	// mergeability, or racing a second target move the catch-up run before
	// it did not see. Both clear on their own — the first with time, the
	// second with another catch-up — so the merge gate absorbs them inside
	// the same attempt rather than spending one of the step's own retries.
	Describe("The merge gate absorbs the forge's own transients", func() {
		It("absorbs a pending mergeability refusal and merges once it clears", func() {
			f.forge.refuseQueue = []error{
				&forge.MergeRefusedError{PR: 1, Status: http.StatusMethodNotAllowed, Pending: true},
			}

			Expect(f.runner(func(o *run.Options) { o.MergeRetryWait = 10 * time.Millisecond }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.mergeCalls()).To(Equal(2), "the refusal was absorbed in place, not charged")
			Expect(stepRuns(f.inspect(ctx), "merge")).To(HaveLen(1), "one step attempt, not two")
		})

		It("absorbs a conflict refusal by catching up again and merges once it clears", func() {
			f.forge.refuseQueue = []error{
				&forge.MergeRefusedError{PR: 1, Status: http.StatusMethodNotAllowed, Conflict: true},
			}

			Expect(f.runner(func(o *run.Options) { o.MergeRetryWait = 10 * time.Millisecond }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.mergeCalls()).To(Equal(2), "the refusal was absorbed by a fresh catch-up, not charged")
			Expect(stepRuns(f.inspect(ctx), "merge")).To(HaveLen(1), "one step attempt, not two")
		})

		It("gives up after the retry budget and surfaces the refusal as an ordinary step failure", func() {
			// mergeRetryBudget (gate.go) is 5: enough pending refusals to
			// spend the whole in-attempt budget without ever clearing.
			for i := 0; i < 5; i++ {
				f.forge.refuseQueue = append(f.forge.refuseQueue,
					&forge.MergeRefusedError{PR: 1, Status: http.StatusMethodNotAllowed, Pending: true})
			}

			Expect(f.runner(func(o *run.Options) { o.MergeRetryWait = 10 * time.Millisecond }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "merge")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED),
				"the budget bounds the in-place retries; a durably stuck merge still surfaces")
			// The step's own retry, unrelated to mill#42, picks the run back
			// up; the queue is spent by then, so the second attempt succeeds.
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.mergeCalls()).To(Equal(6), "five spent on the exhausted budget, one more on the step's own retry")
		})

		It("does not retry in place a refusal that names no transience of its own", func() {
			workflow := f.variant(ctx, "merge-no-retry", func(definition *millv1.Workflow) {
				merge := stepOf(definition, "merge")
				merge.Retries = 0
				merge.OnFailure = nil
			})
			f.forge.refuseQueue = []error{
				&forge.MergeRefusedError{PR: 1, Status: http.StatusMethodNotAllowed, Detail: "blocked"},
			}

			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
			Expect(f.forge.mergeCalls()).To(Equal(1), "a refusal naming no transience of its own is not retried in place")
			Expect(stepRuns(f.inspect(ctx), "merge")).To(HaveLen(1))
		})
	})

	// mill#62: the merge gate used to ask the forge to merge before it ever
	// read the forge's checks on the head, so a red head still merged and the
	// checks were only read afterwards — too late to stop it, and liable to
	// send a revise at a branch whose pull request was already closed.
	Describe("The merge gate reads the forge's checks before it merges", func() {
		It("does not merge a head whose checks the forge reports as failed, and the run revises from implement", func() {
			f.forge.checksAt = forge.Failed

			Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.forge.mergeCalls()).To(Equal(0), "the checks were read before anything was merged")
			detail := f.inspect(ctx)
			Expect(after(detail, "merge")).To(Equal("implement"))
			// A revise supersedes the attempt it revised from (docs/ledger.md
			// §step_runs); checksAt never clears, so the second attempt fails
			// the same way and, past the step's one retry, parks rather than
			// revising again.
			merges := stepRuns(detail, "merge")
			Expect(merges[len(merges)-1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

			// gather-context, implement, commit, pr, then implement again:
			// the revise this finding sent the work back to.
			calls := f.harness.Calls()
			revised := calls[4].Prompt.Text
			Expect(revised).To(ContainSubstring("the forge reports the checks on"))
			Expect(revised).To(ContainSubstring("as failed"))
			Expect(revised).To(ContainSubstring("Nothing was merged"))
		})

		It("waits on a head whose checks are still pending, bounded by the workflow's own timeout", func() {
			workflow := f.variant(ctx, "short-status-timeout", func(definition *millv1.Workflow) {
				definition.Status = &millv1.StatusPolicy{
					Interval: durationpb.New(10 * time.Millisecond),
					Timeout:  durationpb.New(30 * time.Millisecond),
				}
			})
			f.forge.checksAt = forge.Pending

			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.forge.mergeCalls()).To(Equal(0), "a head still pending is never merged")
			detail := f.inspect(ctx)
			// The bounded wait aborts and reopens the same step, so more than
			// one "merge" attempt precedes the judgment failure that revises;
			// implement running a second time is what proves on_failure fired
			// once the workflow's own timeout elapsed.
			Expect(stepRuns(detail, "implement")).To(HaveLen(2))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED),
				"checks that never settle keep failing the step past its one retry")
		})

		It("re-checks the head a conflict's own catch-up produces before merging it", func() {
			// The first check passes, so the first merge attempt is tried and
			// a conflict refuses it; the second check — read again after the
			// conflict's own catch-up — fails, and every check thereafter
			// keeps failing, so nothing is ever merged.
			f.forge.checksQueue = []forge.Status{forge.Success}
			f.forge.checksAt = forge.Failed
			f.forge.refuseQueue = []error{
				&forge.MergeRefusedError{PR: 1, Status: http.StatusMethodNotAllowed, Conflict: true},
			}

			Expect(f.runner(func(o *run.Options) { o.MergeRetryWait = 10 * time.Millisecond }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			merges := stepRuns(detail, "merge")
			Expect(merges[len(merges)-1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
			Expect(after(detail, "merge")).To(Equal("implement"))
			// One merge call ever: the attempt a conflict refused. The
			// re-check after its own catch-up failed, so no second merge was
			// ever tried; the step's own retry then fails the same way before
			// even asking the forge to merge, one more check spent on it.
			Expect(f.forge.mergeCalls()).To(Equal(1))
			Expect(f.forge.checksCalls()).To(Equal(3), "before the first attempt, again after its own catch-up, and again on the step's own retry")
		})

		It("closes on a pull request already merged without asking about its checks, whatever they say now", func() {
			f.forge.pr = forge.PullRequest{
				Number: 1, URL: "https://forge.test/mill/merge_requests/1",
				Target: "main", HeadSHA: strings.Repeat("a", 40),
				State: "merged", MergeCommit: strings.Repeat("b", 40),
			}
			f.forge.merged = true
			// A red verdict now must not undo a merge that already happened
			// (AC2): if this were read at all, the run would revise instead
			// of finishing.
			f.forge.checksAt = forge.Failed

			Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			Expect(f.forge.checksCalls()).To(Equal(0), "an already-merged head's checks are never read")
			Expect(f.forge.mergeCalls()).To(Equal(0), "already merged: there is nothing left to ask the forge to do")
			Expect(evidenceOf(f.inspect(ctx), "merge").GetStatus()).To(Equal(string(forge.Success)))
		})
	})
})
