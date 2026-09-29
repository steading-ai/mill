package cli_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/server"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("mill restart", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("routes the retire through the worker RPC and exits zero", func() {
		m := serving(ctx)
		g := skiftNodeWith(ctx, "https://gitlab.example.test/group/thing.git")
		r := openRunOn(ctx, m, g.nodeRef, g.nodeID)

		var gotProject, gotBranch string
		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			RetireBranch: func(_ context.Context, project, branch, comment string) (string, error) {
				gotProject, gotBranch = project, branch
				return "https://example.test/pr/1", nil
			},
		})

		w.run(ctx, "restart", g.nodeRef,
			"--project", skiftProject,
			"--skift-socket", g.service.Socket,
			"--workspace-dir", GinkgoT().TempDir(),
			"--reason", "test")

		Expect(gotProject).To(Equal(skiftProject))
		Expect(gotBranch).To(Equal(r.run.GetBranch()))
	})

	It("a worker that cannot retire the branch fails the restart, naming the cause", func() {
		m := serving(ctx)
		g := skiftNodeWith(ctx, "https://gitlab.example.test/group/thing.git")
		openRunOn(ctx, m, g.nodeRef, g.nodeID)

		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			RetireBranch: func(context.Context, string, string, string) (string, error) {
				return "", errors.New("no forge credential")
			},
		})

		out, err := w.try(ctx, "restart", g.nodeRef,
			"--project", skiftProject,
			"--skift-socket", g.service.Socket,
			"--workspace-dir", GinkgoT().TempDir(),
			"--reason", "test")

		Expect(err).To(MatchError(ContainSubstring("no forge credential")), out)
	})
})
