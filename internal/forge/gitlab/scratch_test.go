package gitlab_test

import (
	"context"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
)

var _ = Describe("The GitLab scratch namespace", Label("unit"), func() {
	var (
		api     *stub
		scratch forge.Scratch
	)

	BeforeEach(func() {
		api = newStub()
		scratch = gitlab.NewScratch(api.server.URL, "mill/scratch", api.minter(), gitlab.WithHTTPClient(api.server.Client()))
	})

	It("lists the group's projects with the name and age a sweep matches on", func(ctx context.Context) {
		api.on(http.MethodGet, "/api/v4/groups/mill%2Fscratch/projects", http.StatusOK, "group_projects.json")

		projects, err := scratch.ListProjects(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(projects).To(Equal([]forge.ScratchProject{
			{ID: "4711", Name: "mill-acceptance-quick-fix-0badcafe", Path: "mill/scratch/mill-acceptance-quick-fix-0badcafe",
				CreatedAt: time.Date(2026, 9, 9, 8, 15, 0, 0, time.UTC)},
			{ID: "4712", Name: "homestead", Path: "mill/scratch/homestead",
				CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		}))
		Expect(scratch.Namespace()).To(Equal("mill/scratch"))
		calls := api.calls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Token).To(Equal(theToken), "the mill's credential, minted for the call")
	})

	It("deletes a project by id, and takes one the forge no longer has as deleted", func(ctx context.Context) {
		api.on(http.MethodDelete, "/api/v4/projects/4711", http.StatusAccepted, "empty.json")

		Expect(scratch.DeleteProject(ctx, forge.ScratchProject{ID: "4711", Path: "mill/scratch/mill-acceptance-quick-fix-0badcafe"})).To(Succeed())
		Expect(api.count(http.MethodDelete, "/api/v4/projects/4711")).To(Equal(1))

		// No route: the stub answers 404 as GitLab does for a project that
		// is gone.
		Expect(scratch.DeleteProject(ctx, forge.ScratchProject{ID: "4799"})).To(Succeed())
		Expect(scratch.DeleteProject(ctx, forge.ScratchProject{Path: "mill/scratch/nameless"})).To(MatchError(ContainSubstring("needs its id")))
	})
})
