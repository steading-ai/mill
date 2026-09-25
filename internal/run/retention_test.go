package run

// Retention's policy parsing and frame compaction, specified against
// hand-built records for the same reason activity_test.go is: this file is
// in package run, not run_test, because renderCompacted and compactFrame
// are deliberately not exported — nothing outside this package compacts a
// frame directly, only through the sweep (sweep_test.go, package run_test,
// covers that end to end against a real store).

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

var _ = Describe("ParsePolicy", Label("unit"), func() {
	DescribeTable("parses a retention value",
		func(value string, want Policy) {
			got, err := ParsePolicy(value)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("empty is keep", "", Policy{}),
		Entry("keep is keep", "keep", Policy{}),
		Entry("twin_days alone", "twin_days=7", Policy{TwinDays: 7}),
		Entry("compact_days alone", "compact_days=30", Policy{CompactDays: 30}),
		Entry("both, comma-separated", "twin_days=7,compact_days=30", Policy{TwinDays: 7, CompactDays: 30}),
	)

	DescribeTable("rejects a bad retention value",
		func(value string) {
			_, err := ParsePolicy(value)
			Expect(err).To(HaveOccurred())
		},
		Entry("unknown key", "frame_days=7"),
		Entry("not key=value", "twin_days"),
		Entry("zero is not a duration", "twin_days=0"),
		Entry("negative", "twin_days=-1"),
		Entry("not a number", "twin_days=soon"),
	)
})

// compactionFixture is one attempt's worth of records: attempt_started, a
// prompt, a final text block, and attempt_ended — enough to exercise
// renderCompacted's two marker cases plus one line it defers to
// ActivityRenderer.Render for.
func compactionFixture(at time.Time) []*millv1.Activity {
	ts := timestamppb.New(at)
	return []*millv1.Activity{
		{Seq: 1, At: ts, Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body: &millv1.Activity_AttemptStarted{AttemptStarted: &millv1.AttemptStarted{Step: "implement", Attempt: 1}}},
		{Seq: 2, At: ts, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
			Body: &millv1.Activity_Prompt{Prompt: &millv1.Prompt{Text: "do the thing"}}},
		{Seq: 3, At: ts, Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS, Turn: 1,
			Body: &millv1.Activity_Text{Text: &millv1.Text{Text: "done", Final: true}}},
		{Seq: 4, At: ts, Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
			Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{
				Status: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED, EvidenceKind: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT, Reason: "exit status 0"}}},
	}
}

var _ = Describe("renderCompacted", Label("unit"), func() {
	It("renders the same marker lines mill logs' own renderer does, around the session's rendered lines", func() {
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		activities := compactionFixture(at)
		stamp := at.UTC().Format(time.RFC3339)

		// logsRenderer's own marker format (internal/cli/logs.go:433,436),
		// reproduced literally here since it cannot be called from this
		// package (docs/plans/2026-09-21-mill-17.md Decisions) — this is
		// the pin against drift the plan calls for.
		want := "-- implement attempt 1 started --\n" +
			RenderActivity(activities[1]) +
			fmt.Sprintf("%s text done\n", stamp) +
			"-- implement attempt 1 ended: exit status 0 --\n"

		Expect(renderCompacted(activities)).To(Equal(want))
	})
})

var _ = Describe("compactFrame", Label("unit"), func() {
	var dir, path, stepRunID string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		path = filepath.Join(dir, "implement.1.activity.zst")
		stepRunID = "step-run-1"
	})

	writeFixture := func(at time.Time) []*millv1.Activity {
		GinkgoHelper()
		activities := compactionFixture(at)
		_, err := writeFrame(path, stepRunID, activities)
		Expect(err).NotTo(HaveOccurred())
		return activities
	}

	It("rewrites the frame to one truncated record holding the same lines mill logs prints", func() {
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		activities := writeFixture(at)
		want := renderCompacted(activities)

		changed, err := compactFrame(path, stepRunID)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())

		after, err := ReadFrame(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(1))
		Expect(after[0].GetTruncated()).To(BeTrue())
		compacted, ok := after[0].GetBody().(*millv1.Activity_Compacted)
		Expect(ok).To(BeTrue(), "the one record left is the compacted kind")
		Expect(compacted.Compacted.GetText()).To(Equal(want))
	})

	It("is idempotent: a second compaction changes nothing", func() {
		at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		writeFixture(at)

		_, err := compactFrame(path, stepRunID)
		Expect(err).NotTo(HaveOccurred())
		before, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())

		changed, err := compactFrame(path, stepRunID)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeFalse())

		after, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before), "a no-op compaction leaves the file byte-identical")
	})
})
