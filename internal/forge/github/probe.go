package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ivy/mill/internal/forge"
)

// Permission is one permission a fine-grained token needs, and why. The
// list is the contract `mill doctor` probes against and the documentation
// carries, so a reader and the probe agree on what "enough" means.
type Permission struct {
	// Name is the permission as GitHub's token page spells it.
	Name string
	// Access is `read` or `read and write`.
	Access string
	// Why is what the mill does with it.
	Why string
	// header is the permission as GitHub names it in
	// `X-Accepted-GitHub-Permissions`, such as `contents`.
	header string
}

// Permissions is the exact set a fine-grained personal access token needs
// for a delivery, on the repository it delivers to. A classic token or the
// OAuth token `gh auth token` prints carries the `repo` scope, which covers
// all of them.
//
// Reads are probed by `mill doctor`; a write can only be proven by writing,
// which the delivery does. A token with a permission's read and not its
// write fails at the first write the run makes, which is the push.
var Permissions = []Permission{
	{Name: "Contents", Access: "read and write", header: "contents",
		Why: "clone the repository, push the run's branch, delete it after the merge"},
	{Name: "Pull requests", Access: "read and write", header: "pull_requests",
		Why: "open the pull request, comment on it, merge it or close it"},
	{Name: "Checks", Access: "read", header: "checks",
		Why: "read the check runs on the run's head"},
	{Name: "Commit statuses", Access: "read", header: "statuses",
		Why: "read the commit statuses on the run's head"},
	{Name: "Metadata", Access: "read", header: "metadata",
		Why: "read the repository's default branch and merge settings; GitHub grants it with any other"},
}

// permissionFor is the permission a refused request was missing, by the
// header GitHub names it in, or by the path when GitHub named nothing.
func permissionFor(failure *apiError) *Permission {
	for i := range Permissions {
		p := &Permissions[i]
		if failure.Accepted != "" && strings.Contains(failure.Accepted, p.header+"=") {
			return p
		}
	}
	switch {
	case strings.Contains(failure.Path, "/pulls"):
		return &Permissions[1]
	case strings.Contains(failure.Path, "/check-runs"):
		return &Permissions[2]
	case strings.Contains(failure.Path, "/status"):
		return &Permissions[3]
	case strings.Contains(failure.Path, "/branches/"):
		return &Permissions[0]
	default:
		return nil
	}
}

// CredentialError is a token GitHub will not honour at all: expired,
// revoked, or never valid. The remedy is a new token, so the error says so.
type CredentialError struct {
	// Reason is GitHub's message, verbatim.
	Reason string
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("github: the token is not accepted (%s): it has expired or been revoked; mint a new one, or run `gh auth login` and use `gh auth token`", e.Reason)
}

// PermissionError is a token GitHub honours that lacks a permission the
// repository needs. It names the permission as the token page spells it,
// because that page is where the fix is made.
type PermissionError struct {
	// Repository is the repository the permission is missing on.
	Repository string
	// Permission is what is missing, nil when GitHub refused without
	// saying what for.
	Permission *Permission
	// Reason is GitHub's message, verbatim.
	Reason string
}

func (e *PermissionError) Error() string {
	if e.Permission == nil {
		return fmt.Sprintf("github: the token cannot reach %s (%s): grant it access to the repository", e.Repository, e.Reason)
	}
	return fmt.Sprintf("github: the token lacks %s: %s on %s (%s): grant it on the token's page",
		e.Permission.Name, e.Permission.Access, e.Repository, e.Reason)
}

// account is GitHub's answer to who a token is.
type account struct {
	Login string `json:"login"`
}

// Probe asks GitHub who the credential is and whether it can read what a
// delivery reads on this repository: the repository itself, its default
// branch, the checks and statuses on that branch's head, and its pull
// requests. Each read is one permission of [Permissions], so a token
// missing one fails here — before a run spends a session finding out —
// with a *[PermissionError] naming it. A token GitHub does not honour at
// all is a *[CredentialError].
//
// The writes are not probed: nothing on GitHub says whether a token may
// write short of writing. The push is the first write a run makes, and it
// is before any session is spent.
func (c *Client) Probe(ctx context.Context) (forge.Account, error) {
	var who account
	if err := c.call(ctx, http.MethodGet, "/user", nil, nil, &who); err != nil {
		return forge.Account{}, c.classify(err)
	}
	repository, err := c.repository(ctx)
	if err != nil {
		return forge.Account{}, c.classify(err)
	}
	head, err := c.BranchHead(ctx, repository.DefaultBranch)
	if err != nil {
		return forge.Account{}, c.classify(err)
	}
	if head != "" {
		if _, err := c.Checks(ctx, head); err != nil {
			return forge.Account{}, c.classify(err)
		}
	}
	query := url.Values{"state": {"all"}, "per_page": {"1"}}
	if err := c.call(ctx, http.MethodGet, c.path("pulls"), query, nil, &[]pullRequest{}); err != nil {
		return forge.Account{}, c.classify(err)
	}
	return forge.Account{Login: who.Login}, nil
}

// classify turns a refused read into the error the probe reports: 401 is
// the token itself, 403 and a 404 on the repository are a permission it
// lacks, and anything else is the round trip that failed.
func (c *Client) classify(err error) error {
	var failure *apiError
	if !errors.As(err, &failure) {
		return err
	}
	switch failure.Status {
	case http.StatusUnauthorized:
		return &CredentialError{Reason: failure.Message}
	case http.StatusForbidden:
		return &PermissionError{Repository: c.repo.Path, Permission: permissionFor(failure), Reason: failure.Message}
	case http.StatusNotFound:
		if failure.Path == "/repos/"+c.repo.Path {
			return &PermissionError{Repository: c.repo.Path, Reason: failure.Message}
		}
	}
	return err
}

var _ forge.Prober = (*Client)(nil)
