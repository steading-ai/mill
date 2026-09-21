package run

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace"
)

// SealOrphans seals every closed attempt's live activity file still on disk
// under Options.LogDir when the worker starts: the gap a worker crashing
// between an attempt closing (endActivity, execute.go) and its own seal
// landing leaves behind (docs/activity.md). Wired into worker.go's start(),
// right after the sweep and before the host's own reconciliation, since
// sealing local files needs neither dagger nor that pass.
//
// The only error is a LogDir that cannot be listed. Anything wrong with one
// run — the store cannot say what became of it, or a seal itself fails — is
// logged and left for the next worker start to try again, the same
// retry-safety [SealAttempt] already gives a seal that fails partway.
func (r *Runner) SealOrphans(ctx context.Context) error {
	orphans, err := findOrphans(r.options.LogDir)
	if err != nil {
		return fmt.Errorf("seal orphans: %w", err)
	}
	for runID, files := range orphans {
		r.sealRunOrphans(ctx, runID, files)
	}
	return nil
}

// orphan is one live activity file SealOrphans found, with the (step,
// attempt) [ActivityPath] encoded into its name parsed back out.
type orphan struct {
	step    string
	attempt int64
}

// findOrphans lists every live activity file under dir, grouped by the run
// id its own parent directory names — the shape [ActivityPath] writes. A dir
// that keeps no logs ([Options.LogDir] empty) has nothing to find.
func findOrphans(dir string) (map[string][]orphan, error) {
	if dir == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.activity"))
	if err != nil {
		return nil, err
	}
	orphans := map[string][]orphan{}
	for _, path := range matches {
		runID, found, ok := parseActivityFile(path)
		if !ok {
			continue
		}
		orphans[runID] = append(orphans[runID], found)
	}
	return orphans, nil
}

// parseActivityFile inverts [ActivityPath]: `<dir>/<runID>/<step>.<attempt>.activity`
// back into the run id and the (step, attempt) it names. A name this
// package never wrote does not fit the shape and is skipped rather than
// failing the pass.
func parseActivityFile(path string) (runID string, found orphan, ok bool) {
	base := strings.TrimSuffix(filepath.Base(path), ".activity")
	idx := strings.LastIndex(base, ".")
	if idx < 0 {
		return "", orphan{}, false
	}
	attempt, err := strconv.ParseInt(base[idx+1:], 10, 64)
	if err != nil {
		return "", orphan{}, false
	}
	return filepath.Base(filepath.Dir(path)), orphan{step: base[:idx], attempt: attempt}, true
}

// sealRunOrphans seals every orphan found for one run, once [Inspect] has
// said which of its attempts are actually over. A run Inspect no longer
// knows of, or a store that will not answer, leaves every one of the run's
// files exactly as they are.
func (r *Runner) sealRunOrphans(ctx context.Context, runID string, files []orphan) {
	res, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: runID})
	if isNotFound(err) {
		return
	}
	if err != nil {
		r.options.Log.Warn("sealing orphans: inspecting the run", "run", runID, "error", err)
		return
	}
	var twin workspace.TwinSource
	if r.options.Twin != nil {
		twin = r.options.Twin(runID)
	}
	for _, found := range files {
		sr := matchingStepRun(res.GetSteps(), found.step, found.attempt)
		if sr == nil || sr.GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING {
			continue
		}
		if err := SealAttempt(ctx, r.options.LogDir, runID, found.step, found.attempt,
			sr.GetId(), sr.GetSessionId(), r.options.Store, twin); err != nil {
			r.options.Log.Warn("sealing an orphaned attempt", "run", runID,
				"step", found.step, "attempt", found.attempt, "error", err)
		}
	}
}

// matchingStepRun finds the attempt Inspect returned for (step, attempt), or
// nil — a live file for an attempt the ledger has never heard of, which is
// not this pass's problem to solve.
func matchingStepRun(steps []*millv1.StepRunDetail, step string, attempt int64) *millv1.StepRun {
	for _, detail := range steps {
		if sr := detail.GetStepRun(); sr.GetStepName() == step && sr.GetAttempt() == attempt {
			return sr
		}
	}
	return nil
}
