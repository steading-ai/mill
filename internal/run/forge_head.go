package run

import "fmt"

// foreignHeadError is the forge answering a pull request at a head that is
// not the one this run's own push left: a branch name reused across
// generations, the forge's `PR`/`OpenPR` on it answering the pull request an
// earlier generation abandoned rather than this one's own. Both the `pr`
// step (execute.go's pullRequest) and the merge gate (gate.go's approve)
// read a pull request straight off the forge before anything commits to it,
// and this is the one check both do before trusting it.
//
// It is a plain error rather than a [mergeFinding]: both the `pr` and
// `merge` steps already park on an unhandled error, and nothing in the tree
// is wrong for a session to fix — the fix is a restart.
type foreignHeadError struct {
	Branch, Expected, Found string
}

func (e *foreignHeadError) Error() string {
	return fmt.Sprintf("forge: the pull request on %s is at %s, not %s: it belongs to another generation; restart the run to start a fresh one",
		e.Branch, e.Found, e.Expected)
}

// checkForeignHead compares a pull request's head, as the forge answered it,
// against the head this run's own push left. An empty expected head — no
// commit to compare against yet — is not a mismatch: `pullRequest`'s own
// [forge.OpenPRRequest.Head] contract already treats empty the same way.
func checkForeignHead(branch, expected, found string) error {
	if expected == "" || found == expected {
		return nil
	}
	return &foreignHeadError{Branch: branch, Expected: expected, Found: found}
}
