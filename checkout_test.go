package mill_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/mill/internal/testsupport"
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
	cmd.Env = testsupport.ScratchEnv()
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
// exit. The lint specs that drive this helper carry Serial, isolating them
// from this suite's own parallel procs, but a shared guest's other tenants
// can still make a single `make lint` run take longer than idle or
// moderately-loaded measurements suggest: nine minutes is a 3x margin on the
// three-minute wait that a live-tier run on this guest still exceeded.
func makeTarget(dir, target string) *gexec.Session {
	GinkgoHelper()
	cmd := exec.Command("make", target)
	cmd.Dir = dir
	cmd.Env = testsupport.ScratchEnv()
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(9 * time.Minute)
}
