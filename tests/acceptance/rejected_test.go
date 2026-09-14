package acceptance_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
)

// rejectedTimeout is how long the seeded bad implementation has to be
// rejected. The span is `feature`'s, from gather-context through review, and
// then as many rounds of implement → simplify → commit → clean-tree →
// review as the review step's retries allow. One round takes about five
// minutes on a warm host; the budget is for three.
const rejectedTimeout = 45 * time.Minute

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: The reviewer says no
	//   Given a node with two acceptance criteria
	//   And an implementation that satisfies the second and violates the first
	//   When the feature workflow reaches review
	//   Then the verdict is revise
	//   And a blocking finding names the first criterion
	//   And implement runs again with that finding as its prompt
	It("The reviewer says no", func(ctx SpecContext) {
		By("given a node with two acceptance criteria")
		s := reviewerFixture(forgeConfigured()).stage(ctx)
		Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("open"))
		Expect(seeded.Body).To(SatisfyAll(ContainSubstring("AC-1"), ContainSubstring("AC-2")))

		By("and an implementation that satisfies the second and violates the first")
		// The implementation is a prompt, not a patch: the `implement` step
		// runs a scripted skill that dictates the code instead of deriving
		// it. Everything else about the step — its session, its model, its
		// tools — is the workflow's own.
		selected, dir := reviewerWorkflow(ctx, s.Graph.Slug)

		By("when the feature workflow reaches review")
		exit := s.start(ctx, runOptions{
			Workflow:       selected,
			Tier:           "feature",
			WorkflowDir:    dir,
			SkillOverrides: map[string]string{"implement": scriptedImplement()},
		}).wait(rejectedTimeout)
		Expect(exit).To(BeZero(), "mill run stopped on its own terms, not on an error")

		detail := inspect(ctx, s.NodeRef)
		reviews := attempts(detail, "review")
		Expect(reviews).NotTo(BeEmpty(), "the run reached review")
		if !forgeConfigured() {
			Expect(attempts(detail, "pr")).To(BeEmpty(),
				"the run never reached a forge: there is none on this host")
		}

		By("then the verdict is revise")
		view := s.Graph.show(ctx, s.NodeRef)
		number, document := firstVerdict(view)
		// The findings are in the spec's report verbatim, so a
		// retrospective can quote the reviewer's no rather than paraphrase
		// it (docs/prd.md M.2).
		AddReportEntry("the reviewer's first verdict, verbatim", document)
		Expect(document).To(ContainSubstring("- decision: revise"))

		// The verdict on the node names the session that reached it: the
		// reviewer's, which is not the implementer's.
		reviewer := attemptNumbered(reviews, int64(number))
		Expect(reviewer).NotTo(BeNil(), "the ledger has the review attempt the verdict is named for")
		Expect(document).To(ContainSubstring("- session: " + reviewer.GetSessionId()))
		Expect(reviewer.GetSessionId()).NotTo(BeEmpty())
		Expect(reviewer.GetSessionId()).NotTo(Equal(latest(detail)["implement"].GetStepRun().GetSessionId()))

		By("and a blocking finding names the first criterion")
		blocking := blockingFindings(document)
		Expect(blocking).NotTo(BeEmpty(), "the revise carries a blocking finding:\n%s", document)
		Expect(blocking).To(ContainElement(MatchRegexp(`(?i)\bAC[- ]?1\b`)),
			"a blocking finding names the first criterion:\n%s", document)

		By("and implement runs again with that finding as its prompt")
		again := answering(detail, "implement", reviewer.GetId())
		Expect(again).NotTo(BeNil(), "implement ran again, answering the review that failed")
		superseded := attempts(detail, "implement")[0].GetStepRun()
		Expect(superseded.GetAttempt()).To(BeNumerically("<", again.GetAttempt()))
		Expect(superseded.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))

		// Read from the ledger's step log and not from the session: what
		// the mill decided to say is a fact the record holds, and asking
		// the harness what it heard would be asking the thing under test.
		prompt := stepLog(s.LogDir, detail.GetSummary().GetRun().GetId(), "implement", again.GetAttempt())
		Expect(prompt).To(ContainSubstring("The reviewer asked for changes"))
		for _, finding := range blocking {
			Expect(reached(prompt, finding)).To(BeTrue(),
				"the finding %q reached the implementer's next prompt", finding)
		}

		By("and the pipeline rejected the change: it delivered on a later approve, or parked on the findings")
		// Whether the second attempt fixes the code is the model's
		// business. Either ending is the pipeline refusing to let the
		// seeded implementation through unreviewed: what is ruled out is a
		// run that approved the first one.
		switch status := detail.GetSummary().GetRun().GetStatus(); status {
		case millv1.RunStatus_RUN_STATUS_DONE:
			approved, later := "", false
			for _, name := range verdictNames(view) {
				if at := verdictNumber(name); at > number {
					approved, later = view.attachment(name), true
				}
			}
			Expect(later).To(BeTrue(), "a later review reached a verdict")
			Expect(approved).To(ContainSubstring("- decision: approve"))
			Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("done"))
			if forgeConfigured() {
				// Delivered, not merely approved: the fix for the seeded
				// violation is on the default branch of a real repository.
				merged := view.artifact("merge-commit")
				AddReportEntry("the merge commit the run delivered after the reviewer's no", merged)
				Expect(s.Project.branchesContaining(ctx, merged)).To(ContainElement(s.Project.DefaultBranch))
			}
		case millv1.RunStatus_RUN_STATUS_PARKED:
			gates := s.Graph.gates(ctx, detail.GetSummary().GetRun().GetId())
			Expect(gates).NotTo(BeEmpty(), "the breaker filed a gate")
			AddReportEntry("the gate the run parked on", strings.Join(gates, "\n\n---\n\n"))
			Expect(reached(strings.Join(gates, "\n"), blocking[0])).To(BeTrue(),
				"the findings are in the gate a human is asked to answer")
		default:
			Fail("the run neither delivered nor parked; it is " + status.String())
		}
	})
})

// reviewerFixture is the seeded bad implementation, staged for the host it
// runs on. With a forge it is a scratch project, so a run that answers the
// reviewer's no can deliver and the scenario sees the whole of what the
// pipeline does with a rejection; without one it is a bare repository on
// this host, which is what keeps the milestone's own proof off an account
// not every host has (docs/prd.md R-25, docs/runbook.md §"Without a forge").
func reviewerFixture(forge bool) fixture {
	f := seeded
	f.Local = !forge
	return f
}

// reviewerWorkflow is the definition the scenario runs and the directory it
// was loaded from.
//
// With a forge it is `feature` itself, byte for byte. Without one, `feature`
// opens the pull request before the reviewer reads the change, so a run of
// it parks at `pr` and the reviewer never speaks; the scenario then runs the
// same definition less exactly the two steps whose evidence only a forge can
// state, derived from the file at run time so it cannot drift.
func reviewerWorkflow(ctx context.Context, project string) (name, dir string) {
	GinkgoHelper()
	if forgeConfigured() {
		return "feature", workflowDir("feature")
	}
	forgeless, dropped := deriveForgelessFeature(ctx, project)
	Expect(dropped).To(ConsistOf("pr", "merge"),
		"only the steps that talk to a forge were dropped from feature")
	return forgelessFeature, forgeless
}

// verdictPattern matches the artifacts the run attaches for each review
// attempt that reached a verdict: `review-<generation>.<attempt>.md`
// (internal/run/verify.go); the capture is the attempt.
var verdictPattern = regexp.MustCompile(`^review-\d+\.(\d+)\.md$`)

// firstVerdict is the earliest review attempt that reached a verdict, and
// the document the run put on the node for it. It is the earliest and not
// simply `review-1.1.md` because an attempt whose output did not parse is
// judged nothing and records no verdict at all: the mill asks that reviewer
// again, and the first verdict is the first one there is
// (docs/domain-model.md §Verdict).
func firstVerdict(view nodeView) (attempt int, document string) {
	GinkgoHelper()
	names := verdictNames(view)
	Expect(names).NotTo(BeEmpty(), "the node carries a verdict from the review step")
	return verdictNumber(names[0]), view.attachment(names[0])
}

// verdictNames are the review verdicts on a node, by attempt.
func verdictNames(view nodeView) []string {
	var names []string
	for _, artifact := range view.Artifacts {
		if verdictPattern.MatchString(artifact.Name) {
			names = append(names, artifact.Name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return verdictNumber(names[i]) < verdictNumber(names[j]) })
	return names
}

// verdictNumber is the attempt a verdict artifact is named for.
func verdictNumber(name string) int {
	GinkgoHelper()
	match := verdictPattern.FindStringSubmatch(name)
	Expect(match).To(HaveLen(2), "%s is a verdict artifact", name)
	at, err := strconv.Atoi(match[1])
	Expect(err).NotTo(HaveOccurred())
	return at
}

// blockingFinding matches one blocking finding in a verdict document, as
// internal/run's verdictDocument renders it: the severity in bold, then the
// criterion and the summary.
var blockingFinding = regexp.MustCompile(`(?m)^- \*\*blocking\*\* (.+)$`)

// blockingFindings are the blocking findings of a verdict, as the run wrote
// them on the node: `<criterion>: <summary>`, each on its own line.
func blockingFindings(document string) []string {
	var found []string
	for _, match := range blockingFinding.FindAllStringSubmatch(document, -1) {
		found = append(found, strings.TrimSpace(match[1]))
	}
	return found
}

// reached reports whether a finding the run wrote on the node is in a body
// the run wrote somewhere else — the implementer's next prompt, or the gate
// the breaker filed.
//
// The two renderings differ: the node's is `<criterion>: <summary>` and the
// prompt's is `<summary> (blocking) on <criterion>`, and a criterion is free
// text that may itself contain a colon — a reviewer ties a finding to
// `Scenario: Boiling water in Fahrenheit` as readily as to `AC-1`. So the
// spec drops one `<prefix>: ` at a time from the front and asks again,
// rather than pretending the node's rendering can be split unambiguously.
func reached(body, rendered string) bool {
	for text := rendered; ; {
		if strings.Contains(body, text) {
			return true
		}
		_, rest, found := strings.Cut(text, ": ")
		if !found {
			return false
		}
		text = rest
	}
}

// attemptNumbered is one step's numbered attempt, or nil.
func attemptNumbered(tries []*millv1.StepRunDetail, attempt int64) *millv1.StepRun {
	for _, d := range tries {
		if d.GetStepRun().GetAttempt() == attempt {
			return d.GetStepRun()
		}
	}
	return nil
}

// answering is the attempt at a step that was started to answer a named
// attempt's failure — the successor a revise created, which the ledger says
// so on rather than the spec inferring from attempt numbers.
func answering(detail *millv1.InspectResponse, step, failed string) *millv1.StepRun {
	for _, d := range attempts(detail, step) {
		if d.GetStepRun().GetFeedbackFrom() == failed {
			return d.GetStepRun()
		}
	}
	return nil
}

// stepLog is one attempt's log, which is where the mill records the prompt
// it gave a session.
func stepLog(dir, runID, step string, attempt int64) string {
	GinkgoHelper()
	path := run.StepLogPath(dir, runID, step, attempt)
	content, err := os.ReadFile(path) //nolint:gosec // a path the suite computed
	Expect(err).NotTo(HaveOccurred(), "the ledger's log for %s attempt %d", step, attempt)
	return string(content)
}
