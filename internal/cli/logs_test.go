package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"connectrpc.com/connect"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/cli"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// appendActivity and textActivity are internal/testsupport's, shared with
// internal/server's Logs RPC specs so both build the same fixture attempts.
var (
	appendActivity = testsupport.AppendActivity
	textActivity   = testsupport.TextActivity
)

// gateFiled is a gate_filed fixture record, the one activity kind that
// populates RunEnd.Gate (internal/server/logs.go's gateRef: built from the
// record already on file, not a fresh call to skift). It is written to the
// attempt's own activity file before the ledger write that signals the
// observer, exactly as a worker's own gate step would, so a follow opened
// either before or after that write sees it.
func gateFiled(kind, nodeID, nodeRef, title string) *millv1.Activity {
	return &millv1.Activity{Body: &millv1.Activity_GateFiled{GateFiled: &millv1.GateFiled{
		Kind: kind, NodeId: nodeID, NodeRef: nodeRef, Title: title,
	}}}
}

var _ = Describe("mill logs", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	// Acceptance criterion: from a host with no log directory, `mill logs`
	// against a socket whose worker is elsewhere prints every attempt with
	// its boundaries and exits 0. The command takes no --log-dir at all any
	// more — the RPC answers from whichever host holds the record — so this
	// spec's own process holds none either.
	It("prints every attempt with its boundaries and exits 0", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#20", nodeID(20))

		implement := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", implement.GetAttempt()),
			&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{
				Step: "implement", Attempt: implement.GetAttempt(),
			}}},
			textActivity("doing the work"),
			&millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{
				Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, Reason: "closed",
			}}},
		)
		r.close(ctx, implement, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("a", 64)},
				VerifiedBy: "spec",
			},
		})

		commit := r.start(ctx, "commit", millv1.StepKind_STEP_KIND_SKILL)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "commit", commit.GetAttempt()),
			&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{
				Step: "commit", Attempt: commit.GetAttempt(),
			}}},
			textActivity("committing"),
		)

		out := r.cli(ctx, "logs", "millcli#20", "--no-pager")
		Expect(out).To(ContainSubstring("-- implement attempt 1 started --"))
		Expect(out).To(ContainSubstring("doing the work"))
		Expect(out).To(ContainSubstring("-- implement attempt 1 ended: closed --"))
		Expect(out).To(ContainSubstring("-- commit attempt 1 started --"))
		Expect(out).To(ContainSubstring("committing"))
	})

	// Acceptance criterion and the Gherkin scenario "One tail for the whole
	// run": -f prints the last --tail records, follows across a step change
	// with no gap, and ends with "-- run done --" and exit 0 once the run
	// completes.
	It("follows across a step change and ends the stream when the run finishes", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#30", nodeID(30))

		implement := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		path := run.ActivityPath(logDir, r.run.GetId(), "implement", implement.GetAttempt())
		appendActivity(path, &millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{
			Step: "implement", Attempt: implement.GetAttempt(),
		}}})
		for i := 1; i <= 12; i++ {
			appendActivity(path, textActivity(fmt.Sprintf("line-%d", i)))
		}

		out, stderr, done := runLogsFollow(ctx, m, "millcli#30")

		// --tail defaults to 10: the boundary plus 12 lines is 13 records,
		// so only the last 10 (line-3 through line-12) are there to start.
		Eventually(out.String, 10*time.Second, 50*time.Millisecond).Should(ContainSubstring("line-12"))
		Expect(out.String()).NotTo(ContainSubstring("line-1\n"))
		Expect(out.String()).NotTo(ContainSubstring("line-2\n"))
		Expect(out.String()).To(ContainSubstring("line-3\n"))
		Consistently(done, 500*time.Millisecond).ShouldNot(Receive())

		appendActivity(path, &millv1.Activity{Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{
			Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, Reason: "closed",
		}}})
		r.close(ctx, implement, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("b", 64)},
				VerifiedBy: "spec",
			},
		})
		Eventually(out.String, 10*time.Second, 50*time.Millisecond).
			Should(ContainSubstring("-- implement attempt 1 ended: closed --"))

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "commit", 1),
			&millv1.Activity{Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "commit", Attempt: 1}}})
		r.start(ctx, "commit", millv1.StepKind_STEP_KIND_SKILL)
		Eventually(out.String, 10*time.Second, 50*time.Millisecond).
			Should(ContainSubstring("-- commit attempt 1 started --"))

		_, err := r.store.Done(ctx, connectRequest(&millv1.DoneRequest{LeaseId: r.lease, RunId: r.run.GetId()}))
		Expect(err).NotTo(HaveOccurred())

		var runErr error
		Eventually(done, 10*time.Second).Should(Receive(&runErr), stderr.String())
		Expect(runErr).NotTo(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("-- run done --"))
	})

	// Acceptance criterion: a run that files a gate ends a followed stream
	// naming the gate and exits 3; a parked run exits 4. The gate_filed
	// fixture and the ledger write that signals it (wait/park) both happen
	// before the follow opens, which internal/server/logs.go's
	// logsFollowOneRun reads on its very first pass — no race to synchronize
	// around at this level; that ordering is already proven server-side
	// (internal/server/logs_test.go's own gate/parked specs).
	It("names the gate and exits 3 when the run waits on one", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#40", nodeID(40))

		gate := r.start(ctx, "approve", millv1.StepKind_STEP_KIND_GATE)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "approve", gate.GetAttempt()),
			gateFiled("merge", "gate-1", "millcli#40/approve", "Approve millcli#40"))
		r.wait(ctx, "gate-1", gate)

		out, err := m.try(ctx, "logs", "millcli#40", "--follow", "--no-pager")
		Expect(cli.ExitCode(err)).To(Equal(cli.ExitLogsWaiting))
		Expect(out).To(ContainSubstring("-- waiting on gate millcli#40/approve --"))
	})

	It("names the gate and exits 4 when the run is parked on one", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#41", nodeID(41))

		gate := r.start(ctx, "approve", millv1.StepKind_STEP_KIND_GATE)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "approve", gate.GetAttempt()),
			gateFiled("park", "gate-2", "millcli#41/approve", ""))
		r.park(ctx, "gate-2")

		out, err := m.try(ctx, "logs", "millcli#41", "--follow", "--no-pager")
		Expect(cli.ExitCode(err)).To(Equal(cli.ExitLogsParked))
		Expect(out).To(ContainSubstring("-- parked on gate millcli#41/approve --"))
	})

	// The acceptance criterion's own wording: `mill logs` reads byte-identical
	// to the rendered text log it replaces (docs/activity.md's "Today"
	// column), for every kind that column says produced a line. One fixture
	// attempt carries one of each, and the expected text is built from the
	// exact formats the deleted internal/run/logs.go used to write.
	It("renders every activity kind byte-identical to the log format it replaces", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#26", nodeID(26))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

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

		out := r.cli(ctx, "logs", "millcli#26", "--step", "implement", "--no-pager")

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

	// A text block that streamed as deltas reads two different ways: --follow
	// shows the pieces raw, as they arrived; without it, `mill logs` shows the
	// one line the block would have rendered as if it had arrived whole
	// (docs/activity.md §"Streaming text").
	It("shows a streamed block's pieces raw under --follow, and one accumulated line without it", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#28", nodeID(28))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt()),
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: "Hello", Delta: true}}},
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: ", world!", Final: true}}},
		)
		r.close(ctx, sr, &millv1.CloseStepRunRequest{
			Evidence: &millv1.Evidence{
				Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
				Value:      &millv1.Evidence_Artifact{Artifact: "sha256:" + strings.Repeat("e", 64)},
				VerifiedBy: "spec",
			},
		})

		// A step-scoped follow over an attempt that has already left running
		// replays the file and ends with no RunEnd (docs/activity.md §The RPC).
		followed := r.cli(ctx, "logs", "millcli#28", "--step", "implement", "--follow", "--no-pager")
		Expect(followed).To(Equal("Hello, world!"), "the deltas print raw, with no summary line")

		static := r.cli(ctx, "logs", "millcli#28", "--step", "implement", "--no-pager")
		Expect(static).To(ContainSubstring("text Hello, world!\n"), "the whole block renders as one accumulated line")
		Expect(static).NotTo(ContainSubstring("Hello\n"), "no line for the delta alone")
	})

	// A thinking block renders as a word count, the same shape a Text block
	// gets, once its final record arrives (docs/activity.md §"Streaming
	// text").
	It("renders a thinking block's word count once its final record arrives", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#29", nodeID(29))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt()),
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Thinking{Thinking: &millv1.Thinking{BlockId: "b1", Text: "weighing the ", Delta: true}}},
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Thinking{Thinking: &millv1.Thinking{BlockId: "b1", Text: "options", Final: true}}},
		)

		out := r.cli(ctx, "logs", "millcli#29", "--step", "implement", "--no-pager")
		Expect(out).To(ContainSubstring("thinking 3 words\n"))
	})

	It("emits one Activity JSON line per record with --json", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#27", nodeID(27))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt()), textActivity("json line"))

		out := r.cli(ctx, "logs", "millcli#27", "--step", "implement", "--json", "--no-pager")

		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		Expect(lines).To(HaveLen(1))
		var got millv1.Activity
		Expect(protojson.Unmarshal([]byte(lines[0]), &got)).To(Succeed())
		Expect(got.GetText().GetText()).To(Equal("json line"), "the line round-trips as the record it was")
	})

	// Acceptance criterion 5, end to end through the real CLI binary path:
	// `mill logs` with no run merges every run's activity, oldest first,
	// each line prefixed with the run it belongs to — including a body
	// record's own content lines, not only its boundary lines.
	It("prints every run's activity with --since, oldest first, each line prefixed with its run", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#50", nodeID(50))
		g := openRunOn(ctx, m, "millcli#51", nodeID(51))

		now := time.Now().UTC()
		rAt, gAt := timestamppb.New(now), timestamppb.New(now.Add(time.Second))

		implementR := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", implementR.GetAttempt()),
			&millv1.Activity{At: rAt, Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{
				Step: "implement", Attempt: implementR.GetAttempt(),
			}}},
			&millv1.Activity{At: rAt, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "r's work", Final: true}}},
		)

		implementG := g.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		appendActivity(run.ActivityPath(logDir, g.run.GetId(), "implement", implementG.GetAttempt()),
			&millv1.Activity{At: gAt, Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{
				Step: "implement", Attempt: implementG.GetAttempt(),
			}}},
			&millv1.Activity{At: gAt, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "g's work", Final: true}}},
		)

		out := r.cli(ctx, "logs", "--since", "1h", "--no-pager")

		Expect(out).To(ContainSubstring("millcli#50 -- implement attempt 1 started --\n"))
		Expect(out).To(ContainSubstring("millcli#51 -- implement attempt 1 started --\n"))
		Expect(out).To(MatchRegexp(`millcli#50 \S+ text r's work\n`))
		Expect(out).To(MatchRegexp(`millcli#51 \S+ text g's work\n`))

		rIdx := strings.Index(out, "millcli#50")
		gIdx := strings.Index(out, "millcli#51")
		Expect(rIdx).To(BeNumerically(">=", 0))
		Expect(gIdx).To(BeNumerically(">", rIdx), "r's records were stamped before g's, so they sort first")
	})

	// The -f side of acceptance criterion 5: with no run, the follow merges
	// both runs' new records as they land and never ends on its own; the
	// CLI itself ends it on ctrl-C, which for a spec is the context it
	// handed ExecuteContext being canceled (internal/cli/logs.go's
	// signal.NotifyContext wraps exactly that context, so the two are the
	// same event from the command's point of view).
	It("follows every run with no run named, printing both as they're written, and exits cleanly on ctrl-C", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#52", nodeID(52))
		g := openRunOn(ctx, m, "millcli#53", nodeID(53))

		implementR := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		implementG := g.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		followCtx, stopFollow := context.WithCancel(ctx)
		out, stderr, done := runLogsEveryRun(followCtx, m, "--since", "5m")

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", implementR.GetAttempt()),
			&millv1.Activity{At: timestamppb.New(time.Now().UTC()), Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "r live", Final: true}}})
		appendActivity(run.ActivityPath(logDir, g.run.GetId(), "implement", implementG.GetAttempt()),
			&millv1.Activity{At: timestamppb.New(time.Now().UTC()), Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "g live", Final: true}}})

		Eventually(out.String, 10*time.Second, 50*time.Millisecond).Should(ContainSubstring("r live"))
		Eventually(out.String, 10*time.Second, 50*time.Millisecond).Should(ContainSubstring("g live"))
		Consistently(done, 500*time.Millisecond).ShouldNot(Receive())

		stopFollow()
		var followErr error
		Eventually(done, 10*time.Second).Should(Receive(&followErr), stderr.String())
		Expect(followErr).NotTo(HaveOccurred())
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

// runLogsFollow starts `mill logs -f` against m in the background and
// returns its stdout and stderr buffers, live as the command writes to
// them, and the channel its exit error lands on.
func runLogsFollow(ctx context.Context, m mill, reference string) (*bytes.Buffer, *bytes.Buffer, chan error) {
	GinkgoHelper()
	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs([]string{"logs", reference, "--follow", "--socket", m.socket})
	done := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		done <- root.ExecuteContext(ctx)
	}()
	return &stdout, &stderr, done
}

// runLogsEveryRun starts `mill logs -f` with no run named against m in the
// background — the every-run follow that never ends on its own — and
// returns its stdout and stderr buffers, live as the command writes to
// them, and the channel its exit error lands on. Canceling ctx is this
// helper's ctrl-C: internal/cli/logs.go wraps the very context ExecuteContext
// is given with signal.NotifyContext, so the two are indistinguishable from
// the command's side.
func runLogsEveryRun(ctx context.Context, m mill, extra ...string) (*bytes.Buffer, *bytes.Buffer, chan error) {
	GinkgoHelper()
	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs(append([]string{"logs", "--follow", "--socket", m.socket}, extra...))
	done := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		done <- root.ExecuteContext(ctx)
	}()
	return &stdout, &stderr, done
}
