package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace/redact"
)

var _ = Describe("mill logs", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("tails the running step's redacted log and exits when the step run leaves running", func() {
		const secret = "s3cret-forge-token-value"
		r := openRun(ctx, "millcli#20", nodeID(20))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		// The log as the worker writes it: through the redactor, with the
		// value the session registered with `redact` mid-step already
		// masked. Nothing downstream re-reads the unmasked bytes, because
		// there are none.
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.StepLogPath(logDir, r.run.GetId(), "implement", sr.GetAttempt())
		Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
		file, err := os.Create(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(file.Close)
		redacted := redact.New(secret).Writer(file)
		_, err = redacted.Write([]byte("text reaching the forge with " + secret + "\n"))
		Expect(err).NotTo(HaveOccurred())

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"logs", "millcli#20", "--step", "implement", "--follow",
			"--socket", r.mill.socket, "--log-dir", logDir})
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			done <- root.ExecuteContext(ctx)
		}()

		// The first line is tailed while the attempt is still running, and
		// the command does not return.
		Eventually(stdout.String, 10*time.Second, 50*time.Millisecond).
			Should(ContainSubstring("reaching the forge with " + redact.Mask))
		Consistently(done, 500*time.Millisecond).ShouldNot(Receive())

		// A line written after the tail started reaches it too, and closing
		// the attempt ends the command.
		_, err = redacted.Write([]byte("more output, still with " + secret + "\n"))
		Expect(err).NotTo(HaveOccurred())
		r.close(ctx, sr, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + string(bytes.Repeat([]byte("d"), 64))},
				VerifiedBy: "spec",
			},
		})

		Eventually(done, 10*time.Second).Should(Receive(BeNil()), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("more output, still with " + redact.Mask))
		Expect(stdout.String()).NotTo(ContainSubstring(secret))
	})

	It("reads the step the run is on when no step is named", func() {
		r := openRun(ctx, "millcli#21", nodeID(21))
		sr := r.start(ctx, "gather-context", millv1.StepKind_STEP_KIND_SKILL)

		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.StepLogPath(logDir, r.run.GetId(), "gather-context", sr.GetAttempt())
		Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
		Expect(os.WriteFile(path, []byte("tool Read call=call-1\n"), 0o600)).To(Succeed())

		out := r.cli(ctx, "logs", "millcli#21", "--log-dir", logDir)
		Expect(out).To(ContainSubstring("tool Read call=call-1"))
	})

	It("says which attempt has no log rather than printing nothing", func() {
		r := openRun(ctx, "millcli#22", nodeID(22))
		r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		out, err := runCLI(ctx, r.mill.socket, "logs", "millcli#22", "--step", "implement",
			"--log-dir", filepath.Join(GinkgoT().TempDir(), "empty"))
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("no log for implement attempt 1"))
	})

	// A node reference is what an operator has in front of them, and the
	// run it names has usually ended by the time they ask. A run that has
	// ended is on no step, and answering "the run is on no step" to a
	// question about the only run the node has is a tool refusing to read a
	// file it can name.
	It("reads the last step a finished run attempted, given the node's reference", func() {
		r := openRun(ctx, "millcli#24", nodeID(24))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		r.close(ctx, sr, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("b", 64)},
				VerifiedBy: "spec",
			},
		})

		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.StepLogPath(logDir, r.run.GetId(), "implement", sr.GetAttempt())
		Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
		Expect(os.WriteFile(path, []byte("text the implementer's last words\n"), 0o600)).To(Succeed())

		// The rest of quick-fix, closed: the run is on no step now, which
		// is the state every finished run is in.
		last := finish(ctx, r)
		lastPath := run.StepLogPath(logDir, r.run.GetId(), last, 1)
		Expect(os.WriteFile(lastPath, []byte("status merged\n"), 0o600)).To(Succeed())

		out := r.cli(ctx, "logs", "millcli#24", "--log-dir", logDir)

		Expect(out).To(ContainSubstring("status merged"))
		Expect(out).NotTo(ContainSubstring("is on no step"))
	})

	It("says the node has no run rather than printing nothing", func() {
		r := openRun(ctx, "millcli#25", nodeID(25))

		out, err := runCLI(ctx, r.mill.socket, "logs", "millcli#99")

		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("millcli#99"))
		Expect(out).To(ContainSubstring("no run"))
	})

	It("refuses a run it cannot find", func() {
		r := openRun(ctx, "millcli#23", nodeID(23))
		_, err := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: "millcli#404"}))
		Expect(err).To(HaveOccurred())

		out, err := runCLI(ctx, r.mill.socket, "logs", "millcli#404")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("millcli#404"))
	})
})

// finish closes the rest of quick-fix — commit, clean-tree, pr, merge — each
// on the evidence its step takes, and names the last step it closed. A run
// with every step closed is on no step, which is what a finished run looks
// like to the read tools.
func finish(ctx context.Context, r *runs) string {
	GinkgoHelper()
	for _, step := range []struct {
		name  string
		kind  millv1.StepKind
		value *millv1.Evidence
	}{
		{"commit", millv1.StepKind_STEP_KIND_SKILL, &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Commit{Commit: strings.Repeat("c", 40)},
		}},
		{"clean-tree", millv1.StepKind_STEP_KIND_CHECK, &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
			Value: &millv1.Evidence_Exit{Exit: processExit(0)},
		}},
		{"pr", millv1.StepKind_STEP_KIND_SKILL, &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_URL,
			Value: &millv1.Evidence_Url{Url: "https://gitlab.example.test/group/thing/-/merge_requests/1"},
		}},
		{"merge", millv1.StepKind_STEP_KIND_SKILL, &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_STATUS,
			Value: &millv1.Evidence_Status{Status: "merged"},
		}},
	} {
		step.value.VerifiedBy = "spec"
		r.close(ctx, r.start(ctx, step.name, step.kind), &millv1.CloseStepRunRequest{Evidence: step.value})
	}
	return "merge"
}
