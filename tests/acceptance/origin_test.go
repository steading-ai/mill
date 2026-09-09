package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A repository on this host, for the scenarios that end before a run reaches
// a forge.
//
// The rest of the suite delivers to a real GitLab because the facts it
// asserts — a merge landing, a signature verifying — are exactly the facts a
// fake would have to invent. The seeded bad implementation asserts a
// different fact: that the reviewer said no. Nothing about that needs a
// forge, and requiring one would put the milestone's own proof behind an
// account not every host running this suite has (docs/prd.md R-25,
// docs/runbook.md §"Without a forge").
//
// So the repository is bare, on disk, and `mill run --origin` points a
// workspace at it. It is a real git repository cloned by real git: the only
// thing missing is the API no step in this scenario calls.

// localBranch is the branch a local origin is seeded on, and the ref a
// workspace checks out from it.
const localBranch = "main"

// newLocalOrigin makes a bare repository seeded with the fixture's files and
// returns the URL a workspace clones and the branch it checks out.
func newLocalOrigin(ctx context.Context, name string, files map[string]string) (origin, branch string) {
	GinkgoHelper()
	root := filepath.Join(GinkgoT().TempDir(), "origin-"+name+"-"+nonce())
	work := filepath.Join(root, "seed")
	bare := filepath.Join(root, "origin.git")
	Expect(os.MkdirAll(work, 0o700)).To(Succeed())

	git(ctx, work, "init", "--quiet", "--initial-branch", localBranch)
	for path, content := range files {
		full := filepath.Join(work, path)
		Expect(os.MkdirAll(filepath.Dir(full), 0o700)).To(Succeed())
		Expect(os.WriteFile(full, []byte(content), 0o600)).To(Succeed())
	}
	git(ctx, work, "add", "--all")
	git(ctx, work, "commit", "--quiet", "--message", "Seed "+name+" for the acceptance suite")
	git(ctx, work, "clone", "--quiet", "--bare", work, bare)

	// A bare repository git pushes to needs no receive configuration here:
	// nothing in a local-origin scenario pushes. It is an origin to clone.
	return "file://" + bare, localBranch
}

// git runs one git command in a directory, with the identity a fixture's
// seed commit needs and no access to the caller's own git configuration.
func git(ctx context.Context, dir string, args ...string) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec // the suite's own git
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=mill acceptance",
		"GIT_AUTHOR_EMAIL=acceptance@mill.invalid",
		"GIT_COMMITTER_NAME=mill acceptance",
		"GIT_COMMITTER_EMAIL=acceptance@mill.invalid")
	out, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %s: %s", strings.Join(args, " "), out)
}
