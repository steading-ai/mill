package server_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/server/interceptor"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("A connection's uid", Label(testsupport.DBLabel), func() {
	It("the service's own uid writes the ledger", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#30")
		_, err := f.as(serviceUID).Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{RunId: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.as(serviceUID).Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("an operator uid reads and operates but writes no evidence", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#31")

		_, err := f.as(operatorUID).Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())

		_, err = f.as(operatorUID).Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{RunId: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())

		_, err = f.as(operatorUID).StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: lease, RunId: f.run.GetId(), StepName: "implement", WorkerRef: "worker-1",
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err.Error()).To(ContainSubstring(string(interceptor.RoleOperator)))
	})

	It("a reader uid reads", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#32")
		listed, err := f.as(readerUID).Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetRuns()).To(HaveLen(1))

		_, err = f.as(readerUID).Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("a reader calling Cancel is refused naming the role", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#33")
		_, err := f.as(readerUID).Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{RunId: f.run.GetId()}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err.Error()).To(ContainSubstring(string(interceptor.RoleReader)))
		Expect(err.Error()).To(ContainSubstring("Cancel"))

		run := f.reload(ctx)
		Expect(run.GetCancelRequested()).To(BeFalse(), "the refused request changed nothing")
	})

	It("a uid in no role calling Cancel is unauthenticated", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#35")
		_, err := f.as(strangerUID).Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{RunId: f.run.GetId()}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(err.Error()).To(ContainSubstring("9003"), "the refusal names the uid it could not place")
		Expect(f.reload(ctx).GetCancelRequested()).To(BeFalse(), "the refused request changed nothing")
	})

	It("a reader reads the tools that were added for the operator", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#36")
		_, err := f.as(readerUID).History(ctx, connect.NewRequest(&millv1.HistoryRequest{Id: f.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.as(readerUID).Cost(ctx, connect.NewRequest(&millv1.CostRequest{}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.as(readerUID).Health(ctx, connect.NewRequest(&millv1.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("a uid in no role is unauthenticated", func(ctx SpecContext) {
		f := newFixture(ctx, "mill#34")
		_, err := f.as(strangerUID).Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(err.Error()).To(ContainSubstring("9003"), "the refusal names the uid it could not place")
	})
})

var _ = Describe("The unix socket", Label(testsupport.DBLabel), func() {
	It("answers the account it runs as and nobody the map does not name", func(ctx SpecContext) {
		pool := pg.Database(ctx)
		socket := filepath.Join(GinkgoT().TempDir(), "mill.sock")
		srv := server.New(server.Config{
			SocketPath: socket,
			Logger:     specLogger(),
			Interceptors: []connect.Interceptor{interceptor.NewRoles(
				interceptor.RoleMap{ServiceUID: os.Getuid()}, server.Requirement())},
		}, server.NewStoreService(domain.New(pool)))
		Expect(srv.Listen()).To(Succeed())

		serveCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(serveCtx) }()
		DeferCleanup(func() {
			stop()
			Eventually(served).Should(Receive(BeNil()))
		})

		info, err := os.Stat(socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode() & os.ModeSocket).NotTo(BeZero())
		Expect(info.Mode().Perm()).To(Equal(server.DefaultSocketMode.Perm()))

		// The peer uid is this process's own, which the map names as the
		// service: the ordinary case, since mill and the operator share an
		// account.
		client := millv1connect.NewStoreServiceClient(unixClient(socket), "http://mill")
		listed, err := client.Status(ctx, connect.NewRequest(&millv1.StatusRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listed.Msg.GetRuns()).To(BeEmpty())
	})
})

// unixClient dials the socket whatever the URL says, which is what a
// Connect client over a unix socket needs.
func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}
