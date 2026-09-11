package acceptance_test

// The live tier: the sandbox's counterpart of "One node, delivered". It is
// what proves docs/sandbox.md against nothing fake — a real machine on
// `core`, booted by `mill run --workspace machine`, its properties asserted
// from outside the machine while the run is in flight and from the host's
// btrfs volume after it. It is the node the bootstrap gate (`prove-sandbox`)
// runs, and docs/runbook.md §"The live tier" is what it needs, what it
// leaves behind, and how to run it one scenario at a time.
//
// Off unless MILL_SANDBOX_HOST says so, and off with the whole acceptance
// suite when MILL_ACCEPTANCE is unset (the BeforeSuite skips first). So
// `make test` runs it and it skips; on `core`, with the host prepared by
// homestead's `mill-role-sandbox` and both variables set, it runs. The
// scenarios are independent and each is `--focus`able by its name, because
// the first host run is debugged scenario by scenario.
//
// Every machine the suite boots itself is named `mill-acceptance-<scenario>-
// <nonce>` so §External state's sweep finds it after a kill; a machine
// `mill run` boots carries the run's own id and is swept as any `mill-*`
// orphan with no live run. Every network assertion runs from *inside* the
// machine, never from the host with a source-interface trick: squid refuses
// host processes by design (homestead#40), so a host probe would be denied
// whatever the machine's policy is.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

const (
	// sandboxHostVar turns the live tier on. Unset, every spec here skips
	// with sandboxSkipMessage, because a machine costs a real boot, a real
	// session, and a merge on a scratch project.
	sandboxHostVar = "MILL_SANDBOX_HOST"

	// sandboxSkipMessage is the words the existing `live` specs use, so a
	// reader who has seen one skip has seen them all (docs/sandbox.md
	// §Testing).
	sandboxSkipMessage = "set MILL_SANDBOX_HOST=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine"

	// sandboxModelCredentialVar names the `claude setup-token` file the
	// credential gateway presents upstream for a machine's sessions. No
	// login enters a machine, so a machine run has no account credential of
	// its own; the operator running the live tier supplies the setup-token
	// the deployed worker loads from its vault. Unset, the live tier refuses
	// before booting anything, because a session with no way to the model
	// would fail every skill step for a reason that is not the sandbox's.
	sandboxModelCredentialVar = "MILL_SANDBOX_MODEL_CREDENTIAL" //nolint:gosec // the name of a variable, not a credential

	// sandboxRoot is the btrfs volume homestead's `mill_storage` role mounts
	// on `core` (docs/sandbox.md §Storage): where a machine run's directory,
	// its `run.json`, its cache copies and the promoted generations live. The
	// suite reads them back from here to assert what the run recorded and
	// what it promoted.
	sandboxRoot = "/var/lib/mill"

	// sandboxHelper is the root-owned machine helper homestead installs
	// (docs/sandbox.md §Provision). The suite reaches into a machine through
	// it under `sudo -n`, exactly as the worker does, to probe the machine's
	// boundaries from the host while a run is in flight.
	sandboxHelper = "/usr/libexec/mill-machine"
)

// sandboxSkip skips the calling spec when the host was not asked for the
// live tier. It is the BeforeEach of the live Describe, so a laptop running
// `make test-acceptance` runs every other scenario and skips these with the
// message that names their cost.
func sandboxSkip() {
	GinkgoHelper()
	if os.Getenv(sandboxHostVar) == "" {
		Skip(sandboxSkipMessage)
	}
}

// sandboxRefusal is why the live tier will not start against the dagger the
// environment named, or empty when it is safe. The live tier boots machines
// through a root-owned unit and drives `mill run` subprocesses inside them,
// and mill#49 already showed a test process becoming a worker against the
// live graph with no session involved; a flag a stray variable can flip is
// not a safe default for that tier (docs/sandbox.md §Testing). So the
// deployed dagger is refused whether it was named by MILL_ACCEPTANCE_DAGGER_
// SOCKET or reached by DG_SOCKET, and the message names the socket and the
// fixture to use instead.
func sandboxRefusal(daggerSocket string) string {
	if daggerSocket == "" || !namesDeployedDagger(daggerSocket) {
		return ""
	}
	return fmt.Sprintf(
		"the live tier will not file into the deployed dagger at %s: it boots real machines and drives real `mill run` "+
			"subprocesses, and a worker loosed on the operator's graph is exactly what mill#49 found; unset %s and let the "+
			"suite start its own throwaway dagger (internal/testsupport, docs/runbook.md §\"Which dagger\")",
		daggerSocket, DaggerSocketVar)
}

// namesDeployedDagger reports whether a socket is the operator's deployed
// one, following symlinks so a socket reached by another path is still
// recognised. The deployed socket is /run/dagger/dg.sock (environment_test's
// deployedDaggerSocket); a socket in a temporary directory, which is where
// the suite's own dagger and an external test dagger listen, is not it.
func namesDeployedDagger(socket string) bool {
	if socket == deployedDaggerSocket {
		return true
	}
	resolved, err := filepath.EvalSymlinks(socket)
	if err != nil {
		return false
	}
	return resolved == deployedDaggerSocket
}

// The live tier refuses the deployed dagger — the one scenario here that
// needs no host, because refusing is a property of how the tier reads its
// environment and not of any machine. It runs under `make test-acceptance`
// on any host and, like every acceptance spec, skips under `make test` when
// MILL_ACCEPTANCE is unset.
var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: The live tier refuses the deployed dagger
	//   Given the environment names /run/dagger/dg.sock
	//   When the live tier starts
	//   Then it refuses before booting anything
	It("The live tier refuses the deployed dagger", func() {
		By("given the environment names /run/dagger/dg.sock")
		Expect(deployedDaggerSocket).To(Equal("/run/dagger/dg.sock"))

		By("when the live tier starts")
		refusal := sandboxRefusal(deployedDaggerSocket)

		By("then it refuses before booting anything")
		Expect(refusal).To(SatisfyAll(
			ContainSubstring(deployedDaggerSocket),
			ContainSubstring(DaggerSocketVar),
			ContainSubstring("internal/testsupport")),
			"the refusal names the socket and the fixture to use instead")
		// A socket in a directory the suite owns — its own throwaway dagger,
		// or an external test dagger — is not the deployed one and is not
		// refused.
		Expect(sandboxRefusal(filepath.Join(os.TempDir(), "mill-dagger-abc", "dg.sock"))).To(BeEmpty())
		Expect(sandboxRefusal("")).To(BeEmpty())
	})
})

// hostRun runs a command on the host and returns its two streams and status.
// A command that could not be started at all — the binary is not there —
// fails the spec, because that is the host not being prepared rather than a
// command that ran and said no.
func hostRun(ctx context.Context, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the suite's own probes of a prepared host
	var out, errb strings.Builder
	command.Stdout, command.Stderr = &out, &errb
	err := command.Run()
	if command.ProcessState == nil {
		Expect(err).NotTo(HaveOccurred(), "%s could not be started", strings.Join(argv, " "))
	}
	return out.String(), errb.String(), command.ProcessState.ExitCode()
}

// hostMust runs a host command and fails the spec unless it exits zero.
func hostMust(ctx context.Context, argv ...string) string {
	GinkgoHelper()
	out, errb, status := hostRun(ctx, argv...)
	Expect(status).To(BeZero(), "%s: %s%s", strings.Join(argv, " "), out, errb)
	return out
}

// inMachine runs a command inside a machine as `agent`, through the helper
// under `sudo -n`, and returns its combined output and status. It is how the
// suite inspects a machine's boundaries from the host while a run is in
// flight: the run owns its own step slices, so the probe runs under a slice
// of its own, `mill-step-probe.slice`, which the machine's teardown reaps
// with everything else.
//
// This is the same channel the worker uses (docs/sandbox.md §Exec), so what
// it proves — the session's uid, its unreachable home, its egress — is what a
// session inside the machine would see, asserted from outside it.
func inMachine(ctx context.Context, machine string, argv ...string) (combined string, status int) {
	GinkgoHelper()
	args := []string{"-n", sandboxHelper, "exec", machine,
		"-p", "Slice=mill-step-probe.slice", "-p", "WorkingDirectory=/work", "-p", "User=" + "agent", "--"}
	args = append(args, argv...)
	out, errb, status := hostRun(ctx, append([]string{"sudo"}, args...)...)
	return out + errb, status
}

// sandboxRecord is the run's own note of its machine, `runs/<id>/run.json`
// on the host, as much of it as the live tier reads back: the machine's name,
// the digest it booted, the veth the host addresses it on, and the lineages
// it restored. It is docs/sandbox.md §Reconciliation's per-run record — the
// ledger's copy travels on the workspace handle as workspace.Sandbox — and
// the suite reads this one because it is on a file it can open and reach
// before the run's directory is removed at finish.
type sandboxRecord struct {
	RunID    string `json:"run_id"`
	ID       string `json:"id"`
	Machine  string `json:"machine"`
	Unit     string `json:"unit"`
	Image    string `json:"image"`
	Veth     string `json:"veth"`
	Lineages []struct {
		Lineage    string `json:"lineage"`
		Generation int    `json:"generation"`
	} `json:"lineages"`
	ReadyAt time.Time `json:"ready_at"`
}

// runDir is a run's directory on the host: the machine id is the run id in
// lower case (machine.IDFor), and everything on the host is named by it
// (docs/sandbox.md §Naming).
func runDir(runID string) string {
	return filepath.Join(sandboxRoot, "runs", strings.ToLower(runID))
}

// readRecord reads a run's `run.json`, or returns false when it is not there
// — the run has not booted its machine yet, or its directory has been removed
// at finish.
func readRecord(runID string) (sandboxRecord, bool) {
	body, err := os.ReadFile(filepath.Join(runDir(runID), "run.json")) //nolint:gosec // the run directory is the worker's own, the suite's account
	if err != nil {
		return sandboxRecord{}, false
	}
	var record sandboxRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return sandboxRecord{}, false
	}
	return record, true
}

// awaitMachine waits for a run to boot its machine and reach ready, and
// returns the record. It is how a from-the-host scenario finds the machine to
// probe: the record names it, and machinectl confirms it is running.
func awaitMachine(ctx context.Context, runID string, timeout time.Duration) sandboxRecord {
	GinkgoHelper()
	var record sandboxRecord
	Eventually(func() bool {
		var ok bool
		record, ok = readRecord(runID)
		if !ok || record.Machine == "" || record.ReadyAt.IsZero() {
			return false
		}
		state, _, _ := hostRun(ctx, "machinectl", "show", record.Machine, "-p", "State")
		return strings.TrimSpace(state) == "State=running"
	}, timeout, 2*time.Second).Should(BeTrue(), "run %s booted a machine and reached ready", runID)
	return record
}

// capturedRecords collects a run's `run.json` while the run is in flight,
// because the run's directory — and the record with it — is removed when the
// run finishes. A scenario that asserts what the ledger recorded reads the
// last snapshot the capture saw, taken while the machine was up.
type capturedRecords struct {
	done   chan struct{}
	latest chan sandboxRecord
}

// captureRecord starts polling a run's `run.json` and returns a handle whose
// stop yields the last record seen. A run that never booted a machine yields
// the zero record, which the caller asserts against.
func captureRecord(runID string) *capturedRecords {
	c := &capturedRecords{done: make(chan struct{}), latest: make(chan sandboxRecord, 1)}
	go func() {
		var last sandboxRecord
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				c.latest <- last
				return
			case <-ticker.C:
				if record, ok := readRecord(runID); ok && record.Machine != "" {
					last = record
				}
			}
		}
	}()
	return c
}

// stop ends the capture and returns the last record it saw.
func (c *capturedRecords) stop() sandboxRecord {
	close(c.done)
	return <-c.latest
}

// cacheGenerations are the promoted generations under the host's cache root:
// `<scope>/<name>/gen-<n>`, the read-only snapshots a run's evidence-closed
// steps left behind (docs/sandbox.md §Promote). A run that restored a
// lineage empty and promoted its build cache leaves a `gen-1` there, which is
// how the suite proves promotion after the run's own directory is gone.
func cacheGenerations(ctx context.Context) []string {
	GinkgoHelper()
	out := hostMust(ctx, "sudo", "-n", "btrfs", "subvolume", "list", "-o", sandboxRoot)
	var generations []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[len(fields)-1]
		if idx := strings.Index(name, "cache/"); idx >= 0 && strings.Contains(name[idx:], "/gen-") {
			generations = append(generations, name[idx:])
		}
	}
	return generations
}

// The machine fixture: a scratch repository whose `mise.toml` declares go and
// whose README carries a word to change, and a quick-fix node asking for the
// change. It is deliberately the smallest thing that still exercises the
// whole machine — a toolchain to install (§"Preparatory steps"), a go build
// cache to promote, and a one-word diff to commit and merge — because a
// session on the subscription and a merge on a scratch project cost real
// money every run (the work order's own instruction to keep the node small).
var machineTypo = fixture{
	Name: "machine-typo",
	Files: map[string]string{
		"mise.toml": "[tools]\ngo = \"1.25\"\n",
		"README.md": "# Scratch\n\nThis project exists so the sandbox can prove itself.\n\n" +
			"We " + misspelling + " the change inside a machine and merge it.\n",
	},
	Title: "Fix the typo in README.md: '" + misspelling + "' → '" + correction + "'",
	Body: "README.md contains the misspelling \"" + misspelling + "\". It should read \"" + correction + "\".\n\n" +
		"Change that one word and nothing else.\n",
}

var _ = Describe("Acceptance", Label("acceptance", "sandbox"), Label("live"), func() {
	BeforeEach(func() {
		sandboxSkip()
		// Refuse before booting anything: the live tier will not file into
		// the deployed dagger, whatever named it (docs/sandbox.md §Testing).
		if refusal := sandboxRefusal(env.DaggerSocket); refusal != "" {
			Fail(refusal)
		}
		// A machine's sessions reach the model through the gateway, which
		// needs the subscription credential the operator supplies. Named
		// before a boot, because a session with no way out fails every skill
		// step for a reason that is not the sandbox's.
		if os.Getenv(sandboxModelCredentialVar) == "" {
			Skip("set " + sandboxModelCredentialVar + " to the `claude setup-token` file the credential gateway presents upstream; " +
				"a machine's sessions have no login of their own (docs/sandbox.md §Credentials, docs/runbook.md §\"The live tier\")")
		}
	})

	// Scenario: One node, delivered, inside a machine
	//   Given core prepared and a scratch repository declaring go
	//   When mill run works a quick-fix node with workspace machine
	//   Then the node is done with a merge on the default branch
	//   And the ledger records the machine, the digest, and the lineages promoted
	It("One node, delivered, inside a machine", func(ctx SpecContext) {
		needsForge()

		By("given core prepared and a scratch repository declaring go")
		s := machineTypo.stage(ctx)
		Expect(s.Project.file(ctx, "README.md", s.Project.DefaultBranch)).To(ContainSubstring(misspelling)) //nolint:misspell // the misspelling is the fixture
		Expect(s.Project.file(ctx, "mise.toml", s.Project.DefaultBranch)).To(ContainSubstring("go"))

		By("when mill run works a quick-fix node with workspace machine")
		before := cacheGenerations(ctx)
		run := s.start(ctx, runOptions{
			Machine:         true,
			SandboxRoot:     sandboxRoot,
			ModelCredential: os.Getenv(sandboxModelCredentialVar),
		})
		// The record is captured while the run is in flight, because the run
		// directory — and run.json with it — is removed at finish.
		capture := captureRecord(s.runID(ctx))

		By("then the node is done with a merge on the default branch")
		Expect(run.wait(deliveredTimeout)).To(BeZero(), "mill run exited cleanly")
		record := capture.stop()
		view := s.Graph.show(ctx, s.NodeRef)
		Expect(view.Node.Status).To(Equal("done"))
		// The merge commit read off the run's own merge request — the mill's
		// forge identity merged it — rather than the node's merge-commit
		// artifact, which "One node, delivered" already covers; here the point
		// is that a machine's session reached the merge at all.
		branch := inspect(ctx, s.NodeRef).GetSummary().GetRun().GetBranch()
		mr := mergeRequestFor(s.Project.mergeRequests(ctx), branch)
		Expect(mr.State).To(Equal("merged"), "the run's branch was merged")
		merged := mr.MergeCommit
		Expect(merged).To(MatchRegexp(`^[0-9a-f]{40}$`), "the merge is a commit SHA")
		Expect(s.Project.branchesContaining(ctx, merged)).To(ContainElement(s.Project.DefaultBranch),
			"the SHA is on the default branch")
		readme := s.Project.file(ctx, "README.md", s.Project.DefaultBranch)
		Expect(readme).To(ContainSubstring(correction))
		Expect(readme).NotTo(ContainSubstring(misspelling)) //nolint:misspell // the misspelling is the fixture

		By("and every step's evidence is in the ledger")
		detail := inspect(ctx, s.NodeRef)
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		steps := latest(detail)
		for _, step := range quickFixSteps {
			d := steps[step.name]
			Expect(d).NotTo(BeNil(), "the ledger has an attempt of %s", step.name)
			Expect(d.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED), "%s closed", step.name)
			Expect(d.GetEvidence().GetKind()).To(Equal(step.evidence), "%s closed on its evidence", step.name)
		}

		By("and the ledger records the machine, the derived digest, the veth, and the lineages")
		AddReportEntry("run.json", record)
		Expect(record.Machine).To(HavePrefix("mill-"), "the run recorded its machine name")
		Expect(record.Image).To(MatchRegexp(`^[a-z0-9]+$`), "the derived digest the machine booted")
		Expect(record.Veth).NotTo(BeEmpty(), "the host-side veth the machine was reachable on")
		Expect(record.Lineages).NotTo(BeEmpty(), "the lineages the run restored")

		By("and the run promoted its build lineage")
		after := cacheGenerations(ctx)
		Expect(after).To(ContainElement(HaveSuffix("/go-build/gen-1")),
			"the evidence-closed steps promoted the go build cache; before=%v after=%v", before, after)
	})

	// Scenario: The db suite runs inside a machine
	//   Given a machine bound to a checkout of mill
	//   When a session runs make test-db against the machine's podman
	//   Then it exits 0 and no container remains after the step
	It("The db suite runs inside a machine", func(ctx SpecContext) {
		needsForge()

		By("given a machine bound to a checkout of mill")
		// The node is filed against the mill repository itself, so /work
		// inside the machine is a mill checkout `make test-db` can run in. The
		// run is left in flight — it need not deliver — and the machine is
		// probed while a step holds it up.
		s := millCheckout(ctx)
		run := s.start(ctx, runOptions{
			Machine:         true,
			SandboxRoot:     sandboxRoot,
			ModelCredential: os.Getenv(sandboxModelCredentialVar),
		})
		DeferCleanup(run.kill)
		machine := awaitMachine(ctx, s.runID(ctx), deliveredTimeout).Machine

		By("when a session runs make test-db against the machine's podman")
		// The image enables `podman.socket`; the rootless socket is the one
		// inside the machine (docs/sandbox.md §Network). The suite finds
		// whichever of the two the image landed on and points DOCKER_HOST at
		// it, exactly as a session's own shell would.
		socket := podmanSocket(ctx, machine)
		out, status := inMachine(ctx, machine, "sh", "-lc",
			"cd /work && DOCKER_HOST=unix://"+socket+" make test-db")
		AddReportEntry("make test-db", out)

		By("then it exits 0 and no container remains after the step")
		Expect(status).To(BeZero(), "make test-db inside the machine: %s", out)
		remaining, _ := inMachine(ctx, machine, "sh", "-lc",
			"DOCKER_HOST=unix://"+socket+" podman ps -aq")
		Expect(strings.TrimSpace(remaining)).To(BeEmpty(), "no container outlived the step")
	})

	// Scenario: The boundaries hold
	//   Given a run in flight inside a machine
	//   When the host inspects uids, mounts, network, credentials and processes
	//   Then the session is not the operator, the home is unreachable, direct
	//        egress times out, no login is in the machine, and nothing
	//        outlives its step
	It("The boundaries hold", func(ctx SpecContext) {
		needsForge()

		By("given a run in flight inside a machine")
		s := machineTypo.stage(ctx)
		run := s.start(ctx, runOptions{
			Machine:         true,
			SandboxRoot:     sandboxRoot,
			ModelCredential: os.Getenv(sandboxModelCredentialVar),
		})
		DeferCleanup(run.kill)
		machine := awaitMachine(ctx, s.runID(ctx), deliveredTimeout).Machine

		By("when the host inspects uids, mounts, network, credentials and processes")

		By("then the session's uid on the host is in the machine's range and not the operator's")
		leader := strings.TrimSpace(hostMust(ctx, "machinectl", "show", machine, "-p", "Leader", "--value"))
		Expect(leader).NotTo(BeEmpty())
		hostUID := strings.TrimSpace(hostMust(ctx, "sh", "-c", "stat -c %u /proc/"+leader))
		Expect(hostUID).NotTo(Equal(fmt.Sprintf("%d", os.Getuid())), "the machine's processes are not the operator's uid on the host")
		Expect(hostUID).NotTo(Equal("0"), "nor root's")

		By("and /home/ivy is not reachable from inside")
		out, status := inMachine(ctx, machine, "sh", "-c", "ls /home/ivy 2>&1; echo status=$?")
		Expect(status).NotTo(BeZero())
		Expect(out).NotTo(ContainSubstring(".ssh"), "the operator's home is not mounted")

		By("and a direct connection out times out and one through the proxy succeeds")
		// From inside the machine, per homestead#40: a host probe with a
		// source interface is denied by squid whatever the machine's policy,
		// so both probes run in the machine. The direct connection is dropped
		// by the host's nftables and times out; the proxied one is admitted.
		_, direct := inMachine(ctx, machine, "sh", "-c",
			"curl -sS --max-time 8 https://example.com/ -o /dev/null")
		Expect(direct).NotTo(BeZero(), "a direct connection is dropped and times out")
		_, proxied := inMachine(ctx, machine, "sh", "-c",
			"curl -sS --max-time 20 -x \"$HTTPS_PROXY\" https://example.com/ -o /dev/null")
		Expect(proxied).To(BeZero(), "a connection through the proxy the environment names succeeds")

		By("and ~/.claude holds no .credentials.json")
		creds, _ := inMachine(ctx, machine, "sh", "-c",
			"test -e /home/agent/.claude/.credentials.json && echo present || echo absent")
		Expect(strings.TrimSpace(creds)).To(Equal("absent"), "no model login enters the machine")

		By("and a process left behind by a step is gone once the run has moved on")
		// A daemon a session starts does not outlive the step's slice: the
		// step boundary reaps everything under it, containers included
		// (docs/sandbox.md §"Step boundary"). This is the coarse view from
		// outside — a marker process the suite starts under a step slice is
		// gone by the time the run's next step boundary or its teardown has
		// passed; the rigorous within-a-run step-1-to-step-2 proof, which
		// needs to drive the slices, is
		// internal/workspace/machine/live_test.go's "A background process
		// does not survive its step".
		_, _ = inMachine(ctx, machine, "sh", "-c", "setsid sleep 3600 >/dev/null 2>&1 &")
		Eventually(func() int {
			// The probe stops once the machine is gone: the run finished and
			// its teardown reaped every process with it.
			if _, _, s := hostRun(ctx, "machinectl", "show", machine, "-p", "State"); s != 0 {
				return 0
			}
			out, _ := inMachine(ctx, machine, "sh", "-c", "pgrep -c -x sleep || true")
			var n int
			_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
			return n
		}, deliveredTimeout, 3*time.Second).Should(BeZero(), "nothing a step started outlives its slice")

		By("and machinectl list on the host is empty of this machine after the run")
		Expect(run.wait(deliveredTimeout)).To(BeZero())
		Expect(hostMust(ctx, "machinectl", "list", "--no-legend")).NotTo(ContainSubstring(machine))
	})

	// Scenario: The second run is warm
	//   Given a node delivered once
	//   When the same node is delivered again
	//   Then the build lineage restores non-empty and the build step is faster
	It("The second run is warm", func(ctx SpecContext) {
		needsForge()

		By("given a node delivered once")
		s := machineTypo.stage(ctx)
		first := s.start(ctx, runOptions{Machine: true, SandboxRoot: sandboxRoot, ModelCredential: os.Getenv(sandboxModelCredentialVar)})
		Expect(first.wait(deliveredTimeout)).To(BeZero(), "the first run delivered")

		By("when the same node is delivered again")
		// A fresh node against the same repository, so the second run restores
		// the go build lineage the first promoted. The node's own delivery is
		// not the point; the warm cache is.
		again := s.Graph.file(ctx, machineTypo.Title, machineTypo.Body)
		s2 := *s
		s2.NodeRef = again
		second := s2.start(ctx, runOptions{Machine: true, SandboxRoot: sandboxRoot, ModelCredential: os.Getenv(sandboxModelCredentialVar)})
		capture := captureRecord(s2.runID(ctx))
		Expect(second.wait(deliveredTimeout)).To(BeZero(), "the second run delivered")
		record := capture.stop()

		By("then the build lineage restores non-empty")
		var restoredEmpty bool
		for _, l := range record.Lineages {
			if strings.HasSuffix(l.Lineage, "go-build") {
				restoredEmpty = l.Generation == 0
			}
		}
		AddReportEntry("second run.json", record)
		Expect(restoredEmpty).To(BeFalse(), "the second run restored the go-build lineage from a promoted generation, not empty")

		By("and the build step is faster the second time")
		firstBuild := buildDuration(ctx, s.NodeRef)
		secondBuild := buildDuration(ctx, again)
		AddReportEntry("build durations", fmt.Sprintf("first=%s second=%s", firstBuild, secondBuild))
		Expect(secondBuild).To(BeNumerically("<", firstBuild), "a warm build is faster than a cold one")
	})

	// Scenario (acceptance criterion): a killed suite leaves nothing behind.
	//   Killing the suite mid-run and starting the worker leaves no machine
	//   and no run directory after reconciliation, and no project in the
	//   deployed dagger.
	It("A killed suite leaves no machine, run directory, or project after reconciliation", func(ctx SpecContext) {
		needsForge()

		By("given a run in flight inside a machine")
		deployed, deployedBefore := deployedProjects(ctx)
		s := machineTypo.stage(ctx)
		run := s.start(ctx, runOptions{Machine: true, SandboxRoot: sandboxRoot, ModelCredential: os.Getenv(sandboxModelCredentialVar)})
		id := s.runID(ctx)
		record := awaitMachine(ctx, id, deliveredTimeout)

		By("when the suite is killed and a worker starts")
		// A kill releases nothing: the machine is left registered and the run
		// directory on disk. The lease is taken back — killing a worker frees
		// nothing, so the next one could not claim the node until the TTL
		// passed (docs/runbook.md §"What it takes") — and a worker started for
		// the same project reconciles the host against the ledger before its
		// first claim, sweeping what no live run owns (docs/sandbox.md
		// §Reconciliation, §"External state").
		run.kill()
		Expect(hostMust(ctx, "machinectl", "list", "--no-legend")).To(ContainSubstring(record.Machine),
			"the killed run left its machine registered")
		s.Graph.preempt(ctx, s.Graph.leaseID(ctx, s.NodeRef), "acceptance: the suite was killed mid-run")
		startMachineWorker(ctx, s.Graph.Slug)

		By("then no machine, run directory, or project outlives the sweep")
		Eventually(func(ctx context.Context) string {
			return hostMust(ctx, "machinectl", "list", "--no-legend")
		}, 5*time.Minute, 5*time.Second).WithContext(ctx).ShouldNot(ContainSubstring(record.Machine))
		Expect(runDir(id)).NotTo(BeADirectory(), "the run directory was removed")

		By("and the deployed dagger's project list is unchanged")
		if deployed == "" {
			return
		}
		_, deployedAfter := deployedProjects(ctx)
		Expect(deployedAfter).To(ConsistOf(deployedBefore), "nothing the suite made reached the operator's graph")
	})
})

// runID is the run mill made for this node, polled from the ledger. It is
// how a from-the-host scenario finds the run directory and the machine name:
// the machine is `mill-<id>` and the directory `runs/<id>`, both derived from
// this (docs/sandbox.md §Naming). A run that has not reached the store yet is
// waited for, because `mill run` creates the run a moment after it starts.
func (s *staged) runID(ctx context.Context) string {
	GinkgoHelper()
	var id string
	Eventually(func() string {
		if detail := tryInspect(ctx, s.NodeRef); detail != nil {
			id = detail.GetSummary().GetRun().GetId()
		}
		return id
	}, 2*time.Minute, time.Second).ShouldNot(BeEmpty(), "mill run created a run for %s", s.NodeRef)
	return id
}

// millCheckout stages a node whose repository is mill itself, so a machine's
// `/work` is a mill checkout `make test-db` can run in. It is a local-origin
// staging — the run is probed while in flight and killed, never delivered —
// so it needs no forge: `--origin` points at this checkout and the run clones
// it as any other origin (docs/runbook.md §"Without a forge").
func millCheckout(ctx context.Context) *staged {
	GinkgoHelper()
	s := &staged{
		Graph:  newScratchGraph(ctx, "mill-checkout"),
		Origin: "file://" + env.RepoRoot,
		VCSURL: placeholderVCSURL,
	}
	s.NodeRef = s.Graph.file(ctx,
		"Touch the README so a run has a change to make",
		"Add a line to the end of README.md. The point of this node is to boot a machine bound to a mill checkout, not the change.\n")
	root := GinkgoT().TempDir()
	s.WorkspaceDir = filepath.Join(root, "workspaces")
	s.LogDir = filepath.Join(root, "logs")
	Expect(os.MkdirAll(s.WorkspaceDir, 0o700)).To(Succeed())
	Expect(os.MkdirAll(s.LogDir, 0o700)).To(Succeed())
	return s
}

// podmanSocket is the podman API socket inside a machine, whichever of the
// two the base image landed on: the rootless one under the agent's runtime
// directory, or the system one. The image enables `podman.socket`; the work
// order leaves which to the host, so the suite probes for it rather than
// assuming.
func podmanSocket(ctx context.Context, machine string) string {
	GinkgoHelper()
	for _, candidate := range []string{"/run/user/1000/podman/podman.sock", "/run/podman/podman.sock"} {
		out, status := inMachine(ctx, machine, "sh", "-c", "test -S "+candidate+" && echo yes || echo no")
		if status == 0 && strings.TrimSpace(out) == "yes" {
			return candidate
		}
	}
	Fail("no podman socket inside " + machine + " at either the rootless or the system path")
	return ""
}

// buildDuration is how long a run's toolchain step took, from the ledger's
// own timestamps. It is the deterministic setup step the cache lineages warm
// — `mise install` against a warmed mise lineage and a `go build` against a
// warmed go-build lineage — so its cold-to-warm difference is the cache
// working, read off the ledger rather than a wall clock the suite kept
// (docs/sandbox.md §Caching).
func buildDuration(ctx context.Context, nodeRef string) time.Duration {
	GinkgoHelper()
	detail := inspect(ctx, nodeRef)
	for _, d := range attempts(detail, "toolchain") {
		sr := d.GetStepRun()
		if sr.GetStartedAt() == nil || sr.GetEndedAt() == nil {
			continue
		}
		return sr.GetEndedAt().AsTime().Sub(sr.GetStartedAt().AsTime())
	}
	Fail("the ledger has no closed toolchain step for " + nodeRef)
	return 0
}

// startMachineWorker starts `mill work` configured for the machine backend
// against the suite's own service and dagger, and registers its kill. Its
// startup reconciliation is what the killed-suite scenario is about: before
// its first claim it lists machines against the ledger's live runs and
// sweeps what no live run owns (docs/sandbox.md §Reconciliation). It runs in
// a process group of its own, so the scenario's kill of it reaches nothing
// else.
func startMachineWorker(ctx context.Context, project string) {
	GinkgoHelper()
	args := []string{
		"work",
		"--socket", suite.service.Socket,
		"--dagger-socket", env.DaggerSocket,
		"--project", project,
		"--workspace", "machine",
		"--sandbox-root", sandboxRoot,
		"--gateway",
		"--model-credential-file", os.Getenv(sandboxModelCredentialVar),
		"--workspace-dir", GinkgoT().TempDir(),
		"--log-dir", GinkgoT().TempDir(),
		"--sidecar", env.Sidecar,
		// The scratch namespace and budget so the sweep reaches the forge and
		// dagger projects too, by name (docs/sandbox.md §"External state").
		"--scratch-budget", "0s",
	}
	command := exec.CommandContext(ctx, env.Mill, args...) //nolint:gosec // the suite's own binary
	command.Env = append(os.Environ(),
		"MILL_FORGE_TOKEN="+env.ForgeToken,
		"MILL_FORGE_USER="+env.ForgeUser)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout, command.Stderr = GinkgoWriter, GinkgoWriter
	Expect(command.Start()).To(Succeed(), "mill work started")
	var once sync.Once
	DeferCleanup(func() {
		once.Do(func() {
			if command.Process != nil {
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				_, _ = command.Process.Wait()
			}
		})
	})
}
