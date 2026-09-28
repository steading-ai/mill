package run

// The activity writer, specified mostly against hand-built records for
// brevity, plus cases that translate real harness.v1.Event values through
// activityFromEvent where the translation itself — parent_call_id, block
// id/delta/final, redaction against a live stream — is what is under test.
// This file is in package run, not run_test, because activityWriter and
// decodeActivities are deliberately not exported: nothing outside this
// package writes or reads the file directly, only through ActivityPath and
// the CLI's renderer.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

var _ = Describe("The activity writer", Label("unit"), func() {
	var (
		dir string
		now func() time.Time
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		now = func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }
	})

	open := func() *activityWriter {
		GinkgoHelper()
		w, err := newActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(w).NotTo(BeNil())
		return w
	}

	read := func() []*millv1.Activity {
		GinkgoHelper()
		raw, err := os.ReadFile(ActivityPath(dir, "run-1", "gather-context", 1))
		Expect(err).NotTo(HaveOccurred())
		activities, err := decodeActivities(bytes.NewReader(raw))
		Expect(err).NotTo(HaveOccurred())
		return activities
	}

	It("shares a turn between a tool_call and the tool_result that answers it", func() {
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "read"}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1"}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(2))
		Expect(activities[0].GetTurn()).To(Equal(int64(1)))
		Expect(activities[1].GetTurn()).To(Equal(activities[0].GetTurn()))
	})

	It("takes an open call's turn from parent_call_id, regardless of the top-level turn counter", func() {
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "subagent"}},
		})).To(Succeed())
		// A second, unrelated top-level tool call moves currentTurn forward
		// after usage closes the first turn.
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1"}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Usage{Usage: &harnessv1.Usage{}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-2", Name: "other"}},
		})).To(Succeed())
		// A record from inside the subagent that call-1 started names it as
		// parent_call_id: it takes call-1's turn, not whatever currentTurn
		// is doing by now.
		Expect(w.Record(&millv1.Activity{
			Source:       millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			ParentCallId: "call-1",
			Body:         &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: "nested", Final: true}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(5))
		firstCallTurn := activities[0].GetTurn()
		secondCallTurn := activities[3].GetTurn()
		Expect(secondCallTurn).NotTo(Equal(firstCallTurn), "usage closed the first turn, so the next tool_call opens a new one")
		Expect(activities[4].GetTurn()).To(Equal(firstCallTurn), "the subagent record takes the call it nests under, not the current top-level turn")
	})

	It("closes the turn on usage, so the next text opens a new one", func() {
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: "one", Final: true}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Usage{Usage: &harnessv1.Usage{}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b2", Text: "two", Final: true}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(3))
		Expect(activities[2].GetTurn()).To(Equal(activities[0].GetTurn() + 1))
	})

	It("does not split the turn when usage arrives while a text block is still open", func() {
		// The stream's message_delta usage can land between a block's own
		// deltas and its closing final record (docs/activity.md §Turns): the
		// turn must not close until the block does, so every record through
		// the final shares one turn, and only a later usage lets the next
		// text open turn+1.
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: "one", Delta: true}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Usage{Usage: &harnessv1.Usage{}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Final: true}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Usage{Usage: &harnessv1.Usage{}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b2", Text: "two", Final: true}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(5))
		Expect(activities[1].GetTurn()).To(Equal(activities[0].GetTurn()), "usage mid-block takes the open turn")
		Expect(activities[2].GetTurn()).To(Equal(activities[0].GetTurn()), "the block's final record still shares the turn")
		Expect(activities[3].GetTurn()).To(Equal(activities[0].GetTurn()), "the deferred usage closes on the block's final, and itself takes that same turn")
		Expect(activities[4].GetTurn()).To(Equal(activities[0].GetTurn()+1), "only the later usage, after the block closed, lets the next text open a new turn")
	})

	It("gives turn 0 to a record with no session, whatever source it names", func() {
		w := open()
		for _, source := range []millv1.ActivitySource{
			millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
			millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
		} {
			Expect(w.Record(&millv1.Activity{
				Source: source,
				Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "x"}},
			})).To(Succeed())
		}
		Expect(w.Close()).To(Succeed())

		for _, a := range read() {
			Expect(a.GetTurn()).To(Equal(int64(0)))
		}
	})

	It("no-ops every method on a nil writer, touching no file", func() {
		var w *activityWriter
		Expect(w.Record(&millv1.Activity{})).To(Succeed())
		Expect(w.End(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, millv1.EvidenceKind_EVIDENCE_KIND_EXIT, "")).To(Succeed())
		Expect(w.Close()).To(Succeed())

		writer, err := newActivityWriter("", "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(writer).To(BeNil())
	})

	It("primes seq from the file's last record on reopen, so the next Record continues at seq 4", func() {
		w := open()
		for i := 0; i < 3; i++ {
			Expect(w.Record(&millv1.Activity{
				Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
				Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "x"}},
			})).To(Succeed())
		}
		Expect(w.Close()).To(Succeed())

		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(reopened.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "y"}},
		})).To(Succeed())
		Expect(reopened.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(4))
		Expect(activities[3].GetSeq()).To(Equal(int64(4)))
	})

	It("makes the file on reopen when the attempt has none, at seq 0", func() {
		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred(), "a gate filed on another host, or by a mill that wrote no record, still closes")
		Expect(reopened.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "y"}},
		})).To(Succeed())
		Expect(reopened.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(1))
		Expect(activities[0].GetSeq()).To(Equal(int64(1)))
	})

	It("cuts a torn record off the tail on reopen, so what follows still decodes", func() {
		w := open()
		for i := 0; i < 2; i++ {
			Expect(w.Record(&millv1.Activity{
				Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
				Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "x"}},
			})).To(Succeed())
		}
		Expect(w.Close()).To(Succeed())
		path := ActivityPath(dir, "run-1", "gather-context", 1)
		torn, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640)
		Expect(err).NotTo(HaveOccurred())
		// A header promising 100 bytes, and three of them: the writer died here.
		_, err = torn.Write([]byte{0, 0, 0, 100, 1, 2, 3})
		Expect(err).NotTo(HaveOccurred())
		Expect(torn.Close()).To(Succeed())

		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(reopened.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_HarnessStderr{HarnessStderr: &millv1.HarnessStderr{Text: "y"}},
		})).To(Succeed())
		Expect(reopened.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(3), "two whole records, the torn one gone, and the new one")
		Expect(activities[2].GetSeq()).To(Equal(int64(3)))
		Expect(activities[2].GetHarnessStderr().GetText()).To(Equal("y"))
	})

	It("round-trips a record's bytes through the length-prefixed decoder", func() {
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body:   &millv1.Activity_GateFiled{GateFiled: &millv1.GateFiled{Kind: "merge", NodeId: "n1", NodeRef: "mill#99", Title: "Ship it"}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(1))
		Expect(activities[0].GetGateFiled().GetNodeRef()).To(Equal("mill#99"))
		Expect(activities[0].GetSeq()).To(Equal(int64(1)))
	})

	It("ends with attempt_ended and closes the file", func() {
		w := open()
		Expect(w.End(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED, millv1.EvidenceKind_EVIDENCE_KIND_EXIT, "exit status 1")).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(1))
		ended := activities[0].GetAttemptEnded()
		Expect(ended).NotTo(BeNil())
		Expect(ended.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(ended.GetReason()).To(Equal("exit status 1"))

		// The file is really closed: a second write must fail rather than
		// silently succeed against a handle that looks open.
		_, err := w.file.WriteString("x")
		Expect(err).To(HaveOccurred())
	})

	It("truncates results and drops further stream output once the ceiling is crossed, keeping the file within it plus one record", func() {
		const ceiling = 100
		w, err := newActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, ceiling, nil)
		Expect(err).NotTo(HaveOccurred())

		large := strings.Repeat("x", 300)
		for i := 0; i < 5; i++ {
			Expect(w.Record(&millv1.Activity{
				Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{
					CallId: fmt.Sprintf("call-%d", i), Content: large,
				}},
			})).To(Succeed())
		}
		for i := 0; i < 5; i++ {
			Expect(w.Record(&millv1.Activity{
				Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
				Body: &millv1.Activity_CheckOutput{CheckOutput: &millv1.Output{
					Stream: "stdout", Data: []byte(large + "\n"),
				}},
			})).To(Succeed())
		}
		Expect(w.Close()).To(Succeed())

		var results, checkOutputs []*millv1.Activity
		for _, a := range read() {
			switch a.GetBody().(type) {
			case *millv1.Activity_ToolResult:
				results = append(results, a)
			case *millv1.Activity_CheckOutput:
				checkOutputs = append(checkOutputs, a)
			}
		}

		Expect(results).To(HaveLen(5), "every tool_result is kept, truncated or not")
		Expect(results[0].GetTruncated()).To(BeFalse(), "the record that first crosses the ceiling still lands whole")
		for _, r := range results[1:] {
			Expect(r.GetTruncated()).To(BeTrue())
			Expect(r.GetToolResult().GetContent()).To(Equal(summarize(large)))
		}

		Expect(checkOutputs).To(HaveLen(1), "one marker per stream; every chunk after it is dropped outright")
		Expect(checkOutputs[0].GetTruncated()).To(BeTrue())
		Expect(checkOutputs[0].GetCheckOutput().GetData()).To(BeEmpty())

		// Unbounded, five more 300-byte results and five more 300-byte chunks
		// would add well over 3000 bytes past the ceiling; truncation and the
		// dropped stream keep it far short of that.
		Expect(w.written).To(BeNumerically("<", 2000),
			"the ceiling bounds the file's growth once it is crossed, rather than letting every later record land whole")
	})

	It("keeps a several-hundred-KB tool result whole under the default ceiling", func() {
		w := open()
		large := strings.Repeat("x", 300*1024)
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1", Content: large}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(1))
		Expect(activities[0].GetTruncated()).To(BeFalse())
		Expect(activities[0].GetToolResult().GetContent()).To(Equal(large))
	})

	It("sanitizes a tool result carrying invalid UTF-8 instead of dropping the record", func() {
		w := open()
		dirty := "before\xff after"
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1", Content: dirty}},
		})).To(Succeed())
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(1))
		Expect(activities[0].GetSanitized()).To(BeTrue())
		content := activities[0].GetToolResult().GetContent()
		Expect(content).To(Equal("before� after"))
		Expect(utf8.ValidString(content)).To(BeTrue())

		rendered, err := RenderedLog(dir, "run-1", "gather-context", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(rendered).To(ContainSubstring("before� after"))
	})

	It("folds a subagent's turn under its Agent call, translated from real harness events", func() {
		w := open()
		record := func(event *harnessv1.Event) {
			Expect(w.Record(activityFromEvent(event))).To(Succeed())
		}

		record(&harnessv1.Event{Body: &harnessv1.Event_ToolCall{ToolCall: &harnessv1.ToolCall{CallId: "call-1", Name: "Agent"}}})
		// The subagent's own text carries call-1 as parent_call_id, exactly
		// as the sidecar sets it (docs/activity.md §Subagents), not built by
		// hand as [millv1.Activity].
		record(&harnessv1.Event{ParentCallId: "call-1", Body: &harnessv1.Event_Text{Text: &harnessv1.Text{BlockId: "b1", Text: "nested", Final: true}}})
		record(&harnessv1.Event{Body: &harnessv1.Event_ToolResult{ToolResult: &harnessv1.ToolResult{CallId: "call-1"}}})
		record(&harnessv1.Event{Body: &harnessv1.Event_Usage{Usage: &harnessv1.Usage{}}})
		Expect(w.Close()).To(Succeed())

		activities := read()
		Expect(activities).To(HaveLen(4))
		Expect(activities[0].GetParentCallId()).To(BeEmpty())
		Expect(activities[1].GetParentCallId()).To(Equal("call-1"))
		Expect(activities[1].GetTurn()).To(Equal(activities[0].GetTurn()), "the subagent record shares the turn of the call it nests under")
		Expect(activities[1].GetText().GetBlockId()).To(Equal("b1"))
	})

	It("keeps a value registered with redact absent from every delta written after it, translated from real harness events", func() {
		r := redact.New()
		w, err := newActivityWriter(dir, "run-1", "gather-context", 1, r, now, defaultActivityCeiling, nil)
		Expect(err).NotTo(HaveOccurred())

		record := func(event *harnessv1.Event) {
			Expect(w.Record(activityFromEvent(event))).To(Succeed())
		}

		record(&harnessv1.Event{Body: &harnessv1.Event_Text{Text: &harnessv1.Text{BlockId: "b1", Text: "the token is ", Delta: true}}})
		// Registered mid-block: only deltas written from here on owe it
		// protection (docs/activity.md §Redaction).
		r.Add("topsecret123")
		record(&harnessv1.Event{Body: &harnessv1.Event_Text{Text: &harnessv1.Text{BlockId: "b1", Text: "topsecret123", Delta: true}}})
		record(&harnessv1.Event{Body: &harnessv1.Event_Text{Text: &harnessv1.Text{BlockId: "b1", Text: " done", Final: true}}})
		Expect(w.Close()).To(Succeed())

		var whole strings.Builder
		for _, a := range read() {
			whole.WriteString(a.GetText().GetText())
		}
		Expect(whole.String()).NotTo(ContainSubstring("topsecret123"), "no delta written after the value was registered ever carries it")
		Expect(whole.String()).To(ContainSubstring(redact.Mask))
		Expect(whole.String()).To(HavePrefix("the token is "), "text already released before the value was registered is untouched")
	})

	It("keeps two concurrent verbs' partial lines on one stream from crossing", func() {
		w := open()
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			Body:   &millv1.Activity_VerbStarted{VerbStarted: &millv1.VerbStarted{Name: "git.a", VerbId: 1}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			Body:   &millv1.Activity_VerbStarted{VerbStarted: &millv1.VerbStarted{Name: "git.b", VerbId: 2}},
		})).To(Succeed())

		// Verb 1's read completes one line and starts a second, held back
		// with no newline; verb 2's read interleaves its own unterminated
		// line on the same stream before either verb's tail is flushed —
		// exactly the overlap two goroutines racing through Host.Run can
		// produce.
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			Body: &millv1.Activity_VerbOutput{VerbOutput: &millv1.Output{
				Stream: "stdout", Data: []byte("verb-1 line one\nverb-1 tail"), VerbId: 1,
			}},
		})).To(Succeed())
		Expect(w.Record(&millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			Body: &millv1.Activity_VerbOutput{VerbOutput: &millv1.Output{
				Stream: "stdout", Data: []byte("verb-2 tail"), VerbId: 2,
			}},
		})).To(Succeed())

		// Ending verb 1 first flushes only its own held-back tail, never
		// verb 2's, and never glues the two together; verb 2's own end
		// flushes its own tail afterward.
		Expect(w.EndVerbOutput(1, "stdout")).To(Succeed())
		Expect(w.EndVerbOutput(2, "stdout")).To(Succeed())
		Expect(w.Close()).To(Succeed())

		var outputs []*millv1.Output
		for _, a := range read() {
			if out := a.GetVerbOutput(); out != nil {
				outputs = append(outputs, out)
			}
		}
		Expect(outputs).To(HaveLen(3), "verb 1's whole first line, verb 1's flushed tail, then verb 2's flushed tail")
		Expect(outputs[0]).To(Equal(&millv1.Output{Stream: "stdout", Data: []byte("verb-1 line one\n"), VerbId: 1}))
		Expect(outputs[1]).To(Equal(&millv1.Output{Stream: "stdout", Data: []byte("verb-1 tail"), VerbId: 1}),
			"verb 1's own tail, not glued onto verb 2's")
		Expect(outputs[2]).To(Equal(&millv1.Output{Stream: "stdout", Data: []byte("verb-2 tail"), VerbId: 2}),
			"verb 2's tail survives, attributed to verb 2, even though verb 1 ended first")
	})

	It("names the file under the run, the step and the attempt", func() {
		Expect(ActivityPath(dir, "run-1", "gather-context", 2)).
			To(Equal(filepath.Join(dir, "run-1", "gather-context.2.activity")))
	})
})

var _ = Describe("ActivityRenderer", Label("unit"), func() {
	at := timestamppb.New(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	stamp := at.AsTime().UTC().Format(time.RFC3339)

	textDelta := func(blockID, text string, final bool) *millv1.Activity {
		return &millv1.Activity{At: at, Body: &millv1.Activity_Text{Text: &millv1.Text{
			BlockId: blockID, Text: text, Delta: !final, Final: final,
		}}}
	}
	thinkingDelta := func(blockID, text string, final bool) *millv1.Activity {
		return &millv1.Activity{At: at, Body: &millv1.Activity_Thinking{Thinking: &millv1.Thinking{
			BlockId: blockID, Text: text, Delta: !final, Final: final,
		}}}
	}

	It("holds a text block's deltas and renders one line, whole, on its final record", func() {
		r := NewActivityRenderer()
		Expect(r.Render(textDelta("b1", "Hello", false))).To(Equal(""))
		Expect(r.Render(textDelta("b1", ", world", false))).To(Equal(""))
		Expect(r.Render(textDelta("b1", "!", true))).To(Equal(fmt.Sprintf("%s text Hello, world!\n", stamp)))
	})

	It("holds a thinking block's deltas and renders its word count on its final record", func() {
		r := NewActivityRenderer()
		Expect(r.Render(thinkingDelta("b1", "weighing ", false))).To(Equal(""))
		Expect(r.Render(thinkingDelta("b1", "the options", false))).To(Equal(""))
		Expect(r.Render(thinkingDelta("b1", "", true))).To(Equal(fmt.Sprintf("%s thinking 3 words\n", stamp)))
	})

	It("accumulates two interleaved blocks independently", func() {
		r := NewActivityRenderer()
		Expect(r.Render(textDelta("b1", "one", false))).To(Equal(""))
		Expect(r.Render(textDelta("b2", "two", false))).To(Equal(""))
		Expect(r.Render(textDelta("b1", "-more", true))).To(Equal(fmt.Sprintf("%s text one-more\n", stamp)))
		Expect(r.Render(textDelta("b2", "-more", true))).To(Equal(fmt.Sprintf("%s text two-more\n", stamp)))
	})

	It("renders a whole, non-streamed final record the same as if no deltas ever existed", func() {
		r := NewActivityRenderer()
		Expect(r.Render(textDelta("", "no deltas preceded this", true))).
			To(Equal(fmt.Sprintf("%s text no deltas preceded this\n", stamp)))
	})

	It("delegates every other kind to RenderActivity unchanged", func() {
		r := NewActivityRenderer()
		a := &millv1.Activity{At: at, Body: &millv1.Activity_ToolCall{ToolCall: &millv1.ToolCall{CallId: "call-1", Name: "Read"}}}
		Expect(r.Render(a)).To(Equal(RenderActivity(a)))
	})

	It("renders a compacted record's stored text verbatim, with no added stamp, since each line already carries its own", func() {
		text := fmt.Sprintf("-- build attempt 1 started --\n%s text Hello, world!\n-- build attempt 1 ended: exit status 0 --\n", stamp)
		a := &millv1.Activity{At: at, Body: &millv1.Activity_Compacted{Compacted: &millv1.Compacted{Text: text}}}
		Expect(RenderActivity(a)).To(Equal(text))
	})
})

var _ = Describe("activityLogFields", Label("unit"), func() {
	It("names the kind and the call it answered, for a tool_result", func() {
		a := &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_ToolResult{ToolResult: &millv1.ToolResult{CallId: "call-1"}},
		}
		Expect(activityLogFields(a)).To(Equal([]any{
			"source", "ACTIVITY_SOURCE_HARNESS",
			"kind", "tool_result",
			"call_id", "call-1",
		}))
	})

	It("names only the kind for a record with no call to name", func() {
		a := &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body:   &millv1.Activity_Text{Text: &millv1.Text{Text: "hello"}},
		}
		Expect(activityLogFields(a)).To(Equal([]any{
			"source", "ACTIVITY_SOURCE_HARNESS",
			"kind", "text",
		}))
	})
})
