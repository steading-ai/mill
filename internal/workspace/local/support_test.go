package local_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// gitEnv is what the specs' own git invocations run with: none of the
// operator's configuration, and an identity of their own. The workspace sets
// the same floor for itself; this is the spec's half, for building an origin
// to clone and for verifying what the workspace committed.
func gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_AUTHOR_NAME=spec",
		"GIT_AUTHOR_EMAIL=spec@example.test",
		"GIT_COMMITTER_NAME=spec",
		"GIT_COMMITTER_EMAIL=spec@example.test",
		"LC_ALL=C",
	}
}

// git runs git in dir and returns its trimmed output, failing the spec if it
// exits non-zero.
func git(dir string, args ...string) string {
	GinkgoHelper()

	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = gitEnv()
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// origin builds a repository to clone: one commit on `main`, which is its
// default branch.
func origin() string {
	GinkgoHelper()

	dir := filepath.Join(GinkgoT().TempDir(), "origin")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	git(dir, "init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("as cloned\n"), 0o600)).To(Succeed())
	git(dir, "add", "README.md")
	git(dir, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
	return dir
}
