package machine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
)

// clone is sequence step 2 (docs/sandbox.md §"Sequence of a run"): the
// repository into the run's workspace directory on the host, with the
// worker's own forge credential. No session exists yet and no step token
// has been issued, so there is nothing else the clone could carry.
func (w *Workspace) clone(ctx context.Context, origin, ref, checkout string) error {
	auth, err := w.originAuth(ctx, origin)
	if err != nil {
		return err
	}
	args := make([]string, 0, len(auth.Args)+5)
	args = append(args, auth.Args...)
	args = append(args, "clone", "--quiet")
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, origin, checkout)
	_, err = w.gitWith(ctx, filepath.Dir(checkout), auth.Env, args...)
	return err
}

// originAuth is the credential a git command against origin runs with: the
// forge adapter's, minted for the worker itself — the forge is bound to
// no step, because none is live when the clone runs. The zero value is
// what a run with no forge, or one pointed at some other origin, gets: the
// worker presents its credential to its forge's repository and nowhere
// else. Every value is registered with the redactor before git runs.
func (w *Workspace) originAuth(ctx context.Context, origin string) (forge.GitAuth, error) {
	if w.config.Forge == nil || !sameRepo(origin, w.remote()) {
		return forge.GitAuth{}, nil
	}
	auth, err := forge.Bind(w.config.Forge, forge.Step{}).Authenticate(ctx)
	if err != nil {
		return forge.GitAuth{}, err
	}
	for _, value := range auth.Env {
		w.redactor.Add(value)
	}
	return auth, nil
}

// remote is the forge's clone URL, or empty for a forge that has none.
func (w *Workspace) remote() string {
	type repoer interface{ Repo() forge.Repo }
	if named, ok := w.config.Forge.(repoer); ok {
		return named.Repo().CloneURL
	}
	return ""
}

// sameRepo reports whether two remotes name the same repository, allowing
// for the `.git` suffix and a trailing slash.
func sameRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	trim := func(url string) string {
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"), "/")
	}
	return trim(a) == trim(b)
}

// gitWith runs git in dir with extra environment over the workspace's own,
// and returns its standard output. A credential reaches git through that
// environment and a helper, never argv.
func (w *Workspace) gitWith(ctx context.Context, dir string, extra map[string]string, args ...string) (string, error) {
	binary := w.config.Git
	if binary == "" {
		binary = "git"
	}
	verb := gitVerb(args)
	env := gitEnv(dir)
	for name, value := range extra {
		env[name] = value
	}
	process, err := w.execer.Exec(ctx, workspace.Command{Path: binary, Args: args, Dir: dir, Env: env})
	if err != nil {
		return "", fmt.Errorf("machine: git %s: %w", verb, err)
	}
	_ = process.Stdin.Close()
	var stderr []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		stderr, _ = io.ReadAll(process.Stderr)
	}()
	stdout, _ := io.ReadAll(process.Stdout)
	<-done
	result := process.Wait()
	if result.Status != 0 {
		return string(stdout), fmt.Errorf("machine: git %s exited %d: %s",
			verb, result.Status, w.redactor.String(strings.TrimSpace(string(stderr))))
	}
	return string(stdout), nil
}

// gitVerb is the subcommand in a git invocation, for naming it in an error:
// not args[0] when a credential helper arrives as `-c` options ahead of it.
func gitVerb(args []string) string {
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-c", "--config-env":
			index++
		default:
			if strings.HasPrefix(args[index], "-") {
				continue
			}
			return args[index]
		}
	}
	return "git"
}

// gitEnv is what git on the host is given: nothing of the operator's
// configuration, no prompt, HOME inside the run directory.
func gitEnv(dir string) map[string]string {
	return map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                dir,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "true",
		"LC_ALL":              "C",
	}
}

// branchOf is the branch a checkout is on, empty when detached.
func (w *Workspace) branchOf(ctx context.Context, checkout string) (string, error) {
	out, err := w.gitWith(ctx, checkout, nil, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitRunner runs git in the run's checkout on the host, for the `commit`
// verifier and the forge.
type gitRunner struct {
	w   *Workspace
	dir string
}

// Run runs git with env added to the workspace's own, in the checkout.
func (g gitRunner) Run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	if g.dir == "" {
		return "", ErrNotCreated
	}
	return g.w.gitWith(ctx, g.dir, env, args...)
}
