package github_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/github"
)

var _ = Describe("The GitHub forge adapter", Label("unit"), func() {
	var (
		api    *stub
		client forge.Forge
	)

	const (
		branch = "mill/mill-13"
		head   = "9a1f0c3f2b6d4e5a7c8b9d0e1f2a3b4c5d6e7f80"
	)

	BeforeEach(func() {
		api = newStub()
		client = github.New(api.repo(), api.minter(), github.WithHTTPClient(api.httpClient())).
			ForStep(forge.Step{TokenID: "01J0STEP", TokenHash: "e3b0c442", Capabilities: []string{"forge"}})
	})

	It("is selected by the kind forge.go declares", func() {
		registry := forge.NewRegistry()
		github.Register(registry)

		resolved, err := registry.For(forge.Project{Slug: "mill", VCSURL: "https://github.com/octo-org/thing.git"}, api.minter())
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(BeAssignableToTypeOf(&github.Client{}))
		Expect(resolved.(*github.Client).Repo().Path).To(Equal(repoPath))
	})

	Describe("the token", func() {
		It("is sent as a bearer, which is how a fine-grained token and gh's OAuth token are both sent", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")
			tokened := github.New(api.repo(), github.Token("gho_from_gh_auth_token"), github.WithHTTPClient(api.httpClient())) // gitleaks:allow

			_, err := tokened.PR(ctx, branch)
			Expect(err).To(MatchError(forge.ErrNoPullRequest))
			for _, seen := range api.calls() {
				Expect(seen.Bearer).To(Equal("gho_from_gh_auth_token"))
			}
		})

		It("refuses to mint from no token, naming the gap", func(ctx context.Context) {
			_, err := github.Token("").Mint(ctx, forge.Step{})
			Expect(err).To(MatchError(ContainSubstring("no token")))
		})
	})

	Describe("pushing the run's branch", func() {
		It("pushes with a credential that reaches git through its environment, not its remote or its argv", func(ctx context.Context) {
			git := &recordingGit{head: head}

			sha, err := client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())
			Expect(sha).To(Equal(head))

			push := git.runs[0]
			Expect(push.Args).To(ContainElement("refs/heads/" + branch + ":refs/heads/" + branch))
			Expect(push.Args).To(ContainElement(api.repo().CloneURL))
			Expect(git.argv()).NotTo(ContainSubstring(theToken), "the credential must not be in argv")
			Expect(push.Env).To(HaveKeyWithValue(forge.EnvToken, theToken))
			Expect(push.Env).To(HaveKeyWithValue(forge.EnvUsername, "x-access-token"))
			Expect(api.repo().CloneURL).NotTo(ContainSubstring(theToken))
		})

		It("mints a credential for the step it is bound to, per call", func(ctx context.Context) {
			git := &recordingGit{head: head}

			_, err := client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())
			_, err = client.Push(ctx, forge.PushRequest{Git: git, Branch: branch})
			Expect(err).NotTo(HaveOccurred())

			Expect(api.minted).To(Equal(2), "a credential is minted per request, never held")
			Expect(api.forStep[0].TokenHash).To(Equal("e3b0c442"))
		})
	})

	Describe("the pull request", func() {
		It("opens one against the default branch and returns its URL", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")
			api.on(http.MethodPost, repoAPI+"/pulls", http.StatusCreated, "pull_opened.json")

			pr, err := client.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Title: "mill#13: the forge adapter"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.URL).To(Equal("https://github.com/octo-org/thing/pull/7"))
			Expect(pr.Number).To(Equal(7))
			Expect(pr.Branch).To(Equal(branch))
			Expect(pr.Target).To(Equal("main"), "the repository's default branch")
			Expect(pr.Open()).To(BeTrue())
			Expect(pr.MergeCommit).To(BeEmpty(), "an open pull request's merge_commit_sha is a test merge, not a landing")

			var opened call
			for _, seen := range api.calls() {
				if seen.Method == http.MethodPost {
					opened = seen
				}
			}
			Expect(opened.Body).To(MatchJSON(`{"head":"mill/mill-13","base":"main","title":"mill#13: the forge adapter","body":""}`))
		})

		It("looks a pull request up by the owner-qualified head GitHub filters on", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_open.json")

			first, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			second, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(first))
			Expect(api.count(http.MethodPost, repoAPI+"/pulls")).To(BeZero(), "nothing was opened")

			var listed call
			for _, seen := range api.calls() {
				if seen.Path == repoAPI+"/pulls" {
					listed = seen
				}
			}
			Expect(listed.Query).To(ContainSubstring("head=octo-org%3Amill%2Fmill-13"))
			Expect(listed.Query).To(ContainSubstring("state=all"))
		})

		It("The forge catches up before the pull request is opened", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")
			api.onPushedBranch(branch, head, 2, "pull_opened.json")
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithBranchWait(2*time.Second, 10*time.Millisecond))

			pr, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head, Title: "mill#13"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Number).To(Equal(7))
			Expect(api.count(http.MethodGet, repoAPI+"/branches/")).To(Equal(3), "two misses, then the branch")
			Expect(api.count(http.MethodPost, repoAPI+"/pulls")).To(Equal(1))
		})

		It("OpenPR waits for an existing pull request's branch to reach the requested head before reusing it", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_open.json")
			api.onBranchCatchUp(branch, head, 2)
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithBranchWait(2*time.Second, 10*time.Millisecond))

			pr, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head, Title: "mill#13"})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Number).To(Equal(7))
			Expect(api.count(http.MethodGet, repoAPI+"/branches/")).To(Equal(3), "two misses, then the branch")
			Expect(api.count(http.MethodGet, repoAPI+"/pulls")).To(Equal(2), "looked up, then re-read once caught up")
			Expect(api.count(http.MethodPost, repoAPI+"/pulls")).To(BeZero(), "an existing pull request is reused, not recreated")
		})

		It("OpenPR does not reuse a pull request whose branch never reaches the requested head", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_open.json")
			api.onBranchCatchUp(branch, head, -1)
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithBranchWait(50*time.Millisecond, 10*time.Millisecond))

			_, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head})

			var missing *forge.BranchMissingError
			Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
			Expect(missing.Branch).To(Equal(branch))
			Expect(missing.Head).To(Equal(head))
			Expect(api.count(http.MethodGet, repoAPI+"/pulls")).To(Equal(1), "the stale pull request is never trusted")
		})

		It("A push that never landed is a named failure", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")
			api.onPushedBranch(branch, head, -1, "pull_opened.json")
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithBranchWait(50*time.Millisecond, 10*time.Millisecond))

			_, err := quick.OpenPR(ctx, forge.OpenPRRequest{Branch: branch, Head: head})

			var missing *forge.BranchMissingError
			Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
			Expect(missing.Branch).To(Equal(branch))
			Expect(missing.Head).To(Equal(head))
			Expect(missing.Found).To(BeEmpty())
			Expect(api.count(http.MethodPost, repoAPI+"/pulls")).To(BeZero(), "nothing was opened for a branch that is not there")
		})

		It("says so when the branch has no pull request", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_empty.json")

			_, err := client.PR(ctx, branch)
			Expect(err).To(MatchError(forge.ErrNoPullRequest))
		})
	})

	Describe("the checks on a commit", func() {
		check := func(runs, status string) forge.Status {
			GinkgoHelper()
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/check-runs", http.StatusOK, runs)
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/status", http.StatusOK, status)
			result, err := client.Checks(context.Background(), head)
			Expect(err).NotTo(HaveOccurred())
			return result
		}

		It("is green for a commit with no check runs and no statuses: nothing to be red about", func() {
			Expect(check("check_runs_empty.json", "status_empty.json")).To(Equal(forge.Success))
		})
		It("is pending while a check run is in progress", func() {
			Expect(check("check_runs_pending.json", "status_empty.json")).To(Equal(forge.Pending))
		})
		It("is failed when a check run concluded badly", func() {
			Expect(check("check_runs_failed.json", "status_empty.json")).To(Equal(forge.Failed))
		})
		It("is green when every check run concluded well, a skipped one included", func() {
			Expect(check("check_runs_success.json", "status_empty.json")).To(Equal(forge.Success))
		})
		It("is failed when a commit status is red, whatever the check runs say", func() {
			Expect(check("check_runs_success.json", "status_failure.json")).To(Equal(forge.Failed))
		})
	})

	Describe("the failing checks on a commit, and their logs", func() {
		It("gives a failing check run its conclusion and a log id to ask a log for", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/check-runs", http.StatusOK, "check_runs_failed.json")
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/statuses", http.StatusOK, "statuses_empty.json")

			failing, err := client.FailingChecks(ctx, head)
			Expect(err).NotTo(HaveOccurred())
			Expect(failing).To(ConsistOf(forge.CheckResult{Conclusion: "failure", LogID: "2"}))
		})

		It("gives a failing commit status no log id: there is no job behind one", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/check-runs", http.StatusOK, "check_runs_empty.json")
			api.on(http.MethodGet, repoAPI+"/commits/"+head+"/statuses", http.StatusOK, "statuses_failure.json")

			failing, err := client.FailingChecks(ctx, head)
			Expect(err).NotTo(HaveOccurred())
			Expect(failing).To(ConsistOf(forge.CheckResult{
				Name: "buildkite/thing", URL: "https://ci.example/1", Conclusion: "failure",
			}))
		})

		It("reads a check run's job log, filtered to the failing assertion", func(ctx context.Context) {
			body := "noise\nrun-verbs.spec.ts:67\nError: expect(locator).toBeDisabled() failed\nmore noise\n"
			api.onBody(http.StatusOK, []byte(body))

			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(excerpt).To(ContainSubstring("run-verbs.spec.ts:67"))
			Expect(excerpt).To(ContainSubstring("expect(locator).toBeDisabled() failed"))
		})

		It("never lets the minted token survive into a log excerpt", func(ctx context.Context) {
			body := "Error: leaked token " + theToken + " must not reach the excerpt\n"
			api.onBody(http.StatusOK, []byte(body))

			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(excerpt).NotTo(ContainSubstring(theToken))
			Expect(excerpt).To(ContainSubstring("[redacted]"))
		})

		It("names why when GitHub cannot find a job's log", func(ctx context.Context) {
			api.onBody(http.StatusNotFound, []byte(`{"message":"Not Found"}`))

			_, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "2"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("404"))
		})

		It("names why when GitHub forbids a job's log", func(ctx context.Context) {
			api.onBody(http.StatusForbidden, []byte(`{"message":"Forbidden"}`))

			_, err := client.CheckLog(ctx, forge.CheckResult{Name: "e2e", LogID: "2"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("403"))
		})

		It("refuses a check with no log id without a network call", func(ctx context.Context) {
			excerpt, err := client.CheckLog(ctx, forge.CheckResult{Name: "buildkite/thing", Conclusion: "failure"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no log to read"))
			Expect(excerpt).To(Equal(""))
			Expect(api.calls()).To(BeEmpty())
		})
	})

	Describe("merging", func() {
		It("Merging a pull request names the head it merges, by the method the repository allows", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_open.json")
			api.onMerge(head)
			api.onMergeableStates("pull_merged.json", "clean")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			merged, err := client.Merge(ctx, pr)
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Merged()).To(BeTrue())
			Expect(merged.MergeCommit).To(Equal("c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"))
			Expect(merged.Squash).To(BeFalse(), "merge commits are allowed, so that is the method")

			var merge call
			for _, seen := range api.calls() {
				if seen.Method == http.MethodPut {
					merge = seen
				}
			}
			Expect(merge.Body).To(MatchJSON(`{"sha":"` + head + `","merge_method":"merge"}`))
		})

		It("squashes when the repository allows nothing else", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository_squash_only.json")
			api.on(http.MethodGet, repoAPI+"/pulls", http.StatusOK, "pulls_open.json")
			api.onMerge(head)
			api.onMergeableStates("pull_merged.json", "clean")

			pr, err := client.PR(ctx, branch)
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Squash).To(BeTrue())
			merged, err := client.Merge(ctx, pr)
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Squash).To(BeTrue())

			var merge call
			for _, seen := range api.calls() {
				if seen.Method == http.MethodPut {
					merge = seen
				}
			}
			Expect(merge.Body).To(MatchJSON(`{"sha":"` + head + `","merge_method":"squash"}`))
		})

		It("A head that moved is not merged blind", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.onMergeableStates("pull_opened.json", "clean")
			api.onMerge("ffffffffffffffffffffffffffffffffffffffff")

			_, err := client.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: head})

			var moved *forge.HeadMovedError
			Expect(errors.As(err, &moved)).To(BeTrue(), "%v", err)
			Expect(moved.SHA).To(Equal(head))
			Expect(moved.Reason).To(ContainSubstring("Head branch was modified"))
			Expect(forge.Retryable(err)).To(BeTrue(), "the next attempt reads the head again")
		})

		It("waits out GitHub's own mergeability recomputation before asking to merge", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.onMergeableStates("pull_merged.json", "unknown", "clean")
			api.onMerge(head)
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithMergeableWait(2*time.Second, 10*time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP"})

			merged, err := quick.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: head})
			Expect(err).NotTo(HaveOccurred())
			Expect(merged.Merged()).To(BeTrue())
			Expect(api.count(http.MethodGet, repoAPI+"/pulls/7")).To(Equal(3), "unknown, then clean, then the post-merge read")
		})

		It("classifies a refusal that still lands after the wait as mergeability still being computed", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.onMergeableStates("pull_opened.json", "unknown")
			api.on(http.MethodPut, repoAPI+"/pulls/7/merge", http.StatusMethodNotAllowed, "merge_refused.json")
			quick := github.New(api.repo(), api.minter(),
				github.WithHTTPClient(api.httpClient()), github.WithMergeableWait(30*time.Millisecond, 10*time.Millisecond)).
				ForStep(forge.Step{TokenID: "01J0STEP"})

			_, err := quick.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: head})

			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.Pending).To(BeTrue())
			Expect(refused.Conflict).To(BeFalse())
			Expect(forge.Retryable(err)).To(BeTrue(), "an unsettled mergeability clears on its own")
		})

		It("classifies a refusal for a real conflict a catch-up did not see", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.onMergeableStates("pull_opened.json", "dirty")
			api.on(http.MethodPut, repoAPI+"/pulls/7/merge", http.StatusMethodNotAllowed, "merge_refused.json")

			_, err := client.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: head})

			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.Conflict).To(BeTrue())
			Expect(refused.Pending).To(BeFalse())
			Expect(forge.Retryable(err)).To(BeTrue(), "another catch-up sees the target that moved")
		})

		It("returns a typed refusal naming GitHub's reason, which the mill does not retry", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.onMergeableStates("pull_blocked.json", "blocked")
			api.on(http.MethodPut, repoAPI+"/pulls/7/merge", http.StatusMethodNotAllowed, "merge_refused.json")

			_, err := client.Merge(ctx, forge.PullRequest{Number: 7, HeadSHA: head})

			var refused *forge.MergeRefusedError
			Expect(errors.As(err, &refused)).To(BeTrue(), "%v", err)
			Expect(refused.Status).To(Equal(http.StatusMethodNotAllowed))
			Expect(refused.Reason).To(ContainSubstring(`Required status check "ci" is expected.`))
			Expect(refused.Detail).To(Equal("blocked"))
			Expect(refused.Pending).To(BeFalse())
			Expect(refused.Conflict).To(BeFalse())
			Expect(forge.Retryable(err)).To(BeFalse())
		})

		It("reads the head itself when the caller carries none, rather than merging blind", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI, http.StatusOK, "repository.json")
			api.on(http.MethodGet, repoAPI+"/pulls/7", http.StatusOK, "pull_opened.json")
			api.onMerge(head)

			_, err := client.Merge(ctx, forge.PullRequest{Number: 7})
			Expect(err).NotTo(HaveOccurred())
		})

		It("refuses to read a merge commit before the merge", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI+"/pulls/7", http.StatusOK, "pull_opened.json")

			_, err := client.MergeCommit(ctx, forge.PullRequest{Number: 7})
			Expect(err).To(MatchError(forge.ErrNotMerged))
		})

		It("reads the merge commit through the pull request once it has landed", func(ctx context.Context) {
			api.on(http.MethodGet, repoAPI+"/pulls/7", http.StatusOK, "pull_merged.json")

			Expect(client.MergeCommit(ctx, forge.PullRequest{Number: 7})).To(Equal("c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"))
		})
	})

	Describe("closing a pull request the mill is abandoning", func() {
		It("comments first, then closes, so the close is never unexplained", func(ctx context.Context) {
			api.on(http.MethodPost, repoAPI+"/issues/7/comments", http.StatusCreated, "comment_created.json")
			api.on(http.MethodGet, repoAPI+"/pulls/7", http.StatusOK, "pull_opened.json")
			api.on(http.MethodPatch, repoAPI+"/pulls/7", http.StatusOK, "pull_closed.json")

			Expect(client.ClosePR(ctx, forge.PullRequest{Number: 7}, "abandoned by mill restart")).To(Succeed())

			var order []string
			for _, seen := range api.calls() {
				order = append(order, seen.Method+" "+strings.TrimPrefix(seen.Path, repoAPI))
			}
			Expect(order).To(Equal([]string{"POST /issues/7/comments", "GET /pulls/7", "PATCH /pulls/7"}))
		})

		It("leaves a pull request that is already merged alone", func(ctx context.Context) {
			api.on(http.MethodPost, repoAPI+"/issues/7/comments", http.StatusCreated, "comment_created.json")
			api.on(http.MethodGet, repoAPI+"/pulls/7", http.StatusOK, "pull_merged.json")

			Expect(client.ClosePR(ctx, forge.PullRequest{Number: 7}, "late")).To(Succeed())
			Expect(api.count(http.MethodPatch, repoAPI+"/pulls/7")).To(BeZero())
		})
	})

	Describe("deleting a branch", func() {
		It("deletes it", func(ctx context.Context) {
			api.on(http.MethodDelete, repoAPI+"/git/refs/heads/mill%2Fmill-13", http.StatusNoContent, "pulls_empty.json")

			Expect(client.DeleteBranch(ctx, branch)).To(Succeed())
		})

		It("treats a branch GitHub does not have as the state it was asked for", func(ctx context.Context) {
			api.on(http.MethodDelete, repoAPI+"/git/refs/heads/mill%2Fmill-13", http.StatusUnprocessableEntity, "ref_missing.json")

			Expect(client.DeleteBranch(ctx, branch)).To(Succeed())
		})
	})
})
