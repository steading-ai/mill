package machine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
	"github.com/steading-ai/mill/internal/workspace/machine/image"
)

// fakeLedger is the store as reconciliation sees it: the live runs a spec
// declares, and the report it was handed.
type fakeLedger struct {
	mu     sync.Mutex
	live   []machine.LiveRun
	report *machine.Report
	err    error
}

func (l *fakeLedger) LiveRuns(context.Context) ([]machine.LiveRun, error) {
	return append([]machine.LiveRun(nil), l.live...), l.err
}

func (l *fakeLedger) RecordReconciliation(_ context.Context, report machine.Report) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.report = &report
	return nil
}

func (l *fakeLedger) recorded() *machine.Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.report
}

// fakeExternal is one kind of scratch state: what it lists, and what it was
// asked to remove.
type fakeExternal struct {
	kind    string
	listed  []machine.Scratch
	action  string
	mu      sync.Mutex
	removed []string
}

func (e *fakeExternal) Kind() string { return e.kind }

func (e *fakeExternal) List(context.Context) ([]machine.Scratch, error) {
	return append([]machine.Scratch(nil), e.listed...), nil
}

func (e *fakeExternal) Remove(_ context.Context, scratch machine.Scratch) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, scratch.Name)
	return e.action, nil
}

func (e *fakeExternal) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.removed...)
}

var _ = Describe("Reconciliation on resume", Label("unit"), func() {
	var (
		f      *fixture
		ledger *fakeLedger
		now    time.Time
	)

	BeforeEach(func() {
		f = newFixture(origin(map[string]string{
			"mill.toml": "[cache.target]\npath = \"/work/target\"\n",
		}))
		now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		f.config.Now = func() time.Time { return now }
		ledger = &fakeLedger{}
	})

	reconcile := func(ctx context.Context, externals ...machine.External) machine.Report {
		GinkgoHelper()
		report, err := machine.NewReconciler(f.host, f.config, externals...).Reconcile(ctx, ledger)
		Expect(err).NotTo(HaveOccurred())
		Expect(ledger.recorded()).NotTo(BeNil(), "the pass is recorded in the ledger")
		Expect(*ledger.recorded()).To(Equal(report))
		return report
	}

	// Scenario: An orphan machine is stopped and its run directory removed
	//   Given a registered machine with no live run
	//   When the worker starts
	//   Then the machine is gone and its run directory is deleted
	It("An orphan machine is stopped and its run directory removed", func(ctx context.Context) {
		By("given a registered machine with no live run")
		ws := f.workspace()
		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#63", Origin: f.config.Origin})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.host.registered()).To(ConsistOf("mill-" + id))
		Expect(f.runDir()).To(BeADirectory())
		ledger.live = nil

		By("when the worker starts")
		report := reconcile(ctx)

		By("then the machine is gone and its run directory is deleted")
		Expect(f.host.registered()).To(BeEmpty())
		Expect(f.runDir()).NotTo(BeADirectory())
		Expect(f.socketDir()).NotTo(BeADirectory())
		stops := f.host.CallsTo(driver.StopUnit)
		Expect(stops).To(ContainElement(driver.Call{Method: driver.StopUnit, Args: []any{"", "mill-machine@" + id + ".service"}}),
			"a machine that reported ready is stopped through its unit, as Destroy does")
		Expect(f.host.CallsTo(driver.DeleteSubvolume)).NotTo(BeEmpty(), "its cache copies went through the driver")

		By("and the reconciliation is recorded, naming the machine and the unit")
		orphans := report.Of(machine.FindingOrphan)
		Expect(orphans).To(HaveLen(1))
		Expect(orphans[0].Name).To(Equal("mill-" + id))
		Expect(orphans[0].Action).To(Equal("stopped; run directory removed"))
		Expect(orphans[0].Detail).To(ContainSubstring("mill-machine@" + id + ".service"))
	})

	It("terminates an orphan nothing here has a record of, such as a suite's machine", func(ctx context.Context) {
		f.host.add("mill-acceptance-x-0badcafe")
		f.host.add("not-mills-machine")

		report := reconcile(ctx)

		Expect(f.host.registered()).To(ConsistOf("not-mills-machine"), "only mill-* machines are mill's to reconcile")
		Expect(f.host.CallsTo(driver.Terminate)).To(ContainElement(driver.Call{Method: driver.Terminate, Args: []any{"mill-acceptance-x-0badcafe"}}),
			"no record says it reported ready, so it is terminated, never stopped")
		Expect(report.Of(machine.FindingOrphan)).To(HaveLen(1))
		Expect(report.Of(machine.FindingOrphan)[0].Action).To(HavePrefix("terminated"))
	})

	// Scenario: A live run with no machine fails as a lost sandbox
	//   Given a live run at step 2 and no machine for it
	//   When the worker starts
	//   Then the run fails with lost sandbox as evidence
	//   And no new machine is provisioned
	//
	// The failure itself is the run loop's write (internal/run, the same
	// scenario): what this backend does is record the lost sandbox at
	// start, refuse to attach with the evidence, and boot nothing.
	It("A live run with no machine fails as a lost sandbox", func(ctx context.Context) {
		By("given a live run at step 2 and no machine for it")
		ws := f.workspace()
		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#63", Origin: f.config.Origin})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.host.Terminate(ctx, "mill-"+id)).To(Succeed(), "the machine died with the host")
		ledger.live = []machine.LiveRun{{RunID: runID, NodeRef: "mill#63", Step: "implement"}}
		boots := len(f.host.CallsTo(driver.StartUnit))

		By("when the worker starts")
		report := reconcile(ctx)

		By("then the lost sandbox is recorded with the machine and the ledger's step as evidence")
		lost := report.Of(machine.FindingLostSandbox)
		Expect(lost).To(HaveLen(1))
		Expect(lost[0].Name).To(Equal(runID))
		Expect(lost[0].Detail).To(ContainSubstring("mill-" + id))
		Expect(lost[0].Detail).To(ContainSubstring("implement"))

		By("and no new machine is provisioned: the record stays, and attaching to it is the refusal")
		Expect(f.runDir()).To(BeADirectory(), "the run directory is not a leftover while the run is live")
		_, err = f.workspace().Attach(ctx, workspace.Spec{RunID: runID})
		Expect(err).To(MatchError(workspace.ErrLostSandbox))
		Expect(err).To(MatchError(ContainSubstring("mill-" + id)))
		Expect(f.host.CallsTo(driver.StartUnit)).To(HaveLen(boots))
	})

	// Scenario: A live run resumes at its step with a fresh slice
	//   Given a live run at step 2 with a process still in its slice
	//   When the worker starts
	//   Then the slice is stopped and recreated and the run resumes at step 2
	It("A live run resumes at its step with a fresh slice", func(ctx context.Context) {
		By("given a live run at step 2 with a process still in its slice, and a candidate from step 1")
		first := f.workspace()
		_, err := first.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#63", Origin: f.config.Origin})
		Expect(err).NotTo(HaveOccurred())
		Expect(first.BeginStep(ctx, 1)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(f.runDir(), "cache", "target", "built"), []byte("step 1\n"), 0o600)).To(Succeed())
		Expect(first.EndStep(ctx, 1, true)).To(Succeed())
		Expect(first.BeginStep(ctx, 2)).To(Succeed())
		// A process is started in the slice, and the worker dies here:
		// nothing ends step 2, so the slice and whatever it runs stay up.
		process, err := first.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}})
		Expect(err).NotTo(HaveOccurred())
		drainProcess(process)
		ledger.live = []machine.LiveRun{{RunID: runID, NodeRef: "mill#63", Step: "implement"}}
		stopsBefore := len(f.host.CallsTo(driver.StopUnit))

		By("when the worker starts")
		report := reconcile(ctx)

		By("then the slice is stopped")
		Expect(f.host.CallsTo(driver.StopUnit)[stopsBefore:]).To(ContainElement(
			driver.Call{Method: driver.StopUnit, Args: []any{"mill-" + id, "mill-step-2.slice"}}))
		resumable := report.Of(machine.FindingResumable)
		Expect(resumable).To(HaveLen(1))
		Expect(resumable[0].Name).To(Equal(runID))
		Expect(resumable[0].Action).To(Equal("stopped mill-step-2.slice"))
		Expect(resumable[0].Detail).To(ContainSubstring("implement"))
		Expect(f.host.registered()).To(ConsistOf("mill-"+id), "the machine is not touched")

		By("and recreated: the attached workspace resumes at the same ordinal")
		second := f.workspace()
		_, err = second.Attach(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#63"})
		Expect(err).NotTo(HaveOccurred())
		Expect(second.NextOrdinal()).To(Equal(2), "the reaped slice is recreated under its own ordinal")
		Expect(second.BeginStep(ctx, 2)).To(Succeed())
		_, err = second.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).NotTo(HaveOccurred())
		requests := execRequestsAfter(f.host, 0)
		Expect(requests[len(requests)-1].Slice).To(Equal("mill-step-2.slice"))
		Expect(second.EndStep(ctx, 2, false)).To(Succeed())
		Expect(second.NextOrdinal()).To(Equal(3), "and the count continues from there")

		By("and the candidate from step 1 still promotes when the run finishes")
		Expect(second.Destroy(ctx)).To(Succeed())
		lineage := filepath.Join(f.root, "cache", "repo")
		matches, err := filepath.Glob(filepath.Join(lineage, "*", "target", "gen-1", "built"))
		Expect(err).NotTo(HaveOccurred())
		Expect(matches).To(HaveLen(1), "step 1's candidate became generation 1")
	})

	// The ledger is the record and run.json the host's copy (docs/ledger.md
	// §workspaces): when the two name different machines, the pass says so
	// and acts on the ledger's.
	It("records a finding naming both when the ledger and run.json disagree on the machine, and acts on the ledger's", func(ctx context.Context) {
		By("given a live run whose run.json names one machine and whose ledger row names another")
		ws := f.workspace()
		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#80", Origin: f.config.Origin})
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		theirs := "mill-" + id
		ledgers := "mill-somebodyelse"
		f.host.add(ledgers)
		ledger.live = []machine.LiveRun{{RunID: runID, NodeRef: "mill#80", Step: "implement", Machine: ledgers}}
		stopsBefore := len(f.host.CallsTo(driver.StopUnit))

		By("when the worker starts")
		report := reconcile(ctx)

		By("then the disagreement is recorded, naming both")
		disagreements := report.Of(machine.FindingDisagreement)
		Expect(disagreements).To(HaveLen(1))
		Expect(disagreements[0].Name).To(Equal(runID))
		Expect(disagreements[0].Detail).To(ContainSubstring(ledgers))
		Expect(disagreements[0].Detail).To(ContainSubstring(theirs))
		Expect(disagreements[0].Detail).To(ContainSubstring("run.json"))

		By("and the pass acts on the ledger's: that machine is the run's, and its slice is the one stopped")
		Expect(f.host.CallsTo(driver.StopUnit)[stopsBefore:]).To(ContainElement(
			driver.Call{Method: driver.StopUnit, Args: []any{ledgers, "mill-step-1.slice"}}))
		resumable := report.Of(machine.FindingResumable)
		Expect(resumable).To(HaveLen(1))
		Expect(resumable[0].Detail).To(ContainSubstring(ledgers))
		Expect(report.Of(machine.FindingLostSandbox)).To(BeEmpty(), "the ledger's machine is registered, so nothing is lost")
		Expect(f.host.registered()).To(ContainElement(ledgers), "the ledger's machine is not an orphan")
	})

	// Scenario: Scratch projects are swept by name
	//   Given stale mill-acceptance-* projects in dagger and on the forge with no live run
	//   When the worker reconciles
	//   Then each is archived or deleted and named in the report
	It("Scratch projects are swept by name", func(ctx context.Context) {
		By("given stale mill-acceptance-* projects in dagger and on the forge with no live run")
		stale := now.Add(-2 * time.Hour)
		fresh := now.Add(-time.Minute)
		dagger := &fakeExternal{kind: "dagger", action: "archived (dagger offers no project delete)", listed: []machine.Scratch{
			{Name: "mill-acceptance-x-0badcafe", CreatedAt: stale},
			{Name: "mill-acceptance-y-deadbeef", CreatedAt: fresh},
			{Name: "mill-acceptance-live-cafef00d", CreatedAt: stale},
			{Name: "mill", CreatedAt: stale},
			{Name: "mill-acceptance", CreatedAt: stale},
		}}
		forge := &fakeExternal{kind: "forge", action: "deleted", listed: []machine.Scratch{
			{Name: "mill-acceptance-x-0badcafe", CreatedAt: stale},
			{Name: "homestead", CreatedAt: stale},
		}}
		ledger.live = []machine.LiveRun{{RunID: runID, NodeRef: "mill-acceptance-live-cafef00d#1", Step: "implement"}}

		By("when the worker reconciles")
		report := reconcile(ctx, dagger, forge)

		By("then each is archived or deleted and named in the report")
		Expect(dagger.names()).To(Equal([]string{"mill-acceptance-x-0badcafe"}),
			"stale and unclaimed: not the fresh one, not the one a live run works, never a name outside the pattern")
		Expect(forge.names()).To(Equal([]string{"mill-acceptance-x-0badcafe"}))
		swept := report.Of(machine.FindingSwept)
		Expect(swept).To(HaveLen(2))
		Expect(swept[0]).To(Equal(machine.Finding{
			Kind: machine.FindingSwept, Name: "mill-acceptance-x-0badcafe",
			Action: "archived (dagger offers no project delete)", Detail: "dagger project, 2h0m0s old",
		}))
		Expect(swept[1]).To(Equal(machine.Finding{
			Kind: machine.FindingSwept, Name: "mill-acceptance-x-0badcafe",
			Action: "deleted", Detail: "forge project, 2h0m0s old",
		}))
	})

	It("removes a leftover image build and a run directory with no live run at start", func(ctx context.Context) {
		By("given mill-image-<digest>.building and a run directory with no live run")
		digest := image.Digest("0000000000000000000000000000000000000000000000000000000000000063")
		building := filepath.Join(f.machines, image.Prefix+string(digest)+image.BuildingSuffix)
		Expect(os.MkdirAll(building, 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(f.root, "images"), 0o750)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(f.root, "images", string(digest)+".lock"), nil, 0o640)).To(Succeed())
		stray := filepath.Join(f.root, "runs", "01jstrayrundirectory000000")
		Expect(os.MkdirAll(filepath.Join(stray, "workspace"), 0o755)).To(Succeed())
		kept := filepath.Join(f.root, "runs", "01jliverunelsewhere0000000")
		Expect(os.MkdirAll(kept, 0o755)).To(Succeed())
		ledger.live = []machine.LiveRun{{RunID: "01JLIVERUNELSEWHERE0000000", NodeRef: "mill#64", Step: "implement"}}

		By("when the worker starts")
		report := reconcile(ctx)

		By("then both are removed, and the live run's directory is not")
		Expect(building).NotTo(BeADirectory())
		Expect(stray).NotTo(BeADirectory())
		Expect(kept).To(BeADirectory())
		Expect(f.host.CallsTo(driver.DeleteSubvolume)).To(ContainElement(
			driver.Call{Method: driver.DeleteSubvolume, Args: []any{building, false}}), "the subvolume goes through the driver")
		leftovers := report.Of(machine.FindingLeftover)
		Expect(leftovers).To(HaveLen(2))
		Expect(leftovers[0].Name).To(Equal(building))
		Expect(leftovers[1].Name).To(Equal(stray))
	})

	It("does not reconcile at all when the ledger cannot say which runs are live", func(ctx context.Context) {
		f.host.add("mill-" + id)
		ledger.err = errors.New("the store is down")

		_, err := machine.NewReconciler(f.host, f.config).Reconcile(ctx, ledger)

		Expect(err).To(MatchError(ContainSubstring("the store is down")))
		Expect(f.host.registered()).To(ConsistOf("mill-"+id), "nothing is stopped on a guess")
	})

	It("matches scratch names by the suite's pattern and nothing looser", func() {
		Expect(machine.IsScratch("mill-acceptance-quick-fix-0badcafe")).To(BeTrue())
		Expect(machine.IsScratch("mill-acceptance-x-0badcafe")).To(BeTrue())
		Expect(machine.IsScratch("mill")).To(BeFalse())
		Expect(machine.IsScratch("mill-acceptance")).To(BeFalse())
		Expect(machine.IsScratch("mill-acceptance-x")).To(BeFalse())
		Expect(machine.IsScratch("mill-acceptance-x-nonce")).To(BeFalse(), "the nonce is eight hex digits")
	})
})
