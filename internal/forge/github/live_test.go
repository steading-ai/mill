package github_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/forge/github"
)

// The live spec spends a real branch and a real pull request on a scratch
// repository on github.com. It is off by default: `make test` must not
// depend on a credential or on the forge being reachable.
//
// The token is whatever the person holds: a fine-grained token with the
// permissions docs/tools.md lists, or `gh auth token`.
const (
	liveVar    = "MILL_FORGE_LIVE"
	projectVar = "MILL_FORGE_PROJECT"
	tokenVar   = "MILL_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
)

var _ = Describe("The GitHub adapter against github.com", Label("live"), func() {
	var (
		client   *github.Client
		repo     forge.Repo
		checkout string
		branch   string
		token    string
	)

	BeforeEach(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1, with " + projectVar + " (owner/repo) and " + tokenVar +
				", to run the spec that spends a real pull request")
		}
		for _, name := range []string{projectVar, tokenVar} {
			Expect(os.Getenv(name)).NotTo(BeEmpty(), name+" is required by the live spec")
		}
		token = os.Getenv(tokenVar)
		var err error
		repo, err = forge.ParseRepo("https://github.com/" + strings.Trim(os.Getenv(projectVar), "/"))
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.Kind).To(Equal(forge.KindGitHub))
		client = github.New(repo, github.Token(token))

		suffix := make([]byte, 4)
		_, err = rand.Read(suffix)
		Expect(err).NotTo(HaveOccurred())
		branch = "mill/live-" + hex.EncodeToString(suffix)

		checkout = filepath.Join(GinkgoT().TempDir(), "checkout")
		liveGit{dir: GinkgoT().TempDir(), token: token}.run(ctx, "clone", "--quiet", repo.CloneURL, checkout)
	})

	It("names the account the token belongs to", func(ctx context.Context) {
		account, err := client.Probe(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(account.Login).NotTo(BeEmpty())
		GinkgoWriter.Printf("authenticated as %s\n", account.Login)
	})

	It("A run delivers to GitHub", func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		DeferCleanup(cancel)
		runner := liveGit{dir: checkout, token: token}

		runner.run(ctx, "checkout", "--quiet", "-b", branch)
		Expect(os.WriteFile(filepath.Join(checkout, "MILL-LIVE.md"), []byte(branch+"\n"), 0o600)).To(Succeed())
		runner.run(ctx, "add", "MILL-LIVE.md")
		runner.run(ctx, "-c", "user.name=mill", "-c", "user.email=mill@example.test",
			"commit", "--quiet", "--no-gpg-sign", "--message", "mill: live spec "+branch)

		head, err := client.Push(ctx, forge.PushRequest{Git: runner, Branch: branch})
		Expect(err).NotTo(HaveOccurred())
		Expect(head).NotTo(BeEmpty())
		GinkgoWriter.Printf("pushed %s at %s\n", branch, head)

		pr, err := client.OpenPR(ctx, forge.OpenPRRequest{
			Branch:      branch,
			Head:        head,
			Title:       "mill: live spec " + branch,
			Description: "Opened by mill's live forge spec. Safe to close.",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.URL).NotTo(BeEmpty())
		again, err := client.PR(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		// GitHub computes mergeability after the fact and reports it on
		// one pull request and not on a list, so the detail is the one
		// field two reads are allowed to differ on.
		again.Detail, pr.Detail = "", ""
		Expect(again).To(Equal(pr), "asking twice returns the same pull request")
		GinkgoWriter.Printf("opened %s\n", pr.URL)

		Eventually(func() (forge.Status, error) { return client.Checks(ctx, head) }).
			WithTimeout(3*time.Minute).WithPolling(5*time.Second).
			Should(BeElementOf(forge.Success, forge.Failed), "the checks settled")

		merged, err := client.Merge(ctx, pr)
		Expect(err).NotTo(HaveOccurred(), "a forge that refuses says why: %v", err)
		Expect(merged.Merged()).To(BeTrue())

		landed, err := client.MergeCommit(ctx, merged)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("landed as %s\n", landed)

		runner.run(ctx, "fetch", "--quiet", "origin")
		Expect(strings.TrimSpace(runner.run(ctx, "cat-file", "-t", landed))).To(Equal("commit"))

		Expect(client.DeleteBranch(ctx, branch)).To(Succeed())
	})

	// This proves FailingChecks and CheckLog answer for real, against
	// github.com, rather than only against the fixtures github_test.go
	// drives. It does not force a check red: the token
	// docs/tools.md asks a person to mint has read-only access to checks
	// and commit statuses, so the spec takes whatever the scratch
	// repository's own checks say about a plain push, and asserts the two
	// methods agree with what Checks reports either way.
	It("reads the failing checks and their logs off a real commit", func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		DeferCleanup(cancel)
		runner := liveGit{dir: checkout, token: token}

		runner.run(ctx, "checkout", "--quiet", "-b", branch)
		Expect(os.WriteFile(filepath.Join(checkout, "MILL-LIVE.md"), []byte(branch+"\n"), 0o600)).To(Succeed())
		runner.run(ctx, "add", "MILL-LIVE.md")
		runner.run(ctx, "-c", "user.name=mill", "-c", "user.email=mill@example.test",
			"commit", "--quiet", "--no-gpg-sign", "--message", "mill: live spec "+branch)

		head, err := client.Push(ctx, forge.PushRequest{Git: runner, Branch: branch})
		Expect(err).NotTo(HaveOccurred())

		var status forge.Status
		Eventually(func() (forge.Status, error) {
			status, err = client.Checks(ctx, head)
			return status, err
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeElementOf(forge.Success, forge.Failed))

		checks, err := client.FailingChecks(ctx, head)
		Expect(err).NotTo(HaveOccurred())

		switch status {
		case forge.Success:
			Expect(checks).To(BeEmpty(), "a green commit names no failing check")
		case forge.Failed:
			Expect(checks).NotTo(BeEmpty(), "a red commit names at least one failing check")
			for _, check := range checks {
				excerpt, err := client.CheckLog(ctx, check)
				if err != nil {
					GinkgoWriter.Printf("%s: log unreadable, named why: %v\n", check.Name, err)
					continue
				}
				GinkgoWriter.Printf("%s: %d bytes of excerpt\n", check.Name, len(excerpt))
				Expect(len(excerpt)).To(BeNumerically("<=", forge.MaxExcerptBytes))
			}
		}

		Expect(client.DeleteBranch(ctx, branch)).To(Succeed())
	})
})

// liveGit is the workspace's git, for a spec that has no workspace: the
// same credential helper the adapter uses, so the token stays out of argv
// and out of the clone's configuration here too.
type liveGit struct {
	dir   string
	token string
}

func (g liveGit) Run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec // a spec's own git, behind an env gate
	command.Dir = g.dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	for name, value := range env {
		command.Env = append(command.Env, name+"="+value)
	}
	out, err := command.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// run is Run with the credential the clone needs, failing the spec on a
// non-zero exit.
func (g liveGit) run(ctx context.Context, args ...string) string {
	GinkgoHelper()
	auth := forge.HelperAuth(forge.Credential{Username: "x-access-token", Token: g.token})
	out, err := g.Run(ctx, auth.Env, append(auth.Args, args...)...)
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return out
}
