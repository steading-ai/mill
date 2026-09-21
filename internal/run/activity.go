package run

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace/redact"
)

// The activity record is what `mill logs` reads: one file per attempt,
// written through the redactor as the attempt's events arrive
// (docs/activity.md). It replaces the rendered-text step log this file used
// to hold: every record the attempt produced is kept whole, length-prefixed,
// and the CLI renders it back into today's lines rather than the writer
// throwing anything away.
//
// The file is named for the run, the step and the attempt, because an
// attempt is the unit the ledger closes and a re-run of a step must not
// overwrite what the attempt before it said.

// logSummaryLimit is how much of a text fragment or a tool result reaches
// the rendered log line. A transcript is not the ledger's business and a
// log an operator tails is not a place to put one.
const logSummaryLimit = 240

// activityLengthPrefix is the size, in bytes, of the big-endian length
// header that precedes every marshaled Activity on disk.
const activityLengthPrefix = 4

// defaultActivityCeiling is the most raw bytes one attempt's activity record
// keeps before later bodies are truncated (docs/activity.md's number): "a
// loop of large outputs is the only way to reach it."
const defaultActivityCeiling int64 = 64 << 20

// ActivityPath is where one attempt's activity record lives under dir. The
// CLI builds the same path from the ledger's answer, so the writer and the
// reader agree without either storing it.
func ActivityPath(dir, runID, step string, attempt int64) string {
	return filepath.Join(dir, runID, fmt.Sprintf("%s.%d.activity", step, attempt))
}

// summarize flattens a fragment onto one line and truncates it, so the log
// stays a log.
func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) > logSummaryLimit {
		return flat[:logSummaryLimit] + "…"
	}
	return flat
}

// activityWriter is one attempt's open activity record. Record appends one
// Activity at a time, assigning its seq, its timestamp and its turn, and
// redacting its body before it reaches disk. A nil *activityWriter is the
// no-log case ([Options.LogDir] empty): every method is then a no-op, so a
// spec that does not set it keeps writing no file, exactly as before this
// type existed.
//
// The writer is owned by the attempt, not by [Runner.execute]: it is opened
// where the attempt is opened (right after StartStepRun) and closed where
// the attempt is decided (closeStep, failStep, or a deliberate abort), since
// an attempt's ending — status, evidence kind, and a reason that reads alone
// — is only known once execute returns and verify has run.
type activityWriter struct {
	file     *os.File
	redactor *redact.Redactor
	now      func() time.Time

	mu  sync.Mutex
	seq int64

	// Turn state (docs/activity.md §Turns). A turn opens at the first
	// text/thinking/tool_call after a prompt or after the previous turn's
	// usage closes it. callTurn remembers which turn a tool_call's call_id
	// opened in, so its tool_result — or a subagent record naming it as
	// parent_call_id — takes that turn regardless of what currentTurn is
	// doing by the time it arrives.
	currentTurn int64
	turnOpen    bool
	callTurn    map[string]int64

	textHoldback     *redact.DeltaHoldback
	thinkingHoldback *redact.DeltaHoldback
	verbChunks       map[string]*redact.Chunker
	checkChunks      map[string]*redact.Chunker

	// written is the total bytes appended to the file so far — the header
	// and body of every record actually written, not a body's semantic
	// size. It is what the ceiling compares against, so it bounds the
	// file's real size rather than a count that truncation itself changes.
	written int64
	// ceiling is the most raw bytes this attempt keeps before later bodies
	// are truncated. Zero (a writer built directly rather than through
	// [newActivityWriter]/[reopenActivityWriter]) never truncates.
	ceiling int64
	// truncatedStreams remembers which verb/check output streams have
	// already emitted their one truncated marker, so every chunk after it
	// is dropped rather than adding a second marker.
	truncatedStreams map[string]bool

	// onWrite is called after every record actually reaches the file, so a
	// followed Logs stream in the same process learns about it without
	// polling (docs/activity.md §Observer: "the activity writer on every
	// append"). Nil when [Options.OnActivity] is nil — a worker with no
	// co-located server, which the observer's fallback timer covers instead.
	onWrite func()
}

// newActivityWriter creates `<step>.<attempt>.activity` fresh: seq starts at
// 0, so the first Record call, whatever kind it is, becomes seq 1. An empty
// dir keeps no activity record, matching [Options.LogDir]'s existing
// contract, and returns a nil writer.
func newActivityWriter(dir, runID, step string, attempt int64, redactor *redact.Redactor, now func() time.Time, ceiling int64, onWrite func()) (*activityWriter, error) {
	if dir == "" {
		return nil, nil
	}
	path := ActivityPath(dir, runID, step, attempt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	return newActivityWriterOver(file, 0, 0, redactor, now, ceiling, onWrite), nil
}

// reopenActivityWriter opens an attempt's activity file for append and
// primes seq by decoding the file once to find its highest existing seq.
// This is what a gate resumed by a process that did not file it needs: the
// worker answering the gate is frequently not the one that filed it, so seq
// has to be recovered from the file rather than carried in memory.
//
// A file that is not there is made, at seq 0: the attempt was filed by a
// worker on another host, under another log directory, or by a mill that
// wrote no record at all, and none of those is a reason the gate cannot
// close. A file that ends in a torn record — the writer was killed
// mid-frame — is cut back to its last whole record before anything is
// appended, so what follows stays decodable.
func reopenActivityWriter(dir, runID, step string, attempt int64, redactor *redact.Redactor, now func() time.Time, ceiling int64, onWrite func()) (*activityWriter, error) {
	if dir == "" {
		return nil, nil
	}
	path := ActivityPath(dir, runID, step, attempt)
	existing, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return newActivityWriter(dir, runID, step, attempt, redactor, now, ceiling, onWrite)
	}
	if err != nil {
		return nil, fmt.Errorf("activity: reopen: %w", err)
	}
	activities, whole, decodeErr := decodeActivitiesUpTo(existing)
	closeErr := existing.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("activity: reopen: %w", decodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("activity: reopen: %w", closeErr)
	}
	var seq int64
	for _, a := range activities {
		if a.GetSeq() > seq {
			seq = a.GetSeq()
		}
	}
	if err := os.Truncate(path, whole); err != nil {
		return nil, fmt.Errorf("activity: reopen: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("activity: reopen: %w", err)
	}
	return newActivityWriterOver(file, seq, whole, redactor, now, ceiling, onWrite), nil
}

func newActivityWriterOver(file *os.File, seq, written int64, redactor *redact.Redactor, now func() time.Time, ceiling int64, onWrite func()) *activityWriter {
	return &activityWriter{
		file:             file,
		redactor:         redactor,
		now:              now,
		seq:              seq,
		callTurn:         map[string]int64{},
		textHoldback:     redact.NewDeltaHoldback(redactor),
		thinkingHoldback: redact.NewDeltaHoldback(redactor),
		verbChunks:       map[string]*redact.Chunker{},
		checkChunks:      map[string]*redact.Chunker{},
		written:          written,
		ceiling:          ceiling,
		truncatedStreams: map[string]bool{},
		onWrite:          onWrite,
	}
}

// Record appends one Activity: seq, at, and turn are assigned here, its body
// is redacted as its kind requires, and the length-prefixed frame — a
// 4-byte big-endian length header followed by the marshaled Activity — is
// appended to the file. The caller builds the record's source, its body,
// and, when known, its parent_call_id; everything else is the writer's.
func (w *activityWriter) Record(a *millv1.Activity) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.dropped(a) {
		return nil
	}

	w.redactBody(a)
	if emptyChunk(a) {
		return nil
	}
	return w.append(a)
}

// emptyChunk reports whether a is a verb/check output record whose body
// redacted to nothing: the read it carried completed no line, so
// [redact.Chunker.Write] held it back instead of releasing it, and there is
// nothing yet to append (docs/activity.md §Redaction — a chunk is the
// complete lines a read produced).
func emptyChunk(a *millv1.Activity) bool {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_VerbOutput:
		return len(body.VerbOutput.GetData()) == 0
	case *millv1.Activity_CheckOutput:
		return len(body.CheckOutput.GetData()) == 0
	default:
		return false
	}
}

// append assigns seq, at and turn to a — whose body is already redacted —
// applies the ceiling if the file has grown past it, and writes its
// length-prefixed frame. Shared by Record and the stream-end flushes, whose
// tail is already redacted by [redact.Chunker.Close].
func (w *activityWriter) append(a *millv1.Activity) error {
	w.seq++
	a.Seq = w.seq
	a.At = timestamppb.New(w.now())
	w.assignTurn(a)
	a.Bytes = bodySize(a)
	if w.ceiling > 0 && w.written > w.ceiling {
		w.applyCeiling(a)
	}

	raw, err := proto.Marshal(a)
	if err != nil {
		return fmt.Errorf("activity: marshal: %w", err)
	}
	var header [activityLengthPrefix]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := w.file.Write(header[:]); err != nil {
		return fmt.Errorf("activity: write: %w", err)
	}
	if _, err := w.file.Write(raw); err != nil {
		return fmt.Errorf("activity: write: %w", err)
	}
	w.written += int64(activityLengthPrefix + len(raw))
	if w.onWrite != nil {
		w.onWrite()
	}
	return nil
}

// EndCheckOutput flushes stream's trailing partial line — held back because
// it never got a newline — and records it as one last check_output, the
// frame docs/activity.md §Redaction calls "flushed with check": the caller
// runs this once a check's process has exited, before it records the
// check's own terminal result.
func (w *activityWriter) EndCheckOutput(stream string) error {
	if w == nil {
		return nil
	}
	return w.endOutput(w.checkChunks, stream, func(tail []byte) *millv1.Activity {
		return &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_CHECK,
			Body:   &millv1.Activity_CheckOutput{CheckOutput: &millv1.Output{Stream: stream, Data: tail}},
		}
	})
}

// EndVerbOutput flushes stream's trailing partial line — held back because
// it never got a newline — and records it as one last verb_output, the
// frame docs/activity.md §Redaction calls "flushed with verb": the caller
// runs this once a verb has returned, before it records the verb's own
// terminal result.
func (w *activityWriter) EndVerbOutput(stream string) error {
	if w == nil {
		return nil
	}
	return w.endOutput(w.verbChunks, stream, func(tail []byte) *millv1.Activity {
		return &millv1.Activity{
			Source: millv1.ActivitySource_ACTIVITY_SOURCE_WORKSPACE,
			Body:   &millv1.Activity_VerbOutput{VerbOutput: &millv1.Output{Stream: stream, Data: tail}},
		}
	})
}

// endOutput is EndCheckOutput and EndVerbOutput's shared shape: close
// stream's chunker, and if it held anything back, build and append the one
// record that flushes it.
func (w *activityWriter) endOutput(chunkers map[string]*redact.Chunker, stream string, build func(tail []byte) *millv1.Activity) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := chunkers[stream]
	if !ok {
		return nil
	}
	tail := c.Close()
	if len(tail) == 0 {
		return nil
	}
	return w.append(build(tail))
}

// dropped reports whether a is a further chunk on a verb/check output stream
// that has already emitted its one truncated marker (§Caps): the stream is
// over the ceiling and staying quiet, so nothing about it — not even a seq —
// reaches the file.
func (w *activityWriter) dropped(a *millv1.Activity) bool {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_VerbOutput:
		return w.truncatedStreams["verb:"+body.VerbOutput.GetStream()]
	case *millv1.Activity_CheckOutput:
		return w.truncatedStreams["check:"+body.CheckOutput.GetStream()]
	default:
		return false
	}
}

// applyCeiling shrinks a's body once the file has grown past the ceiling
// (§Caps): a tool result is replaced by its own summary, a verb or check
// output chunk is blanked and its stream marked so every later chunk on it
// is dropped outright rather than adding a second marker.
func (w *activityWriter) applyCeiling(a *millv1.Activity) {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_ToolResult:
		body.ToolResult.Content = summarize(body.ToolResult.GetContent())
		a.Truncated = true
	case *millv1.Activity_VerbOutput:
		body.VerbOutput.Data = nil
		a.Truncated = true
		w.truncatedStreams["verb:"+body.VerbOutput.GetStream()] = true
	case *millv1.Activity_CheckOutput:
		body.CheckOutput.Data = nil
		a.Truncated = true
		w.truncatedStreams["check:"+body.CheckOutput.GetStream()] = true
	}
}

// Close flushes and closes the attempt's file, writing nothing.
func (w *activityWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// End records the attempt's outcome and closes the writer: a convenience
// for the three places an attempt is actually decided, since AttemptEnded is
// always a writer's last record.
func (w *activityWriter) End(status millv1.StepRunStatus, evidenceKind millv1.EvidenceKind, reason string) error {
	if w == nil {
		return nil
	}
	recordErr := w.Record(&millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
		Body: &millv1.Activity_AttemptEnded{AttemptEnded: &millv1.AttemptEnded{
			Status:       status,
			EvidenceKind: evidenceKind,
			Reason:       reason,
		}},
	})
	closeErr := w.Close()
	return errors.Join(recordErr, closeErr)
}

// assignTurn is docs/activity.md §Turns, applied to one record. Only a
// harness-sourced record ever carries a nonzero turn: a check, a verb, a
// gate filing, or the mill's own bookkeeping has no session and is written
// with turn 0 unconditionally, never passing through the rules below.
func (w *activityWriter) assignTurn(a *millv1.Activity) {
	if a.GetSource() != millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS {
		a.Turn = 0
		return
	}
	if parent := a.GetParentCallId(); parent != "" {
		if turn, ok := w.callTurn[parent]; ok {
			a.Turn = turn
			if call, ok := a.GetBody().(*millv1.Activity_ToolCall); ok {
				w.callTurn[call.ToolCall.GetCallId()] = turn
			}
			return
		}
	}
	switch body := a.GetBody().(type) {
	case *millv1.Activity_Text, *millv1.Activity_Thinking, *millv1.Activity_ToolCall:
		if !w.turnOpen {
			w.currentTurn++
			w.turnOpen = true
		}
		a.Turn = w.currentTurn
		if call, ok := body.(*millv1.Activity_ToolCall); ok {
			w.callTurn[call.ToolCall.GetCallId()] = w.currentTurn
		}
	case *millv1.Activity_ToolResult:
		if turn, ok := w.callTurn[body.ToolResult.GetCallId()]; ok {
			a.Turn = turn
		} else {
			a.Turn = w.currentTurn
		}
	case *millv1.Activity_Usage:
		a.Turn = w.currentTurn
		w.turnOpen = false
	default:
		a.Turn = w.currentTurn
	}
}

// redactBody applies the redactor to a's body as its kind requires
// (docs/activity.md §Redaction): a streamed text or thinking delta goes
// through the delta hold-back keyed by its block id, a verb or check output
// chunk goes through the line chunker keyed by its stream, and every other
// kind — arriving whole in one Record call — is walked once by
// [redact.Redactor.Message].
func (w *activityWriter) redactBody(a *millv1.Activity) {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_Text:
		body.Text.Text = w.textHoldback.Add(body.Text.GetBlockId(), body.Text.GetText(), body.Text.GetFinal())
	case *millv1.Activity_Thinking:
		body.Thinking.Text = w.thinkingHoldback.Add(body.Thinking.GetBlockId(), body.Thinking.GetText(), body.Thinking.GetFinal())
	case *millv1.Activity_VerbOutput:
		body.VerbOutput.Data = w.chunker(w.verbChunks, body.VerbOutput.GetStream()).Write(body.VerbOutput.GetData())
	case *millv1.Activity_CheckOutput:
		body.CheckOutput.Data = w.chunker(w.checkChunks, body.CheckOutput.GetStream()).Write(body.CheckOutput.GetData())
	default:
		w.redactor.Message(a)
	}
}

func (w *activityWriter) chunker(chunkers map[string]*redact.Chunker, stream string) *redact.Chunker {
	c, ok := chunkers[stream]
	if !ok {
		c = redact.NewChunker(w.redactor)
		chunkers[stream] = c
	}
	return c
}

// activityBodyOneof is Activity's "body" oneof descriptor, resolved once:
// every Activity shares the same descriptor, so bodySize need not look it up
// by name on every record.
var activityBodyOneof = (&millv1.Activity{}).ProtoReflect().Descriptor().Oneofs().ByName("body")

// bodySize is a's body, marshaled alone: what [Activity.bytes] reports,
// found by proto reflection over the oneof so a future body needs no
// matching addition here.
func bodySize(a *millv1.Activity) int64 {
	if activityBodyOneof == nil {
		return 0
	}
	m := a.ProtoReflect()
	field := m.WhichOneof(activityBodyOneof)
	if field == nil || field.Kind() != protoreflect.MessageKind {
		return 0
	}
	return int64(proto.Size(m.Get(field).Message().Interface()))
}

// attemptStartedActivity builds the record every attempt opens with: seq 1,
// the fold's own label so a frame reads alone. A skill step carries its
// session, its skill and the harness it ran under; a check, gate or
// preparatory step has none of those and leaves the fields zero.
func attemptStartedActivity(step *millv1.Step, sr *millv1.StepRun) *millv1.Activity {
	started := &millv1.AttemptStarted{
		Step:    step.GetName(),
		Attempt: sr.GetAttempt(),
		Kind:    step.GetKind(),
	}
	if step.GetKind() == millv1.StepKind_STEP_KIND_SKILL {
		started.SessionId = sr.GetSessionId()
		started.SessionName = step.GetSession()
		started.SkillName = step.GetSkill().GetName()
		started.SkillDigest = step.GetSkill().GetDigest()
		started.Harness, started.Options = harnessOptions(step)
	}
	return &millv1.Activity{
		Source: millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
		Body:   &millv1.Activity_AttemptStarted{AttemptStarted: started},
	}
}

// harnessOptions is harnessFor (execute.go), split into the adapter name and
// its options document: AttemptStarted carries them as two separate fields.
func harnessOptions(step *millv1.Step) (string, *structpb.Struct) {
	name, settings := harnessFor(step)
	return name, settings.GetOptions()
}

// ReadActivities decodes one attempt's whole activity file: the records
// themselves, for a caller that needs more than the CLI's rendered lines —
// today, only this package's own specs, which assert on a record's kind and
// fields directly rather than scraping rendered text.
func ReadActivities(dir, runID, step string, attempt int64) ([]*millv1.Activity, error) {
	file, err := os.Open(ActivityPath(dir, runID, step, attempt))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return decodeActivities(file)
}

// RenderedLog decodes attempt's activity record and renders it into the
// lines `mill logs` prints — what a caller checking log content, rather than
// individual records, reads.
func RenderedLog(dir, runID, step string, attempt int64) (string, error) {
	activities, err := ReadActivities(dir, runID, step, attempt)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, a := range activities {
		b.WriteString(RenderActivity(a))
	}
	return b.String(), nil
}

// RenderActivity is one Activity as `mill logs` prints it: the lines the
// text log this file replaces would have held for the same fact
// (docs/activity.md's "Today" column), so a reader who knew the old format
// reads the new one unchanged. A kind the old log never wrote — a verb, a
// gate filing, an attempt's own boundary — renders as "".
//
// check_started/check_output/check together reproduce the old block a
// finished check wrote in one shot (`check <cmd> exit status=...` and its
// stream bodies), but each now renders alone as its own record arrives, so
// the exit line — known only once the command has returned — comes after
// its output rather than before: docs/activity.md's check streaming is live
// by design (Approach section), and the old log had no such thing to be
// faithful to. A check's own step name is not carried on any of the three
// bodies (the file is already scoped to one step's one attempt), so the
// resolved command stands in for it.
func RenderActivity(a *millv1.Activity) string {
	stamp := a.GetAt().AsTime().UTC().Format(time.RFC3339)
	switch body := a.GetBody().(type) {
	case *millv1.Activity_Prompt:
		text := body.Prompt.GetText()
		if text == "" {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s prompt begin bytes=%d\n", stamp, len(text))
		fmt.Fprintln(&b, strings.TrimRight(text, "\n"))
		fmt.Fprintf(&b, "%s prompt end\n", stamp)
		return b.String()
	case *millv1.Activity_Text:
		if !body.Text.GetFinal() {
			return ""
		}
		return fmt.Sprintf("%s text %s\n", stamp, summarize(body.Text.GetText()))
	case *millv1.Activity_ToolCall:
		return fmt.Sprintf("%s tool %s call=%s\n", stamp, body.ToolCall.GetName(), body.ToolCall.GetCallId())
	case *millv1.Activity_ToolResult:
		status := "ok"
		if body.ToolResult.GetIsError() {
			status = "error"
		}
		return fmt.Sprintf("%s result call=%s %s out=%s\n", stamp, body.ToolResult.GetCallId(), status, summarize(body.ToolResult.GetContent()))
	case *millv1.Activity_Usage:
		models := make([]string, 0, len(body.Usage.GetByModel()))
		for model, counts := range body.Usage.GetByModel() {
			models = append(models, fmt.Sprintf("%s in=%d out=%d", model, counts.GetInput(), counts.GetOutput()))
		}
		return fmt.Sprintf("%s usage %s\n", stamp, strings.Join(models, " "))
	case *millv1.Activity_Heartbeat:
		return fmt.Sprintf("%s heartbeat %d/%d waiting=%ds tasks=%s\n", stamp,
			body.Heartbeat.GetStrike(), body.Heartbeat.GetStrikes(),
			body.Heartbeat.GetWaitingSeconds(), strings.Join(body.Heartbeat.GetTasks(), "; "))
	case *millv1.Activity_HarnessStderr:
		text := body.HarnessStderr.GetText()
		if strings.TrimSpace(text) == "" {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s harness stderr begin bytes=%d\n", stamp, len(text))
		fmt.Fprintln(&b, strings.TrimRight(text, "\n"))
		fmt.Fprintf(&b, "%s harness stderr end\n", stamp)
		return b.String()
	case *millv1.Activity_CheckStarted:
		return fmt.Sprintf("%s check %s begin\n", stamp, strings.Join(body.CheckStarted.GetCommand(), " "))
	case *millv1.Activity_CheckOutput:
		data := body.CheckOutput.GetData()
		if len(data) == 0 {
			return ""
		}
		stream := body.CheckOutput.GetStream()
		var b strings.Builder
		fmt.Fprintf(&b, "%s check %s begin bytes=%d\n", stamp, stream, len(data))
		fmt.Fprintln(&b, strings.TrimRight(string(data), "\n"))
		fmt.Fprintf(&b, "%s check %s end\n", stamp, stream)
		return b.String()
	case *millv1.Activity_Check:
		var b strings.Builder
		fmt.Fprintf(&b, "%s check exit status=%d\n", stamp, body.Check.GetExit().GetStatus())
		if reason := body.Check.GetReason(); reason != "" {
			fmt.Fprintf(&b, "%s check failed: %s\n", stamp, reason)
		}
		return b.String()
	default:
		return ""
	}
}

// ReadActivity reads one length-prefixed Activity from r at its current
// position. When the record is not yet whole — the tail of a file still
// being appended to — it reports ok false and leaves r's position exactly
// where it started, so `mill logs --follow` can retry once more bytes have
// landed instead of mistaking a partial write for the file's end or, worse,
// consuming a partial header it can never complete.
func ReadActivity(r io.ReadSeeker) (*millv1.Activity, bool, error) {
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, false, fmt.Errorf("activity: decode: %w", err)
	}
	var header [activityLengthPrefix]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			_, seekErr := r.Seek(pos, io.SeekStart)
			return nil, false, seekErr
		}
		return nil, false, fmt.Errorf("activity: decode: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			_, seekErr := r.Seek(pos, io.SeekStart)
			return nil, false, seekErr
		}
		return nil, false, fmt.Errorf("activity: decode: %w", err)
	}
	a := &millv1.Activity{}
	if err := proto.Unmarshal(body, a); err != nil {
		return nil, false, fmt.Errorf("activity: decode: %w", err)
	}
	return a, true, nil
}

// decodeActivities reads a length-prefixed stream of Activity records back:
// a 4-byte big-endian length header followed by that many bytes, repeated
// to EOF. A short trailing read — a header or a body cut off mid-write — is
// "not yet written," not corruption (docs/activity.md §Lifecycle; the
// crash-time seal that would tell the two apart is a later node's job), so
// it ends the read cleanly rather than failing it. Shared by
// [reopenActivityWriter] and the CLI's reader, so the file has exactly one
// decode loop.
func decodeActivities(r io.Reader) ([]*millv1.Activity, error) {
	activities, _, err := decodeActivitiesUpTo(r)
	return activities, err
}

// decodeActivitiesUpTo is decodeActivities that also says how many bytes
// held whole records: a cut-off header or body at the end is a writer that
// was killed mid-frame, and the read ends cleanly before it.
func decodeActivitiesUpTo(r io.Reader) ([]*millv1.Activity, int64, error) {
	var out []*millv1.Activity
	var whole int64
	var header [activityLengthPrefix]byte
	for {
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out, whole, nil
			}
			return out, whole, fmt.Errorf("activity: decode: %w", err)
		}
		size := binary.BigEndian.Uint32(header[:])
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out, whole, nil
			}
			return out, whole, fmt.Errorf("activity: decode: %w", err)
		}
		a := &millv1.Activity{}
		if err := proto.Unmarshal(body, a); err != nil {
			return out, whole, fmt.Errorf("activity: decode: %w", err)
		}
		out = append(out, a)
		whole += int64(activityLengthPrefix) + int64(size)
	}
}
