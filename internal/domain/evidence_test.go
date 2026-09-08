package domain_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/domain"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

var _ = Describe("Evidence validation", Label("unit"), func() {
	DescribeTable("a value is the shape its kind defines",
		func(evidence *millv1.Evidence, accepted bool) {
			err := domain.ValidateEvidence(evidence)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			var refusal *domain.EvidenceError
			Expect(err).To(BeAssignableToTypeOf(refusal))
		},
		Entry("a commit is a git object name", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Commit{Commit: sha},
		}, true),
		Entry("a commit that is not an object name is refused", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Commit{Commit: "HEAD"},
		}, false),
		Entry("an exit carries how the process ended", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
			Value: &millv1.Evidence_Exit{Exit: &harnessv1.ProcessExit{Status: 0}},
		}, true),
		Entry("a verdict that approves closes a review", &millv1.Evidence{
			Kind: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
			Value: &millv1.Evidence_Verdict{Verdict: &millv1.Verdict{
				Decision: millv1.VerdictDecision_VERDICT_DECISION_APPROVE,
			}},
		}, true),
		Entry("a verdict that revises closes nothing", &millv1.Evidence{
			Kind: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT,
			Value: &millv1.Evidence_Verdict{Verdict: &millv1.Verdict{
				Decision: millv1.VerdictDecision_VERDICT_DECISION_REVISE,
			}},
		}, false),
		Entry("a url is absolute", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_URL,
			Value: &millv1.Evidence_Url{Url: "https://forge.example/pr/1"},
		}, true),
		Entry("a bare path is not a url", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_URL,
			Value: &millv1.Evidence_Url{Url: "/pr/1"},
		}, false),
		Entry("a gate names the gate node", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_GATE,
			Value: &millv1.Evidence_Gate{Gate: "gate-node"},
		}, true),
		Entry("a gate with no node id is refused", &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_GATE,
			Value: &millv1.Evidence_Gate{Gate: ""},
		}, false),
		Entry("no value at all is refused", &millv1.Evidence{
			Kind: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
		}, false),
		Entry("no kind at all is refused", &millv1.Evidence{
			Value: &millv1.Evidence_Commit{Commit: sha},
		}, false),
		Entry("no evidence at all is refused", nil, false),
	)

	It("refuses a value belonging to another kind, naming both", func() {
		err := domain.ValidateEvidence(&millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Exit{Exit: &harnessv1.ProcessExit{Status: 0}},
		})
		Expect(err).To(MatchError(ContainSubstring("commit")))
		Expect(err).To(MatchError(ContainSubstring("exit")))
	})

	It("accepts structured output with fields and refuses an empty document", func() {
		document, err := structpb.NewStruct(map[string]any{"workflow": "feature"})
		Expect(err).NotTo(HaveOccurred())
		Expect(domain.ValidateEvidence(&millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			Value: &millv1.Evidence_Structured{Structured: document},
		})).To(Succeed())
		Expect(domain.ValidateEvidence(&millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			Value: &millv1.Evidence_Structured{Structured: &structpb.Struct{}},
		})).To(HaveOccurred())
	})
})
