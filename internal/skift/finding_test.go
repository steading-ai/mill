package skift_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/testsupport"
)

// docs/architecture.md §"Failure and stop": a healing reconciler's own
// non-gate node — filed, commented, dropped — kept to the finding's own
// project whichever client instance it is called on.

var _ = Describe("A healing reconciler's finding node", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	It("files as a bug an agent may claim, carrying the healing label and the caller's own", func() {
		filed, err := f.mill.FileFinding(ctx, "mill-spec-file-finding", skift.Finding{
			Title:  "main is red at abc1234: build",
			Body:   "the repository's default branch is red",
			Labels: []string{skift.KeyLabel("red-main:group/thing:main")},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(filed.GetTitle()).To(Equal("main is red at abc1234: build"))
		Expect(filed.GetBody()).To(Equal("the repository's default branch is red"))
		Expect(filed.GetKind()).To(Equal("bug"))
		Expect(filed.GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT))
		Expect(filed.GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P1))
		Expect(filed.GetParentId()).To(BeEmpty())
		Expect(filed.GetLabels()).To(ContainElements(skift.HealingLabel, skift.KeyLabel("red-main:group/thing:main")))
	})

	It("drops a finding nobody claimed, the positive case beside a user node's refusal", func() {
		filed, err := f.mill.FileFinding(ctx, "mill-spec-drop-file", skift.Finding{Title: "t", Body: "b"})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.mill.DropFinding(ctx, "mill-spec-drop-finding", filed.GetReference(),
			"main is green again")).To(Succeed())

		node, err := f.mill.Node(ctx, filed.GetReference())
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetStatus()).To(Equal(skiftv1.Status_STATUS_DROPPED))
	})

	It("comments on a finding node", func() {
		filed, err := f.mill.FileFinding(ctx, "mill-spec-comment-file", skift.Finding{Title: "t", Body: "b"})
		Expect(err).NotTo(HaveOccurred())

		Expect(f.mill.CommentFinding(ctx, "mill-spec-comment-green", filed.GetReference(),
			"main is green now")).To(Succeed())

		comments, err := f.mill.Comments(ctx, filed.GetReference())
		Expect(err).NotTo(HaveOccurred())
		Expect(comments[len(comments)-1].GetBody()).To(Equal("main is green now"))
	})

	It("lists every open finding node in the client's own project and none of another's", func() {
		second := f.secondProject(ctx)
		mine, err := f.mill.FileFinding(ctx, "mill-spec-findings-mine", skift.Finding{Title: "mine", Body: "b"})
		Expect(err).NotTo(HaveOccurred())

		theirs := f.unscoped().Scoped(second)
		_, err = theirs.FileFinding(ctx, "mill-spec-findings-theirs", skift.Finding{Title: "theirs", Body: "b"})
		Expect(err).NotTo(HaveOccurred())

		found, err := f.mill.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1))
		Expect(found[0].GetReference()).To(Equal(mine.GetReference()))
	})

	It("reports a finding claimed once a claim is taken on it", func() {
		filed, err := f.mill.FileFinding(ctx, "mill-spec-findings-claimed", skift.Finding{Title: "t", Body: "b"})
		Expect(err).NotTo(HaveOccurred())

		_, err = f.mill.Claim(ctx, filed.GetReference())
		Expect(err).NotTo(HaveOccurred())

		found, err := f.mill.Findings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(HaveLen(1))
		Expect(found[0].GetClaimed()).To(BeTrue())
	})
})
