package gitlab_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/gitlab"
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

		It("OpenPR waits for an existing merge request's branch to reach the requested head before reusing it", func(ctx context.Context) {
			const head = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
			api.onBranchCatchUp(branch, head, 2)
			quick := gitlab.New(api.repo(), api.minter(),
				gitlab.WithHTTPClient(api.server.Client()), gitlab.WithBranchWait(2*time.Second, time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442"})

			pr, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head, Title: "mill#13"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Number).To(Equal(7))
			Expect(api.count(http.MethodGet, escaped+"/repository/branches")).To(Equal(3), "two misses, then the branch")
			Expect(api.count(http.MethodGet, escaped+"/merge_requests")).To(Equal(2), "looked up, then re-read once caught up")
			Expect(api.count(http.MethodPost, escaped+"/merge_requests")).To(BeZero(), "an existing merge request is reused, not recreated")
		})

		It("OpenPR does not reuse a merge request whose branch never reaches the requested head", func(ctx context.Context) {
			const head = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
			api.onBranchCatchUp(branch, head, -1)
			quick := gitlab.New(api.repo(), api.minter(),
				gitlab.WithHTTPClient(api.server.Client()), gitlab.WithBranchWait(20*time.Millisecond, time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442"})

			_, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head})

			var missing *forge.BranchMissingError
			Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
			Expect(missing.Branch).To(Equal(branch))
			Expect(missing.Head).To(Equal(head))
			Expect(api.count(http.MethodGet, escaped+"/merge_requests")).To(Equal(1), "the stale merge request is never trusted")
		})

		It("The forge catches up before the merge request is opened", func(ctx context.Context) {
			// Given a push the forge takes a moment to show: the branch
			// reads 404 twice before GitLab has it at the pushed head, and
			// a merge request opened before then is refused for a source
			// branch that does not exist.
			const head = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_empty.json")
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.onPushedBranch(branch, head, 2, "merge_request_opened.json")

			// When the pr step runs, naming the head it pushed.
			waiting := gitlab.New(api.repo(), api.minter(),
				gitlab.WithHTTPClient(api.server.Client()),
				gitlab.WithBranchWait(2*time.Second, time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442"})
			pr, err := waiting.OpenPR(ctx, forge.OpenPRRequest{
				Branch: branch, Head: head, Title: "mill#46: the pr step waits for the push",
			})

			// Then it opens the merge request after the branch is visible,
			// on the first attempt: one create, and it is the only one the
			// step needed.
			Expect(err).NotTo(HaveOccurred(), "%v", err)
			Expect(pr.URL).To(Equal("https://gitlab.example.test/group/thing/-/merge_requests/7"))
			Expect(api.count(http.MethodPost, escaped+"/merge_requests")).To(Equal(1),
				"the merge request is created once, after the branch appeared")
			Expect(api.count(http.MethodGet, escaped+"/repository/branches")).To(Equal(3),
				"the branch is read until the forge shows it")

			// And nothing was asked of the merge requests endpoint before
			// the branch was there.
			var order []string
			for _, seen := range api.calls() {
				switch {
				case seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/merge_requests"):
					order = append(order, "create")
				case strings.HasPrefix(seen.Path, escaped+"/repository/branches"):
					order = append(order, "branch")
				}
			}
			Expect(order).To(Equal([]string{"branch", "branch", "branch", "create"}))
		})

		It("A push that never landed is a named failure", func(ctx context.Context) {
			// Given a branch that never appears on the forge.
			const head = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_empty.json")
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.onPushedBranch(branch, head, -1, "merge_request_opened.json")

			// When the pr step runs.
			waiting := gitlab.New(api.repo(), api.minter(),
				gitlab.WithHTTPClient(api.server.Client()),
				gitlab.WithBranchWait(20*time.Millisecond, time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442"})
			_, err := waiting.OpenPR(ctx, forge.OpenPRRequest{
				Branch: branch, Head: head, Title: "mill#46: the pr step waits for the push",
			})

			// Then it fails naming the branch and the head it expected, and
			// no merge request was asked for.
			var missing *forge.BranchMissingError
			Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
			Expect(missing.Branch).To(Equal(branch))
			Expect(missing.Head).To(Equal(head))
			Expect(err.Error()).To(ContainSubstring(branch))
			Expect(err.Error()).To(ContainSubstring(head))
			Expect(api.count(http.MethodPost, escaped+"/merge_requests")).To(BeZero(),
				"nothing is created for a branch the forge does not have")
			Expect(api.count(http.MethodGet, escaped+"/repository/branches")).To(BeNumerically(">", 1),
				"the forge is given more than one look")
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

	Describe("the failing checks on a commit, and their logs", func() {
		const sha = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"

		It("gives a failing job its status and a log id to ask a trace for", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/pipelines", http.StatusOK, "pipelines_failed.json")
			api.on(http.MethodGet, escaped+"/pipelines/333/jobs", http.StatusOK, "jobs_failed.json")

			failing, err := client.FailingChecks(ctx, sha)
			Expect(err).NotTo(HaveOccurred())
			Expect(failing).To(ConsistOf(forge.CheckResult{
				Name: "e2e", URL: "https://gitlab.example.test/group/thing/-/jobs/987",
				Conclusion: "failed", LogID: "987",
			}))
		})

		It("reads a job's trace, filtered to the failing assertion", func(ctx context.Context) {
			body := "noise\nrun-verbs.spec.ts:67\nError: expect(locator).toBeDisabled() failed\nmore noise\n"
			api.onBody(http.StatusOK, []byte(body))

			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "987"})
			Expect(err).NotTo(HaveOccurred())
			Expect(excerpt).To(ContainSubstring("run-verbs.spec.ts:67"))
			Expect(excerpt).To(ContainSubstring("expect(locator).toBeDisabled() failed"))
		})

		It("never lets the minted token survive into a log excerpt", func(ctx context.Context) {
			body := "Error: leaked token " + theToken + " must not reach the excerpt\n"
			api.onBody(http.StatusOK, []byte(body))

			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "987"})
			Expect(err).NotTo(HaveOccurred())
			Expect(excerpt).NotTo(ContainSubstring(theToken))
			Expect(excerpt).To(ContainSubstring("[redacted]"))
		})

		It("names why when GitLab cannot find a job's trace", func(ctx context.Context) {
			api.onBody(http.StatusNotFound, []byte(`{"message":"404 Not Found"}`))

			_, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "987"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("404"))
		})

		It("names why when GitLab forbids a job's trace", func(ctx context.Context) {
			api.onBody(http.StatusForbidden, []byte(`{"message":"403 Forbidden"}`))

			_, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "987"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("403"))
		})

		It("refuses a check with no log id without a network call", func(ctx context.Context) {
			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no log to read"))
			Expect(excerpt).To(Equal(""))
			Expect(api.calls()).To(BeEmpty())
		})
	})

	Describe("closing a merge request the mill is abandoning", func() {
		It("comments first, then closes, so the close is never unexplained", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
			api.on(http.MethodPost, escaped+"/merge_requests/7/notes", http.StatusCreated, "note_created.json")
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_opened.json")
			api.on(http.MethodPut, escaped+"/merge_requests/7", http.StatusOK, "merge_request_closed.json")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.ClosePR(ctx, pr, "Superseded: the run was restarted.")).To(Succeed())

			var order []string
			for _, seen := range api.calls() {
				switch {
				case seen.Method == http.MethodPost && strings.HasSuffix(seen.Path, "/notes"):
					order = append(order, "note")
					Expect(seen.Body).To(ContainSubstring("restarted"))
				case seen.Method == http.MethodPut:
					order = append(order, "close")
					Expect(seen.Body).To(ContainSubstring(`"state_event":"close"`))
				}
			}
			Expect(order).To(Equal([]string{"note", "close"}))
		})

		It("leaves a merge request that is already merged alone", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_squash_merged.json")
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_squash_merged.json")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(client.ClosePR(ctx, pr, "")).To(Succeed())

			Expect(api.count(http.MethodPut, escaped+"/merge_requests/7")).To(BeZero())
		})
	})

	Describe("deleting a branch", func() {
		It("deletes it", func(ctx context.Context) {
			api.on(http.MethodDelete, escaped+"/repository/branches/mill%2Fmill-13", http.StatusNoContent, "empty.json")

			Expect(client.DeleteBranch(ctx, "mill/mill-13")).To(Succeed())
			Expect(api.count(http.MethodDelete, escaped+"/repository/branches")).To(Equal(1))
		})

		It("treats a branch GitLab does not have as the state it was asked for", func(ctx context.Context) {
			// No route: the stub answers 404, which is what GitLab answers
			// for a branch that is not there.
			Expect(client.DeleteBranch(ctx, "mill/gone")).To(Succeed())
		})
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
			api.on(http.MethodGet, escaped+"/merge_requests/8", http.StatusOK, "merge_request_merged.json")
			api.on(http.MethodPut, escaped+"/merge_requests/8/merge", http.StatusOK, "merge_request_merged.json")

			merged, err := client.Merge(ctx, forge.PullRequest{
				Number: 8, Branch: branch, State: "open",
				HeadSHA: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80",
			})
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
			api.on(http.MethodGet, escaped+"/merge_requests/9", http.StatusOK, "merge_request_not_approved_single.json")
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
			Expect(refused.Pending).To(BeFalse())
			Expect(refused.Conflict).To(BeFalse())
			Expect(err.Error()).To(ContainSubstring("Branch cannot be merged"))
			Expect(forge.Retryable(err)).To(BeFalse())

			before := api.count(http.MethodPut, escaped+"/merge_requests/9/merge")
			Expect(before).To(Equal(1), "one attempt, and no retry inside the adapter")
		})

		It("waits out GitLab's own mergeability recomputation before asking to merge", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.onMergeableStates("merge_request_opened.json", "checking", "mergeable")
			api.onMerge("9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80")
			quick := gitlab.New(api.repo(), api.minter(), gitlab.WithHTTPClient(api.server.Client()),
				gitlab.WithMergeableWait(2*time.Second, 10*time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP"})

			merged, err := quick.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"})
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Merged()).To(BeTrue())
			Expect(api.count(http.MethodGet, escaped+"/merge_requests/7")).To(Equal(2), "checking, then mergeable")
		})

		It("classifies a refusal that still lands after the wait as mergeability still being computed", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.onMergeableStates("merge_request_opened.json", "checking")
			api.on(http.MethodPut, escaped+"/merge_requests/7/merge", http.StatusMethodNotAllowed, "merge_refused.json")
			quick := gitlab.New(api.repo(), api.minter(), gitlab.WithHTTPClient(api.server.Client()),
				gitlab.WithMergeableWait(30*time.Millisecond, 10*time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP"})

			_, err := quick.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"})

			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.Pending).To(BeTrue())
			Expect(refused.Conflict).To(BeFalse())
			Expect(forge.Retryable(err)).To(BeTrue(), "an unsettled mergeability clears on its own")
		})

		It("classifies a refusal for a real conflict a catch-up did not see", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.onMergeableStates("merge_request_opened.json", "conflict")
			api.on(http.MethodPut, escaped+"/merge_requests/7/merge", http.StatusMethodNotAllowed, "merge_refused.json")

			_, err := client.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"})

			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.Conflict).To(BeTrue())
			Expect(refused.Pending).To(BeFalse())
			Expect(forge.Retryable(err)).To(BeTrue(), "another catch-up sees the target that moved")
		})

		It("reads the head itself when the caller carries none, rather than merging blind", func(ctx context.Context) {
			api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_opened.json")
			api.onMerge("9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80")

			merged, err := client.Merge(ctx, forge.PullRequest{Number: 7})
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Merged()).To(BeTrue())
			Expect(api.count(http.MethodGet, escaped+"/merge_requests/7")).To(Equal(2),
				"the head is read once, from the merge request, and mergeability once more before merging")
		})

		It("refuses to read a merge commit before the merge", func(ctx context.Context) {
			api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_opened.json")

			_, err := client.MergeCommit(ctx, forge.PullRequest{Number: 7})
			Expect(errors.Is(err, forge.ErrNotMerged)).To(BeTrue())
		})
	})

	It("Merging a merge request names the head it merges", func(ctx context.Context) {
		// Given an open merge request whose source branch is at SHA S.
		const s = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
		api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
		api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
		api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_squash_merged.json")
		api.onMerge(s)

		pr, err := client.PR(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.HeadSHA).To(Equal(s))

		// When the merge step merges it.
		merged, err := client.Merge(ctx, pr)
		Expect(err).NotTo(HaveOccurred(), "%v", err)

		// Then the request to the forge carries sha S. The fake answers a
		// merge with no sha the way GitLab 19 does — 400 — so omitting the
		// field fails this spec at the merge, not only at the assertion.
		var body string
		for _, seen := range api.calls() {
			if seen.Method == http.MethodPut {
				body = seen.Body
			}
		}
		Expect(body).To(ContainSubstring(`"sha":"` + s + `"`))

		// And the merge evidence is the merge commit on the default branch.
		Expect(merged.Merged()).To(BeTrue())
		commit, err := client.MergeCommit(ctx, merged)
		Expect(err).NotTo(HaveOccurred())
		Expect(commit).To(Equal("1c4a5b6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"))
		Expect(commit).NotTo(Equal(s), "the commit on main is not the branch's head")
		Expect(merged.Target).To(Equal("main"))
	})

	It("A head that moved is not merged blind", func(ctx context.Context) {
		// Given an open merge request whose source branch moved after it was
		// read: the mill holds S, and the branch is at S' by the time the
		// merge arrives.
		const moved = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
		api.on(http.MethodGet, escaped, http.StatusOK, "project.json")
		api.on(http.MethodGet, escaped+"/merge_requests", http.StatusOK, "merge_requests_open.json")
		api.on(http.MethodGet, escaped+"/merge_requests/7", http.StatusOK, "merge_request_opened.json")
		api.onMerge(moved)

		pr, err := client.PR(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.HeadSHA).NotTo(Equal(moved))

		// When the merge step merges it.
		_, err = client.Merge(ctx, pr)

		// Then the forge refuses and the step records the refusal as a
		// retryable failure: the failure names the head the mill merged
		// against and GitLab's own words, and it is not swallowed as a
		// success or hidden behind a refusal the mill would never try again.
		Expect(err).To(HaveOccurred())
		var stale *forge.HeadMovedError
		Expect(errors.As(err, &stale)).To(BeTrue(), "%v", err)
		Expect(stale.PR).To(Equal(7))
		Expect(stale.SHA).To(Equal(pr.HeadSHA))
		Expect(stale.Reason).To(Equal("SHA does not match HEAD of source branch"))
		Expect(err.Error()).To(ContainSubstring("SHA does not match HEAD of source branch"))
		Expect(forge.Retryable(err)).To(BeTrue(), "the next attempt reads the head again")

		var refused *forge.MergeRefusedError
		Expect(errors.As(err, &refused)).To(BeFalse(), "a moved head is not the forge declining the merge")
		Expect(api.count(http.MethodPut, escaped+"/merge_requests/7/merge")).To(Equal(1),
			"one attempt, and no retry inside the adapter")
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
