package cli_test

import (
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

			out := m.run(ctx, "run", "millspec#1", "--dry-run", "--project", "millspec")

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

		It("claims nothing: it never reaches dagger", func(ctx SpecContext) {
			m := serving(ctx)
			m.run(ctx, "workflow", "push", quickFix)

			// The dagger socket is a path nothing listens on. A dry run that
			// claimed, explained, or read a node would fail against it.
			out := m.run(ctx, "run", "millspec#1", "--dry-run",
				"--project", "millspec", "--dagger-socket", "/nonexistent/dg.sock")
			Expect(out).To(ContainSubstring("quick-fix"))
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
