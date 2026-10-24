package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"

	"github.com/steading-ai/mill/internal/skift"
)

// Runner.stop when walk returned nil, specified against a skift that only
// records releases: what is under test is whether the lease is released as
// a crash, and no store is reached on this path at all.
var _ = Describe("Stopping a run whose walk finished", Label("unit"), func() {
	var releases *releaseRecorder
	var r *Runner
	var s *state

	BeforeEach(func() {
		releases = &releaseRecorder{}
		mux := http.NewServeMux()
		mux.Handle(skiftv1connect.NewWorkServiceHandler(releases))
		server := httptest.NewServer(mux)
		DeferCleanup(server.Close)

		r = &Runner{options: Options{
			Interval: time.Hour,
			Skift:    skift.New(server.Client(), server.URL, "millspec"),
			Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			Out:      io.Discard,
		}}
		s = &state{}
		s.setLease("lease-1")
		s.beat = r.startHeartbeat(context.Background(), s)
	})

	DescribeTable("is done, and releases nothing, when the heartbeat raised a cancel or a shutdown on its way out",
		func(reason error) {
			s.beat.fail(reason)

			Expect(r.stop(context.Background(), s, nil)).To(Succeed())
			Expect(releases.recorded()).To(BeEmpty(), "a finished run has nothing left to stop, and did not crash")
		},
		Entry("a cancel", errCancelled),
		Entry("a shutdown", errShutdown),
	)

	It("is the heartbeat's failure, released as a crash, when the heartbeat failed some other way", func() {
		failure := errors.New("skift: heartbeat: unavailable")
		s.beat.fail(failure)

		Expect(r.stop(context.Background(), s, nil)).To(MatchError(failure))
		Expect(releases.recorded()).To(Equal([]string{"RELEASE_REASON_CRASH: " + failure.Error()}))
	})
})

// releaseRecorder is a skift work service that answers Release alone, and
// records each as "reason: detail".
type releaseRecorder struct {
	skiftv1connect.UnimplementedWorkServiceHandler

	mu   sync.Mutex
	seen []string
}

func (r *releaseRecorder) Release(_ context.Context, req *connect.Request[skiftv1.ReleaseRequest]) (*connect.Response[skiftv1.ReleaseResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Msg.GetReason().String()+": "+req.Msg.GetDetail())
	return connect.NewResponse(&skiftv1.ReleaseResponse{}), nil
}

func (r *releaseRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}
