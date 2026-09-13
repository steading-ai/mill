package dagger

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
)

// The project verbs the worker's reconciliation sweep uses (docs/sandbox.md
// §"External state"): what the configured dagger holds, and how a scratch
// project the acceptance suite left behind is taken out of it. They are on
// the same client as everything else, against the same socket, because the
// sweep must reach exactly the dagger the worker was configured for and no
// other (mill#49).

// GetProject reads this client's project in full: the slug a bare reference
// resolves in, and the repository it delivers to.
func (c *Client) GetProject(ctx context.Context) (*daggerv1.Project, error) {
	res, err := c.graph.GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: c.project}))
	if err != nil {
		return nil, wrap("dagger: get project "+c.project, err)
	}
	return res.Msg.GetProject(), nil
}

// ListProjects is every active project the principal can see, across every
// page. Archived projects are not listed: they are already swept.
func (c *Client) ListProjects(ctx context.Context) ([]*daggerv1.Project, error) {
	var projects []*daggerv1.Project
	var token string
	for {
		res, err := c.graph.ListProjects(ctx, connect.NewRequest(&daggerv1.ListProjectsRequest{PageToken: token}))
		if err != nil {
			return nil, fmt.Errorf("dagger: list projects: %w", err)
		}
		projects = append(projects, res.Msg.GetProjects()...)
		token = res.Msg.GetNextPageToken()
		if token == "" {
			return projects, nil
		}
	}
}

// ArchiveProject makes a project read-only, under a request id that begins
// `mill-` like every other write mill makes.
func (c *Client) ArchiveProject(ctx context.Context, slug string) (*daggerv1.Project, error) {
	res, err := c.graph.ArchiveProject(ctx, connect.NewRequest(&daggerv1.ArchiveProjectRequest{
		RequestId: nonce("archive-" + slug),
		Project:   slug,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: archive project %s: %w", slug, err)
	}
	return res.Msg.GetProject(), nil
}

// ErrNoProjectDelete is what DeleteProject answers while dagger offers no
// such verb: the vendored contract (proto/dagger/v1) has ArchiveProject and
// nothing that removes a project. The sweep archives instead and says so.
// When dagger's project-delete lands, the vendored contract gains the RPC
// and this becomes a call; nothing above it changes.
var ErrNoProjectDelete = fmt.Errorf("%w: dagger offers no project delete; archive is the most a sweep can do", errors.ErrUnsupported)

// DeleteProject removes a project when the service offers a delete. It
// does not today: the answer is [ErrNoProjectDelete], which a caller tests
// with errors.Is(err, errors.ErrUnsupported) before falling back to
// [Client.ArchiveProject].
func (c *Client) DeleteProject(context.Context, string) error {
	return ErrNoProjectDelete
}

// Hold parks a node with a reason: it leaves every ready set until a person
// releases it. Mill holds a node whose run it has failed and cannot resume
// — a lost sandbox — so the node is not claimed again and again into the
// same refusal, and the person who restarts it finds the reason on it
// (docs/sandbox.md §Reconciliation). The lease is the failed run's, still
// held while this is written.
func (c *Client) Hold(ctx context.Context, requestID, leaseID, ref, reason string) error {
	_, err := c.ApplyPatch(ctx, requestID, leaseID, &daggerv1.PatchOp{
		Op: &daggerv1.PatchOp_HoldNode{HoldNode: &daggerv1.HoldNodeOp{Node: ref, Reason: reason}},
	})
	return err
}
