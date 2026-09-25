package machine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steading-ai/mill/internal/workspace/machine/driver"
	"github.com/steading-ai/mill/internal/workspace/machine/image"
)

// DefaultScratchBudget is how old a scratch project must be before the sweep
// takes it: the acceptance suite's own timeout (`make test-acceptance`
// passes `--timeout=90m`), so a project younger than that may belong to a
// suite still running and one older cannot (docs/sandbox.md §"External
// state").
const DefaultScratchBudget = 90 * time.Minute

// ScratchPattern is the name every scratch thing a suite makes carries:
// `mill-acceptance-<scenario>-<nonce>`, the nonce eight hex digits
// (tests/acceptance, `nonce`). The sweep matches by this name and by age
// and by nothing else, because a sweep that took "everything not in the
// ledger" once nearly finished what a stray worker started (mill#49).
var ScratchPattern = regexp.MustCompile(`^mill-acceptance-[a-z0-9][a-z0-9-]*-[0-9a-f]{8}$`)

// IsScratch reports whether a name is one a suite's scratch state carries.
func IsScratch(name string) bool { return ScratchPattern.MatchString(name) }

// Ledger is what reconciliation asks of the mill's store: which runs are
// live, so a machine can be told from an orphan, and a place to record what
// the pass found and did (docs/sandbox.md §Reconciliation).
type Ledger interface {
	// LiveRuns is every run that has not ended, on any host.
	LiveRuns(ctx context.Context) ([]LiveRun, error)
	// RecordReconciliation writes the pass's report.
	RecordReconciliation(ctx context.Context, report Report) error
}

// LiveRun is one run the ledger holds open.
type LiveRun struct {
	// RunID is the run's id, which names its machine (IDFor).
	RunID string
	// NodeRef is the node the run works, `<project>#<n>`; its project is
	// what a scratch project is matched against.
	NodeRef string
	// Step is the run's current step, or empty while it is selecting.
	Step string
	// Machine is the machine the ledger's workspace record names for the
	// run, or empty when the ledger records none — a run with no machine,
	// or one whose record predates the sandbox fields. When the ledger
	// names one, the pass acts on it: the host's `run.json` is the copy,
	// and one that disagrees is a finding (docs/sandbox.md §Reconciliation).
	Machine string
}

// Project is the skift project a node reference names.
func (r LiveRun) Project() string {
	project, _, _ := strings.Cut(r.NodeRef, "#")
	return project
}

// External is one kind of state a machine creates outside itself and a
// suite names so that it can be swept from the host side: skift's scratch
// projects, the forge's (docs/sandbox.md §"External state"). The reconciler
// decides what is stale — the name pattern, the age, no live run — and the
// external decides how it goes.
type External interface {
	// Kind names the state, such as `skift` or `forge`, for the report.
	Kind() string
	// List is every candidate the external knows, matched or not; the
	// reconciler filters.
	List(ctx context.Context) ([]Scratch, error)
	// Remove takes one and says what it did to it — `archived`,
	// `deleted` — with whatever qualifies that, such as why it was not
	// deleted.
	Remove(ctx context.Context, scratch Scratch) (action string, err error)
}

// Scratch is one thing an [External] lists.
type Scratch struct {
	// Name is what the pattern is matched against: a project's slug on
	// skift, its path's last element on the forge.
	Name string
	// CreatedAt is when it was made, which the budget is measured from.
	CreatedAt time.Time
}

// FindingKind classifies one line of a [Report].
type FindingKind string

// The kinds of finding a pass records, in docs/sandbox.md §Reconciliation's
// words.
const (
	// FindingOrphan is a machine with no live run: stopped, its run
	// directory removed.
	FindingOrphan FindingKind = "orphan"
	// FindingLostSandbox is a live run this host made a machine for and
	// has no machine for now. The run fails when it is next claimed; the
	// pass records it and provisions nothing.
	FindingLostSandbox FindingKind = "lost-sandbox"
	// FindingResumable is a live run with its machine: the step slice it
	// left in flight was stopped, and the run resumes at the ledger's step
	// in a fresh one.
	FindingResumable FindingKind = "resumable"
	// FindingLeftover is a `.building` image or a run directory with no
	// live run, removed.
	FindingLeftover FindingKind = "leftover"
	// FindingSwept is a scratch project archived or deleted by name.
	FindingSwept FindingKind = "swept"
	// FindingFailure is something the pass could not do, kept so the rest
	// of the pass still stands.
	FindingFailure FindingKind = "failure"
	// FindingDisagreement is a run whose ledger record and whose `run.json`
	// on this host name different machines. The pass acts on the ledger's
	// and records both.
	FindingDisagreement FindingKind = "disagreement"
)

// Finding is one line of the report.
type Finding struct {
	// Kind is what was found.
	Kind FindingKind `json:"kind"`
	// Name is the thing: a machine, a run id, a path, a project.
	Name string `json:"name"`
	// Action is what was done to it: `stopped`, `terminated`, `removed`,
	// `archived`, `deleted`, `slice stopped`, or nothing.
	Action string `json:"action,omitempty"`
	// Detail qualifies it: the run's step, the unit, why a delete was an
	// archive.
	Detail string `json:"detail,omitempty"`
}

// String is the finding as one line of a log.
func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	b.WriteString(" ")
	b.WriteString(f.Name)
	if f.Action != "" {
		b.WriteString(": ")
		b.WriteString(f.Action)
	}
	if f.Detail != "" {
		b.WriteString(" (")
		b.WriteString(f.Detail)
		b.WriteString(")")
	}
	return b.String()
}

// Report is what one reconciliation pass found and did.
type Report struct {
	// StartedAt is when the pass began.
	StartedAt time.Time `json:"started_at"`
	// Findings are its lines, in the order the pass made them.
	Findings []Finding `json:"findings"`
}

// Of is the findings of one kind.
func (r Report) Of(kind FindingKind) []Finding {
	var out []Finding
	for _, finding := range r.Findings {
		if finding.Kind == kind {
			out = append(out, finding)
		}
	}
	return out
}

func (r *Report) add(kind FindingKind, name, action, detail string) {
	r.Findings = append(r.Findings, Finding{Kind: kind, Name: name, Action: action, Detail: detail})
}

func (r *Report) fail(name string, err error) {
	r.add(FindingFailure, name, "", err.Error())
}

// Reconciler is docs/sandbox.md §Reconciliation: what a worker runs once,
// before it claims anything, over the host the machine backend lives on.
type Reconciler struct {
	host      driver.Driver
	config    Config
	images    *image.Store
	externals []External
}

// NewReconciler builds a reconciler over the host, with the externals the
// pass sweeps. The configuration is the backend's own, so the pass reads
// the same run directories and image directory the runs write.
func NewReconciler(host driver.Driver, c Config, externals ...External) *Reconciler {
	c = c.withDefaults()
	return &Reconciler{
		host:      host,
		config:    c,
		images:    image.New(host, c.Images),
		externals: externals,
	}
}

// Reconcile is the pass, in docs/sandbox.md §Reconciliation's order: the
// registered machines against the ledger's live runs — orphans stopped and
// their run directories removed, lost sandboxes recorded, live runs' slices
// stopped for a fresh one on resume — then leftover builds and run
// directories, then the external sweeps by name. Eviction is the caller's,
// after this returns. The report is recorded in the ledger and returned.
//
// An error is one that stops the pass from telling orphans from live runs:
// the ledger or machined not answering. Anything narrower — a stop that
// needed terminate, a forge that did not answer — is a finding in the
// report, so one refusal does not leave everything else uncleaned.
func (r *Reconciler) Reconcile(ctx context.Context, ledger Ledger) (Report, error) {
	report := Report{StartedAt: r.config.Now()}
	live, err := ledger.LiveRuns(ctx)
	if err != nil {
		return report, fmt.Errorf("machine: reconcile: live runs: %w", err)
	}
	liveByID := map[string]LiveRun{}
	// The ledger's machine name is the one the pass acts on: a registered
	// machine of that name belongs to its run whatever the run's id would
	// have derived, so live runs are also found by it.
	liveByMachine := map[string]LiveRun{}
	for _, run := range live {
		if id, err := IDFor(run.RunID); err == nil {
			liveByID[id] = run
		}
		if id, ok := strings.CutPrefix(run.Machine, MachinePrefix); ok && id != "" {
			liveByMachine[id] = run
		}
	}
	machines, err := r.host.ListMachines(ctx)
	if err != nil {
		return report, fmt.Errorf("machine: reconcile: machinectl list: %w", err)
	}

	registered := map[string]bool{}
	handled := map[string]bool{}
	for _, m := range machines {
		if !strings.HasPrefix(m.Name, MachinePrefix) {
			continue
		}
		id := strings.TrimPrefix(m.Name, MachinePrefix)
		registered[id] = true
		run, ok := liveByID[id]
		if !ok {
			run, ok = liveByMachine[id]
		}
		if ok {
			// A run whose ledger row and run.json name different
			// registered machines is handled once, on the ledger's; the
			// other is neither the run's nor an orphan to stop, and the
			// disagreement finding names it.
			if !handled[run.RunID] {
				handled[run.RunID] = true
				r.resumable(ctx, &report, run)
			}
			continue
		}
		r.orphan(ctx, &report, id)
	}

	for id, run := range sortedIDs(liveByID) {
		if registered[id] || registered[strings.TrimPrefix(run.Machine, MachinePrefix)] {
			continue
		}
		record, err := readRecord(r.config.runDir(id))
		if err != nil {
			// Never made on this host, or never got as far as a machine:
			// nothing was lost here.
			continue
		}
		machine := r.machineOf(&report, run, record)
		report.add(FindingLostSandbox, run.RunID, "",
			fmt.Sprintf("machine %s is not registered; the ledger's step is %s", machine, stepOrSelecting(run.Step)))
	}

	r.leftovers(ctx, &report, liveByID)
	r.sweep(ctx, &report, live)

	if err := ledger.RecordReconciliation(ctx, report); err != nil {
		return report, fmt.Errorf("machine: reconcile: record: %w", err)
	}
	return report, nil
}

// resumable is a live run with its machine present: the slice a dead worker
// left in flight is stopped now rather than when the run is next claimed,
// so nothing it was running keeps running for nobody. Attach stops it again
// — a stop of an inactive slice is harmless — and recreates it.
func (r *Reconciler) resumable(ctx context.Context, report *Report, run LiveRun) {
	id, _ := IDFor(run.RunID)
	record, err := readRecord(r.config.runDir(id))
	if err != nil {
		name := run.Machine
		if name == "" {
			name = MachineName(id)
		}
		report.add(FindingResumable, run.RunID, "", fmt.Sprintf("machine %s, no run record on this host: %v", name, err))
		return
	}
	machine := r.machineOf(report, run, record)
	if record.Slice == "" {
		report.add(FindingResumable, run.RunID, "", fmt.Sprintf("machine %s at step %s, no slice in flight", machine, stepOrSelecting(run.Step)))
		return
	}
	if err := r.host.StopUnit(ctx, machine, record.Slice); err != nil {
		report.fail(run.RunID, fmt.Errorf("stop %s in %s: %w", record.Slice, machine, err))
		return
	}
	report.add(FindingResumable, run.RunID, "stopped "+record.Slice,
		fmt.Sprintf("machine %s resumes at step %s in a fresh slice", machine, stepOrSelecting(run.Step)))
}

// machineOf is the machine the pass acts on for a live run: the ledger's
// when the ledger names one, else the host record's. The two are the same
// name derived the same way, so a disagreement is a record that was
// tampered with or a host that lost its ledger and grew another; it is
// recorded, naming both, and the ledger's is used (docs/ledger.md
// §workspaces).
func (r *Reconciler) machineOf(report *Report, run LiveRun, record Record) string {
	if run.Machine == "" {
		return record.Machine
	}
	if record.Machine != "" && record.Machine != run.Machine {
		report.add(FindingDisagreement, run.RunID, "",
			fmt.Sprintf("the ledger names machine %s and %s on this host names %s; acting on the ledger's", run.Machine, RecordName, record.Machine))
	}
	return run.Machine
}

// orphan is a machine with no live run: stopped as Destroy would — a stop
// if its record says it reported ready, a terminate otherwise or if the
// stop leaves it registered — and its run directory removed.
func (r *Reconciler) orphan(ctx context.Context, report *Report, id string) {
	name, unit := MachineName(id), UnitName(id)
	w := New(r.host, r.config)
	record, err := readRecord(r.config.runDir(id))
	if err != nil {
		// A machine with no record — a suite's, or a run directory already
		// gone — never reported ready to anyone here, and is terminated.
		record = Record{ID: id, Machine: name, Unit: unit}
	}
	action := "terminated"
	if record.Ready() {
		action = "stopped"
		err = w.teardownReady(ctx, &record)
	} else {
		err = w.teardownUnready(ctx, &record)
	}
	if err != nil {
		report.fail(name, err)
		return
	}
	w.removeRunDirectory(ctx, id)
	report.add(FindingOrphan, name, action+"; run directory removed", "unit "+record.Unit)
}

// leftovers is step 2: `.building` images, and run directories with no
// live run and no machine.
func (r *Reconciler) leftovers(ctx context.Context, report *Report, live map[string]LiveRun) {
	removed, err := r.images.RemoveLeftoverBuilds(ctx)
	for _, path := range removed {
		report.add(FindingLeftover, path, "removed", "unfinished image build")
	}
	if err != nil {
		report.fail(r.config.Machines, err)
	}
	entries, err := os.ReadDir(r.config.runsDir())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			report.fail(r.config.runsDir(), err)
		}
		return
	}
	w := New(r.host, r.config)
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(id, ".") {
			continue
		}
		if _, ok := live[id]; ok {
			continue
		}
		if _, err := os.Stat(r.config.runDir(id)); err != nil {
			// An orphan's, removed above.
			continue
		}
		w.removeRunDirectory(ctx, id)
		report.add(FindingLeftover, r.config.runDir(id), "removed", "run directory with no live run")
	}
}

// sweep is step 3: every external's scratch state that matches the pattern,
// is older than the budget, and belongs to no live run.
func (r *Reconciler) sweep(ctx context.Context, report *Report, live []LiveRun) {
	cutoff := r.config.Now().Add(-r.config.ScratchBudget)
	projects := map[string]bool{}
	for _, run := range live {
		projects[run.Project()] = true
	}
	for _, external := range r.externals {
		listed, err := external.List(ctx)
		if err != nil {
			report.fail(external.Kind(), err)
			continue
		}
		for _, scratch := range listed {
			if !IsScratch(scratch.Name) || !scratch.CreatedAt.Before(cutoff) || projects[scratch.Name] {
				continue
			}
			action, err := external.Remove(ctx, scratch)
			if err != nil {
				report.fail(external.Kind()+" "+scratch.Name, err)
				continue
			}
			report.add(FindingSwept, scratch.Name, action,
				fmt.Sprintf("%s project, %s old", external.Kind(), r.config.Now().Sub(scratch.CreatedAt).Round(time.Minute)))
		}
	}
}

// sortedIDs iterates live runs in id order, so a report reads the same way
// twice.
func sortedIDs(runs map[string]LiveRun) func(func(string, LiveRun) bool) {
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return func(yield func(string, LiveRun) bool) {
		for _, id := range ids {
			if !yield(id, runs[id]) {
				return
			}
		}
	}
}

func stepOrSelecting(step string) string {
	if step == "" {
		return "(selecting)"
	}
	return step
}
