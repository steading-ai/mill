package run_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/claude"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

var _ = Describe("The activity record", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	It("writes one file per attempt, and never a value registered with redact during the step", func() {
		const secret = "s3cret-forge-token-value"
		f.node(ctx, "a logged step", "This one talks.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")

		// Do runs as the turn begins: the value is registered while the step
		// is in flight, through the same Workspace.Redact the socket's
		// `redact` verb dispatches to, and only then does the session say
		// anything.
		f.script(fake.Script{
			Do: func(harness.Session) { f.workspace.Redact(secret) },
			Events: []*harnessv1.Event{
				// Final: true, as a block's last delta always carries it —
				// otherwise the redactor's holdback has nothing proving the
				// block is done and keeps the tail buffered forever.
				{RequestId: "turn-1", Body: &harnessv1.Event_Text{
					Text: &harnessv1.Text{Text: "using " + secret + " to reach the forge", Final: true},
				}},
				toolResult("the answer mentions " + secret + " again"),
			},
			Terminal: fake.Terminal("context gathered", nil, usage(10)),
		})

		done := make(chan error, 1)
		go func() {
			done <- f.runner(func(o *run.Options) { o.LogDir = logDir }).Execute(ctx, f.nodeRef)
		}()
		Eventually(done, 90*time.Second).Should(Receive(BeNil()))

		By("and the closed attempt is sealed: its frame replaces its live file")
		_, err := os.Stat(run.ActivityPath(logDir, f.runOf(ctx).GetId(), "gather-context", 1))
		Expect(err).To(MatchError(os.ErrNotExist))
		Expect(run.FramePath(logDir, f.runOf(ctx).GetId(), "gather-context", 1)).To(BeAnExistingFile())

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "gather-context", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(activities).NotTo(BeEmpty())
		Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(), "the first record is attempt_started")
		last := activities[len(activities)-1]
		Expect(last.GetAttemptEnded()).NotTo(BeNil(), "the last record is attempt_ended")
		Expect(last.GetAttemptEnded().GetReason()).NotTo(BeEmpty())

		var sawText, sawResult bool
		for _, a := range activities {
			if text := a.GetText(); text != nil {
				Expect(text.GetText()).To(ContainSubstring(redact.Mask))
				Expect(text.GetText()).NotTo(ContainSubstring(secret))
				sawText = true
			}
			if result := a.GetToolResult(); result != nil {
				Expect(result.GetContent()).To(ContainSubstring(redact.Mask))
				Expect(result.GetContent()).NotTo(ContainSubstring(secret))
				sawResult = true
			}
		}
		Expect(sawText).To(BeTrue(), "the session's text reached the file, redacted")
		Expect(sawResult).To(BeTrue(), "the tool result reached the file, redacted")
	})

	// Scenario: A crashed sidecar explains itself
	//   Given a sidecar entrypoint that throws before reading its request
	//   When a skill step runs
	//   Then the step run is failed
	//   And its activity file's harness_stderr carries the thrown error's
	//   stack
	//
	// A sidecar that dies before its terminal record leaves nothing on the
	// wire, so its standard error is the only account of what happened. The
	// last line of a Node crash is always `Node.js v24.x`, which is why the
	// whole of it is kept and written here.
	It("A crashed sidecar explains itself", func() {
		By("given a sidecar entrypoint that throws before reading its request")
		f.node(ctx, "a step whose sidecar crashes", "This one never gets a session.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		adapter := claude.New(
			claude.WithEntrypoint("/bin/sh", crashingSidecar()),
			claude.WithGrace(time.Second))

		By("when a skill step runs")
		Expect(f.runner(withHarness(adapter), func(o *run.Options) { o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step run is failed")
		attempts := stepRuns(f.inspect(ctx), "gather-context")
		Expect(attempts).NotTo(BeEmpty())
		for _, attempt := range attempts {
			Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		}

		By("and its activity file's harness_stderr carries the thrown error's stack")
		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "gather-context", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(), "the first record is attempt_started")
		ended := activities[len(activities)-1].GetAttemptEnded()
		Expect(ended).NotTo(BeNil(), "the last record is attempt_ended")
		Expect(ended.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))

		var stderrText string
		for _, a := range activities {
			if stderr := a.GetHarnessStderr(); stderr != nil {
				stderrText += stderr.GetText()
			}
		}
		Expect(stderrText).To(ContainSubstring("Error: the sidecar could not start"))
		Expect(stderrText).To(ContainSubstring("at Object.<anonymous>"))
		Expect(stderrText).To(ContainSubstring("at Module._compile"))

		// And what the failure said for itself is the same stack, not the
		// one line that follows it.
		body := f.theGate(ctx).GetBody()
		Expect(body).To(ContainSubstring("Error: the sidecar could not start"))
		Expect(body).To(ContainSubstring("at Object.<anonymous>"))
	})

	// A check has no session and so no events; its own record is
	// check_started/check_output/check, written as internal/check.Run
	// produces them (docs/plans/2026-09-20-mill-13.md step 6).
	It("writes a failed check's output to the attempt's activity record, in the order produced", func() {
		f.node(ctx, "a failing check", "This one leaves the tree dirty.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		command := []string{
			"sh", "-c", "echo 'the tree is dirty: README.md' >&2; echo 'M README.md'; exit 1",
		}
		workflow := f.variant(ctx, "logged-check-fix", func(definition *millv1.Workflow) {
			check := stepOf(definition, "clean-tree").GetCheck()
			check.Command = command
			// Both streams reach the record, so the contract keeps stdout: a
			// `none` contract drops it before the runner sees it.
			check.Stdout = millv1.StdoutShape_STDOUT_SHAPE_TEXT
		})
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed again", nil, usage(5)), Do: f.commits},
		)

		Expect(f.runner(withHarness(f.harness),
			func(o *run.Options) { o.Workflow = workflow; o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "clean-tree", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(), "the first record is attempt_started")
		Expect(activities[len(activities)-1].GetAttemptEnded()).NotTo(BeNil(), "the last record is attempt_ended")

		var startedAt, checkAt, endedAt = -1, -1, -1
		var stdout, stderr string
		for i, a := range activities {
			switch {
			case a.GetCheckStarted() != nil:
				startedAt = i
			case a.GetCheckOutput() != nil:
				Expect(checkAt).To(Equal(-1), "check_output arrives before the terminal check record")
				out := a.GetCheckOutput()
				switch out.GetStream() {
				case "stdout":
					stdout += string(out.GetData())
				case "stderr":
					stderr += string(out.GetData())
				}
			case a.GetCheck() != nil:
				checkAt = i
			case a.GetAttemptEnded() != nil:
				endedAt = i
			}
		}
		Expect(startedAt).To(BeNumerically(">", 0), "check_started follows attempt_started")
		started := activities[startedAt].GetCheckStarted()
		Expect(started.GetCommand()).To(Equal(command))
		Expect(started.GetContract().GetStdout()).To(Equal(millv1.StdoutShape_STDOUT_SHAPE_TEXT))

		Expect(checkAt).To(BeNumerically(">", startedAt))
		Expect(endedAt).To(Equal(len(activities) - 1))
		Expect(checkAt).To(BeNumerically("<", endedAt))
		Expect(stdout).To(ContainSubstring("M README.md"))
		Expect(stderr).To(ContainSubstring("the tree is dirty: README.md"))
		Expect(activities[checkAt].GetCheck().GetExit().GetStatus()).To(Equal(int32(1)))
	})

	// Scenario: The assessment's own attempt keeps an activity file too
	//   Given a project with no default workflow
	//   When the run starts and assesses the node before walking anything
	//   Then the assess step's own attempt has an activity file
	//   And it opens with attempt_started and ends with attempt_ended
	//
	// select.go's assess dispatches its own skill step outside execute()
	// entirely (docs/plans/2026-09-20-mill-13.md step 4's second review
	// finding): before this it had no writer at all.
	It("gives the assessment its own activity file, shaped like any other attempt", func() {
		f.pushWorkflowFrom(ctx, buildWorkflowDir)
		f.node(ctx, "one line change", "Change the README's first line.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")

		f.script(append([]fake.Script{assessed("quick-fix", "quick-fix",
			"README.md is the only file the ask names")},
			f.quickFixScripts()...)...)

		Expect(f.runner(func(o *run.Options) {
			o.Workflow = ""
			o.Tier = millv1.Tier_TIER_UNSPECIFIED
			o.LogDir = logDir
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		detail := f.inspect(ctx)
		Expect(detail.GetSteps()).NotTo(BeEmpty())
		Expect(detail.GetSteps()[0].GetStepRun().GetStepName()).To(Equal("assess"))

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "assess", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(activities).NotTo(BeEmpty())
		Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(), "the first record is attempt_started")
		last := activities[len(activities)-1].GetAttemptEnded()
		Expect(last).NotTo(BeNil(), "the last record is attempt_ended")
		Expect(last.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	})

	// Scenario: A superseded attempt's file closes before the next opens
	//   Given a check that sends the work back to an earlier step
	//   When the revise supersedes that step's attempt and runs it again
	//   Then the superseded attempt's file ends in attempt_ended
	//   And the new attempt's file starts with attempt_started no earlier
	//
	// The writer is opened and closed at the three places an attempt
	// actually begins and ends, not by execute() itself, precisely so a
	// revise cannot leave two attempts' records interleaved in one file or
	// racing each other across two (docs/plans/2026-09-20-mill-13.md
	// Decisions).
	It("closes a superseded attempt's file before the next attempt's opens", func() {
		f.node(ctx, "one line change", "Change the README's first line.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		workflow := f.variant(ctx, "dirty-fix", func(definition *millv1.Workflow) {
			stepOf(definition, "clean-tree").GetCheck().Command = []string{"false"}
		})
		f.script(f.quickFixScripts()...)

		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow; o.LogDir = logDir }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		commits := stepRuns(f.inspect(ctx), "commit")
		Expect(commits).To(HaveLen(2))
		Expect(commits[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
		Expect(commits[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		superseded, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "commit", commits[0].GetAttempt())
		Expect(err).NotTo(HaveOccurred())
		Expect(superseded).NotTo(BeEmpty())
		endedAt := superseded[len(superseded)-1].GetAttemptEnded()
		Expect(endedAt).NotTo(BeNil(), "the superseded attempt's file still ends in attempt_ended")

		next, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "commit", commits[1].GetAttempt())
		Expect(err).NotTo(HaveOccurred())
		Expect(next).NotTo(BeEmpty())
		Expect(next[0].GetAttemptStarted()).NotTo(BeNil(), "the next attempt's file starts with attempt_started")

		Expect(superseded[len(superseded)-1].GetAt().AsTime()).
			To(BeTemporally("<=", next[0].GetAt().AsTime()),
				"the superseded attempt's last record precedes the next attempt's first, never interleaved")
	})

	// Scenario: A commit step's file records the verb that made the evidence
	//   Given a commit step whose session commits through the workspace's
	//   own git verb
	//   When the step's activity file is read back
	//   Then it holds verb_started for git.commit, then zero or more
	//   verb_output, then verb, in that order
	//
	// The verb is `git.commit`, not the seven verbs the socket names, because
	// [workspace/verbs.Host.Run] is the one chokepoint every git verb funnels
	// through and it names the subcommand it actually ran, past -c and -C
	// (docs/activity.md §Verbs).
	It("records verb_started, verb_output and verb for the commit's own git.commit", func() {
		f.node(ctx, "one line change", "Change the README's first line.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		f.script(f.quickFixScripts()...)

		Expect(f.runner(func(o *run.Options) { o.LogDir = logDir }).Execute(ctx, f.nodeRef)).To(Succeed())

		commit := stepRuns(f.inspect(ctx), "commit")
		Expect(commit).NotTo(BeEmpty())
		attempt := commit[len(commit)-1]
		Expect(attempt.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "commit", attempt.GetAttempt())
		Expect(err).NotTo(HaveOccurred())

		var kinds []string
		for _, a := range activities {
			switch {
			case a.GetVerbStarted() != nil:
				kinds = append(kinds, "started:"+a.GetVerbStarted().GetName())
			case a.GetVerbOutput() != nil:
				kinds = append(kinds, "output")
			case a.GetVerb() != nil:
				kinds = append(kinds, "ended:"+a.GetVerb().GetName())
			}
		}

		start := -1
		end := -1
		for i, k := range kinds {
			switch k {
			case "started:git.commit":
				if start == -1 {
					start = i
				}
			case "ended:git.commit":
				end = i
			}
		}
		Expect(start).NotTo(Equal(-1), "verb_started for git.commit")
		Expect(end).NotTo(Equal(-1), "verb for git.commit")
		Expect(start).To(BeNumerically("<", end), "verb_started precedes verb")
		for _, k := range kinds[start+1 : end] {
			Expect(k).To(Equal("output"), "only verb_output falls between git.commit's start and its own end")
		}
	})

	// Scenario: A resumed attempt that never runs a session still opens
	//   Given a commit step whose interrupted attempt already left its
	//   commit on the branch
	//   When the run resumes and finds that commit through commitAlreadyMade
	//   Then execute never calls runSkill for the resumed attempt
	//   But its activity file still opens with attempt_started
	//
	// attempt_started is recorded at the writer's open site in walk.go's
	// step, right after StartStepRun, not deferred into runSkill: a step
	// whose execute() branch decides the work is already done before any
	// session runs must still leave attempt_started as its first record
	// (docs/plans/2026-09-20-mill-13.md Decisions).
	It("opens with attempt_started even when the resumed attempt never runs a session", func() {
		f.node(ctx, "one line change", "Change the README's first line.")
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
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
			o.LogDir = logDir
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()

		var sha string
		Eventually(committed, time.Minute).Should(Receive(&sha))
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, time.Minute).Should(Receive(BeNil()))

		commitAttempts := stepRuns(f.inspect(ctx), "commit")
		Expect(commitAttempts).NotTo(BeEmpty())
		Expect(commitAttempts[len(commitAttempts)-1].GetStatus()).
			To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))

		// The run is taken over by a worker whose sessions commit nothing:
		// if the resumed commit attempt ran one, it would have nothing new
		// to close on.
		f.script(fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5))})
		Expect(f.runner(func(o *run.Options) { o.LogDir = logDir }).Execute(ctx, f.nodeRef)).To(Succeed())

		resumed := stepRuns(f.inspect(ctx), "commit")
		last := resumed[len(resumed)-1]
		Expect(last.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(evidenceOf(f.inspect(ctx), "commit").GetCommit()).To(Equal(sha))
		Expect(f.harness.Calls()).To(HaveLen(1), "no second session ran for commit")

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "commit", last.GetAttempt())
		Expect(err).NotTo(HaveOccurred())
		Expect(activities).NotTo(BeEmpty())
		Expect(activities[0].GetAttemptStarted()).NotTo(BeNil(),
			"attempt_started is recorded before execute is ever called, so it is seq 1 whether or not a session runs")
	})
})

// crashingSidecar is a sidecar entrypoint that throws before it reads its
// request: a shell script writing what a Node crash writes — the stack, then
// the version line that is all a one-line diagnostic would have kept — and
// exiting non-zero. A real node is not needed to specify what the adapter
// does with a process that dies talking to nobody.
func crashingSidecar() string {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "crash.sh")
	Expect(os.WriteFile(path, []byte(`#!/bin/sh
cat >&2 <<'EOT'
/opt/mill/sidecar/dist/main.js:42
    throw new Error('the sidecar could not start');
    ^

Error: the sidecar could not start
    at Object.<anonymous> (/opt/mill/sidecar/dist/main.js:42:11)
    at Module._compile (node:internal/modules/cjs/loader:1234:14)
    at Module._load (node:internal/modules/cjs/loader:1023:12)

Node.js v24.4.1
EOT
exit 1
`), 0o700)).To(Succeed())
	return path
}

// toolResult is one tool's answer on the wire. The fake harness builds the
// other event shapes; this one it does not.
func toolResult(body string) *harnessv1.Event {
	return &harnessv1.Event{Body: &harnessv1.Event_ToolResult{ToolResult: &harnessv1.ToolResult{
		CallId: "call-1", Content: body,
	}}}
}
