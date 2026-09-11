package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/skill"
	"github.com/ivy/mill/internal/version"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
	"github.com/ivy/mill/internal/workspace/redact"
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
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_AUTHOR_NAME=spec", "GIT_AUTHOR_EMAIL=spec@example.test",
			"GIT_COMMITTER_NAME=spec", "GIT_COMMITTER_EMAIL=spec@example.test",
		}
		out, err := command.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
	}
	git("init", "--quiet", "--initial-branch=main")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o600)).To(Succeed())
	git("add", "README.md")
	git("commit", "--quiet", "--no-gpg-sign", "--message", "seed")
	return dir
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
		workspaces := workspacesFor(kind, opts, nil, redact.New())

		By("then it is the local backend")
		// The proof is what the function makes: a clone under the
		// workspace directory, on the run's branch, recorded as local.
		ws, err := workspaces(ctx, workspace.Spec{
			RunID: "01SPECRUN0000000000000000A", NodeRef: "millspec#1", Origin: seedOrigin(), Ref: "main",
		})
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

		workspaces := workspacesFor(kind, opts, nil, redact.New())

		_, err = workspaces(ctx, workspace.Spec{RunID: "01SPECRUN0000000000000000B", NodeRef: "millspec#1", Origin: "/nonexistent/origin.git"})
		Expect(err).To(MatchError(ContainSubstring("absent-sandbox")))
	})

	It("refuses the machine backend before a claim on a host that cannot run it, naming the reason", func() {
		opts.sandboxRoot = filepath.Join(GinkgoT().TempDir(), "absent-sandbox")
		opts.sandbox.skillDir = filepath.Join(GinkgoT().TempDir(), "skill")

		err := checkWorkspace(workspace.KindMachine, opts)

		Expect(err).To(MatchError(ContainSubstring("--workspace machine")))
		Expect(err).To(MatchError(ContainSubstring("absent-sandbox")))

		By("and the skill every machine would mount was written first, with this binary's version beside it")
		Expect(filepath.Join(opts.sandbox.skillDir, "SKILL.md")).To(BeAnExistingFile())
		Expect(skill.Version(opts.sandbox.skillDir)).To(Equal(version.String()))
	})

	It("refuses a kind the tree does not know, and the remote one it has not built", func() {
		_, err := workspace.ParseKind("podman")
		Expect(err).To(MatchError(ContainSubstring(`"podman" is not a workspace kind`)))
		_, err = workspace.ParseKind("remote")
		Expect(err).To(MatchError(ContainSubstring("not built")))
	})
})
