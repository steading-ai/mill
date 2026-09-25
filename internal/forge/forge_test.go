package forge_test

import (
	"context"
	"errors"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
)

// stubForge is a forge that does nothing, for the specs about resolution and
// registration, which never reach one.
type stubForge struct{ repo forge.Repo }

func (s *stubForge) Authenticate(context.Context) (forge.GitAuth, error) {
	return forge.GitAuth{}, nil
}
func (s *stubForge) Push(context.Context, forge.PushRequest) (string, error) { return "", nil }
func (s *stubForge) OpenPR(context.Context, forge.OpenPRRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, nil
}
func (s *stubForge) PR(context.Context, string) (forge.PullRequest, error) {
	return forge.PullRequest{}, forge.ErrNoPullRequest
}
func (s *stubForge) Checks(context.Context, string) (forge.Status, error) { return forge.Success, nil }
func (s *stubForge) Merge(context.Context, forge.PullRequest) (forge.PullRequest, error) {
	return forge.PullRequest{}, nil
}
func (s *stubForge) MergeCommit(context.Context, forge.PullRequest) (string, error) {
	return "", forge.ErrNotMerged
}
func (s *stubForge) ClosePR(context.Context, forge.PullRequest, string) error { return nil }
func (s *stubForge) DeleteBranch(context.Context, string) error               { return nil }

var _ = Describe("The forge registry", Label("unit"), func() {
	var registry *forge.Registry

	BeforeEach(func() {
		registry = forge.NewRegistry()
		registry.Register(forge.KindGitLab, func(repo forge.Repo, _ forge.Minter) (forge.Forge, error) {
			return &stubForge{repo: repo}, nil
		})
	})

	It("resolves a project's repository from the URL the run was given", func() {
		resolved, err := registry.For(forge.Project{
			Slug:   "mill",
			VCSURL: "https://gitlab.example.test/group/thing.git",
		}, nil)
		Expect(err).NotTo(HaveOccurred())

		repo := resolved.(*stubForge).repo
		Expect(repo.Kind).To(Equal(forge.KindGitLab))
		Expect(repo.BaseURL).To(Equal("https://gitlab.example.test"))
		Expect(repo.Path).To(Equal("group/thing"))
		Expect(repo.CloneURL).To(Equal("https://gitlab.example.test/group/thing.git"))
	})

	It("yields a typed error naming the project when it has no VCS URL", func() {
		_, err := registry.For(forge.Project{Slug: "mill"}, nil)

		var missing *forge.NoVCSURLError
		Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
		Expect(missing.Project).To(Equal("mill"))
		Expect(err.Error()).To(ContainSubstring("mill"))
		// The one source there is, named: the project's own vcs_url in
		// dagger, and the command that sets it. No flag, no variable and
		// no label to send an operator looking elsewhere.
		Expect(err.Error()).To(ContainSubstring("dg project update mill --vcs-url"))
		Expect(err.Error()).NotTo(ContainSubstring("MILL_VCS_URL"))
		Expect(err.Error()).NotTo(ContainSubstring("label"))
		Expect(forge.Retryable(err)).To(BeFalse(), "no number of attempts adds a URL to the project")
	})

	It("names the forge it has no adapter for", func() {
		_, err := registry.For(forge.Project{
			Slug:   "elsewhere",
			VCSURL: "https://github.com/steading-ai/mill.git",
		}, nil)

		var unsupported *forge.UnsupportedError
		Expect(errors.As(err, &unsupported)).To(BeTrue(), "%v", err)
		Expect(unsupported.Kind).To(Equal(forge.KindGitHub))
		Expect(unsupported.Project).To(Equal("elsewhere"))
	})

	DescribeTable("reads a repository URL in either spelling git accepts",
		func(raw string, expected forge.Repo) {
			Expect(forge.ParseRepo(raw)).To(Equal(expected))
		},
		Entry("an HTTPS URL", "https://gitlab.example.test/group/thing.git", forge.Repo{
			Kind: forge.KindGitLab, BaseURL: "https://gitlab.example.test",
			Path: "group/thing", CloneURL: "https://gitlab.example.test/group/thing.git",
		}),
		Entry("an HTTPS URL without the suffix", "https://gitlab.example.test/group/thing", forge.Repo{
			Kind: forge.KindGitLab, BaseURL: "https://gitlab.example.test",
			Path: "group/thing", CloneURL: "https://gitlab.example.test/group/thing.git",
		}),
		Entry("a nested group", "https://gitlab.example.test/group/sub/thing.git", forge.Repo{
			Kind: forge.KindGitLab, BaseURL: "https://gitlab.example.test",
			Path: "group/sub/thing", CloneURL: "https://gitlab.example.test/group/sub/thing.git",
		}),
		Entry("the scp-like spelling", "git@gitlab.example.test:group/thing.git", forge.Repo{
			Kind: forge.KindGitLab, BaseURL: "https://gitlab.example.test",
			Path: "group/thing", CloneURL: "https://gitlab.example.test/group/thing.git",
		}),
		Entry("github, which is a kind and not an implementation", "https://github.com/steading-ai/mill.git", forge.Repo{
			Kind: forge.KindGitHub, BaseURL: "https://github.com",
			Path: "steading-ai/mill", CloneURL: "https://github.com/steading-ai/mill.git",
		}),
	)

	// A self-hosted forge is a GitLab until mill has another adapter: the
	// kind follows the host, and there is nothing on a dagger project to
	// override it with.
	It("takes a self-hosted host for a GitLab", func() {
		repo, err := forge.RepoFor(forge.Project{
			Slug:   "mill",
			VCSURL: "https://code.example.test/group/thing.git",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.Kind).To(Equal(forge.KindGitLab))
	})
})

var _ = Describe("What the mill retries", Label("unit"), func() {
	DescribeTable("a failure is retryable only when trying again could change the answer",
		func(err error, retryable bool) {
			Expect(forge.Retryable(err)).To(Equal(retryable))
		},
		Entry("a merge the forge refused", &forge.MergeRefusedError{PR: 7, Status: http.StatusMethodNotAllowed, Reason: "Branch cannot be merged"}, false),
		Entry("a merge refused while mergeability is still being computed",
			&forge.MergeRefusedError{PR: 7, Status: http.StatusMethodNotAllowed, Reason: "Pull Request is not mergeable", Pending: true}, true),
		Entry("a merge refused for a conflict a catch-up did not see",
			&forge.MergeRefusedError{PR: 7, Status: http.StatusMethodNotAllowed, Reason: "Pull Request is not mergeable", Conflict: true}, true),
		Entry("a project with no repository", &forge.NoVCSURLError{Project: "mill"}, false),
		Entry("a forge with no adapter", &forge.UnsupportedError{Kind: forge.KindGitHub, Project: "mill"}, false),
		Entry("a branch with no pull request", forge.ErrNoPullRequest, false),
		Entry("a merge commit read too early", forge.ErrNotMerged, false),
		Entry("the transport failing", errors.New("dial tcp: connection refused"), true),
		Entry("no failure at all", nil, false),
	)

	It("names the refusal with the forge's own reason", func() {
		err := &forge.MergeRefusedError{PR: 7, Status: http.StatusMethodNotAllowed, Reason: "Branch cannot be merged", Detail: "not_approved"}

		Expect(err.Error()).To(ContainSubstring("Branch cannot be merged"))
		Expect(err.Error()).To(ContainSubstring("not_approved"))
		Expect(err.Error()).To(ContainSubstring("!7"))
	})
})

var _ = Describe("The credential a request is made with", Label("unit"), func() {
	It("is handed out per request and never held by the forge", func(ctx context.Context) {
		minter := forge.Static("mill", "glpat-the-mill-holds-this")

		credential, err := minter.Mint(ctx, forge.Step{TokenID: "01J0STEP"})
		Expect(err).NotTo(HaveOccurred())
		Expect(credential.Token).To(Equal("glpat-the-mill-holds-this"))
		Expect(credential.Username).To(Equal("mill"))
	})

	It("refuses when the mill holds none", func(ctx context.Context) {
		_, err := forge.Static("mill", "").Mint(ctx, forge.Step{})
		Expect(err).To(HaveOccurred())
	})
})
