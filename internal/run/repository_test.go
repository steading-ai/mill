package run_test

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// The repository a run delivers to is the dagger project's, read from the
// claim: never the worker's configuration, and never shared between runs.
var _ = Describe("The run's repository", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)
	})

	It("is resolved from the claim once, before the workspace is opened", func() {
		var resolved atomic.Int64
		var slug string
		inner := f.workspaces()
		var seen run.Repository
		runner := f.runner(func(o *run.Options) {
			o.Repositories = func(_ context.Context, project *skiftv1.Project) (run.Repository, error) {
				resolved.Add(1)
				slug = project.GetSlug()
				return run.Repository{Forge: f.forge, Origin: f.origin}, nil
			}
			o.Workspaces = func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
				seen = repo
				return inner(ctx, spec, repo)
			}
		})

		Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())

		Expect(resolved.Load()).To(BeEquivalentTo(1), "one resolution per run")
		Expect(slug).To(Equal(project), "the project the claim came back with")
		Expect(seen.Origin).To(Equal(f.origin), "the workspace clones what the resolution said")
		Expect(seen.Forge).To(Equal(f.forge))
	})

	It("fails the run before a clone when the project has no repository, and holds the node rather than releasing it", func() {
		var opened atomic.Int64
		runner := f.runner(func(o *run.Options) {
			o.Repositories = func(context.Context, *skiftv1.Project) (run.Repository, error) {
				return run.Repository{}, &forge.NoVCSURLError{Project: project}
			}
			o.Workspaces = func(context.Context, workspace.Spec, run.Repository) (run.Workspace, error) {
				opened.Add(1)
				return nil, nil
			}
		})

		err := runner.Execute(ctx, f.nodeRef)

		Expect(err).To(MatchError(ContainSubstring("dg project update " + project + " --vcs-url")))
		Expect(opened.Load()).To(BeZero(), "nothing was cloned")
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetHold().GetReason()).To(HavePrefix("no repository"),
			"held under the same reason rather than reclaimed into the same refusal")
		Expect(node.GetStatus()).To(Equal(skiftv1.Status_STATUS_OPEN))
	})

	It("reads the project from dagger when the claim carries none", func() {
		var slug string
		runner := f.runner(func(o *run.Options) {
			o.Repositories = func(_ context.Context, project *skiftv1.Project) (run.Repository, error) {
				slug = project.GetSlug()
				return run.Repository{Forge: f.forge, Origin: f.origin}, nil
			}
		})
		claimed, err := f.client.Claim(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed.Project).NotTo(BeNil(), "dagger hands the project back with the claim")
		claimed.Project = nil

		Expect(runner.ExecuteClaimed(ctx, claimed, nil)).To(Succeed())

		Expect(slug).To(Equal(project))
	})
})
