package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness/claude"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/skill"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/version"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

// The selector is where the backend is chosen, so its specs are written
// against it directly: which function comes back for which configuration,
// proven by what that function makes rather than by what it is called.

// seedOrigin is a repository with one commit on `main`, for the local backend
// to clone.
func seedOrigin() string {
	GinkgoHelper()
	dir := filepath.Join(GinkgoT().TempDir(), "origin")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
	git := func(args ...string) {
		GinkgoHelper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = testsupport.MinimalEnv(testsupport.SpecGit()...)
		out, err := command.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	}
	git("init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o600)).To(Succeed())
	git("add", "README.md")
	git("commit", "--quiet", "--no-gpg-sign", "--message", "seed")
	return dir
}

// builtSidecar stands in for `make sidecar-build`'s output at dir: the
// entrypoint and a scrap of node_modules. It returns the entrypoint, as the
// --sidecar flag names it.
func builtSidecar(dir string) string {
	GinkgoHelper()
	entrypoint := filepath.Join(dir, skill.SidecarEntrypoint)
	Expect(os.MkdirAll(filepath.Dir(entrypoint), 0o755)).To(Succeed())
	Expect(os.WriteFile(entrypoint, []byte("//\n"), 0o600)).To(Succeed())
	Expect(os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)).To(Succeed())
	return entrypoint
}

var _ = Describe("Choosing the workspace backend", Label("unit"), func() {
	var opts runOptions

	BeforeEach(func() {
		opts = runOptions{
			workspaceDir: GinkgoT().TempDir(),
			sandboxRoot:  GinkgoT().TempDir(),
			name:         "mill",
			identity:     "mill@spec",
		}
	})

	// Scenario: The default is what exists today
	//   Given no workspace configuration
	//   When the run loop constructs its Workspaces function
	//   Then it is the local backend
	It("The default is what exists today", func(ctx context.Context) {
		By("given no workspace configuration")
		Expect(opts.workspace).To(BeEmpty())
		kind, err := workspace.ParseKind(opts.workspace)
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(workspace.KindLocal))
		Expect(checkWorkspace(kind, opts)).To(Succeed())

		By("when the run loop constructs its Workspaces function")
		workspaces := workspacesFor(kind, opts, redact.New())

		By("then it is the local backend")
		// The proof is what the function makes: a clone under the
		// workspace directory, on the run's branch, recorded as local.
		ws, err := workspaces(ctx, workspace.Spec{
			RunID: "01SPECRUN0000000000000000A", NodeRef: "millspec#1", Origin: seedOrigin(), Ref: "main",
		}, run.Repository{Origin: seedOrigin()})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(ws).To(BeAssignableToTypeOf(&local.Workspace{}))
		Expect(ws.Handle().Kind).To(Equal(workspace.KindLocal))
		Expect(ws.Handle().Root).To(HavePrefix(opts.workspaceDir))
	})

	// The first half of the scenario the run refusal below completes: with
	// `workspace: machine` the function the loop is handed is the machine
	// backend, over the real host: with no sandbox root to write a run
	// directory under, making a workspace fails on that and touches no
	// unit.
	It("chooses the machine backend under workspace: machine", func(ctx context.Context) {
		opts.workspace = "machine"
		opts.sandboxRoot = filepath.Join(GinkgoT().TempDir(), "absent-sandbox")
		kind, err := workspace.ParseKind(opts.workspace)
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(workspace.KindMachine))

		workspaces := workspacesFor(kind, opts, redact.New())

		_, err = workspaces(ctx, workspace.Spec{RunID: "01SPECRUN0000000000000000B", NodeRef: "millspec#1", Origin: "/nonexistent/origin.git"}, run.Repository{})
		Expect(err).To(MatchError(ContainSubstring("absent-sandbox")))
	})

	It("refuses the machine backend before a claim on a host that cannot run it, naming the reason", func() {
		tmp := GinkgoT().TempDir()
		opts.sandboxRoot = filepath.Join(tmp, "absent-sandbox")
		// The skill and the sidecar are materialised into directories of the
		// spec's own, from a built sidecar of the spec's own, so the
		// preflight writes nothing under the real share root.
		opts.sandbox.skillDir = filepath.Join(tmp, "skill")
		opts.sandbox.sidecarDir = filepath.Join(tmp, "sidecar")
		opts.sandbox.workflowCopyDir = filepath.Join(tmp, "workflow")
		opts.sidecar = builtSidecar(filepath.Join(tmp, "src", "sidecar"))
		opts.workflowDir = filepath.Join("..", "..", "workflows", "quick-fix")

		err := checkWorkspace(workspace.KindMachine, opts)

		Expect(err).To(MatchError(ContainSubstring("--workspace machine")))
		Expect(err).To(MatchError(ContainSubstring("absent-sandbox")))

		By("and the skill every machine would mount was written first, with this binary's version beside it")
		Expect(filepath.Join(opts.sandbox.skillDir, "SKILL.md")).To(BeAnExistingFile())
		Expect(skill.Version(opts.sandbox.skillDir)).To(Equal(version.String()))

		By("and the sidecar beside it, copied from the entrypoint's build, at the same version")
		Expect(filepath.Join(opts.sandbox.sidecarDir, skill.SidecarEntrypoint)).To(BeAnExistingFile())
		Expect(skill.Version(opts.sandbox.sidecarDir)).To(Equal(version.String()))

		By("and the workflow directory beside them, its check scripts still executable")
		script, err := os.Stat(filepath.Join(opts.sandbox.workflowCopyDir, "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())
		Expect(script.Mode().Perm() & 0o100).NotTo(BeZero())
	})

	It("refuses the machine backend when the sidecar the flag names is not a built one", func() {
		tmp := GinkgoT().TempDir()
		opts.sandbox.skillDir = filepath.Join(tmp, "skill")
		opts.sandbox.sidecarDir = filepath.Join(tmp, "sidecar")
		opts.sidecar = filepath.Join(tmp, "src", "sidecar", skill.SidecarEntrypoint)
		Expect(os.MkdirAll(filepath.Dir(opts.sidecar), 0o755)).To(Succeed())

		err := checkWorkspace(workspace.KindMachine, opts)

		Expect(err).To(MatchError(ContainSubstring("not a built sidecar")))
		Expect(opts.sandbox.sidecarDir).NotTo(BeADirectory())
	})

	// Scenario: The local backend is untouched
	//   Given a run with the local backend
	//   When the adapter is built
	//   Then its options are exactly what they were before this node
	It("The local backend is untouched", func() {
		opts.sidecar = "sidecar/dist/main.js"

		By("when the adapter is built for the local backend")
		got := claudeAdapter(workspace.KindLocal, opts, claude.EnvStrict, nil)

		By("then it is exactly the adapter this command built before this node")
		want := claude.New(
			claude.WithEntrypoint(claude.DefaultCommand, opts.sidecar),
			claude.WithEnvMode(claude.EnvStrict))
		Expect(reflect.DeepEqual(got, want)).To(BeTrue(),
			"the local adapter's options changed:\n got %+v\nwant %+v", got, want)

		By("and the machine backend's adapter is a different thing: its execer is the run's workspace")
		machineAdapter := claudeAdapter(workspace.KindMachine, opts, claude.EnvStrict, nil)
		Expect(reflect.DeepEqual(got, machineAdapter)).To(BeFalse())
	})

	It("refuses a kind the tree does not know, and the remote one it has not built", func() {
		_, err := workspace.ParseKind("podman")
		Expect(err).To(MatchError(ContainSubstring(`"podman" is not a workspace kind`)))
		_, err = workspace.ParseKind("remote")
		Expect(err).To(MatchError(ContainSubstring("not built")))
	})
})
