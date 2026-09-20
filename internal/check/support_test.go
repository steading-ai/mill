package check_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
)

// checkout is the workspace as a check sees it: the process chokepoint, with
// cwd set to the run's clone. It is what internal/workspace/local does around
// the same spawner, without a socket, a token or a clone to set up — the
// check package's contract is with [workspace.Execer] and nothing more.
type checkout struct {
	dir     string
	spawner workspace.Spawner
}

func newCheckout() *checkout {
	return &checkout{dir: GinkgoT().TempDir(), spawner: workspace.Spawner{Floor: scratchEnv()}}
}

func (c *checkout) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	command.Dir = c.dir
	return c.spawner.Exec(ctx, command)
}

var _ workspace.Execer = (*checkout)(nil)

// git runs git in the checkout, for the specs that make one dirty.
func (c *checkout) git(args ...string) {
	GinkgoHelper()
	cmd := exec.Command("git", args...) //nolint:gosec // a spec's own arguments
	cmd.Dir = c.dir
	cmd.Env = scratchEnv("GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

// scratchEnv is the environment for the checkout's own git, with the host's
// GIT_* variables cleared: under a git hook the process inherits GIT_DIR and
// GIT_INDEX_FILE, and this git would otherwise act on the hook's repository
// rather than the checkout, whatever cmd.Dir says.
func scratchEnv(extra ...string) []string {
	env := os.Environ()
	clean := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return append(clean, extra...)
}

// repository makes the checkout a git repository with one commit and a clean
// tree, which is the state clean-tree passes on.
func (c *checkout) repository() {
	GinkgoHelper()
	c.git("init", "--initial-branch", "mill/mill-14")
	c.git("config", "user.name", "mill")
	c.git("config", "user.email", "mill@example.test")
	c.write("README.md", "committed\n")
	c.git("add", "README.md")
	c.git("commit", "--message", "first")
}

func (c *checkout) write(name, content string) {
	GinkgoHelper()
	Expect(os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o600)).To(Succeed())
}

// script writes an executable script into a definition directory and returns
// the directory, which is what a workflow or a skill hands a check.
func script(dir, name, body string) string {
	GinkgoHelper()
	path := filepath.Join(dir, name)
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
	Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed()) //nolint:gosec // a script is executable
	return dir
}

// workflowDir is this repository's own quick-fix workflow, which is where the
// canonical clean-tree check lives.
func workflowDir() string {
	GinkgoHelper()
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	dir := filepath.Join(wd, "..", "..", "workflows", "quick-fix")
	Expect(dir).To(BeADirectory())
	return dir
}

// firstLine is the first line of a stream, which is where a check's own
// diagnostic goes.
func firstLine(text string) string {
	return strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
}
