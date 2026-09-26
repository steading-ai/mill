package server_test

import (
	"net/http"
	"net/http/httptest"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/server"
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

	It("reports State and InFlight from the worker's own configuration", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			State:    func() millv1.WorkerState { return millv1.WorkerState_WORKER_STATE_DRAINING },
			InFlight: func() []run.RunningNode { return []run.RunningNode{{NodeRef: "millspec#1"}} },
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		res, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetState()).To(Equal(millv1.WorkerState_WORKER_STATE_DRAINING))
		Expect(res.Msg.GetInFlight()).To(HaveLen(1))
		Expect(res.Msg.GetInFlight()[0].GetNodeRef()).To(Equal("millspec#1"))
	})

	It("reports UpstreamOutage only while State is WAITING_ON_UPSTREAM", func(ctx SpecContext) {
		since := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		reset := since.Add(2 * time.Minute)
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			State: func() millv1.WorkerState { return millv1.WorkerState_WORKER_STATE_WAITING_ON_UPSTREAM },
			Outage: func() *gateway.UpstreamRefusal {
				return &gateway.UpstreamRefusal{Since: since, Reset: reset}
			},
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		res, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetState()).To(Equal(millv1.WorkerState_WORKER_STATE_WAITING_ON_UPSTREAM))
		Expect(res.Msg.GetUpstreamOutage().GetSince().AsTime()).To(BeTemporally("==", since))
		Expect(res.Msg.GetUpstreamOutage().GetResetAt().AsTime()).To(BeTemporally("==", reset))
	})

	It("carries no UpstreamOutage while claiming, even with one still recorded on the worker", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			State: func() millv1.WorkerState { return millv1.WorkerState_WORKER_STATE_CLAIMING },
			Outage: func() *gateway.UpstreamRefusal {
				return &gateway.UpstreamRefusal{Since: time.Now()}
			},
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		res, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetUpstreamOutage()).To(BeNil())
	})

	It("Drain calls the worker's own Drain function exactly once", func(ctx SpecContext) {
		calls := 0
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			Drain: func() { calls++ },
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		_, err := client.Drain(ctx, connect.NewRequest(&millv1.DrainRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(1))
	})

	It("Drain is CodeFailedPrecondition when this worker has nothing to drain", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		_, err := client.Drain(ctx, connect.NewRequest(&millv1.DrainRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
	})

	It("answers Claims from this process's own configuration, with no probe and no side effect", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(server.NewWorkerService(server.WorkerServiceConfig{
			Host:     "mill-worker.example",
			Projects: []string{"mill"},
			Labels:   []string{"quick"},
		})))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		res, err := client.Claims(ctx, connect.NewRequest(&millv1.ClaimsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetHost()).To(Equal("mill-worker.example"))
		Expect(res.Msg.GetProjects()).To(Equal([]string{"mill"}))
		Expect(res.Msg.GetLabels()).To(Equal([]string{"quick"}))
	})

	It("is CodeUnimplemented for Claims too, when no worker runs in this process", func(ctx SpecContext) {
		ts := httptest.NewServer(workerMux(nil))
		DeferCleanup(ts.Close)
		client := millv1connect.NewWorkerServiceClient(ts.Client(), ts.URL)

		_, err := client.Claims(ctx, connect.NewRequest(&millv1.ClaimsRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
	})
})
