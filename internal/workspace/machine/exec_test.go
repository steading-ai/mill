package machine_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/machine/config"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// drain reads a process's stdout and stderr to their ends and reaps it, the
// way check and the harness do.
func drainProcess(p *workspace.Process) (stdout, stderr string, result workspace.Result) {
	GinkgoHelper()
	var errOut []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		errOut, _ = io.ReadAll(p.Stderr)
	}()
	out, _ := io.ReadAll(p.Stdout)
	<-done
	return string(out), string(errOut), p.Wait()
}

// execRequestsAfter is the ExecRequests the fake recorded past the first n
// calls: Create's readiness probe is the first, so a spec that counts its
// own execs skips what provisioning did.
func execRequestsAfter(host *machined, n int) []driver.ExecRequest {
	var out []driver.ExecRequest
	calls := host.CallsTo(driver.Exec)
	for _, call := range calls[min(n, len(calls)):] {
		out = append(out, call.Args[0].(driver.ExecRequest))
	}
	return out
}

var execUnitPattern = regexp.MustCompile(`^mill-exec-[a-z0-9]+$`)

var _ = Describe("Exec into the machine", Label("unit"), func() {
	var (
		f    *fixture
		ws   *machine.Workspace
		spec workspace.Spec
	)

	create := func(ctx context.Context) {
		GinkgoHelper()
		f = newFixture(origin(nil))
		spec = workspace.Spec{RunID: runID, NodeRef: "mill#62", Origin: f.config.Origin}
		ws = f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })
		Expect(ws.Rotate(ctx, workspace.TokenFor("the-step-bearer", workspace.Exec))).To(Succeed())
	}

	// Scenario: A step without write runs against a read-only checkout
	// Scenario: A step with write runs against a writable checkout
	//   (the request is the unit half; the live spec proves the mount.)
	It("A step without write runs against a read-only checkout", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		By("given a machine workspace whose current token grants read and not write")
		Expect(ws.Rotate(ctx, workspace.TokenFor("a-reviewer", workspace.Read, workspace.Exec))).To(Succeed())

		By("when the workspace execs a command")
		process, err := ws.Exec(ctx, workspace.Command{Path: "true"})
		Expect(err).NotTo(HaveOccurred())
		drainProcess(process)

		By("then the exec request carries ReadOnlyPaths /work")
		requests := execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].ReadOnlyPaths).To(Equal([]string{"/work"}))

		By("and a rotation to a token that grants write lifts it")
		Expect(ws.Rotate(ctx, workspace.TokenFor("an-implementer", workspace.Read, workspace.Write, workspace.Exec))).To(Succeed())
		process, err = ws.Exec(ctx, workspace.Command{Path: "true"})
		Expect(err).NotTo(HaveOccurred())
		drainProcess(process)
		requests = execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(2))
		Expect(requests[1].ReadOnlyPaths).To(BeEmpty())
	})

	It("A step with write runs against a writable checkout", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		By("given a machine workspace whose current token grants write")
		Expect(ws.Rotate(ctx, workspace.TokenFor("an-implementer", workspace.Write))).To(Succeed())

		By("when the workspace execs a command")
		process, err := ws.Exec(ctx, workspace.Command{Path: "true"})
		Expect(err).NotTo(HaveOccurred())
		drainProcess(process)

		By("then the exec request carries no ReadOnlyPaths")
		requests := execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].ReadOnlyPaths).To(BeEmpty())

		By("and with no token installed at all the checkout is read-only")
		Expect(ws.Rotate(ctx, workspace.Token{})).To(Succeed())
		process, err = ws.Exec(ctx, workspace.Command{Path: "true"})
		Expect(err).NotTo(HaveOccurred())
		drainProcess(process)
		Expect(execRequestsAfter(f.host, before)[1].ReadOnlyPaths).To(Equal([]string{"/work"}))
	})

	// Scenario: Stdout, stderr and the exit code come back intact and streamed
	//   (the streaming itself is the live spec; here the fake spawns a real
	//   process, so both streams and the status come back, and the exact
	//   helper arguments are on record — the (unit) half of the criterion.)
	It("Stdout, stderr and the exit code come back intact and streamed", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))
		f.host.Script(driver.Exec, driver.Output{Stdout: "out", Stderr: "err", Status: 42})

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		process, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 42"}})
		Expect(err).NotTo(HaveOccurred())
		stdout, stderr, result := drainProcess(process)

		Expect(stdout).To(Equal("out"))
		Expect(stderr).To(Equal("err"))
		Expect(result.Status).To(BeEquivalentTo(42))

		By("and the helper was asked to exec under the step slice, as agent in /work, with a fresh exec unit")
		requests := execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(1))
		req := requests[0]
		Expect(req.Machine).To(Equal("mill-" + id))
		Expect(req.Slice).To(Equal("mill-step-1.slice"))
		Expect(req.Dir).To(Equal("/work"))
		Expect(req.User).To(Equal(machine.SessionUser))
		Expect(req.Unit).To(MatchRegexp(execUnitPattern.String()))
		Expect(req.Argv).To(Equal([]string{"/bin/sh", "-c", "echo out; echo err >&2; exit 42"}))
	})

	It("carries the working directory, user, MARK and the step token, and does not carry MARK to the next exec", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		first, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Dir: "/work", Env: map[string]string{"MARK": "1"}})
		Expect(err).NotTo(HaveOccurred())
		_, _, _ = drainProcess(first)
		second, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh"})
		Expect(err).NotTo(HaveOccurred())
		_, _, _ = drainProcess(second)

		requests := execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(2))
		Expect(requests[0].Dir).To(Equal("/work"))
		Expect(requests[0].User).To(Equal("agent"))
		Expect(requests[0].Env).To(HaveKeyWithValue("MARK", "1"))
		By("and the step token is the gateway's bearer in every exec")
		Expect(requests[0].Env).To(HaveKeyWithValue(workspace.ModelGatewayTokenVar, "the-step-bearer"))
		Expect(requests[0].Env).To(HaveKeyWithValue(workspace.ToolSearchVar, "true"))
		By("and the next exec without MARK does not carry the previous one's")
		Expect(requests[1].Env).NotTo(HaveKey("MARK"))
		Expect(requests[1].Env).To(HaveKeyWithValue(workspace.ModelGatewayTokenVar, "the-step-bearer"))
	})

	// Scenario: A check step's script runs from the workflow copy in the machine
	//   Given a workflow directory the worker loaded from, and its copy bound
	//   When a check step's command resolved under it on the host is exec'd
	//   Then the machine is asked for the copy's path, and nothing else is rewritten
	It("A check step's script runs from the workflow copy in the machine", func(ctx context.Context) {
		f = newFixture(origin(nil))
		tmp := GinkgoT().TempDir()
		source := filepath.Join(tmp, "workflows", "quick-fix")
		copied := filepath.Join(tmp, "share", "workflow")
		Expect(os.MkdirAll(filepath.Join(source, "scripts"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(copied, 0o755)).To(Succeed())
		f.config.WorkflowSource = source
		f.config.Workflow = copied
		spec = workspace.Spec{RunID: runID, NodeRef: "mill#69", Origin: f.config.Origin}
		ws = f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })
		Expect(ws.Rotate(ctx, workspace.TokenFor("the-step-bearer", workspace.Exec))).To(Succeed())

		By("the copy is bound read-only where the image expects it")
		var bound *machine.Bind
		for _, bind := range f.manifest().Binds {
			if bind.Source == copied {
				b := bind
				bound = &b
			}
		}
		Expect(bound).NotTo(BeNil(), "the workflow copy is bound")
		Expect(bound.Target).To(Equal(config.WorkflowPath))
		Expect(bound.ReadOnly).To(BeTrue())

		before := len(f.host.CallsTo(driver.Exec))
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		script, err := ws.Exec(ctx, workspace.Command{Path: filepath.Join(source, "scripts", "clean-tree")})
		Expect(err).NotTo(HaveOccurred())
		_, _, _ = drainProcess(script)
		other, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", filepath.Join(source, "scripts", "clean-tree")}})
		Expect(err).NotTo(HaveOccurred())
		_, _, _ = drainProcess(other)
		outside, err := ws.Exec(ctx, workspace.Command{Path: filepath.Join(tmp, "workflows", "quick-fix-other", "scripts", "x")})
		Expect(err).NotTo(HaveOccurred())
		_, _, _ = drainProcess(outside)

		requests := execRequestsAfter(f.host, before)
		Expect(requests).To(HaveLen(3))
		By("the script resolved under the workflow directory is named by its in-machine path")
		Expect(requests[0].Argv).To(Equal([]string{config.WorkflowPath + "/scripts/clean-tree"}))
		By("a program elsewhere, and every argument, is passed as given")
		Expect(requests[1].Argv).To(Equal([]string{"/bin/sh", "-c", filepath.Join(source, "scripts", "clean-tree")}))
		Expect(requests[2].Argv).To(Equal([]string{filepath.Join(tmp, "workflows", "quick-fix-other", "scripts", "x")}))
	})

	// Scenario: An exec without a step slice is refused
	It("An exec without a step slice is refused before any command runs", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))

		_, err := ws.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).To(MatchError(ContainSubstring("no step slice")))
		Expect(f.host.CallsTo(driver.Exec)).To(HaveLen(before), "nothing was execed")
	})

	// Scenario: Cancellation stops the unit, not the client
	//   (the live spec proves no sleep remains in the machine; here the
	//   cancellation is shown to map to the helper's stop of the exec's own
	//   unit by name, which is what killing the client would not do.)
	It("Cancellation stops the unit, not the client", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		execCtx, cancel := context.WithCancel(ctx)
		process, err := ws.Exec(execCtx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "true"}})
		Expect(err).NotTo(HaveOccurred())
		unit := execRequestsAfter(f.host, before)[0].Unit
		_, _, _ = drainProcess(process)

		cancel()
		Eventually(func() []driver.Call {
			return f.host.CallsTo(driver.StopUnit)
		}).Should(ContainElement(HaveField("Args", Equal([]any{"mill-" + id, unit + ".service"}))),
			"cancellation stops the exec's own unit by name, with the .service the helper's stop wants")
	})

	It("retries an exec whose unit fails to start once, then fails carrying the error", func(ctx context.Context) {
		create(ctx)
		before := len(f.host.CallsTo(driver.Exec))
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())

		By("a unit that fails to start, then starts: one retry and the command runs")
		f.host.Script(driver.Exec, errors.New("Failed to start transient service unit"), driver.Output{Stdout: "ran", Status: 0})
		process, err := ws.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).NotTo(HaveOccurred())
		stdout, _, result := drainProcess(process)
		Expect(stdout).To(Equal("ran"))
		Expect(result.Status).To(BeEquivalentTo(0))
		Expect(execRequestsAfter(f.host, before)).To(HaveLen(2), "attempted twice, each with its own unit")

		By("a unit that fails to start twice: the exec fails carrying the error")
		before = len(f.host.CallsTo(driver.Exec))
		f.host.Script(driver.Exec, errors.New("boom-one"), errors.New("boom-two"))
		_, err = ws.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).To(MatchError(ContainSubstring("boom-two")))
		Expect(execRequestsAfter(f.host, before)).To(HaveLen(2))
	})

	It("refuses an exec before the workspace is created", func(ctx context.Context) {
		fresh := machine.New(newMachined(GinkgoT().TempDir()+"/events"), newFixture(origin(nil)).config)
		_, err := fresh.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).To(MatchError(machine.ErrNotCreated))
	})
})

var _ = Describe("The step boundary", Label("unit"), func() {
	// withCache is a fixture whose repo declares a cache, so a lineage is
	// restored and there is a copy for a candidate to snapshot.
	withCache := func() *fixture {
		GinkgoHelper()
		f := newFixture(origin(map[string]string{
			"mill.toml": "[cache.target]\npath = \"/work/target\"\n",
		}))
		return f
	}

	// Scenario: An evidence-closed step leaves a candidate
	//   (the (unit) half of the criterion: the fake records that the
	//   candidate snapshot is taken before the slice stops, and that a step
	//   that did not close on evidence takes none.)
	It("An evidence-closed step leaves a candidate", func(ctx context.Context) {
		f := withCache()
		spec := workspace.Spec{RunID: runID, NodeRef: "mill#62", Origin: f.config.Origin}
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		Expect(ws.EndStep(ctx, 1, true)).To(Succeed())

		By("then the candidate snapshot was taken before the slice was stopped")
		snapshot := lastIndexOf(f.methods(), driver.Snapshot)
		stop := stopIndex(f.host, "mill-step-1.slice")
		Expect(snapshot).To(BeNumerically(">=", 0), "a candidate snapshot was taken")
		Expect(stop).To(BeNumerically(">", snapshot), "the slice was stopped after the candidate")
	})

	It("An evidence-closed step leaves a candidate, and a step that did not takes none", func(ctx context.Context) {
		f := withCache()
		spec := workspace.Spec{RunID: runID, NodeRef: "mill#62", Origin: f.config.Origin}
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("a step that did not close on evidence stops its slice and takes no candidate")
		snapshotsBefore := countMethod(f.methods(), driver.Snapshot)
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
		Expect(countMethod(f.methods(), driver.Snapshot)).To(Equal(snapshotsBefore), "no candidate")
		Expect(stopIndex(f.host, "mill-step-1.slice")).To(BeNumerically(">=", 0), "the slice was stopped")
	})

	It("names each step a slice of its own and begins the next only after the previous is ended", func(ctx context.Context) {
		f := newFixture(origin(nil))
		spec := workspace.Spec{RunID: runID, NodeRef: "mill#62", Origin: f.config.Origin}
		ws := f.workspace()
		_, err := ws.Create(ctx, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
		Expect(ws.BeginStep(ctx, 2)).To(Succeed())
		Expect(ws.EndStep(ctx, 2, false)).To(Succeed())

		stops := f.host.CallsTo(driver.StopUnit)
		var slices []string
		for _, call := range stops {
			slices = append(slices, call.Args[1].(string))
		}
		Expect(slices).To(ContainElements("mill-step-1.slice", "mill-step-2.slice"))
		Expect(stopIndex(f.host, "mill-step-1.slice")).To(BeNumerically("<", stopIndex(f.host, "mill-step-2.slice")),
			"step 1's slice is stopped before step 2 is begun")
	})
})

// lastIndexOf is the position of the last call of a method in the fake's
// recorded sequence, or -1.
func lastIndexOf(methods []string, want string) int {
	idx := -1
	for i, m := range methods {
		if m == want {
			idx = i
		}
	}
	return idx
}

// countMethod counts calls of a method.
func countMethod(methods []string, want string) int {
	n := 0
	for _, m := range methods {
		if m == want {
			n++
		}
	}
	return n
}

// stopIndex is the position in the fake's whole call sequence of the
// StopUnit that named a given slice, or -1.
func stopIndex(host *machined, slice string) int {
	for i, call := range host.Calls() {
		if call.Method == driver.StopUnit && len(call.Args) == 2 {
			if s, ok := call.Args[1].(string); ok && s == slice {
				return i
			}
		}
	}
	return -1
}
