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
		Entry("run", "run"),
		Entry("restart", "restart"),
	)

	It("an unknown subcommand is an error", func() {
		Expect(run("nonesuch")).NotTo(BeZero())
	})
})
