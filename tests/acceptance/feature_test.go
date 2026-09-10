package acceptance_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/notify"
)

// featureTimeout is how long one feature node has to reach its pull request.
// Nine sessions rather than quick-fix's four, two of them a reviewer, so it
// is three times the quick-fix budget and no more: a step that needs longer
// is a step whose plan was wrong.
const featureTimeout = 30 * time.Minute

// featureSteps are the steps of workflows/feature up to and including `pr`,
// each with the evidence its definition names. Everything after `pr` — the
// reviewer and the merge gate — is the `review` node's to prove.
var featureSteps = []struct {
	name     string
	evidence millv1.EvidenceKind
}{
	{"gather-context", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"think", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"plan", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"review-plan", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT},
	{"plan-approval", millv1.EvidenceKind_EVIDENCE_KIND_GATE},
	{"implement", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"simplify", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"commit", millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
	{"clean-tree", millv1.EvidenceKind_EVIDENCE_KIND_EXIT},
	{"pr", millv1.EvidenceKind_EVIDENCE_KIND_URL},
}

// featureFixture is the typo node again, filed for the tier that plans first.
// The change is deliberately trivial: what is under test is the workflow, not
// the code, and a larger change would only buy a longer session.
var featureFixture = fixture{
	Name:  "feature",
	Files: typo.Files,
	Title: typo.Title,
	Body:  typo.Body,
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	It("One node, planned and delivered", func(ctx SpecContext) {
		needsForge()

		s := featureFixture.stage(ctx)

		By("given a feature node on a scratch project with one file to change")
		Expect(s.Project.file(ctx, "README.md", s.Project.DefaultBranch)).To(ContainSubstring(misspelling)) //nolint:misspell // the misspelling is the fixture

		By("when mill run is invoked for it with the feature workflow")
		Expect(s.start(ctx, runOptions{Workflow: "feature", Tier: "feature"}).wait(featureTimeout)).
			To(BeZero(), "mill run exited cleanly")

		By("then every step through pr is closed on the evidence its definition names")
		detail := inspect(ctx, s.NodeRef)
		steps := latest(detail)
		for _, step := range featureSteps {
			d := steps[step.name]
			Expect(d).NotTo(BeNil(), "the ledger has an attempt of %s", step.name)
			Expect(d.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
				"%s closed", step.name)
			Expect(d.GetEvidence().GetKind()).To(Equal(step.evidence),
				"%s closed on the evidence its step names", step.name)
		}

		By("and the plan is an artifact dagger holds for the run")
		// `artifact` evidence only closes when dagger confirms it holds an
		// artifact of that digest, so a closed plan step is the attachment.
		Expect(steps["plan"].GetEvidence().GetArtifact()).NotTo(BeEmpty())

		By("and the plan is in the branch, under docs/plans, naming the run")
		branch := detail.GetSummary().GetRun().GetBranch()
		plans := s.Project.tree(ctx, "docs/plans", branch)
		Expect(plans).To(HaveLen(1), "one plan, named for the run's node and date")
		Expect(s.Project.file(ctx, plans[0], branch)).To(SatisfyAll(
			ContainSubstring("run: "+detail.GetSummary().GetRun().GetId()),
			ContainSubstring("node: "+s.NodeRef)))

		By("and the reviewer ran in a session of its own")
		Expect(steps["review-plan"].GetStepRun().GetSessionId()).NotTo(BeEmpty())
		Expect(steps["review-plan"].GetStepRun().GetSessionId()).
			NotTo(Equal(steps["implement"].GetStepRun().GetSessionId()))

		By("and the pull request the run opened is for its branch")
		mr := mergeRequestFor(s.Project.mergeRequests(ctx), branch)
		Expect(mr.SourceBranch).To(Equal(branch))
		Expect(strings.TrimSpace(steps["pr"].GetEvidence().GetUrl())).NotTo(BeEmpty())
	})
})

// The plan gate, answered by a person (docs/prd.md §8 O1, docs/roadmap.md
// §O1). This is the milestone's whole: a node nobody sized by hand, the
// workflow the run chose for itself, the plan put to a human before
// anything is implemented, and a merge on the default branch afterwards.

// gatedPlanTimeout is how long the first pass has to reach the plan gate:
// the assessment, gather-context, think, plan and review-plan. Five
// sessions, three of them Opus.
const gatedPlanTimeout = 20 * time.Minute

// gatedDeliveryTimeout is how long the pass after the answer has to
// deliver: implement, simplify, commit, clean-tree, pr, review and the
// merge gate.
const gatedDeliveryTimeout = 25 * time.Minute

// gatedFeatureSteps are every step of a delivered `feature` run in order,
// each with the evidence its definition names — the two the run makes for
// itself included. `assess` is `build-workflow`'s one step, which a run with
// no project default runs as step 0, and `select` is the attempt that
// records the choice (docs/domain-model.md §Selection).
var gatedFeatureSteps = []struct {
	name     string
	evidence millv1.EvidenceKind
}{
	{"assess", millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED},
	{"select", millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED},
	{"gather-context", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"think", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"plan", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"review-plan", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT},
	{"plan-approval", millv1.EvidenceKind_EVIDENCE_KIND_GATE},
	{"implement", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"simplify", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"commit", millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
	{"clean-tree", millv1.EvidenceKind_EVIDENCE_KIND_EXIT},
	{"pr", millv1.EvidenceKind_EVIDENCE_KIND_URL},
	{"review", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT},
	{"merge", millv1.EvidenceKind_EVIDENCE_KIND_STATUS},
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: A feature is delivered through the gate
	//   Given a scratch project and a feature-sized node with no workflow named
	//   When mill run is invoked, the plan gate is filed, a person completes it, and mill run is invoked again
	//   Then the node is done with a merge commit on the default branch
	//   And the plan artifact, the review verdict and the assessment are on the run
	It("A feature is delivered through the gate", func(ctx SpecContext) {
		needsForge()

		By("given a scratch project and a feature-sized node with no workflow named")
		s := feature.stage(ctx)
		// `feature` for this project only, and identical to the delivering
		// workflow but for the one thing the scenario is about: the plan
		// gate has no `auto`, so it is always put to a person. Everything
		// the assessment may choose between is still what a deployment has.
		dir := deriveGatedFeature(ctx, s.Graph.Slug)
		// Before a session is spent: what the assessment will be offered is
		// what this suite pushed and nothing else.
		expectCandidates(ctx, s.Graph.Slug, "feature", "quick-fix")
		notices := recordNotices()
		options := runOptions{Assess: true, WorkflowDir: dir, NotifyCommand: notices.Command}

		By("when mill run is invoked")
		Expect(s.start(ctx, options).wait(gatedPlanTimeout)).To(BeZero(), "the first mill run exited cleanly")
		detail := inspect(ctx, s.NodeRef)
		runID := detail.GetSummary().GetRun().GetId()

		By("then the run sized the node for itself and chose feature")
		assessment := latest(detail)["select"].GetEvidence().GetStructured().AsMap()
		AddReportEntry("the assessment", assessment)
		Expect(assessment["basis"]).To(Equal("assessment"), "no workflow was named on the command line")
		Expect(assessment["workflow"]).To(Equal("feature"))
		Expect(assessment["tier"]).To(Equal("feature"))
		Expect(assessment["signals"]).NotTo(BeEmpty(), "the assessment rests on checkable facts")
		// The workflow the run walked, and the definition it walked: the
		// project's own, which is the org `feature` with the gate's `auto`
		// removed. A run that resolved the org definition instead would
		// carry the same name and auto-approve the plan.
		Expect(detail.GetWorkflow().GetName()).To(Equal("feature"))
		Expect(detail.GetWorkflow().GetScope()).To(Equal(millv1.Scope_SCOPE_PROJECT))
		Expect(detail.GetWorkflow().GetProject()).To(Equal(s.Graph.Slug))

		By("and the plan gate is filed and the run waits on a person")
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
		filed := s.Graph.gateNodes(ctx, runID)
		Expect(filed).To(HaveLen(1), "one gate: the plan's")
		gate := filed[0]
		AddReportEntry("the gate", gate.Reference+" — "+gate.Title+"\n\n"+gate.Body)
		Expect(gate.Body).To(ContainSubstring(s.NodeRef))

		By("and the ask quotes the plan the run wrote")
		plan := s.Graph.show(ctx, s.NodeRef).artifactOf("plan")
		Expect(plan).NotTo(BeEmpty(), "the plan is an artifact on the node")
		Expect(gate.Body).To(ContainSubstring(firstLine(plan)))

		By("and the notify command was handed the gate")
		carried := notices.read()
		AddReportEntry("the notify payload", carried)
		Expect(carried).To(ContainElement(SatisfyAll(
			HaveField("Kind", notify.KindPlan),
			HaveField("Gate", gate.Reference),
			HaveField("Node", s.NodeRef),
			HaveField("Run", runID))), "a plan notice naming the gate reached the notifier")

		By("when a person completes the gate")
		// As the person and not as the mill: `dg complete` under the
		// operator's own configuration, with no lease, which is what a
		// human answering in dagger does.
		s.Graph.complete(ctx, gate.Reference, "Approved by the acceptance suite, standing in for the person the gate was filed for.")

		By("and mill run is invoked again")
		Expect(s.start(ctx, options).wait(gatedDeliveryTimeout)).To(BeZero(), "the second mill run exited cleanly")

		By("then the node is done with a merge commit on the default branch")
		view := s.Graph.show(ctx, s.NodeRef)
		Expect(view.Node.Status).To(Equal("done"))
		merged := view.artifact("merge-commit")
		AddReportEntry("the merge commit", merged)
		Expect(merged).To(MatchRegexp(`^[0-9a-f]{40}$`), "the artifact is a commit SHA")
		Expect(s.Project.branchesContaining(ctx, merged)).To(ContainElement(s.Project.DefaultBranch))

		By("and the plan artifact, the review verdict and the assessment are on the run")
		detail = inspect(ctx, s.NodeRef)
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		steps := latest(detail)
		Expect(steps["plan"].GetEvidence().GetArtifact()).NotTo(BeEmpty(),
			"dagger confirmed it holds the plan of that digest")
		verdicts := verdictNames(view)
		Expect(verdicts).NotTo(BeEmpty(), "the reviewer reached a verdict")
		approved := view.attachment(verdicts[len(verdicts)-1])
		AddReportEntry("the reviewer's verdict", approved)
		Expect(approved).To(ContainSubstring("- decision: approve"))
		Expect(latest(detail)["select"].GetEvidence().GetStructured().AsMap()["basis"]).To(Equal("assessment"))

		By("and every step closed on the evidence its definition names")
		for _, step := range gatedFeatureSteps {
			d := steps[step.name]
			Expect(d).NotTo(BeNil(), "the ledger has an attempt of %s", step.name)
			Expect(d.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
				"%s closed", step.name)
			Expect(d.GetEvidence().GetKind()).To(Equal(step.evidence),
				"%s closed on the evidence its step names", step.name)
		}

		By("and mill inspect shows no closed step with unspecified evidence")
		// The tool an operator reads, not the store it reads from: what the
		// criterion is about is what `mill inspect` prints. An attempt that
		// failed or was superseded closed on nothing and says so; a closed
		// one that said `unspecified` would be a step the mill let through.
		printed := millInspect(ctx, s.NodeRef)
		AddReportEntry("mill inspect", printed)
		for _, row := range closedRows(printed) {
			Expect(row).NotTo(ContainSubstring("unspecified"), "a closed attempt with no evidence: %s", row)
		}

		By("and dagger holds what the run cost, across at least three models")
		Expect(view.Usage.Actual).NotTo(BeEmpty(), "usage reached dagger")
		models := map[string]bool{}
		for _, total := range view.Usage.Actual {
			models[total.Model] = true
			Expect(total.InputTokens+total.OutputTokens).To(BeNumerically(">", 0),
				"model %s billed something", total.Model)
		}
		Expect(len(models)).To(BeNumerically(">=", 3),
			"the workflow's models are on the record; got %s", strings.Join(modelNames(models), ", "))
	})
})

// recordNotices writes the notify command the suite configures: a script
// that keeps its standard input and tells nobody, which is the smallest
// thing a deployment's notifier can be (internal/notify).
func recordNotices() *noticeLog {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	log := &noticeLog{
		Command: filepath.Join(dir, "notify"),
		path:    filepath.Join(dir, "notices.jsonl"),
	}
	script := "#!/bin/sh\n# The acceptance suite's notifier: keep the notice, wake nobody.\nexec cat >>\"" + log.path + "\"\n"
	Expect(os.WriteFile(log.Command, []byte(script), 0o700)).To(Succeed()) //nolint:gosec // a program, which has to be executable
	return log
}

// noticeLog is that command and the file it appends each notice to, one
// JSON object per line.
type noticeLog struct {
	Command string
	path    string
}

// read is every notice the command received, in order. A run that filed no
// gate wrote no file, which is no notices rather than an error.
func (l *noticeLog) read() []notify.Notice {
	GinkgoHelper()
	content, err := os.ReadFile(l.path) //nolint:gosec // a path the suite made
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	var notices []notify.Notice
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var notice notify.Notice
		Expect(json.Unmarshal([]byte(line), &notice)).To(Succeed(), "a notice the command received: %s", line)
		notices = append(notices, notice)
	}
	return notices
}

// artifactOf is the text of the artifact a step attached to the node, found
// by the name the run gives it: `<run>-<step>-<attempt>`
// (internal/run/verify.go, attachArtifact).
func (v nodeView) artifactOf(step string) string {
	named := regexp.MustCompile(`-` + regexp.QuoteMeta(step) + `-\d+$`)
	for _, artifact := range v.Artifacts {
		if named.MatchString(artifact.Name) {
			return artifact.text()
		}
	}
	return ""
}

// firstLine is a document's first line with anything in it, which is enough
// of a plan to recognise it quoted inside a gate's ask.
func firstLine(document string) string {
	for _, line := range strings.Split(document, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// closedRow matches one `mill inspect` row for an attempt that closed: the
// step, the attempt, the status, and the rest of the row — the exit, the
// evidence kind and the evidence itself.
var closedRow = regexp.MustCompile(`(?m)^(\S+)\s+(\d+)\s+closed\s+(.*)$`)

// closedRows are those rows, as printed.
func closedRows(printed string) []string { return closedRow.FindAllString(printed, -1) }
