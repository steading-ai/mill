package cli_test

import (
	"bytes"
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("mill run", func() {
	Describe("--dry-run", Label(testsupport.DBLabel), func() {
		It("prints the workflow's steps and their evidence without executing anything", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)

			out := m.run(ctx, "run", "millspec#1", "--dry-run", "--project", "millspec", "--workflow", "quick-fix")

			Expect(out).To(ContainSubstring("millspec#1 would run workflow quick-fix@"))
			for step, evidence := range map[string]string{
				"gather-context": "artifact",
				"implement":      "artifact",
				"commit":         "commit",
				"clean-tree":     "exit",
				"pr":             "url",
				"merge":          "status",
			} {
				Expect(lineFor(out, step)).To(ContainSubstring(evidence),
					"step %s closes on %s evidence", step, evidence)
			}
			Expect(lineFor(out, "gather-context")).To(ContainSubstring("read"))
			Expect(lineFor(out, "implement")).To(ContainSubstring("read,write,exec"))
		})

		It("prints the feature workflow's twelve steps with their evidence and capabilities", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", feature)

			out := m.run(ctx, "run", "millspec#1", "--dry-run", "--project", "millspec", "--workflow", "feature")

			Expect(out).To(ContainSubstring("millspec#1 would run workflow feature@"))
			for step, evidence := range map[string]string{
				"gather-context": "artifact",
				"think":          "artifact",
				"plan":           "artifact",
				"review-plan":    "verdict",
				"plan-approval":  "gate",
				"implement":      "artifact",
				"simplify":       "artifact",
				"commit":         "commit",
				"clean-tree":     "exit",
				"pr":             "url",
				"review":         "verdict",
				"merge":          "status",
			} {
				Expect(lineFor(out, step)).To(ContainSubstring(evidence),
					"step %s closes on %s evidence", step, evidence)
			}
			Expect(lineFor(out, "plan")).To(ContainSubstring("read,write"))
			Expect(lineFor(out, "review")).To(ContainSubstring("read"))
			Expect(lineFor(out, "review")).NotTo(ContainSubstring("write"))
		})

		// Selection's second rule as an operator sees it before spending
		// anything: no --workflow means no project default, which means the
		// assessment chooses (docs/domain-model.md §Selection).
		It("says it would assess, and names the workflows the assessment could choose between", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)
			m.run(ctx, "workflow", "push", feature)
			m.run(ctx, "workflow", "push", buildWorkflow)

			out := m.run(ctx, "run", "millspec#1", "--dry-run", "--project", "millspec")

			Expect(out).To(ContainSubstring("would assess"))
			Expect(out).To(ContainSubstring("build-workflow@"))
			Expect(out).To(ContainSubstring("assess step"))
			Expect(lineFor(out, "quick-fix")).NotTo(BeEmpty())
			Expect(lineFor(out, "feature")).NotTo(BeEmpty())
			// The assessment's own workflow is not something it may choose.
			Expect(out).NotTo(MatchRegexp(`(?m)^build-workflow\s`))
		})

		It("refuses to dry-run an assessment the store has no build-workflow for", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)

			_, err := m.try(ctx, "run", "millspec#1", "--dry-run", "--project", "millspec")

			Expect(err).To(MatchError(ContainSubstring("build-workflow")))
		})

		It("claims nothing: it never reaches dagger", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)

			// The dagger socket is a path nothing listens on. A dry run that
			// claimed, explained, or read a node would fail against it.
			out := m.run(ctx, "run", "millspec#1", "--dry-run", "--workflow", "quick-fix",
				"--project", "millspec", "--dagger-socket", "/nonexistent/dg.sock")
			Expect(out).To(ContainSubstring("quick-fix"))
		})
	})

	// Scenario: The seam is closed in production
	//   Given MILL_ACCEPTANCE is unset
	//   When mill run is invoked with a skill override
	//   Then it refuses before claiming
	Describe("--skill-override", Label("unit"), func() {
		var stdout, stderr bytes.Buffer

		BeforeEach(func() {
			stdout.Reset()
			stderr.Reset()
		})

		// The socket and the dagger socket are paths nothing listens on, so
		// a command that got as far as reading a workflow, claiming a node
		// or taking a lease would fail on one of them and say so. The
		// refusal has to arrive before any of that.
		invoke := func(args ...string) int {
			return cli.Run(append([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, args...), strings.NewReader(""), &stdout, &stderr)
		}

		It("The seam is closed in production", func() {
			By("given MILL_ACCEPTANCE is unset")
			GinkgoT().Setenv("MILL_ACCEPTANCE", "")

			By("when mill run is invoked with a skill override")
			exit := invoke("--skill-override", "implement="+GinkgoT().TempDir())

			By("then it refuses before claiming")
			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).To(ContainSubstring("test seam"))
			Expect(stderr.String()).To(ContainSubstring("MILL_ACCEPTANCE"))
			Expect(stderr.String()).To(ContainSubstring("Nothing was claimed"))
			Expect(stderr.String()).NotTo(ContainSubstring("nonexistent"),
				"the refusal came before anything was read, claimed or leased")
		})

		It("is allowed under the acceptance suite, and then the directory has to be a skill", func() {
			GinkgoT().Setenv("MILL_ACCEPTANCE", "1")

			// Past the refusal, the seam loads a skill directory exactly as
			// `mill workflow push` would: an empty directory is not one.
			Expect(invoke("--skill-override", "implement="+GinkgoT().TempDir())).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("test seam"))
			Expect(stderr.String()).To(ContainSubstring("--skill-override implement="))
		})
	})

	Describe("exit status", func() {
		It("gives a fenced run a status of its own", func() {
			Expect(cli.ExitCode(run.ErrFenced)).To(Equal(cli.ExitFenced))
			Expect(cli.ExitCode(context.Canceled)).To(Equal(1))
		})
	})
})

// lineFor is the dry run's row for one step.
func lineFor(output, step string) string {
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == step {
			return line
		}
	}
	return ""
}
