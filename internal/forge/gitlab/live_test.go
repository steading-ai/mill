package gitlab_test

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
	"github.com/steading-ai/mill/internal/forge/gitlab"
)

// The live spec spends a real branch and a real merge request on a scratch
// project on the host's GitLab. It is off by default: `make test` must not
// depend on a credential or on the forge being reachable.
const (
	liveVar    = "MILL_FORGE_LIVE"
	urlVar     = "MILL_FORGE_URL"
	projectVar = "MILL_FORGE_PROJECT"
	tokenVar   = "MILL_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
)

var _ = Describe("The GitLab adapter against a real GitLab", Label("live"), func() {
	var (
		client   forge.Forge
		repo     forge.Repo
		checkout string
		branch   string
		token    string
	)

	BeforeEach(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1, with " + urlVar + ", " + projectVar + " and " + tokenVar +
				", to run the spec that spends a real merge request")
		}
		for _, name := range []string{urlVar, projectVar, tokenVar} {
			Expect(os.Getenv(name)).NotTo(BeEmpty(), name+" is required by the live spec")
		}
		token = os.Getenv(tokenVar)
		base := strings.TrimSuffix(os.Getenv(urlVar), "/")
		path := strings.Trim(os.Getenv(projectVar), "/")
		repo = forge.Repo{Kind: forge.KindGitLab, BaseURL: base, Path: path, CloneURL: base + "/" + path + ".git"}
		client = gitlab.New(repo, forge.MinterFunc(func(context.Context, forge.Step) (forge.Credential, error) {
			return forge.Credential{Username: "mill", Token: token}, nil
		}))

		suffix := make([]byte, 4)
		_, err := rand.Read(suffix)
		Expect(err).NotTo(HaveOccurred())
		branch = "mill/live-" + hex.EncodeToString(suffix)

		checkout = filepath.Join(GinkgoT().TempDir(), "checkout")
		liveGit{dir: GinkgoT().TempDir(), token: token}.run(ctx, "clone", "--quiet", repo.CloneURL, checkout)
	})

	It("pushes, opens a merge request, reads its checks, merges, and lands on the default branch", func(ctx context.Context) {
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

		pr, err := client.OpenPR(ctx, forge.OpenPRRequest{
			Branch:      branch,
			Title:       "mill: live spec " + branch,
			Description: "Opened by mill's live forge spec. Safe to close.",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.URL).NotTo(BeEmpty())
		Expect(client.PR(ctx, branch)).To(Equal(pr), "asking twice returns the same merge request")

		Eventually(func() (forge.Status, error) { return client.Checks(ctx, head) }).
			WithTimeout(3*time.Minute).WithPolling(5*time.Second).
			Should(BeElementOf(forge.Success, forge.Failed), "the checks settled")

		merged, err := client.Merge(ctx, pr)
		Expect(err).NotTo(HaveOccurred(), "a forge that refuses says why: %v", err)
		Expect(merged.Merged()).To(BeTrue())

		landed, err := client.MergeCommit(ctx, merged)
		Expect(err).NotTo(HaveOccurred())

		runner.run(ctx, "fetch", "--quiet", "origin")
		Expect(strings.TrimSpace(runner.run(ctx, "cat-file", "-t", landed))).To(Equal("commit"))
	})
})

// liveGit is the workspace's git, for a spec that has no workspace: the same
// credential helper the adapter uses, so the token stays out of argv and out
// of the clone's configuration here too.
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
	credentialed := append([]string{
		"-c", "credential.helper=",
		"-c", `credential.helper=!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$MILL_FORGE_USERNAME" "$MILL_FORGE_TOKEN"; }; f`,
	}, args...)
	out, err := g.Run(ctx, map[string]string{"MILL_FORGE_USERNAME": "mill", "MILL_FORGE_TOKEN": g.token}, credentialed...)
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return out
}
