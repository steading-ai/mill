package run_test

import (
	"context"
	"log/slog"
	"os"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("SealOrphans", Label(testsupport.DBLabel), func() {
	// closingRunner builds a bare *run.Runner over sealFixture's store
	// client and log dir — no dagger, no workspace, exactly what
	// SealOrphans itself touches.
	closingRunner := func(f *sealFixture) *run.Runner {
		return run.New(run.Options{
			Store:  f.store,
			LogDir: f.dir,
			Log:    slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		})
	}

	It("seals a closed attempt's orphaned live file", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))

		_, err := f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId:   sealLease,
			StepRunId: f.stepRun.GetId(),
			Evidence: &millv1.Evidence{
				Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
				Value: &millv1.Evidence_Commit{Commit: "0123456789abcdef0123456789abcdef01234567"},
			},
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(closingRunner(f).SealOrphans(ctx)).To(Succeed())

		_, err = os.Stat(f.live())
		Expect(err).To(MatchError(os.ErrNotExist), "a closed attempt's orphaned live file is sealed")
		Expect(run.FramePath(f.dir, f.runID, "implement", f.stepRun.GetAttempt())).To(BeAnExistingFile())
	})

	It("leaves a still-running attempt's live file untouched", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))

		Expect(closingRunner(f).SealOrphans(ctx)).To(Succeed())

		Expect(f.live()).To(BeAnExistingFile(), "the attempt is still running; SealOrphans leaves it for the worker actually driving it")
	})

	It("leaves a run Inspect no longer knows about untouched", func(ctx context.Context) {
		f := newSealFixture(ctx)
		testsupport.AppendActivity(f.live(), testsupport.TextActivity("hello"))

		// A live file under a run id the store has no record of at all —
		// deleted, or written by a different deployment's worker.
		other := run.ActivityPath(f.dir, "01JNEVERCREATED00000000000", "implement", 1)
		testsupport.AppendActivity(other, testsupport.TextActivity("hello"))

		Expect(closingRunner(f).SealOrphans(ctx)).To(Succeed())

		Expect(f.live()).To(BeAnExistingFile())
		Expect(other).To(BeAnExistingFile())
	})
})
