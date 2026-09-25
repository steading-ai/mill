package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Policy is one deployment's or project's retention setting: after
// TwinDays an attempt's twin is removed, and after CompactDays its frame is
// compacted. Zero means never; the zero value is `keep`, the default most
// deployments never change (docs/activity.md §Lifecycle "Retention").
type Policy struct {
	TwinDays    int
	CompactDays int
}

// String renders a policy the way its own `retention`/`retention-projects`
// grammar spells it, [ParsePolicy]'s inverse.
func (p Policy) String() string {
	if p == (Policy{}) {
		return "keep"
	}
	var parts []string
	if p.TwinDays > 0 {
		parts = append(parts, fmt.Sprintf("twin_days=%d", p.TwinDays))
	}
	if p.CompactDays > 0 {
		parts = append(parts, fmt.Sprintf("compact_days=%d", p.CompactDays))
	}
	return strings.Join(parts, ",")
}

// PolicyFor resolves the retention policy in force for a project: its own
// entry in projects, or the deployment's default.
func PolicyFor(policy Policy, projects map[string]Policy, project string) Policy {
	if p, ok := projects[project]; ok {
		return p
	}
	return policy
}

// ParsePolicy parses a `retention`/`retention-projects` value: `keep` (or
// empty, its equivalent) or a comma-separated `twin_days=N`/`compact_days=N`
// pair, following the error shape parseGlobalCaps already established for a
// small key=value grammar (internal/cli/run.go).
func ParsePolicy(s string) (Policy, error) {
	if s == "" || s == "keep" {
		return Policy{}, nil
	}
	var p Policy
	for _, part := range strings.Split(s, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return Policy{}, fmt.Errorf("retention: %q is not keep or key=value", part)
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return Policy{}, fmt.Errorf("retention: %s: %q is not a positive number of days", key, value)
		}
		switch key {
		case "twin_days":
			p.TwinDays = n
		case "compact_days":
			p.CompactDays = n
		default:
			return Policy{}, fmt.Errorf("retention: %q is not twin_days, compact_days or keep", key)
		}
	}
	return p, nil
}

// renderCompacted renders a sealed attempt's activities the way `mill logs`
// prints them, markers included: `logsRenderer.render`'s two marker cases
// (internal/cli/logs.go), reproduced here from the frame's own
// attempt_started/attempt_ended records rather than shared across the
// internal/cli/internal/run package boundary (docs/plans/2026-09-21-mill-17.md
// Decisions). A sealed attempt's attempt_started always precedes its
// attempt_ended in the same frame, so the step/attempt it names is enough
// local state; everything else defers to ActivityRenderer.Render.
func renderCompacted(activities []*millv1.Activity) string {
	blocks := NewActivityRenderer()
	var step string
	var attempt int64
	var b strings.Builder
	for _, a := range activities {
		switch body := a.GetBody().(type) {
		case *millv1.Activity_AttemptStarted:
			step = body.AttemptStarted.GetStep()
			attempt = body.AttemptStarted.GetAttempt()
			fmt.Fprintf(&b, "-- %s attempt %d started --\n", step, attempt)
		case *millv1.Activity_AttemptEnded:
			fmt.Fprintf(&b, "-- %s attempt %d ended: %s --\n", step, attempt, body.AttemptEnded.GetReason())
		default:
			b.WriteString(blocks.Render(a))
		}
	}
	return b.String()
}

// compactFrame rewrites the sealed frame at path to hold exactly one
// record — the summary lines renderCompacted produces for it, every row's
// truncated set — and reports whether it actually rewrote anything.
// Idempotent: a frame that already decodes to one Activity_Compacted record
// is a no-op, since the sweep runs repeatedly and must not re-render an
// already-compacted frame.
func compactFrame(path, stepRunID string) (bool, error) {
	activities, err := ReadFrame(path)
	if err != nil {
		return false, err
	}
	if len(activities) == 1 {
		if _, ok := activities[0].GetBody().(*millv1.Activity_Compacted); ok {
			return false, nil
		}
	}
	text := renderCompacted(activities)
	rec := &millv1.Activity{
		Seq:       1,
		Source:    millv1.ActivitySource_ACTIVITY_SOURCE_MILL,
		Truncated: true,
		Bytes:     int64(len(text)),
		Body:      &millv1.Activity_Compacted{Compacted: &millv1.Compacted{Text: text}},
	}
	if n := len(activities); n > 0 {
		rec.At = activities[n-1].GetAt()
	}
	if _, err := writeFrame(path, stepRunID, []*millv1.Activity{rec}); err != nil {
		return false, err
	}
	if err := verifyFrame(path, []*millv1.Activity{rec}); err != nil {
		return false, err
	}
	return true, nil
}

// RetentionReport is one retention sweep's tally, for the caller to log
// beside [SweepRetention]'s own summary line.
type RetentionReport struct {
	TwinsRemoved    int
	FramesCompacted int
}

// policyFor resolves the retention policy in force for a project: its own
// entry in RetentionProjects, or the deployment's default.
func (r *Runner) policyFor(project string) Policy {
	return PolicyFor(r.options.Retention, r.options.RetentionProjects, project)
}

// SweepRetention enforces the deployment's retention policy over every
// sealed frame and twin under Options.LogDir: past TwinDays, an attempt's
// twin is removed; past CompactDays, its frame is compacted
// (docs/activity.md §Lifecycle "Retention"). Never touches the activity
// index — both are filesystem-only operations. Called on worker start and
// on the sweep interval (internal/run/worker.go), the same two trigger
// points [Runner.Sweep] itself is.
//
// A policy of keep everywhere — the zero value, and the default — logs one
// line and returns without walking the filesystem, so a host that never
// touches the setting pays nothing for it.
func (r *Runner) SweepRetention(ctx context.Context) (RetentionReport, error) {
	var report RetentionReport
	if r.options.Retention == (Policy{}) && len(r.options.RetentionProjects) == 0 {
		r.options.Log.Info("retention: keep (default); nothing to sweep")
		return report, nil
	}

	frames, twins, err := findRetentionCandidates(r.options.LogDir)
	if err != nil {
		return report, fmt.Errorf("sweep retention: %w", err)
	}
	runIDs := make(map[string]struct{}, len(frames)+len(twins))
	for runID := range frames {
		runIDs[runID] = struct{}{}
	}
	for runID := range twins {
		runIDs[runID] = struct{}{}
	}

	now := r.options.Now()
	for runID := range runIDs {
		res, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: runID})
		if isNotFound(err) {
			continue
		}
		if err != nil {
			r.options.Log.Warn("sweep retention: inspecting the run", "run", runID, "error", err)
			continue
		}
		project, _, _ := strings.Cut(res.GetSummary().GetRun().GetNodeRef(), "#")
		r.sweepRunRetention(&report, runID, r.policyFor(project), res.GetSteps(), frames[runID], twins[runID], now)
	}

	r.options.Log.Info("retention swept", "log_dir", r.options.LogDir,
		"twins_removed", report.TwinsRemoved, "frames_compacted", report.FramesCompacted)
	return report, nil
}

// sweepRunRetention applies policy to one run's candidate frames and twins,
// ageing each against the attempt's own ended_at from steps.
func (r *Runner) sweepRunRetention(report *RetentionReport, runID string, policy Policy, steps []*millv1.StepRunDetail, frames, twins []orphan, now time.Time) {
	if policy.TwinDays > 0 {
		threshold := time.Duration(policy.TwinDays) * 24 * time.Hour
		for _, found := range twins {
			sr := matchingStepRun(steps, found.step, found.attempt)
			if sr == nil || sr.GetEndedAt() == nil || now.Sub(sr.GetEndedAt().AsTime()) < threshold {
				continue
			}
			path := TwinPath(r.options.LogDir, runID, found.step, found.attempt)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				r.options.Log.Warn("sweep retention: removing a twin", "run", runID,
					"step", found.step, "attempt", found.attempt, "error", err)
				continue
			}
			report.TwinsRemoved++
		}
	}
	if policy.CompactDays > 0 {
		threshold := time.Duration(policy.CompactDays) * 24 * time.Hour
		for _, found := range frames {
			sr := matchingStepRun(steps, found.step, found.attempt)
			if sr == nil || sr.GetEndedAt() == nil || now.Sub(sr.GetEndedAt().AsTime()) < threshold {
				continue
			}
			path := FramePath(r.options.LogDir, runID, found.step, found.attempt)
			changed, err := compactFrame(path, sr.GetId())
			if err != nil {
				r.options.Log.Warn("sweep retention: compacting a frame", "run", runID,
					"step", found.step, "attempt", found.attempt, "error", err)
				continue
			}
			if changed {
				report.FramesCompacted++
			}
		}
	}
}

// FrameSuffix and TwinSuffix name a sealed frame's and twin's full file
// suffix ([FramePath], [TwinPath]) — checked in full, never a bare `.zst`,
// which a twin also ends in — so a caller classifying a `--log-dir` listing
// (findRetentionCandidates, internal/cli's activity probe) has one place
// both spellings come from.
const (
	FrameSuffix = ".activity.zst"
	TwinSuffix  = ".twin.tar.zst"
)

// findRetentionCandidates lists every sealed frame and twin under dir,
// grouped by the run id their parent directory names, matching [orphan]'s
// (step, attempt) shape so both share [matchingStepRun]. A dir that keeps no
// logs (empty) has nothing to find.
func findRetentionCandidates(dir string) (frames, twins map[string][]orphan, err error) {
	if dir == "" {
		return nil, nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.zst"))
	if err != nil {
		return nil, nil, err
	}
	frames = map[string][]orphan{}
	twins = map[string][]orphan{}
	for _, path := range matches {
		base := filepath.Base(path)
		runID := filepath.Base(filepath.Dir(path))
		switch {
		case strings.HasSuffix(base, FrameSuffix):
			if step, attempt, ok := parseSealedName(base, FrameSuffix); ok {
				frames[runID] = append(frames[runID], orphan{step: step, attempt: attempt})
			}
		case strings.HasSuffix(base, TwinSuffix):
			if step, attempt, ok := parseSealedName(base, TwinSuffix); ok {
				twins[runID] = append(twins[runID], orphan{step: step, attempt: attempt})
			}
		}
	}
	return frames, twins, nil
}

// parseSealedName inverts [FramePath]/[TwinPath]'s file name — `<step>.<attempt><suffix>` —
// back into the (step, attempt) it names, or ok false for a name that does
// not fit the shape.
func parseSealedName(base, suffix string) (step string, attempt int64, ok bool) {
	trimmed := strings.TrimSuffix(base, suffix)
	if trimmed == base {
		return "", 0, false
	}
	idx := strings.LastIndex(trimmed, ".")
	if idx < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(trimmed[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return trimmed[:idx], n, true
}
