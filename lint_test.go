package mill_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Lint and package boundaries", Label("unit", "slow"), func() {
	It("make lint passes on the current tree", func() {
		Expect(makeTarget(checkout(), "lint")).To(gexec.Exit(0))
	})

	It("The forbidigo rule guards the workspace chokepoint", func() {
		By("given a file internal/run/x.go that calls exec.Command")
		dir := checkout()
		writeFile(dir, "internal/run/x.go",
			"package run\n\nimport \"os/exec\"\n\n// X spawns a process outside the workspace chokepoint.\nfunc X() error { return exec.Command(\"true\").Run() }\n")

		By("when make lint runs")
		session := makeTarget(dir, "lint")

		By("then it fails naming forbidigo and the os/exec pattern")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/run/x.go"),
			ContainSubstring("(forbidigo)"),
			ContainSubstring("os/exec"),
		))
	})

	It("Specs stay in Ginkgo", func() {
		By(`given a test file that is not a suite bootstrap importing "testing"`)
		dir := checkout()
		writeFile(dir, "internal/version/helper_test.go",
			"package version_test\n\nimport \"testing\"\n\nvar _ = func(t *testing.T) { t.Helper() }\n")

		By("when make lint runs")
		session := makeTarget(dir, "lint")

		By("then it fails naming the forbidigo rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/version/helper_test.go"),
			ContainSubstring("(forbidigo)"),
		))
	})

	It("Only the domain layer may reach the store", func() {
		By(`given a file under "internal/worker" importing "internal/store"`)
		dir := checkout()
		writeFile(dir, "internal/store/store.go",
			"// Package store is the persistence layer.\npackage store\n\n// Open is a placeholder the depguard spec imports.\nfunc Open() {}\n")
		writeFile(dir, "internal/worker/worker.go",
			"package worker\n\nimport \"github.com/ivy/mill/internal/store\"\n\n// Work reaches the store directly.\nfunc Work() { store.Open() }\n")

		By("when make lint runs")
		session := makeTarget(dir, "lint")

		By("then it fails naming the depguard rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/worker/worker.go"),
			ContainSubstring("(depguard)"),
		))
	})
})
