package mill_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

// The version a build carries comes from the tree it was built from: a
// clean tag is a release and everything else says so.
var _ = Describe("The version", Label("unit"), func() {
	var repo string

	// Under a git hook the process inherits GIT_DIR and GIT_INDEX_FILE, and
	// every git command below would then act on the real repository rather
	// than the scratch one, whatever its working directory. The scratch
	// repository is named by its directory alone.
	in := func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(name, args...)
		cmd.Dir = repo
		cmd.Env = testsupport.ScratchEnv()
		return cmd
	}

	git := func(args ...string) string {
		GinkgoHelper()
		cmd := in("git", append([]string{"-c", "user.name=spec", "-c", "user.email=spec@example.invalid"}, args...)...)
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		return strings.TrimSpace(string(out))
	}

	version := func() string {
		GinkgoHelper()
		out, err := in("make", "-s", "version").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		return strings.TrimSpace(string(out))
	}

	BeforeEach(func() {
		repo = GinkgoT().TempDir()
		makefile, err := os.ReadFile("Makefile")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(repo, "Makefile"), makefile, 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)).To(Succeed())
		git("init", "-q")
		git("add", "Makefile", "main.go")
		git("commit", "-q", "-m", "release")
		git("tag", "v1.2.3")
	})

	It("is the tag, and nothing more, for a build from a clean tag", func() {
		Expect(version()).To(Equal("1.2.3"))
	})

	It("carries a development marker for a build from a dirty tree", func() {
		Expect(os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // edited\n"), 0o644)).To(Succeed())
		Expect(version()).To(MatchRegexp(`^1\.2\.3\+dev\.0\.[0-9a-f]+\.dirty$`))
	})

	It("carries a development marker, with the distance and the commit, for a build past the tag", func() {
		Expect(os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // next\n"), 0o644)).To(Succeed())
		git("commit", "-q", "-am", "next")
		sha := git("rev-parse", "--short", "HEAD")
		Expect(version()).To(Equal("1.2.3+dev.1." + sha))
	})
})
