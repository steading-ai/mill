package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workflow"
)

// quickFix is the workflow directory in this repository, which is what the
// round-trip below pushes.
var quickFix = filepath.Join("..", "..", "workflows", "quick-fix")

// feature is the workflow that plans first, and the one that reaches for
// skills quick-fix's directory carries.
var feature = filepath.Join("..", "..", "workflows", "feature")

// buildWorkflow is the workflow that chooses a workflow: what a run with no
// project default assesses with (docs/domain-model.md §Selection).
var buildWorkflow = filepath.Join("..", "..", "workflows", "build-workflow")

// mill runs the binary's command tree against a socket a service is
// listening on, and returns everything it printed.
type mill struct {
	socket string
	// databaseURL is the ledger behind the socket, for a spec that needs a
	// second face on the same database — one under a different role map.
	databaseURL string
}

func (m mill) run(ctx context.Context, args ...string) string {
	GinkgoHelper()
	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs(append(args, "--socket", m.socket))
	Expect(root.ExecuteContext(ctx)).To(Succeed(), stderr.String())
	return stdout.String()
}

// try is run for a command a spec expects to refuse: what it printed, and
// why it stopped.
func (m mill) try(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs(append(args, "--socket", m.socket))
	err := root.ExecuteContext(ctx)
	return stdout.String(), err
}

// serving starts `mill serve` on its own database and socket for one spec.
func serving(ctx context.Context) mill {
	GinkgoHelper()
	return servingWith(ctx)
}

// servingWithLogDir is serving with a worker's --log-dir given explicitly,
// for a spec that writes activity files straight to disk the way a worker
// would rather than through the store's own write path.
func servingWithLogDir(ctx context.Context, logDir string) mill {
	GinkgoHelper()
	return servingWith(ctx, "--log-dir", logDir)
}

// servingWith is serving with extra `mill serve` flags, for the one spec
// shape (servingWithLogDir) that needs to pick a setting serving otherwise
// defaults.
func servingWith(ctx context.Context, extra ...string) mill {
	GinkgoHelper()
	databaseURL := pg.Database(ctx).Config().ConnString()
	socket := filepath.Join(GinkgoT().TempDir(), "mill.sock")

	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs(append([]string{"serve", "--socket", socket, "--database-url", databaseURL}, extra...))
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		served <- root.ExecuteContext(serveCtx)
	}()
	DeferCleanup(func() {
		stop()
		Eventually(served).Should(Receive(BeNil()), stderr.String())
	})
	// The service migrates before it binds, and several of these run at
	// once, so the wait is generous: this is "has it started", not a bound
	// on how long starting may take.
	Eventually(func() error {
		_, err := os.Stat(socket)
		return err
	}, 60*time.Second, 50*time.Millisecond).Should(Succeed(), stderr.String())
	return mill{socket: socket, databaseURL: databaseURL}
}

// storedVersion reads the version out of a `workflow …` line of push or
// pull output.
func storedVersion(output string) string {
	GinkgoHelper()
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 3 && fields[0] == "workflow" {
			return fields[3]
		}
	}
	Fail("no workflow line in:\n" + output)
	return ""
}

var _ = Describe("mill workflow", Label(testsupport.DBLabel), func() {
	It("push stores one version and its skills, and pushing it again stores nothing new", func(ctx SpecContext) {
		m := serving(ctx)

		first := m.run(ctx, "workflow", "push", quickFix)
		Expect(first).To(ContainSubstring("skill implement"))
		Expect(strings.Count(first, "stored")).To(Equal(5), "four skills and the workflow")

		again := m.run(ctx, "workflow", "push", quickFix)
		Expect(storedVersion(again)).To(Equal(storedVersion(first)), "the same files are the same version")
		Expect(again).NotTo(ContainSubstring("stored"))
		Expect(strings.Count(again, "unchanged")).To(Equal(5))

		listed := m.run(ctx, "workflow", "list", "--all-versions")
		Expect(strings.Count(listed, "quick-fix")).To(Equal(1), "one push, one version")
	})

	It("changing one byte of a skill body is a new version, and the old one is untouched", func(ctx SpecContext) {
		m := serving(ctx)
		before := storedVersion(m.run(ctx, "workflow", "push", quickFix))

		// A copy of the workflow with one character added to one skill.
		edited := GinkgoT().TempDir()
		definition, skills, err := workflow.Load(quickFix)
		Expect(err).NotTo(HaveOccurred())
		for _, skill := range skills {
			if skill.GetName() == "implement" {
				skill.Body += "!"
			}
		}
		Expect(workflow.Write(edited, definition, skills)).To(Succeed())

		after := storedVersion(m.run(ctx, "workflow", "push", edited))
		Expect(after).NotTo(Equal(before))

		listed := m.run(ctx, "workflow", "list", "--all-versions")
		Expect(listed).To(ContainSubstring(before), "the version a run in flight follows is still there")
		Expect(listed).To(ContainSubstring(after))
	})

	It("pull writes a directory push accepts with no new version", func(ctx SpecContext) {
		m := serving(ctx)
		pushed := storedVersion(m.run(ctx, "workflow", "push", quickFix))

		pulled := filepath.Join(GinkgoT().TempDir(), "quick-fix")
		m.run(ctx, "workflow", "pull", "quick-fix", pulled)
		Expect(filepath.Join(pulled, workflow.DefinitionFile)).To(BeAnExistingFile())
		Expect(filepath.Join(pulled, workflow.SkillsDir, "implement", workflow.SkillFile)).To(BeAnExistingFile())

		repushed := m.run(ctx, "workflow", "push", pulled)
		Expect(storedVersion(repushed)).To(Equal(pushed))
		Expect(repushed).NotTo(ContainSubstring("stored"))
	})

	It("push stores feature with its twelve steps and every skill it references", func(ctx SpecContext) {
		m := serving(ctx)

		out := m.run(ctx, "workflow", "push", feature)
		// Nine skills and the workflow: five of feature's own and the four
		// it reaches for in quick-fix's directory.
		Expect(strings.Count(out, "stored")).To(Equal(10))
		for _, skill := range []string{"think", "plan", "review-plan", "simplify", "review",
			"gather-context", "implement", "commit", "pr"} {
			Expect(out).To(ContainSubstring("skill " + skill))
		}

		shown := m.run(ctx, "workflow", "show", "feature")
		for _, step := range []string{"gather-context", "think", "plan", "review-plan", "plan-approval",
			"implement", "simplify", "commit", "clean-tree", "pr", "review", "merge"} {
			Expect(shown).To(ContainSubstring(step))
		}
		Expect(lineFor(shown, "review")).To(ContainSubstring("verdict"))

		By("and the skills quick-fix already stored are the same records")
		quick := m.run(ctx, "workflow", "push", quickFix)
		Expect(strings.Count(quick, "unchanged")).To(Equal(4), "feature stored all four already")

		listed := m.run(ctx, "workflow", "list")
		Expect(listed).To(ContainSubstring("feature"))
		Expect(listed).To(ContainSubstring("quick-fix"))
	})

	It("show prints quick-fix's six steps with the skill digest each pins", func(ctx SpecContext) {
		m := serving(ctx)
		pushed := m.run(ctx, "workflow", "push", quickFix)

		shown := m.run(ctx, "workflow", "show", "quick-fix")
		// Six steps, in the order a run walks them.
		steps := []string{"gather-context", "implement", "commit", "clean-tree", "pr", "merge"}
		for _, step := range steps {
			Expect(rowFor(shown, step)).NotTo(BeEmpty(), "step %s is printed", step)
		}
		Expect(rowFor(shown, "STEP")).To(ContainSubstring("SKILL"))

		// Each skill step names the digest the push stored, so what an
		// operator reads here is the body the harness will be handed.
		for _, step := range []string{"gather-context", "implement", "commit", "pr"} {
			digest := digestOf(pushed, step)
			Expect(rowFor(shown, step)).To(ContainSubstring(step+"@"+digest),
				"%s pins the skill the push stored", step)
		}
		// A check step and a gate step pin no skill.
		Expect(strings.Fields(rowFor(shown, "clean-tree"))).To(ContainElement("-"))

		// And the version show prints is the one list prints: the digests
		// above are what that version is the hash of.
		version := storedVersion(pushed)
		Expect(shown).To(ContainSubstring(version))
		Expect(rowFor(m.run(ctx, "workflow", "list"), "quick-fix")).To(ContainSubstring(version))
	})

	It("show takes the version as an @ suffix or as a flag", func(ctx SpecContext) {
		m := serving(ctx)
		m.run(ctx, "workflow", "push", quickFix)
		// The header line is `<name> <scope> <version>`, and the version
		// there is the full hash a --version resolves against.
		version := strings.Fields(lines(m.run(ctx, "workflow", "show", "quick-fix"))[0])[2]

		suffixed := m.run(ctx, "workflow", "show", "quick-fix@"+version)
		flagged := m.run(ctx, "workflow", "show", "quick-fix", "--version", version)
		Expect(suffixed).To(Equal(flagged))
		Expect(suffixed).To(ContainSubstring(version))
	})

	It("a project push shadows an org one without replacing it", func(ctx SpecContext) {
		m := serving(ctx)
		m.run(ctx, "workflow", "push", quickFix)
		m.run(ctx, "workflow", "push", quickFix, "--scope", "project", "--project", "P")

		Expect(m.run(ctx, "workflow", "list")).To(ContainSubstring("project:P"))
		Expect(m.run(ctx, "workflow", "list", "--scope", "org")).NotTo(ContainSubstring("project:P"))
	})
})

// digestOf is the short digest a `workflow push` reported for one skill.
func digestOf(output, skill string) string {
	GinkgoHelper()
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "skill" && fields[1] == skill {
			return fields[2]
		}
	}
	Fail("no skill line for " + skill + " in:\n" + output)
	return ""
}
