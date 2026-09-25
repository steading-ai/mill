package machine

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/steading-ai/mill/internal/workspace/verbs"
)

// clone is sequence step 2 (docs/sandbox.md §"Sequence of a run"): the
// repository into the run's workspace directory on the host, with the
// worker's own forge credential. No session exists yet and no step token
// has been issued, so there is nothing else the clone could carry.
func (w *Workspace) clone(ctx context.Context, origin, ref, checkout string) error {
	auth, err := w.verbs.OriginAuth(ctx, origin)
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
	_, err = w.verbs.Host.Run(ctx, filepath.Dir(checkout), auth.Env, args...)
	return err
}

// repoPath is a repository's path on its forge as an origin or a grant
// spells it: `https://gitlab.com/wellspring/mill.git`,
// `git@gitlab.com:wellspring/mill` and `wellspring/mill` are all
// `wellspring/mill`.
func repoPath(ref string) string {
	path := strings.TrimSpace(ref)
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
		if j := strings.Index(path, "/"); j >= 0 {
			path = path[j+1:]
		} else {
			path = ""
		}
	} else if i := strings.Index(path, "@"); i >= 0 && !strings.Contains(path[:i], "/") {
		if _, after, found := strings.Cut(path[i+1:], ":"); found {
			path = after
		}
	}
	path = strings.Trim(path, "/")
	return strings.TrimSuffix(path, ".git")
}

// sameRepo reports whether two remotes name the same repository.
func sameRepo(a, b string) bool { return verbs.SameRepo(a, b) }
