package server_test

import (
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/server"
)

// workerMux is a server built with (or without) a WorkerService mounted, the
// way runServe builds one only inside `if opts.work`.
func workerMux(worker millv1connect.WorkerServiceHandler) http.Handler {
	return server.New(server.Config{Logger: specLogger()}, nil, worker).Handler()
}

var _ = Describe("WorkerService", Label("unit"), func() {
	It("answers Probe from this process's own configuration when a worker runs here", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			SidecarEntrypoint: "/nonexistent/sidecar",
			WorkspaceDir:      GinkgoT().TempDir(),
			Name:              "mill",
			Identity:          "mill@example.test",
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		res, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetSidecar().GetOk()).To(BeFalse(), "no such sidecar to start")
		Expect(res.Msg.GetSigning().GetOk()).To(BeFalse(), "no signing key under an empty workspace")
		Expect(res.Msg.GetForge().GetOk()).To(BeFalse())
		Expect(res.Msg.GetForge().GetReason()).To(ContainSubstring("no project configured"))
	})

	It("is CodeUnimplemented, not a bare 404, when no worker runs in this process", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(nil))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		_, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
	})
})
