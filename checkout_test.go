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

// checkout copies the working tree, tracked and untracked files alike, into
// a fresh directory. Ginkgo compiles each suite to a binary beside its
// package and removes it afterwards, so two runs over one tree would race on
// those files.
func checkout() string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	cmd := exec.Command("sh", "-euc",
		`git ls-files -z --cached --others --exclude-standard | tar --null --files-from - -cf - | tar -C "$0" -xf -`, dir)
	cmd.Env = scratchEnv()
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Expect(session.Wait(time.Minute)).To(gexec.Exit(0))
	trustConfig(filepath.Join(dir, "mise.toml"))
	return dir
}

// trustConfig lets mise read the copied mise.toml at its new path, so the
// tools make invokes in the copy resolve to the pinned versions. Trust is
// per path; it is withdrawn when the copy is removed.
func trustConfig(path string) {
	GinkgoHelper()
	run := func(args ...string) {
		session, err := gexec.Start(exec.Command("mise", args...), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(time.Minute)).To(gexec.Exit(0))
	}
	run("trust", "--quiet", path)
	DeferCleanup(run, "trust", "--quiet", "--untrust", path)
}

// writeFile creates a Go source file inside a copied checkout, making any
// directories it needs.
func writeFile(root, rel, content string) {
	GinkgoHelper()
	path := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
}

// makeTarget runs one make target in the given checkout and waits for it to
// exit. Every package compiles from the shared build cache, so a couple of
// minutes covers a cold golangci-lint run with margin.
func makeTarget(dir, target string) *gexec.Session {
	GinkgoHelper()
	cmd := exec.Command("make", target)
	cmd.Dir = dir
	cmd.Env = scratchEnv()
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(3 * time.Minute)
}

// scratchEnv is the environment for a subprocess that operates on a scratch
// checkout rather than the repository this suite runs from. Under a git
// hook the process inherits GIT_DIR, GIT_INDEX_FILE and the rest of git's
// GIT_* variables, and every git command a scratch-checkout subprocess runs
// — directly, or indirectly through a make target — would otherwise act on
// the real repository regardless of the subprocess's own working directory.
func scratchEnv(extra ...string) []string {
	env := os.Environ()
	clean := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return append(clean, extra...)
}
