package run

// The activity writer, specified directly against hand-built records rather
// than a real harness event: harness.v1.Event carries no parent_call_id yet
// (deferred to a later node), so the turn rule's subagent branch can only be
// proven by calling Record with one set by hand. This file is in package
// run, not run_test, because activityWriter and decodeActivities are
// deliberately not exported: nothing outside this package writes or reads
// the file directly, only through ActivityPath and the CLI's renderer.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace/redact"
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
		w, err := newActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling)
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

		writer, err := newActivityWriter("", "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling)
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

		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling)
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
		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling)
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

		reopened, err := reopenActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, defaultActivityCeiling)
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
		w, err := newActivityWriter(dir, "run-1", "gather-context", 1, redact.New(), now, ceiling)
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

	It("names the file under the run, the step and the attempt", func() {
		Expect(ActivityPath(dir, "run-1", "gather-context", 2)).
			To(Equal(filepath.Join(dir, "run-1", "gather-context.2.activity")))
	})
})
