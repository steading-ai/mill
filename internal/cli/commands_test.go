package cli_test

import (
	"bytes"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/version"
)

var _ = Describe("The command tree", Label("unit"), func() {
	var stdout, stderr bytes.Buffer

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	run := func(args ...string) int {
		return cli.Run(args, strings.NewReader(""), &stdout, &stderr)
	}

	It("version prints the version of the build", func() {
		Expect(run("version")).To(BeZero())
		Expect(stdout.String()).To(ContainSubstring(version.String()))
		Expect(stderr.String()).To(BeEmpty())
	})

	It("serve is wired into the tree and is the service face", func() {
		Expect(run("serve", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--socket"))
		Expect(stdout.String()).To(ContainSubstring("--database-url"))
	})

	DescribeTable("a face is wired into the tree and exits zero",
		func(name string) {
			Expect(run(name)).To(BeZero(), stderr.String())
			Expect(stdout.String()).To(ContainSubstring(name))
		},
		Entry("work", "work"),
	)

	It("restart is wired into the tree and takes the node it restarts", func() {
		Expect(run("restart", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--project"))
		Expect(stdout.String()).To(ContainSubstring("generation"))
		// Restarting is a deliberate act on one named node: without one
		// there is nothing to abandon.
		Expect(run("restart")).NotTo(BeZero())
	})

	It("run is wired into the tree and takes the node it drives", func() {
		Expect(run("run", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--dry-run"))
		Expect(stdout.String()).To(ContainSubstring("--project"))
		// The node is the argument that replaces the claim, so a run
		// without one is a usage error rather than a claim of anything.
		Expect(run("run")).NotTo(BeZero())
	})

	It("workflow carries the verbs that move definitions in and out", func() {
		Expect(run("workflow", "--help")).To(BeZero(), stderr.String())
		for _, verb := range []string{"push", "pull", "list"} {
			Expect(stdout.String()).To(ContainSubstring(verb))
		}
	})

	DescribeTable("a read tool is wired into the tree and sizes its answer with a view",
		func(name string) {
			Expect(run(name, "--help")).To(BeZero(), stderr.String())
			Expect(stdout.String()).To(ContainSubstring("--socket"))
			Expect(stdout.String()).To(ContainSubstring("--view"))
		},
		Entry("status", "status"),
		Entry("inspect", "inspect"),
		Entry("history", "history"),
		Entry("cost", "cost"),
	)

	It("logs takes the step to read and whether to follow it", func() {
		Expect(run("logs", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--step"))
		Expect(stdout.String()).To(ContainSubstring("--follow"))
		Expect(run("logs")).NotTo(BeZero(), "a log is a log of one run")
	})

	It("explain reaches dagger for the gate a run is blocked on", func() {
		Expect(run("explain", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--dagger-socket"))
		Expect(run("explain")).NotTo(BeZero())
	})

	It("cancel takes the run it stops and a reason for the trail", func() {
		Expect(run("cancel", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("--reason"))
		Expect(run("cancel")).NotTo(BeZero())
	})

	It("doctor takes what it probes and needs no argument", func() {
		Expect(run("doctor", "--help")).To(BeZero(), stderr.String())
		for _, flag := range []string{"--dagger-socket", "--sidecar", "--vcs-url", "--workspace-dir"} {
			Expect(stdout.String()).To(ContainSubstring(flag))
		}
	})

	It("mcp serves the same tools over stdio", func() {
		Expect(run("mcp", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("MCP server"))
		Expect(stdout.String()).To(ContainSubstring("--socket"))
	})

	It("workflow show is among the workflow verbs", func() {
		Expect(run("workflow", "--help")).To(BeZero(), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("show"))
	})

	It("an unknown subcommand is an error", func() {
		Expect(run("nonesuch")).NotTo(BeZero())
	})
})
