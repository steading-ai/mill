package workflow

import (
	"context"
	"fmt"
	"log/slog"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Lookup is what resolving a name needs of the store: the newest version of
// a workflow in one scope, or nil when that scope has none. An error means
// the lookup failed, not that nothing was found.
type Lookup interface {
	Workflow(ctx context.Context, name string, scope millv1.Scope, project string) (*millv1.Workflow, error)
}

// Resolve picks the version a run for a project follows. A project
// definition overrides an org one, and a project definition that shadows an
// org one of the same name logs a warning — the agent-skills precedence
// rule, and the reason a shadow is loud rather than silent: the org
// definition stays in the store, and a reader has no other way to learn it
// was passed over.
func Resolve(ctx context.Context, lookup Lookup, log *slog.Logger, name, project string) (*millv1.Workflow, error) {
	var chosen *millv1.Workflow
	if project != "" {
		var err error
		if chosen, err = lookup.Workflow(ctx, name, millv1.Scope_SCOPE_PROJECT, project); err != nil {
			return nil, err
		}
	}
	org, err := lookup.Workflow(ctx, name, millv1.Scope_SCOPE_ORG, "")
	if err != nil {
		return nil, err
	}
	switch {
	case chosen != nil && org != nil:
		log.Warn("project workflow shadows the org definition",
			"workflow", name,
			"project", project,
			"version", chosen.GetVersion(),
			"shadowed", org.GetVersion())
	case chosen == nil && org != nil:
		chosen = org
	case chosen == nil:
		return nil, fmt.Errorf("workflow %s: no definition in project %s or at org scope", name, project)
	}
	return chosen, nil
}
