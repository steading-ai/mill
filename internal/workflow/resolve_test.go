package workflow_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workflow"
)

// scoped is a store holding one version of a name per scope. Nothing here
// needs the database: what is under test is which of two versions a run
// gets, and what the loader says about the one it passed over.
type scoped struct {
	org     *millv1.Workflow
	project map[string]*millv1.Workflow
	err     error
}

func (s scoped) Workflow(_ context.Context, name string, scope millv1.Scope, project string) (*millv1.Workflow, error) {
	if s.err != nil {
		return nil, s.err
	}
	if scope == millv1.Scope_SCOPE_ORG {
		if s.org.GetName() == name {
			return s.org, nil
		}
		return nil, nil
	}
	if found, ok := s.project[project]; ok && found.GetName() == name {
		return found, nil
	}
	return nil, nil
}

// logged captures what the loader wrote while resolving.
func logged() (*slog.Logger, *strings.Builder) {
	var b strings.Builder
	return slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo})), &b
}

var _ = Describe("Selecting a workflow for a run", Label("unit"), func() {
	It("Project scope shadows org scope with a warning", func(ctx SpecContext) {
		// Given quick-fix pushed at org scope and again at project scope
		// for project P.
		store := scoped{
			org: &millv1.Workflow{Name: "quick-fix", Scope: millv1.Scope_SCOPE_ORG, Version: "org-version"},
			project: map[string]*millv1.Workflow{
				"P": {Name: "quick-fix", Scope: millv1.Scope_SCOPE_PROJECT, Project: "P", Version: "project-version"},
			},
		}
		log, out := logged()

		// When a run for P selects quick-fix.
		chosen, err := workflow.Resolve(ctx, store, log, "quick-fix", "P")

		// Then the project version is chosen, and the load log contains one
		// shadow warning.
		Expect(err).NotTo(HaveOccurred())
		Expect(chosen.GetVersion()).To(Equal("project-version"))
		Expect(chosen.GetScope()).To(Equal(millv1.Scope_SCOPE_PROJECT))
		Expect(strings.Count(out.String(), "level=WARN")).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("shadows the org definition"))
		Expect(out.String()).To(ContainSubstring("shadowed=org-version"))
	})

	It("falls back to the org definition, silently, when the project has none", func(ctx SpecContext) {
		store := scoped{org: &millv1.Workflow{Name: "quick-fix", Scope: millv1.Scope_SCOPE_ORG, Version: "org-version"}}
		log, out := logged()

		chosen, err := workflow.Resolve(ctx, store, log, "quick-fix", "P")
		Expect(err).NotTo(HaveOccurred())
		Expect(chosen.GetVersion()).To(Equal("org-version"))
		Expect(out.String()).NotTo(ContainSubstring("level=WARN"), "a project that defines nothing shadows nothing")
	})

	It("takes the project definition without a warning when no org one exists", func(ctx SpecContext) {
		store := scoped{project: map[string]*millv1.Workflow{
			"P": {Name: "quick-fix", Scope: millv1.Scope_SCOPE_PROJECT, Project: "P", Version: "project-version"},
		}}
		log, out := logged()

		chosen, err := workflow.Resolve(ctx, store, log, "quick-fix", "P")
		Expect(err).NotTo(HaveOccurred())
		Expect(chosen.GetVersion()).To(Equal("project-version"))
		Expect(out.String()).NotTo(ContainSubstring("level=WARN"))
	})

	It("says so when neither scope has the name", func(ctx SpecContext) {
		log, _ := logged()
		_, err := workflow.Resolve(ctx, scoped{}, log, "quick-fix", "P")
		Expect(err).To(MatchError(ContainSubstring("quick-fix")))
		Expect(err).To(MatchError(ContainSubstring("P")))
	})

	It("reports a lookup that failed rather than falling back", func(ctx SpecContext) {
		log, _ := logged()
		_, err := workflow.Resolve(ctx, scoped{err: errors.New("no route to store")}, log, "quick-fix", "P")
		Expect(err).To(MatchError(ContainSubstring("no route to store")))
	})
})
