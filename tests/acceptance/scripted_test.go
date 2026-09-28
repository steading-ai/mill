package acceptance_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
)

// scriptedFeatureDir is this package's own copy of the fixture workflow:
// workflows/feature mirrored, pr and merge dropped, harness.scripted on
// every skill step (tests/acceptance/testdata/workflows/scripted-feature).
// Relative, because `mill run` and `mill workflow push` here inherit this
// package's own working directory, the same way
// scripted_workflow_test.go's workflow.Load(dir) already reads it.
const scriptedFeatureDir = "testdata/workflows/scripted-feature"

// scriptedFeatureFixture is a bare, local repository: scripted-feature
// drops pr and merge, so nothing in this scenario ever needs a forge, and
// its scripts never read the fixture's own files — every dispatch's
// content is the script's own. What the fixture provides is only something
// real for commit and clean-tree to act on.
var scriptedFeatureFixture = fixture{
	Name:  "scripted",
	Local: true,
	Files: typo.Files,
	Title: "mill#21: a run that costs no tokens",
	Body:  "Every step's turns are read from a script; the body is not.",
}

// scriptedTimeout is how long a scripted-feature run has to reach done. No
// step waits on a model, so the whole two-round shape is fast; the budget
// is generous only for the suite's own process overhead — the clone, the
// checks, two `mill run` invocations for the determinism check.
const scriptedTimeout = 5 * time.Minute

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: A whole run for no tokens
	//   Given MILL_ACCEPTANCE=1 and the scripted-feature workflow pushed
	//   When mill work claims the node
	//   Then the run reaches done, and mill cost reports zero tokens for every step
	It("A run costs no tokens", func(ctx SpecContext) {
		By("without MILL_ACCEPTANCE, mill workflow push refuses scripted-feature naming the variable")
		refused := exec.CommandContext(ctx, env.Mill, //nolint:gosec // the suite's own binary
			"workflow", "push", scriptedFeatureDir, "--socket", suite.service.Socket)
		refused.Env = withoutAcceptance()
		var stderr strings.Builder
		refused.Stderr = &stderr
		Expect(refused.Run()).To(HaveOccurred(), "mill workflow push succeeded without MILL_ACCEPTANCE")
		Expect(stderr.String()).To(ContainSubstring("MILL_ACCEPTANCE"))

		By("given the fixture pushed into a scratch project, and ANTHROPIC_BASE_URL pointed at a closed port")
		// No step of scripted-feature ever reaches for a model — that is the
		// point — but pointing the run at a port nothing listens on is the
		// node's own belt-and-braces: a run that somehow did reach for one
		// would fail fast and visibly rather than hang.
		GinkgoT().Setenv("ANTHROPIC_BASE_URL", "http://"+closedPort())
		s := scriptedFeatureFixture.stage(ctx)
		pushScriptedFeature(ctx, s.Graph.Slug)

		By("when mill run is invoked for it with the scripted-feature workflow")
		options := runOptions{Workflow: "scripted-feature", Tier: "feature", WorkflowDir: scriptedFeatureDir}
		worker := s.start(ctx, options)

		By("and while implement's after: 5s holds its dispatch open, mill status reports the run running at implement")
		running := runningStep(ctx, s.NodeRef, "implement", scriptedTimeout)
		statusRes, err := suite.service.Store.Status(ctx, connect.NewRequest(&millv1.StatusRequest{Project: s.Graph.Slug}))
		Expect(err).NotTo(HaveOccurred(), "mill status")
		summary := runSummary(statusRes.Msg.GetRuns(), running.GetRunId())
		Expect(summary).NotTo(BeNil(), "mill status listed the run implement is running on")
		Expect(summary.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
		Expect(summary.GetCurrentStep()).To(Equal("implement"))

		Expect(worker.wait(scriptedTimeout)).To(BeZero(), "mill run exited cleanly")

		By("then the run reached done")
		detail := inspect(ctx, s.NodeRef)
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))

		By("and implement's first attempt took at least the 5s its after named, the window a poller had to observe it running")
		first := attempts(detail, "implement")[0].GetStepRun()
		Expect(first.GetStartedAt()).NotTo(BeNil())
		Expect(first.GetEndedAt()).NotTo(BeNil())
		Expect(first.GetEndedAt().AsTime().Sub(first.GetStartedAt().AsTime())).To(BeNumerically(">=", 5*time.Second))

		By("and mill cost reports zero tokens: no usage bucket exists for a run that never emitted one")
		cost, err := suite.service.Store.Cost(ctx, connect.NewRequest(&millv1.CostRequest{Project: s.Graph.Slug}))
		Expect(err).NotTo(HaveOccurred())
		Expect(cost.Msg.GetByRun()).To(BeEmpty())
		Expect(cost.Msg.GetByWorkflow()).To(BeEmpty())
		Expect(cost.Msg.GetByStep()).To(BeEmpty())
		Expect(cost.Msg.GetByModel()).To(BeEmpty())

		By("and review's first attempt failed on a revise with two blocking findings")
		reviews := attempts(detail, "review")
		Expect(reviews).To(HaveLen(2), "review ran once per round: a revise, then an approve")
		Expect(reviews[0].GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		view := s.Graph.show(ctx, s.NodeRef)
		number, document := firstVerdict(view)
		Expect(number).To(Equal(1))
		Expect(document).To(ContainSubstring("- decision: revise"))
		Expect(blockingFindings(document)).To(HaveLen(2))

		By("and implement's second attempt answers that failure")
		again := answering(detail, "implement", reviews[0].GetStepRun().GetId())
		Expect(again).NotTo(BeNil(), "implement ran again, answering the review that failed")

		By("and clean-tree failed once before it closed, within the first round")
		cleanTrees := attempts(detail, "clean-tree")
		Expect(cleanTrees).To(HaveLen(3), "a failed and a closed attempt in round 1, one closed attempt in round 2")
		Expect(cleanTrees[0].GetStepRun().GetAttempt()).To(Equal(int64(1)))
		Expect(cleanTrees[0].GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(cleanTrees[1].GetStepRun().GetAttempt()).To(Equal(int64(2)))
		Expect(cleanTrees[1].GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		By("and review's second attempt approved")
		Expect(reviews[1].GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		names := verdictNames(view)
		Expect(names).To(HaveLen(2))
		Expect(view.attachment(names[1])).To(ContainSubstring("- decision: approve"))

		By("and every attempt's activity file replayed its scripted events in order, each with a turn")
		assertScriptedActivity(s.LogDir, detail)

		By("and a second run of the same scripts produces the same shape, ids and hashes aside")
		second := scriptedFeatureFixture.stage(ctx)
		pushScriptedFeature(ctx, second.Graph.Slug)
		Expect(second.start(ctx, options).wait(scriptedTimeout)).To(BeZero(), "the second mill run exited cleanly")
		Expect(millInspectRefs(ctx, second.NodeRef)).To(Equal(millInspectRefs(ctx, s.NodeRef)),
			"two runs of the same scripts close every attempt the same way, in the same order")
	})
})

// pushScriptedFeature pushes the fixture workflow at project scope into the
// scenario's own project, exactly as workflows_test.go's stageWorkflow does
// for its own derived definitions: an org-scope push outlives the scratch
// project and would leak scripted-feature into every later assessment's
// candidate list, in this suite run and every one after it (mill#34).
func pushScriptedFeature(ctx context.Context, project string) {
	GinkgoHelper()
	push := exec.CommandContext(ctx, env.Mill, //nolint:gosec // the suite's own binary
		"workflow", "push", scriptedFeatureDir, "--socket", suite.service.Socket,
		"--scope", "project", "--project", project)
	push.Stdout, push.Stderr = GinkgoWriter, GinkgoWriter
	Expect(push.Run()).To(Succeed(), "mill workflow push %s", scriptedFeatureDir)
}

// closedPort is a TCP address on this host nothing is listening on: opened
// and immediately closed, so a connection to it refuses fast rather than
// hanging on a port that was merely never bound.
func closedPort() string {
	GinkgoHelper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := l.Addr().String()
	Expect(l.Close()).To(Succeed())
	return addr
}

// withoutAcceptance is the process environment with MILL_ACCEPTANCE
// removed. The suite's own process needs the variable set just to start —
// SynchronizedBeforeSuite skips otherwise — so proving the CLI refuses
// without it means stripping it back out for that one subprocess.
func withoutAcceptance() []string {
	kept := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MILL_ACCEPTANCE=") {
			kept = append(kept, kv)
		}
	}
	return kept
}

// assertScriptedActivity reads every skill step's attempt back off disk and
// checks the harness-sourced records it holds: at least one, each with a
// turn assigned, and the turns non-decreasing — the activity record's own
// account of the scripted events replaying in order (mill#13, mill#21
// acceptance criterion 4).
func assertScriptedActivity(logDir string, detail *millv1.InspectResponse) {
	GinkgoHelper()
	runID := detail.GetSummary().GetRun().GetId()
	for _, step := range detail.GetSteps() {
		sr := step.GetStepRun()
		if sr.GetKind() != millv1.StepKind_STEP_KIND_SKILL {
			continue
		}
		activities, err := run.ReadSealedActivities(logDir, runID, sr.GetStepName(), sr.GetAttempt())
		Expect(err).NotTo(HaveOccurred(), "%s attempt %d activity file", sr.GetStepName(), sr.GetAttempt())

		var turns []int64
		for _, a := range activities {
			if a.GetSource() != millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS {
				continue
			}
			Expect(a.GetTurn()).To(BeNumerically(">", 0),
				"%s attempt %d: a harness record with no turn assigned", sr.GetStepName(), sr.GetAttempt())
			turns = append(turns, a.GetTurn())
		}
		Expect(turns).NotTo(BeEmpty(), "%s attempt %d recorded no scripted events", sr.GetStepName(), sr.GetAttempt())
		for i := 1; i < len(turns); i++ {
			Expect(turns[i]).To(BeNumerically(">=", turns[i-1]),
				"%s attempt %d: turns out of order: %v", sr.GetStepName(), sr.GetAttempt(), turns)
		}
	}
}

// runSummary is the entry mill status carries for a run, found by id.
func runSummary(runs []*millv1.RunSummary, runID string) *millv1.RunSummary {
	for _, r := range runs {
		if r.GetRun().GetId() == runID {
			return r
		}
	}
	return nil
}

// millInspectRefs is `mill inspect --view refs`: step, attempt and status
// alone, with none of the ids, timestamps or hashes the determinism
// criterion has to look past.
func millInspectRefs(ctx context.Context, nodeRef string) string {
	GinkgoHelper()
	command := exec.CommandContext(ctx, env.Mill, //nolint:gosec // the suite's own binary
		"inspect", nodeRef, "--socket", suite.service.Socket, "--view", "refs")
	var printed strings.Builder
	command.Stdout, command.Stderr = &printed, GinkgoWriter
	Expect(command.Run()).To(Succeed(), "mill inspect %s --view refs", nodeRef)
	return printed.String()
}
