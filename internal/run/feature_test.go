package run_test

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/socket"
)

// The `feature` workflow, driven end to end against a real dagger, ledger,
// git and workspace, with the sessions scripted.
//
// What this is about is the two things `feature` adds to `quick-fix` that the
// mill itself has to do: a step whose artifact is a *file in the checkout*
// rather than what the session said, and a reviewer in a session of its own.
// Both leave a record a person reads afterwards, which is the scenario.

// featureDir is the definition this repository seeds the store with.
const featureDir = "../../workflows/feature"

// planPath is where the plan step's file lands, which is what the mill
// renders from the run and tells the session. The spec computes it the same
// way rather than reading it back, so a change to either side is a failure
// here rather than a spec that agrees with whatever happened.
func planPath(nodeRef string) string {
	slug := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(nodeRef))
	return "docs/plans/" + time.Now().UTC().Format("2006-01-02") + "-" + slug + ".md"
}

// planBody is the file a plan session writes: frontmatter naming the run and
// the node, then the plan.
func planBody(runID, nodeRef string) string {
	return "---\n" +
		"run: " + runID + "\n" +
		"node: " + nodeRef + "\n" +
		"generation: 1\n" +
		"workflow: feature\n" +
		"date: " + time.Now().UTC().Format("2006-01-02") + "\n" +
		"---\n\n" +
		"# The plan\n\n## Steps\n\n1. Change the README's first line, proven by the README's own spec.\n"
}

// approves is a reviewer's structured output: a decision and its findings,
// in the shape docs/domain-model.md §Verdict fixes.
func approves(criterion, summary string) map[string]any {
	return map[string]any{
		"decision": "approve",
		"findings": []any{map[string]any{
			"criterion": criterion,
			"severity":  "note",
			"summary":   summary,
			"location":  "docs/plans:1",
		}},
	}
}

var _ = Describe("The feature workflow", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context
	var planned string
	// refused is what the workspace said when the reviewer's session tried
	// to write under its own step token.
	var refused error

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.pushWorkflowFrom(ctx, featureDir)
		f.node(ctx, "one line change", "Change the README's first line.")
		planned = ""
	})

	// featureRun drives the whole workflow with every session scripted: nine
	// skill turns in order, the plan turn writing the plan file the mill
	// reads back, the two review turns answering with a verdict.
	featureRun := func() {
		GinkgoHelper()
		writesPlan := func(session harness.Session) {
			defer GinkgoRecover()
			planned = planPath(f.nodeRef)
			Expect(f.workspace.Write(context.Background(), planned,
				[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
		}
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("explored the problem", nil, usage(10))},
			fake.Script{Terminal: fake.Terminal("planned", nil, usage(10)), Do: writesPlan},
			fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the plan names its proof"), usage(5))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("simplified", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the change matches the plan"), usage(5)),
				Do: func(session harness.Session) {
					// What a reviewer would be doing if it tried: the step's
					// own credential, on the workspace's own socket. The
					// capability set is mill's half of read-only
					// (docs/domain-model.md §Step).
					refused = socket.Dial(session.Workspace.Socket, session.Workspace.Token).
						Write(context.Background(), "reviewer-was-here.txt", []byte("no"))
				}},
		)
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
		}).Execute(ctx, f.nodeRef)).To(Succeed())
	}

	It("Plan and review-plan leave evidence a person can read", func() {
		By("given a feature run past review-plan")
		f.keepWorkspace = true
		featureRun()

		By("when the run is inspected")
		detail := f.inspect(ctx)
		closed := closedSteps(detail)
		Expect(closed).To(HaveKeyWithValue("plan", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT))
		Expect(closed).To(HaveKeyWithValue("review-plan", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT))

		By("then the plan step's artifact is the docs/plans file's content and its frontmatter names the run")
		attached := artifactText(f.artifactsOn(ctx), evidenceOf(detail, "plan").GetArtifact())
		content, err := f.workspace.Read(ctx, planned)
		Expect(err).NotTo(HaveOccurred())
		Expect(attached).To(Equal(string(content)), "the attached artifact is the file, byte for byte")
		Expect(attached).To(ContainSubstring("run: " + f.runOf(ctx).GetId()))
		Expect(attached).To(ContainSubstring("node: " + f.nodeRef))
		Expect(attached).NotTo(ContainSubstring("planned"), "the session's last word closed nothing here")

		By("and the review-plan step's verdict names its decision and findings")
		verdict := evidenceOf(detail, "review-plan").GetVerdict()
		Expect(verdict.GetDecision()).To(Equal(millv1.VerdictDecision_VERDICT_DECISION_APPROVE))
		Expect(verdict.GetFindings()).To(HaveLen(1))
		Expect(verdict.GetFindings()[0].GetCriterion()).To(Equal("AC-1"))
		Expect(verdict.GetFindings()[0].GetSummary()).To(Equal("the plan names its proof"))
	})

	It("the plan the run delivers is committed with the change", func() {
		f.keepWorkspace = true
		featureRun()

		tracked, err := f.workspace.GitRunner().Run(ctx, nil, "ls-tree", "--name-only", "-r", f.runOf(ctx).GetBranch())
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(tracked)).To(ContainElement(planned))
	})

	It("the reviewer runs in a session of its own, and cannot write through the socket", func() {
		refused = nil
		featureRun()

		By("the workspace refuses a write under the review step's token")
		Expect(refused).To(HaveOccurred())
		var refusal *socket.Error
		Expect(errors.As(refused, &refusal)).To(BeTrue(), "%v", refused)
		Expect(refusal.Capability).To(Equal(workspace.Write))

		sessions := map[string]string{}
		for _, d := range f.inspect(ctx).GetSteps() {
			if id := d.GetStepRun().GetSessionId(); id != "" {
				sessions[d.GetStepRun().GetStepName()] = id
			}
		}
		Expect(sessions["review"]).NotTo(BeEmpty())
		Expect(sessions["review"]).NotTo(Equal(sessions["implement"]))
		Expect(sessions["review"]).To(Equal(sessions["review-plan"]),
			"one reviewer's session carries both reviews")
		for _, step := range []string{"gather-context", "think", "plan", "commit", "pr"} {
			Expect(sessions[step]).To(Equal(sessions["implement"]), "%s is the implementer's", step)
		}
	})

	It("the mill tells the plan session where to write, and asks the reviewer for a verdict", func() {
		featureRun()

		calls := f.harness.Calls()
		Expect(calls).To(HaveLen(9))
		Expect(calls[2].Prompt.Text).To(ContainSubstring(planPath(f.nodeRef)))
		Expect(calls[2].Prompt.Text).To(ContainSubstring("run: " + f.runOf(ctx).GetId()))
		Expect(calls[2].Prompt.Text).To(ContainSubstring("node: " + f.nodeRef))
		Expect(calls[0].Prompt.Text).NotTo(ContainSubstring("The file this step closes on"),
			"a step that names no file is told about none")
		Expect(calls[3].Prompt.OutputSchema).To(ContainSubstring(`"decision"`),
			"a verdict step is asked for the shape the verifier will accept")
	})
})

// artifactsOn is every artifact dagger holds for the run's node.
func (f *fixture) artifactsOn(ctx context.Context) []*daggerv1.Artifact {
	GinkgoHelper()
	held, err := f.client.Artifacts(ctx, f.nodeRef)
	Expect(err).NotTo(HaveOccurred())
	return held
}

// artifactText is the text of the artifact carrying a digest, which is what
// `artifact` evidence records.
func artifactText(held []*daggerv1.Artifact, digest string) string {
	GinkgoHelper()
	for _, artifact := range held {
		if artifact.GetMetadata().GetFields()["digest"].GetStringValue() != digest {
			continue
		}
		var b strings.Builder
		for _, part := range artifact.GetParts() {
			b.WriteString(part.GetText().GetText())
		}
		return b.String()
	}
	Fail("no artifact with digest " + digest)
	return ""
}
