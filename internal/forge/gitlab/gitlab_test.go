package gitlab_test

import (
	"context"
	"errors"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
)

var _ = Describe("The GitLab forge adapter", Label("unit"), func() {
	var (
		api    *stub
		client forge.Forge
	)

	const branch = "mill/mill-13"

	BeforeEach(func() {
		api = newStub()
		client = gitlab.New(api.repo(), api.minter(), gitlab.WithHTTPClient(api.server.Client())).
			ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442", Capabilities: []string{"forge"}})
	})

	Describe("pushing the run's branch", func() {
		It("pushes with a credential that reaches git through its environment, not its remote or its argv", func(ctx context.Context) {
			git := &recordingGit{head: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"}

			sha, err := client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())
			Expect(sha).To(Equal("9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"))

			push := git.runs[0]
			Expect(push.Args).To(ContainElement("refs/heads/" + branch + ":refs/heads/" + branch))
			Expect(push.Args).To(ContainElement(api.repo().CloneURL))
			Expect(git.argv()).NotTo(ContainSubstring(theToken), "the credential must not be in argv")
			Expect(push.Env).To(HaveKeyWithValue("MILL_FORGE_TOKEN", theToken))
			Expect(api.repo().CloneURL).NotTo(ContainSubstring(theToken))
		})

		It("mints a credential for the step it is bound to, per call", func(ctx context.Context) {
			git := &recordingGit{head: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"}

			_, err := client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())

			Expect(api.minted).To(Equal(2), "a credential is minted per request, never held")
			Expect(api.forStep[0].TokenHash).To(Equal("e3b0c442"))
		})
	})

	Describe("the pull request", func() {
		It("opens one and returns its URL", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_empty.json")
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.on(http.MethodPost, escaped+"/merge_requests", http.StatusCreated, "merge_request_opened.json")

			pr, err := client.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Title: "mill#13: the forge adapter"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.URL).To(Equal("https://gitlab.example.test/group/thing/-/merge_requests/7"))
			Expect(pr.Number).To(Equal(7))
			Expect(pr.Branch).To(Equal(branch))
			Expect(pr.Target).To(Equal("main"), "the project's default branch")
			Expect(pr.Open()).To(BeTrue())
		})

		It("returns the same merge request for the same branch on a second call, opening nothing", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")

			first, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			second, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(first))

			opened, err := client.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Title: "a second attempt"})
			Expect(err).NotTo(HaveOccurred())
			Expect(opened).To(Equal(first), "opening twice is opening once")
			Expect(api.count(http.MethodPost, escaped+"/merge_requests")).To(BeZero())
		})

		It("says so when the branch has no merge request", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_empty.json")

			_, err := client.PR(ctx, branch)
			Expect(errors.Is(err, forge.ErrNoPullRequest)).To(BeTrue())
		})
	})

	Describe("the checks on a commit", func() {
		const sha = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"

		DescribeTable("reports what the pipeline says",
			func(ctx context.Context, fixture string, expected forge.Status) {
				api.on(http.MethodGet, escaped+"/pipelines", http.StatusOK, fixture)

				Expect(client.Checks(ctx, sha)).To(Equal(expected))
			},
			Entry("a pipeline still running is pending", "pipelines_pending.json", forge.Pending),
			Entry("a green pipeline is success", "pipelines_success.json", forge.Success),
			Entry("a red pipeline is failed", "pipelines_failed.json", forge.Failed),
			Entry("no pipeline at all is success: the project configures none", "pipelines_empty.json", forge.Success),
		)
	})

	Describe("merging", func() {
		It("merges a green merge request, squashing because the project's setting says so", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
			api.on(http.MethodPut, escaped+"/merge_requests/7/merge", http.StatusOK, "merge_request_squash_merged.json")
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_squash_merged.json")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())

			merged, err := client.Merge(ctx, pr)
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Merged()).To(BeTrue())

			var body string
			for _, seen := range api.calls() {
				if seen.Method == http.MethodPut {
					body = seen.Body
				}
			}
			Expect(body).To(ContainSubstring(`"squash":true`))

			Expect(client.MergeCommit(ctx, merged)).To(Equal("1c4a5b6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"))
		})

		It("does not squash when the project's setting says not to", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project_no_squash.json")
			api.on(http.MethodPut, escaped+"/merge_requests/8/merge", http.StatusOK, "merge_request_merged.json")

			merged, err := client.Merge(ctx, forge.PullRequest{Number: 8, Branch: branch, State: "open"})
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.MergeCommit).To(Equal("5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f"))

			var body string
			for _, seen := range api.calls() {
				if seen.Method == http.MethodPut {
					body = seen.Body
				}
			}
			Expect(body).To(ContainSubstring(`"squash":false`))
		})

		It("returns a typed refusal naming GitLab's reason, which the mill does not retry", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_request_not_approved.json")
			api.on(http.MethodPut, escaped+"/merge_requests/9/merge", http.StatusMethodNotAllowed, "merge_refused.json")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())

			_, err = client.Merge(ctx, pr)
			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.PR).To(Equal(9))
			Expect(refused.Status).To(Equal(http.StatusMethodNotAllowed))
			Expect(refused.Reason).To(Equal("Branch cannot be merged"), "GitLab's own words")
			Expect(refused.Detail).To(Equal("not_approved"))
			Expect(err.Error()).To(ContainSubstring("Branch cannot be merged"))
			Expect(forge.Retryable(err)).To(BeFalse())

			before := api.count(http.MethodPut, escaped+"/merge_requests/9/merge")
			Expect(before).To(Equal(1), "one attempt, and no retry inside the adapter")
		})

		It("refuses to read a merge commit before the merge", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_opened.json")

			_, err := client.MergeCommit(ctx, forge.PullRequest{Number: 7})
			Expect(errors.Is(err, forge.ErrNotMerged)).To(BeTrue())
		})
	})

	It("Squash merge evidence is the merge commit, not the branch", func(ctx context.Context) {
		// Given an MR from branch B squash-merged into main.
		api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_squash_merged.json")
		api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_squash_merged.json")
		pr, err := client.PR(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.Merged()).To(BeTrue())

		// When MergeCommit is read.
		commit, err := client.MergeCommit(ctx, pr)
		Expect(err).NotTo(HaveOccurred())

		// Then it is a SHA on main that is not B's head, and B may no
		// longer exist: the branch was removed on merge, so nothing but
		// the merge request could have said what the commit is.
		Expect(commit).To(Equal("1c4a5b6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"))
		Expect(commit).NotTo(Equal(pr.HeadSHA), "the squash commit is a new commit, not the branch's head")
		Expect(pr.Target).To(Equal("main"))
		Expect(api.count(http.MethodGet, escaped+"/repository/branches")).To(BeZero(),
			"the evidence is read from the merge request, never from the branch")
	})
})
