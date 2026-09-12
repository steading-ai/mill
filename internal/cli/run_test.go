package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace/socket"
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

	// Scenario: A socket path that cannot bind is refused before the claim
	//   Given a workspace directory whose socket path would exceed the limit
	//   When mill run is invoked for a ready node
	//   Then it exits before claiming
	//   And the message names the length and the limit
	//
	// The workspace socket lives under --workspace-dir, one directory per
	// run, so the run id and the socket's own name are part of the path a
	// claim will later try to bind. A directory that is short enough to
	// create and too long to serve is the case an operator hit: the failure
	// arrived as "bind: invalid argument", after the node was claimed and
	// the repository cloned.
	Describe("--workspace-dir", Label("unit"), func() {
		var stdout, stderr bytes.Buffer

		BeforeEach(func() {
			stdout.Reset()
			stderr.Reset()
		})

		It("A socket path that cannot bind is refused before the claim", func() {
			By("given a workspace directory whose socket path would exceed the limit")
			dir := filepath.Join(GinkgoT().TempDir(), strings.Repeat("w", socket.MaxPathLen))

			By("when mill run is invoked for a ready node")
			// Every socket named here is a path nothing listens on, so a
			// command that reached the store, dagger or a lease would fail
			// on one of them and say so.
			exit := cli.Run([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--workspace-dir", dir,
				"--vcs-url", "https://gitlab.example.test/group/thing.git",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			By("then it exits before claiming")
			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("nonexistent"),
				"the refusal came before anything was read, claimed or leased")

			By("and the message names the length and the limit")
			Expect(stderr.String()).To(ContainSubstring("--workspace-dir"))
			Expect(stderr.String()).To(ContainSubstring(strconv.Itoa(socket.MaxPathLen)))
			// The path it would have bound, and how long that is.
			Expect(stderr.String()).To(ContainSubstring(dir))
			Expect(stderr.String()).To(MatchRegexp(`is \d+ bytes long`))
			Expect(stderr.String()).NotTo(ContainSubstring("invalid argument"))
		})

		It("takes a workspace directory whose socket path fits", func() {
			// The same command, with a directory short enough to serve
			// from: past the check, it fails on the store it cannot reach,
			// which is proof the check let it through.
			exit := cli.Run([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--workspace-dir", GinkgoT().TempDir(),
				"--vcs-url", "https://gitlab.example.test/group/thing.git",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("bytes long"))
		})
	})

	// The credential gateway is configuration: on, a run's sessions are
	// pointed at it and it must hold the upstream credential before a node
	// is claimed; off, a run behaves as it always has.
	Describe("--gateway", Label("unit"), func() {
		var stdout, stderr bytes.Buffer

		BeforeEach(func() {
			stdout.Reset()
			stderr.Reset()
		})

		It("refuses to start without the upstream credential, before claiming", func() {
			By("given gateway: true in the file and no credential file on the host")
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			missing := filepath.Join(GinkgoT().TempDir(), "claude-oauth")
			Expect(os.WriteFile(path, []byte("gateway: true\nmodel-credential-file: "+missing+"\n"), 0o600)).To(Succeed())

			By("when a worker is started")
			exit := cli.Run([]string{
				"work",
				"--config", path,
				"--project", "millspec",
				"--vcs-url", "https://gitlab.example.test/group/thing.git",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			By("then it fails naming the credential file, and nothing was claimed")
			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("nonexistent"))
			Expect(stderr.String()).To(ContainSubstring("--gateway"))
			Expect(stderr.String()).To(ContainSubstring("no upstream credential"))
			Expect(stderr.String()).To(ContainSubstring(missing))
		})

		It("never reads the credential from the environment", func() {
			// A variable is inherited by every child; the one process that
			// may hold the credential is the gateway's, from a file.
			GinkgoT().Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-from-the-environment")
			GinkgoT().Setenv("CREDENTIALS_DIRECTORY", "")
			exit := cli.Run([]string{
				"run", "millspec#1",
				"--gateway",
				"--project", "millspec",
				"--vcs-url", "https://gitlab.example.test/group/thing.git",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)
			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).To(ContainSubstring("no upstream credential"))
			Expect(stderr.String()).To(ContainSubstring("CREDENTIALS_DIRECTORY"))
			Expect(stderr.String()).NotTo(ContainSubstring("from-the-environment"))
		})
	})

	// Scenario: The machine backend is chosen and honest about what it cannot do yet
	//   Given workspace: machine
	//   When a run is started on a host with no sandbox root
	//   Then it fails before claiming with an error naming what the host lacks
	Describe("--workspace", Label("unit"), func() {
		var stdout, stderr bytes.Buffer

		BeforeEach(func() {
			stdout.Reset()
			stderr.Reset()
		})

		It("The machine backend is chosen and honest about what it cannot do yet", func() {
			By("given workspace: machine")
			// The skill and the sidecar are materialised under a home of the
			// spec's own, and the sidecar is copied from a built tree of the
			// spec's own, so the preflight touches nothing under the real
			// home and fails on the one thing the host lacks. The home is
			// what redirects them: `mill run` takes the skill and sidecar
			// directories from their defaults, which are under $HOME — the
			// file's `skill-dir` and `sidecar-dir` are read by `mill work`
			// alone (applySandboxConfig) — and inside a machine the real one
			// is the read-only bind the machine mounts them from.
			tmp := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", tmp)
			entrypoint := filepath.Join(tmp, "src", "sidecar", "dist", "main.js")
			Expect(os.MkdirAll(filepath.Dir(entrypoint), 0o755)).To(Succeed())
			Expect(os.WriteFile(entrypoint, []byte("//\n"), 0o600)).To(Succeed())
			path := filepath.Join(tmp, "config.yaml")
			Expect(os.WriteFile(path, []byte("workspace: machine\n"), 0o600)).To(Succeed())
			GinkgoT().Setenv("MILL_CONFIG", path)

			By("when a run is started on a host with no sandbox root")
			// Every socket named here is a path nothing listens on, so a
			// command that reached the store, dagger or a lease would fail
			// on one of them and say so. The sandbox root is a path that is
			// not there, which is the preflight's first question.
			exit := cli.Run([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--workspace", "machine",
				"--sandbox-root", filepath.Join(tmp, "absent-sandbox"),
				"--sidecar", entrypoint,
				"--vcs-url", "https://gitlab.example.test/group/thing.git",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			By("then it fails before claiming with an error naming what the host lacks")
			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("nonexistent"),
				"the refusal came before anything was read, claimed or leased")
			Expect(stderr.String()).To(ContainSubstring("--workspace machine"))
			Expect(stderr.String()).To(ContainSubstring("absent-sandbox"))
		})

		It("refuses a kind it does not know before claiming", func() {
			exit := cli.Run([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--workspace", "podman",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).NotTo(ContainSubstring("nonexistent"))
			Expect(stderr.String()).To(ContainSubstring("--workspace"))
			Expect(stderr.String()).To(ContainSubstring("podman"))
		})
	})

	// The project a run delivers to comes from the flag or the variable and
	// from nowhere else: dagger's project model carries neither a
	// repository field nor labels (dagger#23), so a refusal that named a
	// label sent an operator looking for something that cannot exist.
	Describe("without a repository", Label("unit"), func() {
		It("names the flag and the variable, and mentions no label", func() {
			var stdout, stderr bytes.Buffer
			GinkgoT().Setenv("MILL_VCS_URL", "")

			exit := cli.Run([]string{
				"run", "millspec#1",
				"--project", "millspec",
				"--socket", "/nonexistent/mill.sock",
				"--dagger-socket", "/nonexistent/dg.sock",
			}, strings.NewReader(""), &stdout, &stderr)

			Expect(exit).NotTo(BeZero())
			Expect(stderr.String()).To(ContainSubstring("--vcs-url"))
			Expect(stderr.String()).To(ContainSubstring("MILL_VCS_URL"))
			Expect(stderr.String()).To(ContainSubstring("dagger#23"))
			Expect(stderr.String()).NotTo(ContainSubstring("label"))
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
