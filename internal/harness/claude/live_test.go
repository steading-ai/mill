package claude

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/harness"
)

// liveVar turns on the spec that spends a real session against the account's
// own Claude Code login. It is off by default: `make test` must not depend on
// a credential or on the model being reachable.
const liveVar = "MILL_SIDECAR_LIVE"

var _ = Describe("The claude adapter against the real sidecar", Label("live"), func() {
	var entrypoint string

	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 to run the specs that spend a real session")
		}

		root, err := filepath.Abs(filepath.Join("..", "..", "..", "sidecar"))
		Expect(err).NotTo(HaveOccurred())
		build := exec.Command("pnpm", "run", "build")
		build.Dir = root
		build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
		Expect(build.Run()).To(Succeed(), "the sidecar did not build")
		entrypoint = filepath.Join(root, "dist", "main.js")
	})

	It("streams the session's events before the terminal record, and returns what it billed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		DeferCleanup(cancel)
		log := &recorder{}
		adapter := New(WithEntrypoint("node", entrypoint), WithTimeout(4*time.Minute))

		turn, err := adapter.Start(ctx, harness.Session{
			ID:     newSessionID(),
			Cwd:    GinkgoT().TempDir(),
			Events: log.sink(),
		}, harness.Prompt{Text: "Reply with the single word: ready. Use no tools."}, nil)
		Expect(err).NotTo(HaveOccurred())
		log.watch(turn)

		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)
		Expect(log.collected()).NotTo(BeEmpty(), "the turn streamed no events")
		Expect(log.afterTerminal()).To(BeZero(), "an event arrived after the terminal record")
		Expect(terminal.Usage).NotTo(BeEmpty(), "the adapter is the only component that sees the harness's numbers")
		for model, counts := range terminal.Usage {
			Expect(counts.Output).To(BeNumerically(">", 0), "model %s billed no output tokens", model)
		}
	})
	// The scenario the first end-to-end run failed on: the session's `git
	// add` was refused by the SDK's permission layer with "This command
	// requires approval", and nothing was there to answer. It runs under
	// both environment modes, because a commit needs more of the host than a
	// prompt does — git, a HOME to read ~/.gitconfig from — and strict mode
	// is the one that could take it away.
	for _, mode := range []EnvMode{EnvInherit, EnvStrict} {
		It("A commit step commits ("+string(mode)+")", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			DeferCleanup(cancel)

			By("given a workspace checkout with README.md modified and a run at the commit step")
			checkout := scratchCheckout()
			before := headOf(checkout)

			log := &recorder{}
			adapter := New(WithEntrypoint("node", entrypoint), WithEnvMode(mode), WithTimeout(8*time.Minute))

			By("when the commit skill's session runs under the default permission mode")
			turn, err := adapter.Start(ctx, harness.Session{
				ID:  newSessionID(),
				Cwd: checkout,
				// No permission_mode: the default is the classifier, which
				// is what this spec is about.
				Options: map[string]any{
					"model": "sonnet",
					"tools": map[string]any{"allow": []any{"Bash", "Read", "Glob", "Grep"}},
				},
				Events: log.sink(),
			}, harness.Prompt{Text: commitSkill() + "\n\n## The work\n\nThe change is already staged. Commit it.\n\nRefs: mill#24"}, nil)
			Expect(err).NotTo(HaveOccurred())
			log.watch(turn)
			terminal, err := turn.Wait(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(terminal.Exit.NeverRan()).To(BeFalse(), "the session never ran: %s", terminal.FinalMessage)

			By("then the branch head is a new commit whose tree contains the change")
			after := headOf(checkout)
			Expect(after).NotTo(Equal(before), "no commit was made: %s", terminal.FinalMessage)
			Expect(git(checkout, "show", after+":README.md")).To(ContainSubstring("the change under test"))

			By("and no tool call in the step log was refused for approval")
			for _, event := range log.collected() {
				if result := event.GetToolResult(); result != nil {
					Expect(result.GetContent()).NotTo(ContainSubstring("requires approval"), "a tool call was refused for approval")
				}
			}
		})
	}
})

// scratchCheckout is a git repository with one staged change to README.md,
// as a run's workspace is when it reaches the commit step. Identity and
// signing are set on the repository itself so the spec does not depend on
// the account's global git configuration.
func scratchCheckout() string {
	GinkgoHelper()

	dir := GinkgoT().TempDir()
	git(dir, "init", "--initial-branch", "mill/spec")
	git(dir, "config", "user.email", "mill@localhost")
	git(dir, "config", "user.name", "mill")
	git(dir, "config", "commit.gpgsign", "false")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spec\n"), 0o600)).To(Succeed())
	git(dir, "add", "README.md")
	git(dir, "commit", "-m", "chore: seed the checkout")
	Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spec\n\nthe change under test\n"), 0o600)).To(Succeed())
	git(dir, "add", "README.md")
	return dir
}

// commitSkill is the body of the skill the session is given, read from the
// workflow this repository seeds the store with: the spec runs the real
// instructions rather than a paraphrase of them.
func commitSkill() string {
	GinkgoHelper()

	body, err := os.ReadFile(filepath.Join("..", "..", "..", "workflows", "quick-fix", "skills", "commit", "SKILL.md"))
	Expect(err).NotTo(HaveOccurred())
	return string(body)
}

func headOf(dir string) string {
	GinkgoHelper()

	return strings.TrimSpace(git(dir, "rev-parse", "HEAD"))
}

// git runs one command in the checkout. The spec is the workspace here, so
// it spawns git itself rather than through one.
func git(dir string, args ...string) string {
	GinkgoHelper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v: %s", args, out)
	return string(out)
}

// newSessionID mints the kind of id a run assigns a session: a UUID the
// harness accepts and has never seen.
func newSessionID() string {
	GinkgoHelper()

	raw := make([]byte, 16)
	_, err := rand.Read(raw)
	Expect(err).NotTo(HaveOccurred())
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
