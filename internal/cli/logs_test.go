package cli_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"connectrpc.com/connect"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// appendActivity writes one or more activity records to path, length-prefixed
// exactly as the writer does (internal/run/activity.go), so a spec can hand
// `mill logs` a fixture attempt without driving a real one through the
// worker.
func appendActivity(path string, activities ...*millv1.Activity) {
	GinkgoHelper()
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(file.Close()).To(Succeed()) }()
	for _, a := range activities {
		raw, err := proto.Marshal(a)
		Expect(err).NotTo(HaveOccurred())
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
		_, err = file.Write(header[:])
		Expect(err).NotTo(HaveOccurred())
		_, err = file.Write(raw)
		Expect(err).NotTo(HaveOccurred())
	}
}

// textActivity is a harness `text` record, the simplest kind that renders as
// one line: "text <content>".
func textActivity(text string) *millv1.Activity {
	return &millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
		Body:   &millv1.Activity_Text{Text: &millv1.Text{Text: text, Final: true}},
	}
}

var _ = Describe("mill logs", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("tails the running step's activity record and exits when the step run leaves running", func() {
		r := openRun(ctx, "millcli#20", nodeID(20))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		// The record as the worker writes it: length-prefixed, already
		// redacted by the time it lands (the writer redacts before it ever
		// marshals a record — there is no unredacted copy to re-read).
		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt())
		appendActivity(path, textActivity("reaching the forge with a masked value"))

		var stdout, stderr bytes.Buffer
		root := cli.NewRootCommand(&stdout, &stderr)
		root.SetArgs([]string{"logs", "millcli#20", "--step", "implement", "--follow",
			"--socket", r.mill.socket, "--log-dir", logDir})
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			done <- root.ExecuteContext(ctx)
		}()

		// The first record is tailed while the attempt is still running, and
		// the command does not return.
		Eventually(stdout.String, 10*time.Second, 50*time.Millisecond).
			Should(ContainSubstring("reaching the forge with a masked value"))
		Consistently(done, 500*time.Millisecond).ShouldNot(Receive())

		// A record written after the tail started reaches it too, and
		// closing the attempt ends the command.
		appendActivity(path, textActivity("more output, still masked"))
		r.close(ctx, sr, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + string(bytes.Repeat([]byte("d"), 64))},
				VerifiedBy: "spec",
			},
		})

		Eventually(done, 10*time.Second).Should(Receive(BeNil()), stderr.String())
		Expect(stdout.String()).To(ContainSubstring("more output, still masked"))
	})

	It("reads the step the run is on when no step is named", func() {
		r := openRun(ctx, "millcli#21", nodeID(21))
		sr := r.start(ctx, "gather-context", millv1.StepKind_STEP_KIND_SKILL)

		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.ActivityPath(logDir, r.run.GetId(), "gather-context", sr.GetAttempt())
		appendActivity(path, &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "Read"}},
		})

		out := r.cli(ctx, "logs", "millcli#21", "--log-dir", logDir)
		Expect(out).To(ContainSubstring("tool Read call=call-1"))
	})

	// The acceptance criterion's own wording: `mill logs` reads byte-identical
	// to the rendered text log it replaces (docs/activity.md's "Today"
	// column), for every kind that column says produced a line. One fixture
	// attempt carries one of each, and the expected text is built from the
	// exact formats the deleted internal/run/logs.go used to write.
	It("renders every activity kind byte-identical to the log format it replaces", func() {
		r := openRun(ctx, "millcli#26", nodeID(26))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		logDir := filepath.Join(GinkgoT().TempDir(), "logs")
		path := run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt())
		at := timestamppb.New(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
		stamp := at.AsTime().UTC().Format(time.RFC3339)

		appendActivity(path,
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Prompt{Prompt: &millv1.Prompt{Text: "do the thing\n"}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "thinking about it", Final: true}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "Read"}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1", Content: "file contents"}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Usage{Usage: &harnessv1.Usage{ByModel: map[string]*harnessv1.TokenCounts{
					"claude": {Input: 100, Output: 20},
				}}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Heartbeat{Heartbeat: &harnessv1.Heartbeat{
					Strike: 1, Strikes: 3, WaitingSeconds: 30, Tasks: []string{"task-a", "task-b"},
				}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
				Body: &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "warning: something\n"}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
				Body: &millv1.Activity_CheckStarted{CheckStarted: &millv1.CheckStarted{Command: []string{"make", "lint"}}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
				Body: &millv1.Activity_CheckOutput{CheckOutput: &millv1.Output{Stream: "stdout", Data: []byte("lint passed")}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
				Body: &millv1.Activity_CheckOutput{CheckOutput: &millv1.Output{Stream: "stderr", Data: []byte("no problems")}}},
			&millv1.Activity{At: at, Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
				Body: &millv1.Activity_Check{Check: &millv1.CheckResult{Exit: processExit(1), Reason: "lint failed"}}},
		)

		out := r.cli(ctx, "logs", "millcli#26", "--step", "implement", "--log-dir", logDir)

		expected := strings.Join([]string{
			fmt.Sprintf("%s prompt begin bytes=%d", stamp, len("do the thing\n")),
			"do the thing",
			fmt.Sprintf("%s prompt end", stamp),
			fmt.Sprintf("%s text thinking about it", stamp),
			fmt.Sprintf("%s tool Read call=call-1", stamp),
			fmt.Sprintf("%s result call=call-1 ok out=file contents", stamp),
			fmt.Sprintf("%s usage claude in=100 out=20", stamp),
			fmt.Sprintf("%s heartbeat 1/3 waiting=30s tasks=task-a; task-b", stamp),
			fmt.Sprintf("%s harness stderr begin bytes=%d", stamp, len("warning: something\n")),
			"warning: something",
			fmt.Sprintf("%s harness stderr end", stamp),
			fmt.Sprintf("%s check make lint begin", stamp),
			fmt.Sprintf("%s check stdout begin bytes=%d", stamp, len("lint passed")),
			"lint passed",
			fmt.Sprintf("%s check stdout end", stamp),
			fmt.Sprintf("%s check stderr begin bytes=%d", stamp, len("no problems")),
			"no problems",
			fmt.Sprintf("%s check stderr end", stamp),
			fmt.Sprintf("%s check exit status=1", stamp),
			fmt.Sprintf("%s check failed: lint failed", stamp),
			"",
		}, "\n")

		Expect(out).To(Equal(expected))
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
		path := run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt())
		appendActivity(path, textActivity("the implementer's last words"))

		// The rest of quick-fix, closed: the run is on no step now, which
		// is the state every finished run is in.
		last := finish(ctx, r)
		lastPath := run.ActivityPath(logDir, r.run.GetId(), last, 1)
		appendActivity(lastPath, textActivity("status merged"))

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
