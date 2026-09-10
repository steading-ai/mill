package mill_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// The field the tree under test removes. Nothing outside the generated code
// reads Cancel.grace_ms, so removing it is a pure contract change: the tree
// still compiles, still lints, and still passes every generated-code check
// once `make generate` has run.
const removedField = "grace_ms"

var _ = Describe("The contract gate", Ordered, Label("unit", "slow"), func() {
	// A checkout whose main branch carries the contract as it stands and
	// whose working tree removes a field from it. Both specs read this one
	// tree, and neither depends on the other having run: `buf breaking`
	// reads the files on disk, so it reports the removal whether or not the
	// commit spec has already landed it on a branch beside main.
	var dir string

	BeforeAll(func() {
		By("given a tree that removes a proto field and passes every other lint")
		dir = checkout()
		gitInit(dir)

		path := filepath.Join(dir, "proto/harness/v1/harness.proto")
		source, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		field := "  // How long the session may take to stop cleanly before it is killed, in\n" +
			"  // milliseconds. Zero means the sidecar's default grace.\n" +
			"  int32 " + removedField + " = 2;\n"
		Expect(string(source)).To(ContainSubstring(field))
		Expect(os.WriteFile(path, []byte(strings.Replace(string(source), field, "", 1)), 0o644)).To(Succeed())

		// Generated code is committed, so the removal is only half a change
		// until both templates have been rewritten from the proto.
		Expect(makeIn(dir, 10*time.Minute, "generate")).To(gexec.Exit(0))
		git(dir, "add", "-A")
	})

	It("A deliberate contract change commits", func() {
		By("when the pre-commit hook runs")
		session := gitSession(dir, "commit", "-m", "harness.v1: drop Cancel."+removedField)

		By("then the commit succeeds")
		Expect(session).To(gexec.Exit(0), hookOutput(session))
		Expect(git(dir, "log", "-1", "--format=%s")).To(Equal("harness.v1: drop Cancel." + removedField))
		Expect(git(dir, "status", "--porcelain")).To(BeEmpty())
	})

	It("The contract check still speaks", func() {
		By("when make lint-contract runs")
		session := makeIn(dir, 5*time.Minute, "lint-contract")

		By("then it fails and names the removed field")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(hookOutput(session)).To(SatisfyAll(
			ContainSubstring(removedField),
			ContainSubstring("Cancel"),
			ContainSubstring("proto/harness/v1/harness.proto"),
		))
	})
})

// gitInit makes a copied checkout a repository whose main branch holds the
// tree as it stands, and moves off main so a later commit leaves main as the
// thing `buf breaking` compares against.
//
// core.hooksPath is set before the first commit, not after: git resolves it
// from the developer's own configuration otherwise, and whatever hooks they
// have installed globally are not what these specs are about. The baseline
// commit skips hooks entirely — it is setup, and the hook is what the spec
// below exercises.
func gitInit(dir string) {
	GinkgoHelper()
	git(dir, "init", "--quiet", "--initial-branch=main")

	hooks := GinkgoT().TempDir()
	// The hook is hk's, minus the unit suite: this spec is in that suite,
	// so running it inside the copy would re-enter the suite that spawned
	// it. The step under test is `lint`, which is where the contract check
	// used to live.
	Expect(os.WriteFile(filepath.Join(hooks, "pre-commit"),
		[]byte("#!/bin/sh\nexec hk run pre-commit --skip-step test-unit \"$@\"\n"), 0o755)).To(Succeed())
	git(dir, "config", "core.hooksPath", hooks)

	git(dir, "add", "-A")
	git(dir, "commit", "--quiet", "--no-verify", "-m", "the contract as it stands")
	git(dir, "checkout", "--quiet", "-b", "contract-change")
}

// git runs one git command in the checkout, insists it succeeds, and returns
// its trimmed standard output.
func git(dir string, args ...string) string {
	GinkgoHelper()
	session := gitSession(dir, args...)
	Expect(session).To(gexec.Exit(0), hookOutput(session))
	return strings.TrimSpace(string(session.Out.Contents()))
}

// gitSession runs one git command in the checkout and waits for it, without
// judging the result. The identity is this suite's, and HK_CONFIG_DIR points
// at an empty directory so the hook runs the repository's steps and not
// whatever the developer keeps in their own hk config.
func gitSession(dir string, args ...string) *gexec.Session {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=mill specs", "GIT_AUTHOR_EMAIL=specs@mill.invalid",
		"GIT_COMMITTER_NAME=mill specs", "GIT_COMMITTER_EMAIL=specs@mill.invalid",
		"HK_CONFIG_DIR="+GinkgoT().TempDir())
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(10 * time.Minute)
}

// makeIn is makeTarget with a deadline of its own: regenerating both
// templates costs more than a lint run.
func makeIn(dir string, timeout time.Duration, target string) *gexec.Session {
	GinkgoHelper()
	cmd := exec.Command("make", target)
	cmd.Dir = dir
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(timeout)
}

// hookOutput is everything a session said. buf and hk report on standard
// error, make on standard output, and a failure is worth reading whole.
func hookOutput(session *gexec.Session) string {
	return string(session.Out.Contents()) + string(session.Err.Contents())
}
