package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/notify"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// Plan approval, and the notifier that carries a filed gate to a person.
//
// The workflow is `feature`'s own, because the policy under test is written
// in its definition and not in Go: `verdict('review-plan').decision ==
// approve && tier <= feature`. What the mill contributes is the evaluation,
// the gate, and the one errand it runs when a gate is filed
// (docs/prd.md R-21 and D1, docs/architecture.md §"Gate steps").

// notifyScript writes a notify command that records the notice it was given
// and ends the way the spec says. A notify command is somebody else's
// script, so the specs run one rather than a fake in Go.
func notifyScript(body string) (command *notify.Command, noticePath string) {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	noticePath = filepath.Join(dir, "notice.json")
	path := filepath.Join(dir, "notify.sh")
	Expect(os.WriteFile(path, []byte("#!/bin/sh\ncat >> "+noticePath+"\n"+body), 0o700)).To(Succeed()) //nolint:gosec // a spec's own scratch script
	return &notify.Command{Path: path}, noticePath
}

// notices is every notice the script was handed, one JSON document per line.
func notices(path string) []string {
	GinkgoHelper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path this spec made
	if os.IsNotExist(err) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// notified is what the audit trail says about carrying a gate to a person:
// the kind, the gate, the command's exit status and its diagnostic.
func (f *fixture) notified(ctx context.Context) []map[string]any {
	GinkgoHelper()
	res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
	Expect(err).NotTo(HaveOccurred())
	var found []map[string]any
	for _, event := range res.Msg.GetEvents() {
		if event.GetOp() == "notified" {
			found = append(found, event.GetAfter().AsMap())
		}
	}
	return found
}

var _ = Describe("Plan approval", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context
	var planned string

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.workflow = f.pushWorkflowFrom(ctx, featureDir)
		f.node(ctx, "one line change", "Change the README's first line.")
		planned = ""
	})

	// writesPlan is the plan session: it writes the file the mill reads back
	// as the step's artifact and quotes into the gate's ask.
	writesPlan := func(harness.Session) {
		defer GinkgoRecover()
		planned = planPath(f.nodeRef)
		Expect(f.workspace.Write(context.Background(), planned,
			[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
	}

	// upToTheGate scripts every session from gather-context to review-plan,
	// with review-plan answering as the spec says.
	upToTheGate := func(decision string) []fake.Script {
		return []fake.Script{
			{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			{Terminal: fake.Terminal("explored the problem", nil, usage(10))},
			{Terminal: fake.Terminal("planned", nil, usage(10)), Do: writesPlan},
			{Terminal: fake.Terminal("", decides(decision), usage(5))},
		}
	}

	// therest is everything after the gate: implement through review, each
	// turn leaving a commit so the clean-tree check and the commit step have
	// something to close on.
	therest := func() []fake.Script {
		return []fake.Script{
			{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			{Terminal: fake.Terminal("simplified", nil, usage(10)), Do: f.commits},
			{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
			{Terminal: fake.Terminal("", approves("AC-1", "the change matches the plan"), usage(5))},
		}
	}

	// featureRunner drives the `feature` workflow at the given tier.
	featureRunner := func(tier millv1.Tier, options ...func(*run.Options)) *run.Runner {
		all := append([]func(*run.Options){func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
			o.Tier = tier
		}}, options...)
		return f.runner(all...)
	}

	Describe("An approved plan does not wait", func() {
		It("An approved plan does not wait", func() {
			By("given a feature run whose review-plan verdict is approve")
			command, noticePath := notifyScript("exit 0")
			f.script(append(upToTheGate("approve"), therest()...)...)

			By("when the plan-approval gate is evaluated")
			Expect(featureRunner(millv1.Tier_TIER_FEATURE, func(o *run.Options) {
				o.Notifier = command
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			By("then it is auto-approved and nothing is filed in dagger")
			var approval *millv1.StepRun
			for _, sr := range stepRuns(f.inspect(ctx), "plan-approval") {
				approval = sr
			}
			Expect(approval).NotTo(BeNil())
			Expect(approval.GetAutoApproved()).To(BeTrue())
			Expect(closedSteps(f.inspect(ctx))).To(
				HaveKeyWithValue("plan-approval", millv1.EvidenceKind_EVIDENCE_KIND_GATE))
			Expect(f.gatesOn(ctx)).To(BeEmpty())
			Expect(notices(noticePath)).To(BeEmpty(), "nobody was asked, so nobody was told")
		})

		It("auto-approves a quick fix too: quick-fix is at most a feature", func() {
			f.script(append(upToTheGate("approve"), therest()...)...)

			Expect(featureRunner(millv1.Tier_TIER_QUICK_FIX).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_QUICK_FIX))
			Expect(f.gatesOn(ctx)).To(BeEmpty())
			Expect(closedSteps(f.inspect(ctx))).To(
				HaveKeyWithValue("plan-approval", millv1.EvidenceKind_EVIDENCE_KIND_GATE))
		})

		It("a revised plan never reaches the gate: it goes back to plan", func() {
			// review-plan revises once and approves the second plan. The
			// gate is reached once, after an approval, and never on the
			// verdict that sent the run back (workflows/feature: on_failure
			// revise plan).
			f.script(append(append(upToTheGate("revise"),
				fake.Script{Terminal: fake.Terminal("planned again", nil, usage(10)), Do: writesPlan},
				fake.Script{Terminal: fake.Terminal("", decides("approve"), usage(5))}),
				therest()...)...)

			Expect(featureRunner(millv1.Tier_TIER_FEATURE).Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "plan")).To(HaveLen(2), "the revise sent the run back to plan")
			Expect(stepRuns(detail, "review-plan")).To(HaveLen(2))
			Expect(stepRuns(detail, "plan-approval")).To(HaveLen(1),
				"the gate was reached once, after the approval")
			Expect(f.gatesOn(ctx)).To(BeEmpty())
		})
	})

	Describe("A gate a person has to answer", func() {
		// asked is `feature` with the plan-approval policy taken out, which
		// is every plan the policy does not cover: a larger tier, or a
		// reviewer who did not approve.
		asked := func() string {
			return f.variant(ctx, "asked-feature", func(definition *millv1.Workflow) {
				stepOf(definition, "plan-approval").GetGate().Auto = "false"
			})
		}

		It("A person is asked, and told", func() {
			By("given a feature run at plan-approval with auto evaluating false")
			command, noticePath := notifyScript("exit 0")
			workflow := asked()
			f.script(upToTheGate("approve")...)

			By("when the gate is filed")
			Expect(featureRunner(millv1.Tier_TIER_FEATURE, func(o *run.Options) {
				o.Workflow = workflow
				o.Notifier = command
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			By("then the notify command receives the gate reference and title on stdin")
			gate := f.theGate(ctx)
			Expect(notices(noticePath)).To(HaveLen(1))
			notice := notices(noticePath)[0]
			Expect(notice).To(ContainSubstring(`"gate":"` + gate.GetReference() + `"`))
			Expect(notice).To(ContainSubstring(`"node":"` + f.nodeRef + `"`))
			Expect(notice).To(ContainSubstring(`"kind":"plan"`))
			Expect(notice).To(ContainSubstring("Approve the plan for " + f.nodeRef + "?"))
			Expect(notice).To(ContainSubstring("# The plan"), "the ask quotes the plan itself")

			By("and the run is waiting on the gate")
			waiting := f.runOf(ctx)
			Expect(waiting.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
			Expect(waiting.GetGateNodeId()).To(Equal(gate.GetId()))
			Expect(waiting.GetLeaseId()).To(BeEmpty())
			Expect(f.notified(ctx)).To(HaveLen(1))
			Expect(f.notified(ctx)[0]).To(HaveKeyWithValue("exit_code", BeNumerically("==", 0)))
		})

		It("continues at implement once the gate is completed", func() {
			workflow := asked()
			f.script(append(upToTheGate("approve"), therest()...)...)
			runner := featureRunner(millv1.Tier_TIER_FEATURE, func(o *run.Options) { o.Workflow = workflow })
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())
			gate := f.theGate(ctx)

			f.approveGate(ctx, gate.GetReference())
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "plan-approval")).To(HaveLen(1),
				"waiting for a human is not a second attempt")
			Expect(evidenceOf(detail, "plan-approval").GetGate()).To(Equal(gate.GetId()))
			Expect(closedSteps(detail)).To(HaveKey("implement"))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		})

		It("fails the run with the step's deny_result when the gate is dropped", func() {
			// A dropped gate never satisfies the edge out of it, so the run
			// that records the denial is the one still holding the lease:
			// single-run mode, where the ask goes to the terminal.
			workflow := asked()
			f.script(upToTheGate("approve")...)
			person := &answerer{approve: false}

			Expect(featureRunner(millv1.Tier_TIER_FEATURE, withAnswers(person),
				func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(person.answered()).To(HaveLen(1))
			Expect(person.answered()[0].Title).To(Equal("Approve the plan for " + f.nodeRef + "?"))
			Expect(f.theGate(ctx).GetStatus()).To(Equal(daggerv1.Status_STATUS_DROPPED))

			result, err := f.result(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.GetBody()).To(ContainSubstring("rejected at the plan-approval gate"))
			Expect(result.GetBody()).To(ContainSubstring("nothing was implemented and the run stopped"))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
			Expect(closedSteps(f.inspect(ctx))).NotTo(HaveKey("implement"))
		})

		Describe("Notification failure is not run failure", func() {
			It("Notification failure is not run failure", func() {
				By("given a notify command that exits 1")
				command, noticePath := notifyScript("echo 'no agent named ops' >&2; exit 1")
				workflow := asked()
				f.script(upToTheGate("approve")...)

				By("when a gate is filed")
				Expect(featureRunner(millv1.Tier_TIER_FEATURE, func(o *run.Options) {
					o.Workflow = workflow
					o.Notifier = command
				}).Execute(ctx, f.nodeRef)).To(Succeed())

				By("then the gate exists in dagger")
				gate := f.theGate(ctx)
				Expect(gate.GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
				Expect(f.blockedBy(ctx)).To(ContainElement(gate.GetId()))
				Expect(notices(noticePath)).To(HaveLen(1))

				By("and the run's history records the command's exit")
				Expect(f.notified(ctx)).To(HaveLen(1))
				recorded := f.notified(ctx)[0]
				Expect(recorded).To(HaveKeyWithValue("exit_code", BeNumerically("==", 1)))
				Expect(recorded).To(HaveKeyWithValue("gate", gate.GetReference()))
				Expect(recorded).To(HaveKeyWithValue("kind", "plan"))
				Expect(recorded["detail"]).To(ContainSubstring("no agent named ops"))
				Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
			})

			It("records a notify command that hangs past its bound, and still waits on the gate", func() {
				command, _ := notifyScript("sleep 60")
				command.Timeout = 500 * time.Millisecond
				workflow := asked()
				f.script(upToTheGate("approve")...)

				started := time.Now()
				Expect(featureRunner(millv1.Tier_TIER_FEATURE, func(o *run.Options) {
					o.Workflow = workflow
					o.Notifier = command
				}).Execute(ctx, f.nodeRef)).To(Succeed())
				Expect(time.Since(started)).To(BeNumerically("<", notify.Timeout),
					"the run did not wait on the command longer than the bound")

				Expect(f.theGate(ctx).GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
				Expect(f.notified(ctx)).To(HaveLen(1))
				Expect(f.notified(ctx)[0]).To(HaveKeyWithValue("exit_code", BeNumerically("==", -1)))
				Expect(f.notified(ctx)[0]["detail"]).To(ContainSubstring("did not answer in time"))
				Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
			})
		})
	})
})

// decides is a reviewer's structured output: one decision and the finding
// that carries it. A revise names a blocking finding, because a revise that
// blocks nothing is not a verdict the mill accepts (docs/domain-model.md
// §Verdict).
func decides(decision string) map[string]any {
	severity, summary := "note", "the plan names its proof"
	if decision == "revise" {
		severity, summary = "blocking", "the plan names no proof for AC-1"
	}
	return map[string]any{
		"decision": decision,
		"findings": []any{map[string]any{
			"criterion": "AC-1",
			"severity":  severity,
			"summary":   summary,
			"location":  "docs/plans:1",
		}},
	}
}
