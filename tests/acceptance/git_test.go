package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A clone of the scratch project, for the two questions only git can answer:
// what a commit's parents are, and whether its signature verifies.
//
// docs/architecture.md §"Identity and credentials" says the mill signs every
// commit with an SSH key its account owns and that a project verifies it
// with an allowed_signers file. Verification here is exactly that — `git
// verify-commit` against the line the mill published — because a signature
// nobody checks is not evidence of anything.
type clone struct{ dir string }

// cloneScratch clones the project into a directory of the spec's own.
func cloneScratch(ctx context.Context, project *scratchProject) *clone {
	GinkgoHelper()
	// The clone goes in a directory of its own so the allowed_signers file
	// the verification needs can sit beside it rather than inside it.
	root := GinkgoT().TempDir()
	dir := filepath.Join(root, "clone")
	c := &clone{dir: root}
	c.run(ctx, "clone", "--quiet", project.CloneURL, dir)
	c.dir = dir
	return c
}

// parentsOf are a commit's parents, oldest first. A merge commit has two:
// the default branch it landed on, and the run's branch, which is the commit
// the mill signed.
func (c *clone) parentsOf(ctx context.Context, sha string) []string {
	GinkgoHelper()
	fields := strings.Fields(c.run(ctx, "rev-list", "--parents", "-n", "1", sha))
	Expect(fields).NotTo(BeEmpty())
	return fields[1:]
}

// signedCommitIn is the commit a merge brought onto the default branch: the
// merge's second parent, or the commit itself when the merge fast-forwarded.
func (c *clone) signedCommitIn(ctx context.Context, sha string) string {
	GinkgoHelper()
	parents := c.parentsOf(ctx, sha)
	if len(parents) > 1 {
		return parents[len(parents)-1]
	}
	return sha
}

// verifyCommit is `git verify-commit` against an allowed_signers file
// carrying the mill's line, as a project that wanted to check mill's
// signatures itself would hold it. It returns git's own output, which names
// the principal when the signature is good.
func (c *clone) verifyCommit(ctx context.Context, sha, allowedSigners string) (string, error) {
	GinkgoHelper()
	signers := filepath.Join(filepath.Dir(c.dir), "allowed_signers")
	line, err := os.ReadFile(allowedSigners) //nolint:gosec // a path the suite computed
	Expect(err).NotTo(HaveOccurred(), "the mill published an allowed_signers line at %s", allowedSigners)
	Expect(os.WriteFile(signers, line, 0o600)).To(Succeed())

	command := exec.CommandContext(ctx, "git", //nolint:gosec // the suite's own git
		"-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", sha)
	command.Dir = c.dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := command.CombinedOutput()
	return string(out), err
}

// run is git in the clone, with the forge credential in the environment and
// never in a URL, failing the spec on a non-zero exit.
func (c *clone) run(ctx context.Context, args ...string) string {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	credentialed := append([]string{
		"-c", "credential.helper=",
		"-c", `credential.helper=!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$MILL_FORGE_USERNAME" "$MILL_FORGE_TOKEN"; }; f`,
	}, args...)
	command := exec.CommandContext(ctx, "git", credentialed...) //nolint:gosec // the suite's own git
	command.Dir = c.dir
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"MILL_FORGE_USERNAME="+env.ForgeUser,
		"MILL_FORGE_TOKEN="+env.ForgeToken)
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}

// goodSignatureFor is what git prints for a good SSH signature made by the
// key an allowed_signers file trusts, naming that line's principal — which
// is the identity the mill commits under (internal/workspace/signing,
// Key.AllowedSignersLine). The mill signs with `gpg.format=ssh`, so git
// hands verification to ssh-keygen and prints its words rather than GPG's
// status keywords.
func goodSignatureFor(allowedSigners string) string {
	GinkgoHelper()
	line, err := os.ReadFile(allowedSigners) //nolint:gosec // a path the suite computed
	Expect(err).NotTo(HaveOccurred(), "the mill published an allowed_signers line at %s", allowedSigners)
	fields := strings.Fields(string(line))
	Expect(fields).NotTo(BeEmpty(), "the allowed_signers line names a principal")
	return `Good "git" signature for ` + fields[0]
}

// allowedSignersPath is where the local workspace publishes the mill's line,
// beside the key and above the run directories.
func allowedSignersPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, "signing", "allowed_signers")
}
