package run

// The predicate language, specified directly. It is a language, small as it
// is, and the gate specs exercise one sentence of it; these are the rest.
//
// The file is in package run rather than run_test because what is being
// specified — the evaluator and the environment it reads — is deliberately
// not exported. Nothing outside this package evaluates a predicate.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

var _ = Describe("A gate's auto predicate", Label("unit"), func() {
	var env predicateEnv

	BeforeEach(func() {
		env = newPredicateEnv(
			&millv1.Run{Tier: millv1.Tier_TIER_FEATURE},
			&millv1.InspectResponse{Steps: []*millv1.StepRunDetail{
				{
					StepRun: &millv1.StepRun{StepName: "review-plan"},
					Evidence: &millv1.Evidence{
						Kind: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
						Value: &millv1.Evidence_Verdict{Verdict: &millv1.Verdict{
							Decision: millv1.VerdictDecision_VERDICT_DECISION_REVISE,
						}},
					},
				},
				{
					StepRun: &millv1.StepRun{StepName: "review"},
					Evidence: &millv1.Evidence{
						Kind: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
						Value: &millv1.Evidence_Verdict{Verdict: &millv1.Verdict{
							Decision: millv1.VerdictDecision_VERDICT_DECISION_APPROVE,
						}},
					},
				},
				{
					StepRun: &millv1.StepRun{StepName: "pr"},
					Evidence: &millv1.Evidence{
						Kind:  millv1.EvidenceKind_EVIDENCE_KIND_URL,
						Value: &millv1.Evidence_Url{Url: "https://forge.test/mr/1"},
					},
				},
			}})
	})

	holds := func(predicate string) bool {
		GinkgoHelper()
		result, err := evaluate(predicate, env)
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	It("is false when it is empty: a gate with no policy is a gate for a human", func() {
		Expect(holds("")).To(BeFalse())
		Expect(holds("   ")).To(BeFalse())
	})

	It("closes on the literal true and never on the literal false", func() {
		Expect(holds("true")).To(BeTrue())
		Expect(holds("false")).To(BeFalse())
	})

	It("reads prior evidence by step name and by evidence kind", func() {
		Expect(holds("verdict.decision == approve")).To(BeTrue())
		Expect(holds("review.decision == approve")).To(BeTrue())
		Expect(holds("review.decision == revise")).To(BeFalse())
		// A bare word is a string literal, but one carrying dots would be
		// read as a path into the environment, so a URL is quoted.
		Expect(holds("url == 'https://forge.test/mr/1'")).To(BeTrue())
	})

	It("orders tiers by rank rather than by spelling", func() {
		Expect(holds("tier <= feature")).To(BeTrue())
		Expect(holds("tier <= small")).To(BeFalse())
		Expect(holds("tier > quick-fix")).To(BeTrue())
		Expect(holds("large >= tier")).To(BeTrue())
	})

	It("joins comparisons with &&, ||, ! and parentheses", func() {
		Expect(holds("verdict.decision == approve && tier <= feature")).To(BeTrue())
		Expect(holds("verdict.decision == approve && tier <= small")).To(BeFalse())
		Expect(holds("verdict.decision == revise || tier <= feature")).To(BeTrue())
		Expect(holds("!(tier <= small)")).To(BeTrue())
	})

	It("is false for evidence the run has not produced", func() {
		// Undefined equals nothing, so a gate whose evidence does not exist
		// is answered by a human rather than by an accident.
		Expect(holds("plan.approved == true")).To(BeFalse())
		Expect(holds("plan.approved == false")).To(BeFalse())
		Expect(holds("verdict.missing == approve")).To(BeFalse())
	})

	It("compares numbers numerically", func() {
		Expect(holds("verdict.blocking == 0")).To(BeTrue())
		Expect(holds("verdict.findings < 1")).To(BeTrue())
	})

	Describe("verdict(<step>)", func() {
		It("reads the named step's verdict, which a run with two reviewers needs", func() {
			Expect(holds("verdict('review').decision == approve")).To(BeTrue())
			Expect(holds("verdict('review-plan').decision == revise")).To(BeTrue())
			Expect(holds("verdict('review-plan').decision == approve")).To(BeFalse())
			Expect(holds("verdict('review-plan').decision == approve && tier <= feature")).To(BeFalse())
			Expect(holds("verdict('review').decision == approve && tier <= feature")).To(BeTrue())
		})

		It("is false for a step that produced no verdict, and records the reason", func() {
			Expect(holds("verdict('review-nothing').decision == approve")).To(BeFalse())
			Expect(holds("verdict('pr').decision == approve")).To(BeFalse(),
				"pr closed on a url, not a verdict")
			Expect(holds("verdict('review').missing == approve")).To(BeFalse())

			Expect(env.why()).To(ConsistOf(
				ContainSubstring("no step named review-nothing has closed on a verdict"),
				ContainSubstring("no step named pr has closed on a verdict"),
				ContainSubstring("the verdict of review has no missing"),
			))
		})

		It("refuses a call with no step to name", func() {
			_, err := evaluate("verdict() == approve", env)
			Expect(err).To(MatchError(ContainSubstring("needs the name of a step")))
		})
	})

	It("refuses a predicate that is not one, naming what it choked on", func() {
		_, err := evaluate("verdict.decision ==", env)
		Expect(err).To(MatchError(ContainSubstring("operand is missing")))

		_, err = evaluate("tier <= enormous", env)
		Expect(err).To(MatchError(ContainSubstring("is not a tier")))

		_, err = evaluate("approve", env)
		Expect(err).To(MatchError(ContainSubstring("not a condition")))
	})
})
