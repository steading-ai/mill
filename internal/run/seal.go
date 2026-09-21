package run

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	"github.com/klauspost/compress/zstd"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/workspace"
)

// Sealing turns a closed attempt's live activity file (activity.go) into its
// durable form (docs/activity.md's "Seal"): a coalesced, zstd-compressed
// frame, one ActivityIndexRow per record recorded through the store, and —
// when the attempt ran a session — a twin of the harness transcript
// alongside it. The live file is removed only once every one of those has
// succeeded, so a seal that fails partway leaves exactly the live file it
// started with: the signal that it still needs sealing, and safe to retry,
// since both the frame write and the store insert are idempotent.

// sealCompressionLevel is "level 3" (docs/activity.md, the plan this file
// implements): [zstd.SpeedDefault]'s own doc names it "roughly equivalent to
// the default Zstandard mode (level 3)."
const sealCompressionLevel = zstd.SpeedDefault

// FramePath is where one attempt's sealed frame lives under dir, once
// [SealAttempt] has run. Named for [ActivityPath] plus the outer
// compression, so a reader that already has one path can find the other.
func FramePath(dir, runID, step string, attempt int64) string {
	return ActivityPath(dir, runID, step, attempt) + ".zst"
}

// TwinPath is where one attempt's captured harness transcript lives under
// dir, once [SealAttempt] has run one. Nothing decodes it (docs/activity.md);
// it exists so a transcript is not lost when its session's workspace is torn
// down.
func TwinPath(dir, runID, step string, attempt int64) string {
	return filepath.Join(dir, runID, fmt.Sprintf("%s.%d.twin.tar.zst", step, attempt))
}

// SealAttempt closes a finished attempt's live activity file into its sealed
// form: coalesce, write and verify the frame, record its index rows through
// store, capture the twin when the attempt ran a session, and only then
// remove the live file. An attempt with no live file — [dir] empty, or one
// already sealed by an earlier, uncompleted attempt at this — is a no-op,
// not an error: the caller does not need to know whether this is the first
// try.
//
// Exported, unlike most of this package's attempt-lifecycle machinery,
// because it is the one piece a spec needs to drive directly against a real
// store to prove the round trip end to end — the same reason [ActivityPath]
// and [ReadActivities] already are.
func SealAttempt(ctx context.Context, dir, runID, step string, attempt int64, stepRunID, sessionID string, store millv1connect.StoreServiceClient, twin workspace.TwinSource) error {
	path := ActivityPath(dir, runID, step, attempt)
	activities, err := ReadActivities(dir, runID, step, attempt)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("seal %s: %w", path, err)
	}

	sealed := coalesceActivities(activities)
	renumberSeq(sealed)

	framePath := FramePath(dir, runID, step, attempt)
	rows, err := writeFrame(framePath, stepRunID, sealed)
	if err != nil {
		return fmt.Errorf("seal %s: %w", path, err)
	}
	if err := verifyFrame(framePath, sealed); err != nil {
		return fmt.Errorf("seal %s: %w", path, err)
	}

	if _, err := call(ctx, store.SealAttempt, &millv1.SealAttemptRequest{
		StepRunId: stepRunID,
		Records:   rows,
	}); err != nil {
		return fmt.Errorf("seal %s: %w", path, err)
	}

	if twin != nil {
		if err := writeTwin(ctx, TwinPath(dir, runID, step, attempt), twin, sessionID); err != nil {
			return fmt.Errorf("seal %s: %w", path, err)
		}
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("seal %s: %w", path, err)
	}
	return nil
}

// writeFrame zstd-encodes activities, length-prefixed exactly as the live
// file already is (activityLengthPrefix), to path.tmp and renames it into
// place — so a crash mid-write never leaves a half-written file that could
// be mistaken for a sealed one. It returns one ActivityIndexRow per record,
// offset set to that record's byte position in the uncompressed frame.
func writeFrame(path, stepRunID string, activities []*millv1.Activity) ([]*millv1.ActivityIndexRow, error) {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, fmt.Errorf("write frame: %w", err)
	}
	enc, err := zstd.NewWriter(file, zstd.WithEncoderLevel(sealCompressionLevel))
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write frame: %w", err)
	}

	rows := make([]*millv1.ActivityIndexRow, 0, len(activities))
	var offset int64
	for _, a := range activities {
		raw, err := proto.Marshal(a)
		if err != nil {
			_ = enc.Close()
			_ = file.Close()
			return nil, fmt.Errorf("write frame: marshal: %w", err)
		}
		var header [activityLengthPrefix]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
		if _, err := enc.Write(header[:]); err != nil {
			_ = enc.Close()
			_ = file.Close()
			return nil, fmt.Errorf("write frame: %w", err)
		}
		if _, err := enc.Write(raw); err != nil {
			_ = enc.Close()
			_ = file.Close()
			return nil, fmt.Errorf("write frame: %w", err)
		}
		rows = append(rows, activityIndexRow(stepRunID, a, offset))
		offset += int64(activityLengthPrefix + len(raw))
	}

	if err := enc.Close(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write frame: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("write frame: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("write frame: %w", err)
	}
	return rows, nil
}

// ReadFrame decompresses a sealed frame at path, previously written by
// [SealAttempt], back into the records it holds.
func ReadFrame(path string) ([]*millv1.Activity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read frame: %w", err)
	}
	defer func() { _ = file.Close() }()
	dec, err := zstd.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read frame: %w", err)
	}
	defer dec.Close()
	activities, err := decodeActivities(dec)
	if err != nil {
		return nil, fmt.Errorf("read frame: %w", err)
	}
	return activities, nil
}

// ReadSealedActivities reads one attempt's records whether or not it has
// been sealed yet: the live file when it still exists, its frame
// ([ReadFrame]) once [SealAttempt] has replaced it. Logs (internal/server/
// logs.go) and any caller that does not itself track whether an attempt has
// sealed reads through this rather than [ReadActivities] directly.
func ReadSealedActivities(dir, runID, step string, attempt int64) ([]*millv1.Activity, error) {
	activities, err := ReadActivities(dir, runID, step, attempt)
	if err == nil {
		return activities, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return ReadFrame(FramePath(dir, runID, step, attempt))
}

// verifyFrame decompresses path and compares it record-for-record against
// want — what writeFrame was just given — refusing to let a corrupt write
// pass as sealed.
func verifyFrame(path string, want []*millv1.Activity) error {
	got, err := ReadFrame(path)
	if err != nil {
		return fmt.Errorf("verify frame: %w", err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("verify frame: wrote %d records, read back %d", len(want), len(got))
	}
	for i := range got {
		if !proto.Equal(got[i], want[i]) {
			return fmt.Errorf("verify frame: record %d does not match what was written", i)
		}
	}
	return nil
}

// writeTwin captures sessionID's harness transcript through twin,
// zstd-compresses it, and writes it to path.tmp then renames it into place —
// the same write-then-rename discipline writeFrame uses.
func writeTwin(ctx context.Context, path string, twin workspace.TwinSource, sessionID string) error {
	r, err := twin.CaptureTwin(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("write twin: %w", err)
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("write twin: %w", err)
	}
	enc, err := zstd.NewWriter(file, zstd.WithEncoderLevel(sealCompressionLevel))
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("write twin: %w", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		_ = enc.Close()
		_ = file.Close()
		return fmt.Errorf("write twin: %w", err)
	}
	if err := enc.Close(); err != nil {
		_ = file.Close()
		return fmt.Errorf("write twin: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write twin: %w", err)
	}
	return os.Rename(tmp, path)
}

// activityIndexRow builds one sealed record's index row: everything a query
// needs to find and page it by, and none of its body — Logs reads the body
// back from the frame at offset.
func activityIndexRow(stepRunID string, a *millv1.Activity, offset int64) *millv1.ActivityIndexRow {
	row := &millv1.ActivityIndexRow{
		StepRunId:    stepRunID,
		Seq:          a.GetSeq(),
		At:           a.GetAt(),
		EndedAt:      a.GetEndedAt(),
		Kind:         activityKind(a),
		Source:       a.GetSource(),
		Turn:         a.GetTurn(),
		ParentCallId: a.GetParentCallId(),
		Bytes:        a.GetBytes(),
		Truncated:    a.GetTruncated(),
		Offset:       offset,
	}
	switch body := a.GetBody().(type) {
	case *millv1.Activity_ToolCall:
		row.CallId = body.ToolCall.GetCallId()
		row.Tool = body.ToolCall.GetName()
	case *millv1.Activity_ToolResult:
		row.CallId = body.ToolResult.GetCallId()
	}
	return row
}

// activityKind is a's body oneof field name, e.g. "tool_call" — the same
// resolution bodySize already does by protoreflect (activity.go), reused
// here so ActivityIndexRow's kind never drifts from what the oneof actually
// holds.
func activityKind(a *millv1.Activity) string {
	field := activityBodyField(a)
	if field == nil {
		return ""
	}
	return string(field.Name())
}

// coalesceActivities groups consecutive Text/Thinking records that share
// source, turn and block_id into one, concatenating their text in seq order,
// and never merges across a Final:true record, which always closes its
// group. On today's code shape — activityFromEvent (execute.go) always
// emits Final:true with an empty block_id — this changes nothing: every
// group is exactly one record.
func coalesceActivities(activities []*millv1.Activity) []*millv1.Activity {
	out := make([]*millv1.Activity, 0, len(activities))
	for _, a := range activities {
		if len(out) > 0 && coalescesInto(out[len(out)-1], a) {
			mergeActivity(out[len(out)-1], a)
			continue
		}
		out = append(out, a)
	}
	return out
}

// coalescesInto reports whether next continues prev's still-open group:
// prev is a Text or Thinking record that has not yet seen a Final:true
// record of its own, and next is the same kind, source, turn and block id.
func coalescesInto(prev, next *millv1.Activity) bool {
	_, prevBlock, prevFinal, prevOK := textLike(prev)
	if !prevOK || prevFinal {
		return false
	}
	_, nextBlock, _, nextOK := textLike(next)
	if !nextOK {
		return false
	}
	return prev.GetSource() == next.GetSource() &&
		prev.GetTurn() == next.GetTurn() &&
		prevBlock == nextBlock &&
		sameBodyCase(prev, next)
}

// mergeActivity appends next's text onto prev, the group's accumulated
// record, and carries next's final flag forward: ended_at becomes the
// group's last record's own at.
func mergeActivity(prev, next *millv1.Activity) {
	nextText, _, final, _ := textLike(next)
	switch body := prev.GetBody().(type) {
	case *millv1.Activity_Text:
		body.Text.Text += nextText
		body.Text.Final = final
	case *millv1.Activity_Thinking:
		body.Thinking.Text += nextText
		body.Thinking.Final = final
	}
	prev.EndedAt = next.GetAt()
	prev.Bytes = bodySize(prev)
	if next.GetTruncated() {
		prev.Truncated = true
	}
}

// textLike reports a Text or Thinking record's text, block id and final
// flag — the shape coalesceActivities groups on — or ok false for every
// other kind.
func textLike(a *millv1.Activity) (text, blockID string, final, ok bool) {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_Text:
		return body.Text.GetText(), body.Text.GetBlockId(), body.Text.GetFinal(), true
	case *millv1.Activity_Thinking:
		return body.Thinking.GetText(), body.Thinking.GetBlockId(), body.Thinking.GetFinal(), true
	default:
		return "", "", false, false
	}
}

// sameBodyCase reports whether a and b hold the same one of Text/Thinking.
// Callers already know both are one of the two, via textLike.
func sameBodyCase(a, b *millv1.Activity) bool {
	_, aText := a.GetBody().(*millv1.Activity_Text)
	_, bText := b.GetBody().(*millv1.Activity_Text)
	return aText == bText
}

// renumberSeq assigns seq contiguously from 1 over the coalesced records.
// The live file's seq is not preserved past coalescing (docs/activity.md):
// the frame and the index are the durable form, and it is their count that
// matters from here on.
func renumberSeq(activities []*millv1.Activity) {
	for i, a := range activities {
		a.Seq = int64(i + 1)
	}
}
