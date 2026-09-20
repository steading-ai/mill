package run_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// What closes a commit step.
//
// The fact a commit step produces is a commit that did not exist when the
// step began (docs/domain-model.md §Evidence, docs/core-principles.md §8).
// The head of the branch is not that fact: it was there before the session
// started, and a step that closed on it would close on work nobody did. So
// the attempt records the head it started at, and the evidence has to descend
// from it, be on the run's branch, and be the mill's own commit.

// someoneElse is an identity that is not the mill's, for the commit that
// reaches the branch by some other hand.
var someoneElse = map[string]string{
	"GIT_AUTHOR_NAME":     "Someone Else",
	"GIT_AUTHOR_EMAIL":    "else@example.test",
	"GIT_COMMITTER_NAME":  "Someone Else",
	"GIT_COMMITTER_EMAIL": "else@example.test",
}

var _ = Describe("Commit evidence", Label(testsupport.DBLabel), func() {
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
	})

	// commitStep is the run's one attempt at the commit step.
	commitStep := func() *millv1.StepRun {
		GinkgoHelper()
		attempts := stepRuns(f.inspect(ctx), "commit")
		Expect(attempts).NotTo(BeEmpty())
		return attempts[len(attempts)-1]
	}

	It("No commit, no evidence", func() {
		By("given a run at the commit step with base_commit recorded")
		// The commit step's turn is the third, and it leaves the checkout
		// exactly as it found it. The script that repeats after it does the
		// same, so nothing else can move the head either.
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20))},
			fake.Script{Terminal: fake.Terminal("committed it, honest", nil, usage(5))},
		)

		By("when the session ends without committing")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run is failed")
		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempt.GetBaseCommit()).NotTo(BeEmpty())

		By("and the failure names the head that did not move")
		head := f.head(ctx)
		Expect(attempt.GetBaseCommit()).To(Equal(head), "the head never moved")
		Expect(f.comments(ctx)).To(ContainElement(ContainSubstring(head)))
		Expect(f.comments(ctx)).To(ContainElement(ContainSubstring("the session made no commit")))
	})

	It("The commit that was made is the evidence", func() {
		By("given a run at the commit step")
		// The checkout outlives the run here, because the assertion is
		// about what git says of the branch the run committed on.
		f.keepWorkspace = true
		f.script(f.quickFixScripts()...)

		By("when the session commits one change as the mill's identity")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step closes on that commit's SHA")
		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		// The commit step's turn is the third of the four, so the commit it
		// made is the third the run's sessions made.
		made := f.commitsMade()
		closed := evidenceOf(f.inspect(ctx), "commit")
		Expect(closed.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_COMMIT))
		Expect(made).To(HaveLen(4))
		Expect(closed.GetCommit()).To(Equal(made[2]))

		By("and the SHA is a descendant of base_commit on the run's branch")
		base := attempt.GetBaseCommit()
		Expect(base).NotTo(BeEmpty())
		Expect(closed.GetCommit()).NotTo(Equal(base))
		git := f.workspace.GitRunner()
		_, err := git.Run(ctx, nil, "merge-base", "--is-ancestor", base, closed.GetCommit())
		Expect(err).NotTo(HaveOccurred(), "the evidence descends from the base")
		_, err = git.Run(ctx, nil, "merge-base", "--is-ancestor", closed.GetCommit(), f.runOf(ctx).GetBranch())
		Expect(err).NotTo(HaveOccurred(), "and it is on the run's branch")
	})

	It("Someone else's commit is not the step's", func() {
		By("given a run at the commit step")
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20))},
			fake.Script{Terminal: fake.Terminal("something committed", nil, usage(5)), Do: f.commitsAsSomeoneElse},
		)

		By("when the branch head advances by a commit whose committer is not the mill")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run is failed")
		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(f.head(ctx)).NotTo(Equal(attempt.GetBaseCommit()), "the head did move")

		By("and the failure names the committer found")
		Expect(f.comments(ctx)).To(ContainElement(ContainSubstring("Someone Else <else@example.test>")))
	})

	It("A revise that changes nothing still closes commit", func() {
		By("given a clean-tree that fails once, then passes")
		// A bare host command, resolved on PATH exactly as `scripts/clean-tree`
		// is, that fails the first time it is asked and remembers to pass the
		// second: the shape of a check whose complaint the revise round
		// already answered.
		marker := filepath.Join(GinkgoT().TempDir(), "revised")
		workflow := f.variant(ctx, "revise-once", func(definition *millv1.Workflow) {
			stepOf(definition, "clean-tree").GetCheck().Command = []string{
				"sh", "-c", fmt.Sprintf(`test -f %q && exit 0 || { touch %q; exit 1; }`, marker, marker),
			}
		})

		By("and a commit step whose resumed attempt makes no new commit")
		f.keepWorkspace = true
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("nothing left to change", nil, usage(3))},
			fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
		)

		By("when clean-tree's first failure revises the work back to commit")
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step closes on the commit the first attempt made")
		made := f.commitsMade()
		Expect(made).To(HaveLen(4), "the resumed attempt committed nothing of its own")
		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(attempt.GetBaseCommit()).To(Equal(made[2]),
			"the resumed attempt's own base is the head it never moved from")
		closed := evidenceOf(f.inspect(ctx), "commit")
		Expect(closed.GetCommit()).To(Equal(made[2]))

		By("and the first attempt's commit was superseded, not lost")
		commits := stepRuns(f.inspect(ctx), "commit")
		Expect(commits).To(HaveLen(2))
		Expect(commits[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
		Expect(commits[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		By("and clean-tree ran again and passed")
		checks := stepRuns(f.inspect(ctx), "clean-tree")
		Expect(checks).To(HaveLen(2))
		Expect(checks[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
		Expect(checks[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	})

	It("Uncommitted work is never closed over", func() {
		By("given a session that edits the tree without committing")
		f.keepWorkspace = true
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20))},
			fake.Script{Terminal: fake.Terminal("done, I think", nil, usage(5)), Do: f.writesWithoutCommitting},
		)

		By("when the session ends with the tree dirty and no commit made")
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run is failed, naming the head that did not move")
		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		head := f.head(ctx)
		Expect(attempt.GetBaseCommit()).To(Equal(head), "the head never moved")
		Expect(f.comments(ctx)).To(ContainElement(SatisfyAll(
			ContainSubstring("the branch head is still"),
			ContainSubstring(head),
			ContainSubstring("the session made no commit"),
		)))

		By("and the uncommitted work is still in the tree, not discarded")
		git := f.workspace.GitRunner()
		status, err := git.Run(ctx, nil, "status", "--porcelain")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(status)).NotTo(BeEmpty())
	})

	It("closes a resumed step on the commit the interrupted attempt made, without a second session", func() {
		// A worker killed between the session's commit and the write that
		// closes the step: the commit is on the branch and the ledger says
		// which head the attempt started at, which is all the next worker
		// needs (docs/ledger.md §step_runs).
		const interval = time.Second
		committed := make(chan string, 1)
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20))},
			fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil), Do: func(session harness.Session) {
				f.commits(session)
				committed <- f.head(ctx)
			}},
		)

		runner := f.runner(func(o *run.Options) {
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = 5 * time.Second
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()

		var sha string
		Eventually(committed, time.Minute).Should(Receive(&sha))
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, time.Minute).Should(Receive(BeNil()))
		Expect(commitStep().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))

		// The run is taken over by a worker whose sessions commit nothing.
		// If the commit step ran one, it would have nothing to close on.
		f.script(fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5))})
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		attempt := commitStep()
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(evidenceOf(f.inspect(ctx), "commit").GetCommit()).To(Equal(sha))
		Expect(f.harness.Calls()).To(HaveLen(1),
			"one session in the second run: the pr step's, and none for commit")
	})
})

// writesWithoutCommitting edits the checkout and stops, the way a session
// that never reaches its `git.commit` call does. The file has to survive:
// the point of the spec this backs is that an unmoved head is not always a
// head with nothing behind it, and closing the step would discard the edit
// along with the evidence that it happened.
func (f *fixture) writesWithoutCommitting(harness.Session) {
	defer GinkgoRecover()
	ctx := context.Background()
	Expect(f.workspace).NotTo(BeNil(), "the run made a workspace before the session ran")
	Expect(f.workspace.Write(ctx, "left-behind.txt", []byte("nobody committed this\n"))).To(Succeed())
}

// commitsAsSomeoneElse advances the run's branch by a commit the mill did not
// make. It goes round the workspace's git verb on purpose: that verb commits
// under the mill's identity, and the point of this commit is that it does
// not.
func (f *fixture) commitsAsSomeoneElse(harness.Session) {
	defer GinkgoRecover()
	root := f.workspace.Handle().Root
	// A file per call: the step is retried, and a second identical write
	// would leave git with nothing to commit and the spec asserting
	// something it did not set up.
	name := fmt.Sprintf("theirs-%d.txt", f.theirs.Add(1))
	Expect(os.WriteFile(filepath.Join(root, name), []byte("not the mill's work\n"), 0o600)).To(Succeed())
	gitAs(root, someoneElse, "add", name)
	gitAs(root, someoneElse, "commit", "--message", "a commit the mill did not make")
}
