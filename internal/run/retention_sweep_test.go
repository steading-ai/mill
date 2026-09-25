package run_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// stubTwin is a workspace.TwinSource that always succeeds with a few fixed
// bytes — enough for SealAttempt to produce a real twin file on disk without
// the local backend's transcript-directory setup seal_test.go's own
// "captures a real local twin" spec needs, which retention has no reason to
// exercise again.
type stubTwin struct{}

func (stubTwin) CaptureTwin(context.Context, string) (io.Reader, error) {
	return strings.NewReader("twin bytes"), nil
}

// closeSealed closes f's one step run with a commit, so its step run holds
// an ended_at [SweepRetention] can age against — SealAttempt itself does not
// require this (seal_test.go seals an open attempt directly), but retention
// only ever acts on an attempt whose store record says when it ended.
func closeSealed(ctx context.Context, f *sealFixture) {
	GinkgoHelper()
	_, err := f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
		LeaseId:   sealLease,
		StepRunId: f.stepRun.GetId(),
		Evidence: &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Commit{Commit: "0123456789abcdef0123456789abcdef01234567"},
		},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// retentionRunner builds a bare *run.Runner over f's store client and log
// dir with the policy and clock a spec is testing — no skift, no
// workspace, exactly what SweepRetention itself touches.
func retentionRunner(f *sealFixture, policy run.Policy, projects map[string]run.Policy, now time.Time) *run.Runner {
	return run.New(run.Options{
		Store:             f.store,
		LogDir:            f.dir,
		Log:               slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Retention:         policy,
		RetentionProjects: projects,
		Now:               func() time.Time { return now },
	})
}

var _ = Describe("SweepRetention", Label(testsupport.DBLabel), func() {
	It("removes an old attempt's twin past twin_days, leaving its frame and index alone", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))
		closeSealed(ctx, f)
		Expect(f.seal(ctx, "session-1", stubTwin{})).To(Succeed())

		var before int
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE step_run_id = $1`, f.stepRun.GetId()).Scan(&before)).To(Succeed())

		report, err := retentionRunner(f, run.Policy{TwinDays: 1}, nil, time.Now().Add(48*time.Hour)).SweepRetention(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(report.TwinsRemoved).To(Equal(1))
		Expect(report.FramesCompacted).To(Equal(0))
		_, err = os.Stat(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt()))
		Expect(err).To(MatchError(os.ErrNotExist), "the old twin is removed")
		Expect(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile(), "the frame is untouched by a twin-only policy")

		var after int
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE step_run_id = $1`, f.stepRun.GetId()).Scan(&after)).To(Succeed())
		Expect(after).To(Equal(before), "retention never touches the activity index")
	})

	It("compacts an old attempt's frame past compact_days, leaving its twin and index alone", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))
		closeSealed(ctx, f)
		Expect(f.seal(ctx, "session-1", stubTwin{})).To(Succeed())

		report, err := retentionRunner(f, run.Policy{CompactDays: 1}, nil, time.Now().Add(48*time.Hour)).SweepRetention(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(report.TwinsRemoved).To(Equal(0))
		Expect(report.FramesCompacted).To(Equal(1))
		Expect(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile(), "the twin is untouched by a frame-only policy")

		frame, err := run.ReadFrame(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt()))
		Expect(err).NotTo(HaveOccurred())
		Expect(frame).To(HaveLen(1))
		Expect(frame[0].GetTruncated()).To(BeTrue())
	})

	It("leaves a young attempt untouched under the same policy that would remove an old one", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))
		closeSealed(ctx, f)
		Expect(f.seal(ctx, "session-1", stubTwin{})).To(Succeed())

		report, err := retentionRunner(f, run.Policy{TwinDays: 1, CompactDays: 1}, nil, time.Now().Add(time.Minute)).SweepRetention(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(report.TwinsRemoved).To(Equal(0))
		Expect(report.FramesCompacted).To(Equal(0))
		Expect(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
		Expect(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
	})

	It("does nothing and says so in one line when the policy is keep everywhere", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))
		closeSealed(ctx, f)
		Expect(f.seal(ctx, "session-1", stubTwin{})).To(Succeed())

		var buf strings.Builder
		runner := run.New(run.Options{
			Store:  f.store,
			LogDir: f.dir,
			Log:    slog.New(slog.NewTextHandler(&buf, nil)),
		})

		report, err := runner.SweepRetention(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(report.TwinsRemoved).To(Equal(0))
		Expect(report.FramesCompacted).To(Equal(0))
		Expect(buf.String()).To(ContainSubstring("nothing to sweep"))
		Expect(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
		Expect(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
	})

	It("resolves a project's own policy over the deployment default", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))
		closeSealed(ctx, f)
		Expect(f.seal(ctx, "session-1", stubTwin{})).To(Succeed())

		// f's run holds NodeRef "seal#1" (newSealFixture): its project is "seal".
		report, err := retentionRunner(f, run.Policy{TwinDays: 1}, map[string]run.Policy{"seal": {}}, time.Now().Add(48*time.Hour)).SweepRetention(ctx)
		Expect(err).NotTo(HaveOccurred())

		Expect(report.TwinsRemoved).To(Equal(0), "seal's own policy overrides the deployment default that would have removed it")
		Expect(run.TwinPath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
	})
})
