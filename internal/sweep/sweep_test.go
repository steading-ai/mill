package sweep_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/sweep"
	"github.com/ivy/mill/internal/workspace/machine"
)

// fakeProjects is a dagger's project verbs: what it lists, whether it
// offers a delete, and what it was asked to do.
type fakeProjects struct {
	projects []*daggerv1.Project
	deletes  bool
	archived []string
	deleted  []string
}

func (f *fakeProjects) ListProjects(context.Context) ([]*daggerv1.Project, error) {
	return f.projects, nil
}

func (f *fakeProjects) ArchiveProject(_ context.Context, slug string) (*daggerv1.Project, error) {
	f.archived = append(f.archived, slug)
	return &daggerv1.Project{Slug: slug, ArchivedAt: timestamppb.Now()}, nil
}

func (f *fakeProjects) DeleteProject(_ context.Context, slug string) error {
	if !f.deletes {
		return errors.ErrUnsupported
	}
	f.deleted = append(f.deleted, slug)
	return nil
}

// fakeScratch is a forge namespace.
type fakeScratch struct {
	projects []forge.ScratchProject
	deleted  []forge.ScratchProject
}

func (*fakeScratch) Namespace() string { return "mill/scratch" }

func (f *fakeScratch) ListProjects(context.Context) ([]forge.ScratchProject, error) {
	return f.projects, nil
}

func (f *fakeScratch) DeleteProject(_ context.Context, project forge.ScratchProject) error {
	f.deleted = append(f.deleted, project)
	return nil
}

var _ = Describe("The external state adapters", Label("unit"), func() {
	made := time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC)

	Describe("dagger's scratch projects", func() {
		var projects *fakeProjects

		BeforeEach(func() {
			projects = &fakeProjects{projects: []*daggerv1.Project{
				{Slug: "mill-acceptance-x-0badcafe", CreatedAt: timestamppb.New(made)},
				{Slug: "mill", CreatedAt: timestamppb.New(made)},
				{Slug: "mill-acceptance-y-deadbeef", CreatedAt: timestamppb.New(made), ArchivedAt: timestamppb.New(made)},
			}}
		})

		It("lists the active projects by slug and age", func(ctx context.Context) {
			external := sweep.Dagger(projects)
			Expect(external.Kind()).To(Equal("dagger"))
			Expect(external.List(ctx)).To(Equal([]machine.Scratch{
				{Name: "mill-acceptance-x-0badcafe", CreatedAt: made},
				{Name: "mill", CreatedAt: made},
			}), "an archived project is already swept")
		})

		It("archives when the service offers no delete, and says so", func(ctx context.Context) {
			action, err := sweep.Dagger(projects).Remove(ctx, machine.Scratch{Name: "mill-acceptance-x-0badcafe"})
			Expect(err).NotTo(HaveOccurred())
			Expect(action).To(Equal("archived (dagger offers no project delete)"))
			Expect(projects.archived).To(Equal([]string{"mill-acceptance-x-0badcafe"}))
			Expect(projects.deleted).To(BeEmpty())
		})

		It("deletes when the service offers it", func(ctx context.Context) {
			projects.deletes = true
			action, err := sweep.Dagger(projects).Remove(ctx, machine.Scratch{Name: "mill-acceptance-x-0badcafe"})
			Expect(err).NotTo(HaveOccurred())
			Expect(action).To(Equal("deleted"))
			Expect(projects.deleted).To(Equal([]string{"mill-acceptance-x-0badcafe"}))
			Expect(projects.archived).To(BeEmpty())
		})
	})

	Describe("the forge's scratch namespace", func() {
		It("lists by name and age, and deletes by the forge's own id", func(ctx context.Context) {
			scratch := &fakeScratch{projects: []forge.ScratchProject{
				{ID: "4711", Name: "mill-acceptance-x-0badcafe", Path: "mill/scratch/mill-acceptance-x-0badcafe", CreatedAt: made},
			}}
			external := sweep.Forge(scratch)
			Expect(external.Kind()).To(Equal("forge"))
			Expect(external.List(ctx)).To(Equal([]machine.Scratch{{Name: "mill-acceptance-x-0badcafe", CreatedAt: made}}))

			action, err := external.Remove(ctx, machine.Scratch{Name: "mill-acceptance-x-0badcafe"})
			Expect(err).NotTo(HaveOccurred())
			Expect(action).To(Equal("deleted from mill/scratch"))
			Expect(scratch.deleted).To(Equal(scratch.projects))

			_, err = external.Remove(ctx, machine.Scratch{Name: "mill-acceptance-z-00000000"})
			Expect(err).To(MatchError(ContainSubstring("not listed")))
		})
	})
})
