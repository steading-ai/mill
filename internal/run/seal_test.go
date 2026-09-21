package run_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
)

// sealLease is the lease sealFixture's run and step run are created under.
// SealAttempt itself is not fenced (docs/activity.md), so nothing here ever
// presents it again after StartStepRun.
const sealLease = "seal-lease-1"

// sealFixture is a store client and one open step run, against a real
// database but with none of support_test.go's dagger or scripted-session
// machinery: SealAttempt only needs a step_run_id the activity table's
// foreign key accepts and a store to call it through.
type sealFixture struct {
	store   millv1connect.StoreServiceClient
	pool    *pgxpool.Pool
	dir     string
	runID   string
	stepRun *millv1.StepRun
}

// newSealFixture opens a run and its one step run directly through the
// store's own handlers, the same CreateRun-then-StartStepRun sequence
// openRun uses (run.go), with a synthetic node id (unregistered with any
// dagger, as sweep_test.go's fixture already does) since sealing an
// attempt has nothing to do with the graph.
func newSealFixture(ctx context.Context) *sealFixture {
	GinkgoHelper()
	pool := pg.Database(ctx)
	srv := server.New(server.Config{Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))},
		server.NewStoreService(domain.New(pool), server.NewObserver(server.DefaultObserverFallback), GinkgoT().TempDir()), nil)
	ts := httptest.NewServer(srv.Handler())
	DeferCleanup(ts.Close)
	store := millv1connect.NewStoreServiceClient(ts.Client(), ts.URL)

	created, err := store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId:   sealLease,
		NodeId:    "seal-node",
		NodeRef:   "seal#1",
		ContextId: "seal-ctx",
		WorkerRef: "seal-worker",
		Branch:    "mill/seal",
	}))
	Expect(err).NotTo(HaveOccurred())
	runID := created.Msg.GetRun().GetId()

	started, err := store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId:   sealLease,
		RunId:     runID,
		StepName:  "implement",
		Kind:      millv1.StepKind_STEP_KIND_SKILL,
		WorkerRef: "seal-worker",
	}))
	Expect(err).NotTo(HaveOccurred())

	return &sealFixture{store: store, pool: pool, dir: GinkgoT().TempDir(), runID: runID, stepRun: started.Msg.GetStepRun()}
}

// live is the path SealAttempt reads from and, on success, removes.
func (f *sealFixture) live() string {
	return run.ActivityPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())
}

// seal calls run.SealAttempt for this fixture's one attempt.
func (f *sealFixture) seal(ctx context.Context, sessionID string, twin workspace.TwinSource) error {
	return run.SealAttempt(ctx, f.dir, f.runID, "implement", f.stepRun.GetAttempt(), f.stepRun.GetId(), sessionID, f.store, twin)
}

// textActivity is a harness Text record on turn 1, with the block id and
// final flag coalescing groups on, for the specs that build a streamed run
// of fragments by hand rather than through the real activityWriter
// (unexported, and streaming itself is not something today's harness emits
// — see coalesceActivities).
func textActivity(blockID, text string, final bool) *millv1.Activity {
	return &millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
		Turn:   1,
		Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: blockID, Text: text, Final: final}},
	}
}

// failingTwin is a workspace.TwinSource that always refuses, so a spec can
// prove a twin failure leaves the live file in place rather than actually
// exercising a real workspace's capture.
type failingTwin struct{}

func (failingTwin) CaptureTwin(context.Context, string) (io.Reader, error) {
	return nil, errors.New("twin: capture refused")
}

// localTwin adapts local.CaptureTwin's free-function signature to
// workspace.TwinSource, for the one spec that captures a real twin rather
// than a fake, closing the local backend's half of the twin end to end.
type localTwin struct{ root, runID string }

func (t localTwin) CaptureTwin(ctx context.Context, sessionID string) (io.Reader, error) {
	return local.CaptureTwin(ctx, t.root, t.runID, sessionID)
}

var _ = Describe("SealAttempt", Label(testsupport.DBLabel), func() {
	It("seals a real-shaped live file: same records, frame decompresses, live file gone", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(),
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL, Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "implement", Attempt: 1}}},
			textActivity("", "hello there", true),
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Turn: 1, Body: &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "read"}}},
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Turn: 1, Body: &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1", Content: "contents"}}},
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL, Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, EvidenceKind: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT}}},
		)

		Expect(f.seal(ctx, "", nil)).To(Succeed())

		_, err := os.Stat(f.live())
		Expect(err).To(MatchError(os.ErrNotExist), "the live file is removed once every step of the seal has succeeded")

		frame, err := os.Open(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = frame.Close() }()
		Expect(io.ReadAll(frame)).NotTo(BeEmpty())
	})

	It("coalesces a streamed run of Text fragments into one record, concatenated in order", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(),
			textActivity("block-1", "hel", false),
			textActivity("block-1", "lo ", false),
			textActivity("block-1", "there", true),
		)

		Expect(f.seal(ctx, "", nil)).To(Succeed())

		var count int
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE step_run_id = $1`, f.stepRun.GetId()).Scan(&count)).To(Succeed())
		Expect(count).To(Equal(1), "three fragments of the same block, turn and source coalesce into one row")
	})

	It("leaves the live file in place when the twin fails after the frame and the store row are already sealed", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))

		err := f.seal(ctx, "session-1", failingTwin{})
		Expect(err).To(HaveOccurred())

		_, statErr := os.Stat(f.live())
		Expect(statErr).NotTo(HaveOccurred(), "a failed twin must not remove the live file: a retry needs it")

		_, statErr = os.Stat(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt()))
		Expect(statErr).To(MatchError(os.ErrNotExist), "no partial twin is ever renamed into place")
	})

	It("is a no-op when there is no live file to seal", func(ctx context.Context) {
		f := newSealFixture(ctx)
		Expect(f.seal(ctx, "", nil)).To(Succeed())
	})

	It("captures a real local twin alongside the frame", func(ctx context.Context) {
		home := GinkgoT().TempDir()
		GinkgoT().Setenv("HOME", home)
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))

		cwd := filepath.Join(f.dir, f.runID, "checkout")
		slug := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
		transcriptDir := filepath.Join(home, ".claude", "projects", slug)
		Expect(os.MkdirAll(transcriptDir, 0o750)).To(Succeed())
		transcript := []byte(`{"type":"turn","role":"user","content":"hi"}` + "\n")
		Expect(os.WriteFile(filepath.Join(transcriptDir, "session-1.jsonl"), transcript, 0o600)).To(Succeed())

		Expect(f.seal(ctx, "session-1", localTwin{root: f.dir, runID: f.runID})).To(Succeed())

		twin, err := os.Open(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = twin.Close() }()
		dec, err := zstd.NewReader(twin)
		Expect(err).NotTo(HaveOccurred())
		defer dec.Close()
		files := testsupport.Untar(dec)
		Expect(files).To(HaveKeyWithValue("session-1.jsonl", transcript))
	})
})
