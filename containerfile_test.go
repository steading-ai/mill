package mill_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// The image packaging/Containerfile describes is scratch: no shell, no
// coreutils, nothing but the files a consumer copies out or the mill binary
// itself. A spec that inspected it by running `ls` or `cat` inside it would
// fail with "exec: not found" whether or not the COPYs landed, so every
// spec below inspects the built image from outside instead: podman run's
// own command is the image's one binary, and podman export lists the
// filesystem without starting anything.
var _ = Describe("The container image", Label("unit", "slow"), Ordered, func() {
	var (
		dir string
		tag string
	)

	// output runs cmd to completion within timeout, requires it to succeed,
	// and returns its trimmed stdout — the one place every podman/make
	// invocation below asserts success, instead of each repeating the same
	// gexec.Start/Wait/Exit(0) triple.
	output := func(cmd *exec.Cmd, timeout time.Duration) string {
		GinkgoHelper()
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(timeout)).To(gexec.Exit(0))
		return strings.TrimSpace(string(session.Out.Contents()))
	}

	BeforeAll(func() {
		dir = checkout()
		tag = "mill-ci-test-" + strings.ToLower(filepath.Base(dir))

		By("when make stage-amd64 runs")
		Expect(makeTarget(dir, "stage-amd64")).To(gexec.Exit(0))

		By("and podman builds packaging/Containerfile from the staged tree")
		cmd := exec.Command("podman", "build",
			"--file", "packaging/Containerfile",
			"--build-arg", "REVISION=test",
			"--tag", tag,
			".")
		cmd.Dir = dir
		cmd.Env = scratchEnv()
		output(cmd, 3*time.Minute)
	})

	AfterAll(func() {
		output(exec.Command("podman", "rmi", "--force", tag), 30*time.Second)
	})

	It("reports the version the checkout was staged from", func() {
		got := output(exec.Command("podman", "run", "--rm", tag, "mill", "version"), 30*time.Second)

		// The same source of truth Makefile:1-20 stamps a local build with,
		// so the image's own report is checked against it rather than
		// against a hardcoded string.
		versionCmd := exec.Command("make", "-s", "version")
		versionCmd.Dir = dir
		versionCmd.Env = scratchEnv()
		want := "mill version " + output(versionCmd, 30*time.Second)

		Expect(got).To(Equal(want))
	})

	It("carries the binary, the sidecar bundle and the workflows at the paths a consumer's mill is told to find them", func() {
		container := output(exec.Command("podman", "create", tag), 30*time.Second)
		DeferCleanup(func() {
			output(exec.Command("podman", "rm", "--force", container), 30*time.Second)
		})

		By("then podman export lists the flattened filesystem, since scratch has no shell to ls with")
		listing := output(exec.Command("sh", "-euc", `podman export "$0" | tar -tf -`, container), 30*time.Second)
		files := strings.Split(listing, "\n")
		Expect(files).To(ContainElement("usr/local/bin/mill"))
		Expect(files).To(ContainElement("usr/local/libexec/mill/sidecar/dist/main.js"))
		Expect(files).To(ContainElement("usr/local/share/mill/workflows/feature/workflow.yaml"))
	})
})
