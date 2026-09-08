package mill_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Lint and package boundaries", Label("unit", "slow"), func() {
	It("make lint passes on the current tree", func() {
		Expect(makeTarget(checkout(), "lint")).To(gexec.Exit(0))
	})

	It("os/exec is imported only under internal/workspace", func() {
		By("given the tree as it stands")
		var importers []string

		By("when every package under internal/ is read")
		Expect(filepath.WalkDir("internal", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(source), `"os/exec"`) {
				importers = append(importers, filepath.ToSlash(path))
			}
			return nil
		})).To(Succeed())

		By("then only files under internal/workspace/ import it")
		Expect(importers).NotTo(BeEmpty(), "the chokepoint itself should be in the list")
		for _, path := range importers {
			// Specs drive subprocesses of their own; the rule is about what
			// mill does at runtime, which is what the chokepoint owns.
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			Expect(path).To(HavePrefix("internal/workspace/"))
		}
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
		writeFile(dir, "internal/worker/worker.go",
			"package worker\n\nimport \"github.com/ivy/mill/internal/store\"\n\n// Fenced reaches the store directly.\nvar Fenced = store.ErrFenced\n")

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
