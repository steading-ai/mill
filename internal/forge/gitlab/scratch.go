package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/steading-ai/mill/internal/forge"
)

// Scratch is one GitLab group as a scratch namespace (forge.Scratch): the
// projects in it, and their deletion. It lists and deletes and does nothing
// else, and only inside the group it was given: the sweep that uses it
// matches names against the suite's pattern, and this adapter is never
// handed a project outside the namespace to delete.
type Scratch struct {
	client    *Client
	namespace string
}

// NewScratch returns the namespace on the forge at baseURL, reached with
// credentials minter mints. The namespace is the group's full path or its
// numeric id, as GitLab addresses one.
func NewScratch(baseURL, namespace string, minter forge.Minter, options ...Option) *Scratch {
	return &Scratch{
		client:    New(forge.Repo{Kind: forge.KindGitLab, BaseURL: baseURL}, minter, options...),
		namespace: namespace,
	}
}

// Namespace is the group's path or id, as configured.
func (s *Scratch) Namespace() string { return s.namespace }

// ListProjects is every project directly in the group, across every page.
func (s *Scratch) ListProjects(ctx context.Context) ([]forge.ScratchProject, error) {
	if s.namespace == "" {
		return nil, errors.New("gitlab: no scratch namespace")
	}
	var projects []forge.ScratchProject
	for page := 1; ; page++ {
		var listed []struct {
			ID        int       `json:"id"`
			Name      string    `json:"name"`
			Path      string    `json:"path"`
			Full      string    `json:"path_with_namespace"`
			CreatedAt time.Time `json:"created_at"`
		}
		query := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "simple": {"true"}}
		if err := s.client.call(ctx, http.MethodGet, "/api/v4/groups/"+url.PathEscape(s.namespace)+"/projects", query, nil, &listed); err != nil {
			return nil, err
		}
		for _, project := range listed {
			projects = append(projects, forge.ScratchProject{
				ID:        strconv.Itoa(project.ID),
				Name:      project.Path,
				Path:      project.Full,
				CreatedAt: project.CreatedAt,
			})
		}
		if len(listed) < 100 {
			return projects, nil
		}
	}
}

// DeleteProject removes a project by id. GitLab queues the deletion and
// answers 202; a project it no longer has answers 404, which is the state
// asked for.
func (s *Scratch) DeleteProject(ctx context.Context, project forge.ScratchProject) error {
	if project.ID == "" {
		return fmt.Errorf("gitlab: deleting %s needs its id", project.Path)
	}
	err := s.client.call(ctx, http.MethodDelete, "/api/v4/projects/"+url.PathEscape(project.ID), nil, nil, nil)
	var failure *apiError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		return nil
	}
	return err
}

var _ forge.Scratch = (*Scratch)(nil)
