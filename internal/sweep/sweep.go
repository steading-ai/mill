// Package sweep is the external state a machine may leave, as the worker's
// reconciliation pass reaches it: dagger's scratch projects and the forge's,
// each behind [machine.External] (docs/sandbox.md §"External state"). The
// reconciler decides what is stale — the suite's name pattern, the age, no
// live run — and these adapters only list and remove.
//
// The dagger adapter takes the worker's own client, so the sweep reaches
// exactly the dagger the worker was configured for: never one named by the
// environment, which is how a test process once became a worker against the
// live graph (mill#49).
package sweep

import (
	"context"
	"errors"
	"fmt"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace/machine"
)

// Projects is what the dagger adapter needs of a client: the three project
// verbs of internal/dagger's [dagger.Client].
type Projects interface {
	ListProjects(ctx context.Context) ([]*daggerv1.Project, error)
	ArchiveProject(ctx context.Context, slug string) (*daggerv1.Project, error)
	DeleteProject(ctx context.Context, slug string) error
}

// Dagger is the configured dagger's projects as an external the reconciler
// sweeps. A stale project is deleted when the service offers a delete and
// archived when it does not, and the action says which.
func Dagger(client Projects) machine.External { return daggerProjects{client: client} }

type daggerProjects struct{ client Projects }

func (daggerProjects) Kind() string { return "dagger" }

func (d daggerProjects) List(ctx context.Context) ([]machine.Scratch, error) {
	projects, err := d.client.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	listed := make([]machine.Scratch, 0, len(projects))
	for _, project := range projects {
		if project.GetArchivedAt() != nil {
			continue
		}
		listed = append(listed, machine.Scratch{Name: project.GetSlug(), CreatedAt: project.GetCreatedAt().AsTime()})
	}
	return listed, nil
}

func (d daggerProjects) Remove(ctx context.Context, scratch machine.Scratch) (string, error) {
	err := d.client.DeleteProject(ctx, scratch.Name)
	switch {
	case err == nil:
		return "deleted", nil
	case errors.Is(err, errors.ErrUnsupported):
		if _, err := d.client.ArchiveProject(ctx, scratch.Name); err != nil {
			return "", err
		}
		return "archived (dagger offers no project delete)", nil
	default:
		return "", fmt.Errorf("delete project %s: %w", scratch.Name, err)
	}
}

// Forge is a forge's scratch namespace as an external the reconciler
// sweeps: a stale project is deleted. List is asked before Remove, as the
// reconciler does, because the forge deletes by its own id and the name is
// all the reconciler hands back.
func Forge(scratch forge.Scratch) machine.External {
	return &forgeProjects{scratch: scratch, byName: map[string]forge.ScratchProject{}}
}

type forgeProjects struct {
	scratch forge.Scratch
	byName  map[string]forge.ScratchProject
}

func (*forgeProjects) Kind() string { return "forge" }

func (f *forgeProjects) List(ctx context.Context) ([]machine.Scratch, error) {
	projects, err := f.scratch.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	listed := make([]machine.Scratch, 0, len(projects))
	for _, project := range projects {
		f.byName[project.Name] = project
		listed = append(listed, machine.Scratch{Name: project.Name, CreatedAt: project.CreatedAt})
	}
	return listed, nil
}

func (f *forgeProjects) Remove(ctx context.Context, scratch machine.Scratch) (string, error) {
	project, ok := f.byName[scratch.Name]
	if !ok {
		return "", fmt.Errorf("delete project %s: not listed in %s", scratch.Name, f.scratch.Namespace())
	}
	if err := f.scratch.DeleteProject(ctx, project); err != nil {
		return "", fmt.Errorf("delete project %s: %w", project.Path, err)
	}
	return "deleted from " + f.scratch.Namespace(), nil
}
