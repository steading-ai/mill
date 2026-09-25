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
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
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
	return inMachineUnder(ctx, machine, "mill-step-probe.slice", argv...)
}

// inMachineUnder is inMachine under a named step slice: the run's own, when
// what is being proved is that the run's step boundary reaps what a step
// left behind.
func inMachineUnder(ctx context.Context, machine, slice string, argv ...string) (combined string, status int) {
	GinkgoHelper()
	return inMachineWith(ctx, machine, slice, nil, argv...)
}

// inMachineWith is inMachineUnder with an environment for the exec, each
// entry one `NAME=value` the helper admits; nil is the helper's bare
// environment, which carries nothing of the run's.
func inMachineWith(ctx context.Context, machine, slice string, env []string, argv ...string) (combined string, status int) {
	GinkgoHelper()
	args := []string{"-n", sandboxHelper, "exec", machine,
		"-p", "Slice=" + slice, "-p", "WorkingDirectory=/work", "-p", "User=" + "agent"}
	for _, assignment := range env {
		args = append(args, "-p", "Environment="+assignment)
	}
	args = append(args, "--")
	args = append(args, argv...)
	out, errb, status := hostRun(ctx, append([]string{"sudo"}, args...)...)
	return out + errb, status
}

// asSession runs a command inside a machine the way a session's shell runs
// one: under a probe slice, with the egress the run decided for the machine
// — the proxy, its bypass list, the gateway — read off the environment.d
// file the run wrote for the machine's user manager (docs/sandbox.md
// §Network). The helper's exec carries no environment of its own, and a
// tool that reaches out with none — `go` fetching a module — dials direct
// and is dropped by the host's table.
func asSession(ctx context.Context, runID, machine string, argv ...string) (combined string, status int) {
	GinkgoHelper()
	body, err := os.ReadFile(filepath.Join(runDir(runID), "environment.d", "50-mill-proxy.conf")) //nolint:gosec // the run directory is the worker's own, the suite's account
	Expect(err).NotTo(HaveOccurred(), "the run wrote the machine's environment")
	var env []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		env = append(env, line)
	}
	Expect(env).NotTo(BeEmpty())
	return inMachineWith(ctx, machine, "mill-step-probe.slice", env, argv...)
}

// sandboxRecord is the run's own note of its machine, `runs/<id>/run.json`
// on the host, as much of it as the live tier reads: the machine's name and
// when it reported ready. It is how a from-the-host scenario finds the
// machine to probe while the run is in flight (docs/sandbox.md §Provision);
// what the run recorded is asserted off the ledger, which is the record
// (docs/ledger.md §Workspaces, `sandbox`).
type sandboxRecord struct {
	RunID   string    `json:"run_id"`
	Machine string    `json:"machine"`
	ReadyAt time.Time `json:"ready_at"`
	// Slice is the step slice in flight, `mill-step-<ordinal>.slice`, and
	// Ordinal its number (docs/sandbox.md §"Step boundary"): what a probe
	// that must be reaped by the run's own step boundary runs under.
	Slice   string `json:"slice"`
	Ordinal int    `json:"ordinal"`
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
func awaitMachine(ctx context.Context, runID string) sandboxRecord {
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
	}, deliveredTimeout, 2*time.Second).Should(BeTrue(), "run %s booted a machine and reached ready", runID)
	return record
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
//
// The build is mise's `postinstall` hook: `go vet` over one package that
// pulls in net/http, so the `toolchain` step compiles a good slice of the
// standard library into `~/.cache/go-build` — the repo-scoped `go-build`
// lineage, empty for every fresh scratch repository and restored from the
// first run's promotion by the second (docs/sandbox.md §Caching). That is
// what makes the toolchain step's duration a cold-against-warm measure of
// the lineage rather than of `mise install` alone, which is warm on any host
// whose global `mise` lineage already holds this go. `go vet` writes nothing
// into the tree, so the clean-tree check after commit is unaffected.
//
// The go release is pinned exactly. A partial pin (`1.25`) makes mise list
// the releases upstream before it can settle on one — `git ls-remote` of
// golang/go, a network round trip the toolchain step pays on every run and
// that the fixture is not about; an exact release already installed in the
// worker's global `mise` lineage costs the step no lookup at all.
//
// The second word the fixture's README misspells, for the node the warm
// scenario files after the first has been delivered: the same repository
// and the same kind of change, against a README the first run already
// corrected once, so the second session has a change of its own to make.
const (
	secondMisspelling = "sep" + "erate"
	secondCorrection  = "separate"
)

var machineTypo = fixture{
	Name: "machine-typo",
	Files: map[string]string{
		"mise.toml": "[tools]\ngo = \"1.25.14\"\n\n[hooks]\npostinstall = \"go vet ./...\"\n",
		"go.mod":    "module scratch\n\ngo 1.25\n",
		// The binary a session's own `go build` would leave, ignored as a Go
		// repository ignores it, so the clean-tree check after commit sees
		// the tree and not the session's curiosity.
		".gitignore": "/scratch\n",
		"main.go": "package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"net/http\"\n)\n\n" +
			"func main() {\n\tbody, _ := json.Marshal(http.StatusText(http.StatusTeapot))\n\tfmt.Println(string(body))\n}\n",
		"README.md": "# Scratch\n\nThis project exists so the sandbox can prove itself.\n\n" +
			"We " + misspelling + " the change inside a machine and merge it.\n\n" +
			"A " + secondMisspelling + " run proves the cache is warm.\n",
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
		// The machine's name off the run's own record while it is in flight,
		// to hold the ledger's copy to afterwards.
		booted := awaitMachine(ctx, s.runID(ctx)).Machine

		By("then the node is done with a merge on the default branch")
		Expect(run.wait(deliveredTimeout)).To(BeZero(), "mill run exited cleanly")
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
		sandbox := detail.GetSummary().GetWorkspace().GetSandbox()
		AddReportEntry("sandbox", sandbox)
		Expect(sandbox).NotTo(BeNil(), "the run's workspace row carries its machine")
		Expect(sandbox.GetMachine()).To(Equal(booted), "the ledger names the machine that booted")
		Expect(sandbox.GetUnit()).To(Equal("mill-machine@" + strings.TrimPrefix(booted, "mill-") + ".service"))
		Expect(sandbox.GetImageDigest()).To(MatchRegexp(`^[a-f0-9]{64}$`), "the derived digest the machine booted")
		Expect(sandbox.GetVeth()).To(HavePrefix("ve-mill-"), "the host-side veth the machine was reachable on")
		Expect(sandbox.GetLineages()).NotTo(BeEmpty(), "the lineages the run restored")

		By("and the run promoted its build lineage")
		build := lineageNamed(sandbox, "go-build")
		Expect(build).NotTo(BeNil(), "a go repository restores a go-build lineage")
		Expect(build.Promoted).NotTo(BeNil(), "the evidence-closed steps promoted the go build cache")
		Expect(build.GetPromoted()).To(BeNumerically(">", 0))
		after := cacheGenerations(ctx)
		Expect(after).To(ContainElement(HaveSuffix(fmt.Sprintf("/go-build/gen-%d", build.GetPromoted()))),
			"the promoted generation is on the volume; before=%v after=%v", before, after)
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
		id := s.runID(ctx)
		DeferCleanup(func(ctx context.Context) {
			run.kill()
			sweepKilledRun(ctx, id)
		})
		machine := awaitMachine(ctx, id).Machine

		By("when a session runs make test-db against the machine's podman")
		// After the run's own toolchain step has installed what mill's
		// mise.toml pins — go, ginkgo, goose — through the proxy: the suite's
		// exec is a session's shell, and a session runs once the preparatory
		// step has closed (docs/sandbox.md §"Preparatory steps"). The image
		// enables `podman.socket`; the rootless socket is the one inside the
		// machine (docs/sandbox.md §Network). The suite finds whichever of
		// the two the image landed on and points DOCKER_HOST at it, exactly
		// as a session's own shell would. The exec is a step of its own —
		// its slice is the probe's — so what the assertion after it reads is
		// what that step left in podman once it ended.
		awaitClosed(ctx, s.NodeRef, "toolchain")
		socket := podmanSocket(ctx, machine)
		// The reaper's window is the suite's own choice (internal/testsupport
		// sets five minutes unless told otherwise): a container the machine's
		// podman runs for a step is not in the step's slice — the API service
		// under agent's user manager starts it, not the step's exec — so the
		// step boundary does not take it, and a reaper that lingers five
		// minutes after the step is a container that remains after the step.
		// Ten seconds is ryuk's own default.
		out, status := asSession(ctx, s.runID(ctx), machine, "sh", "-lc",
			"cd /work && DOCKER_HOST=unix://"+socket+" RYUK_RECONNECTION_TIMEOUT=10s mise exec -- make test-db")
		AddReportEntry("make test-db", out)

		By("then it exits 0 and no container remains after the step")
		Expect(status).To(BeZero(), "make test-db inside the machine: %s", out)
		// testcontainers' reaper (ryuk) is the one container the suite does
		// not stop itself: it stops what the suite made and then exits, its
		// window after the suite's last connection to it closes. So the
		// machine's podman is given that long to be empty, and what it held
		// at the step's end is on the record.
		var seen string
		Eventually(func() string {
			remaining, _ := inMachine(ctx, machine, "sh", "-lc",
				"DOCKER_HOST=unix://"+socket+" podman ps -a --format '{{.ID}} {{.Image}} {{.Status}}'")
			if seen == "" {
				seen = remaining
			}
			return strings.TrimSpace(remaining)
		}, 30*time.Second, 2*time.Second).Should(BeEmpty(), "no container outlived the step")
		AddReportEntry("podman ps -a at the step's end", seen)
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
		id := s.runID(ctx)
		// The run is left to finish; the sweep is for a run the scenario's
		// failure left in flight.
		DeferCleanup(func(ctx context.Context) {
			if run.running() {
				run.kill()
				sweepKilledRun(ctx, id)
			}
		})
		machine := awaitMachine(ctx, id).Machine

		By("when the host inspects uids, mounts, network, credentials and processes")
		// The marker for the last assertion goes in first, while the run is
		// on its earliest step: a machine run delivers a quick fix in about a
		// minute, and the probes between take seconds each.
		left := leaveBehind(ctx, s.runID(ctx), machine)

		By("then the session's uid on the host is in the machine's range and not the operator's")
		leader := strings.TrimSpace(hostMust(ctx, "machinectl", "show", machine, "-p", "Leader", "--value"))
		Expect(leader).NotTo(BeEmpty())
		var hostUID int
		_, err := fmt.Sscanf(strings.TrimSpace(hostMust(ctx, "sh", "-c", "stat -c %u /proc/"+leader)), "%d", &hostUID)
		Expect(err).NotTo(HaveOccurred())
		Expect(hostUID).NotTo(Equal(os.Getuid()), "the machine's processes are not the operator's uid on the host")
		Expect(hostUID).NotTo(BeZero(), "nor root's")
		// nspawn's --private-users=pick draws the machine's 65536 uids from
		// above the host's own users (docs/sandbox.md §Provision).
		Expect(hostUID).To(BeNumerically(">=", 65536), "the uid is in a range picked for the machine")

		By("and /home/ivy is not reachable from inside")
		out, status := inMachine(ctx, machine, "ls", "-a", "/home/ivy")
		Expect(status).NotTo(BeZero(), "ls /home/ivy: %s", out)
		Expect(out).To(ContainSubstring("No such file or directory"), "the operator's home is not there at all")
		Expect(out).NotTo(ContainSubstring(".ssh"), "the operator's home is not mounted")

		By("and a direct connection out times out and one through the proxy succeeds")
		// From inside the machine, per homestead#40: a host probe with a
		// source interface is denied by squid whatever the machine's policy,
		// so both probes run in the machine. The direct connection is dropped
		// by the host's nftables and times out; the proxied one is admitted.
		_, direct := inMachine(ctx, machine, "sh", "-c",
			"curl -sS --max-time 8 https://example.com/ -o /dev/null")
		Expect(direct).NotTo(BeZero(), "a direct connection is dropped and times out")
		// The probe's exec carries no environment of its own (docs/sandbox.md
		// §Exec), so the proxy is read from where the run bound it for the
		// machine: the environment.d file agent's user manager reads at boot
		// (§Network).
		out, proxied := inMachine(ctx, machine, "sh", "-c",
			". /etc/environment.d/50-mill-proxy.conf && test -n \"$HTTPS_PROXY\" && "+
				"curl -sS --max-time 20 -x \"$HTTPS_PROXY\" https://example.com/ -o /dev/null")
		Expect(proxied).To(BeZero(), "a connection through the proxy the environment names succeeds: %s", out)

		By("and ~/.claude holds no .credentials.json")
		creds, _ := inMachine(ctx, machine, "sh", "-c",
			"test -e /home/agent/.claude/.credentials.json && echo present || echo absent")
		Expect(strings.TrimSpace(creds)).To(Equal("absent"), "no model login enters the machine")

		By("and a process left behind by a step is gone once the run has moved on")
		// A daemon a session starts does not outlive the step's slice: the
		// step boundary reaps everything under it, containers included
		// (docs/sandbox.md §"Step boundary"). The marker was started under
		// the run's own step slice in flight — read off run.json, which
		// BeginStep writes and EndStep clears — so what reaps it is the run's
		// next step boundary, not the machine's teardown; the assertion runs
		// once the record shows a later ordinal, while the machine is still
		// up. A run that finished before that reading has torn the machine
		// down with everything in it, which is the same property from the
		// other end; the report entry says which was seen.
		var reaped string
		Eventually(func() string {
			record, ok := readRecord(s.runID(ctx))
			if !ok || record.Ordinal <= left {
				if !run.running() {
					reaped = "the run ended and took the machine, and the marker, with it"
				}
				return reaped
			}
			out, _ := inMachine(ctx, machine, "sh", "-c", "pgrep -c -x sleep || true")
			if strings.TrimSpace(out) != "0" {
				return ""
			}
			reaped = fmt.Sprintf("no sleep in the machine on step %d", record.Ordinal)
			return reaped
		}, deliveredTimeout, time.Second).ShouldNot(BeEmpty(), "nothing step %d started is there once the run is on a later step", left)
		AddReportEntry("left behind", fmt.Sprintf("a sleep under step %d's slice; %s", left, reaped))

		By("and machinectl list on the host is empty of this machine after the run")
		Expect(run.wait(deliveredTimeout)).To(BeZero())
		Expect(hostMust(ctx, "machinectl", "list", "--no-legend")).NotTo(ContainSubstring(machine))

		By("and the gateway log shows the step token inbound, by name and never by value")
		// The gateway logs every forwarded request on `mill run`'s stderr
		// with the step token by id and the credential by fingerprint
		// (docs/sandbox.md §Credentials). The token's value is what the
		// session held; it is in no log line.
		log := run.output.String()
		Expect(log).To(MatchRegexp(`msg="gateway: forwarding" .*token=\S+ .*credential=credential:[0-9a-f]+`),
			"the gateway forwarded a machine session's request under the step token")
		Expect(log).NotTo(ContainSubstring("Bearer "), "no bearer value reaches the log")
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
		// A fresh node against the same repository — the README's second
		// misspelling, since the first run corrected the first — so the second
		// run restores the go build lineage the first promoted. The node's
		// own delivery is not the point; the warm cache is.
		again := s.Graph.file(ctx,
			"Fix the typo in README.md: '"+secondMisspelling+"' → '"+secondCorrection+"'",
			"README.md contains the misspelling \""+secondMisspelling+"\". It should read \""+secondCorrection+"\".\n\n"+
				"Change that one word and nothing else.\n")
		s2 := *s
		s2.NodeRef = again
		second := s2.start(ctx, runOptions{Machine: true, SandboxRoot: sandboxRoot, ModelCredential: os.Getenv(sandboxModelCredentialVar)})
		Expect(second.wait(deliveredTimeout)).To(BeZero(), "the second run delivered")

		By("then the build lineage restores non-empty")
		cold := lineageNamed(inspect(ctx, s.NodeRef).GetSummary().GetWorkspace().GetSandbox(), "go-build")
		warm := lineageNamed(inspect(ctx, again).GetSummary().GetWorkspace().GetSandbox(), "go-build")
		AddReportEntry("go-build lineages", fmt.Sprintf("first=%v second=%v", cold, warm))
		Expect(cold).NotTo(BeNil())
		Expect(warm).NotTo(BeNil())
		Expect(cold.GetRestored()).To(BeZero(), "the first run started the repo's build lineage empty")
		Expect(cold.GetPromoted()).To(BeNumerically(">", 0), "and promoted it")
		Expect(warm.GetRestored()).To(Equal(cold.GetPromoted()),
			"the second run restored the go-build lineage from the generation the first promoted, not empty")

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
		// Should the worker not finish the run, the scenario sweeps what its
		// kill left, as the boundaries scenario does.
		DeferCleanup(func(ctx context.Context) {
			if _, err := os.Stat(runDir(id)); err == nil {
				sweepKilledRun(ctx, id)
			}
		})
		record := awaitMachine(ctx, id)

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
		w := startMachineWorker(ctx, s)

		By("then no machine, run directory, or project outlives the sweep")
		Eventually(func(ctx context.Context) string {
			if !w.running() {
				StopTrying("the worker ended before the machine was gone:\n" + w.output.String()).Now()
			}
			return hostMust(ctx, "machinectl", "list", "--no-legend")
		}, 5*time.Minute, 5*time.Second).WithContext(ctx).ShouldNot(ContainSubstring(record.Machine))
		Eventually(runDir(id), time.Minute, time.Second).ShouldNot(BeADirectory(), "the run directory was removed")

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
// so it needs no forge: `--origin` points at a clone of this checkout's HEAD
// and the run clones it as any other origin (docs/runbook.md §"Without a
// forge").
//
// The clone carries one commit of its own, with two things in it. The
// working tree's uncommitted changes, so the suite inside the machine is
// the tree under test and not the last commit's. And a `mill.toml`
// declaring `btrfs-progs` in the image: a worker that finds itself inside a
// machine — which mill's own `db` suite is, `mill doctor` and `mill run`
// under it — defaults its sandbox root to a subvolume it creates under the
// machine's ephemeral root with `btrfs` (docs/sandbox.md §Self-hosting,
// "The inner worker's root"), and the base image does not carry the tool:
// the nested image variant does, and this tree declares neither yet. So
// the checkout the machine is bound to is mill's, with the declaration
// §Self-hosting's third step will put in the tree, and nothing else.
func millCheckout(ctx context.Context) *staged {
	GinkgoHelper()
	clone := filepath.Join(GinkgoT().TempDir(), "mill")
	git(ctx, env.RepoRoot, "clone", "--quiet", "--no-hardlinks", env.RepoRoot, clone)
	carryWorkingTree(ctx, env.RepoRoot, clone)
	Expect(os.WriteFile(filepath.Join(clone, "mill.toml"),
		[]byte("# The db suite inside a machine: the inner worker's root is a btrfs subvolume\n# (docs/sandbox.md §Self-hosting).\n[image]\npackages = [\"btrfs-progs\"]\n"), 0o600)).To(Succeed())
	git(ctx, clone, "add", "--all")
	git(ctx, clone, "commit", "--quiet", "--message", "The working tree, and btrfs-progs for the db suite inside a machine")
	s := &staged{
		Graph:  newScratchGraph(ctx, "mill-checkout", placeholderVCSURL),
		Origin: "file://" + clone,
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

// sweepKilledRun is the teardown a killed run never does, for a run a
// scenario killed on purpose and nothing will resume: the machine terminated
// through the helper, every subvolume under the run directory deleted and
// the directory removed, the socket directory removed — §Destroy's steps
// done by hand (docs/runbook.md §"The live tier", what it leaves behind).
// The store still has the run as live; the next worker's reconciliation
// records it as a lost sandbox and claims nothing for it, because its node
// is in a dagger that ended with the suite.
func sweepKilledRun(ctx context.Context, runID string) {
	GinkgoHelper()
	if record, ok := readRecord(runID); ok && record.Machine != "" {
		_, _, _ = hostRun(ctx, "sudo", "-n", sandboxHelper, "terminate", record.Machine)
	}
	dir := runDir(runID)
	var subvolumes []string
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == dir {
			return nil //nolint:nilerr // what cannot be read is not a subvolume to delete
		}
		if info, err := entry.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == btrfsSubvolumeInode {
				subvolumes = append(subvolumes, path)
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, subvolume := range subvolumes {
		hostRun(ctx, "btrfs", "property", "set", "-f", subvolume, "ro", "false")
		hostMust(ctx, "btrfs", "subvolume", "delete", subvolume)
	}
	Expect(os.RemoveAll(dir)).To(Succeed())
	Expect(os.RemoveAll(filepath.Join("/run/mill", strings.ToLower(runID)))).To(Succeed())
}

// btrfsSubvolumeInode is the inode number every btrfs subvolume's root
// directory has, which is how one is told from a directory.
const btrfsSubvolumeInode = 256

// awaitClosed waits for the ledger to show a closed attempt of a step on a
// node's run: the moment a from-the-host scenario may treat what that step
// prepared — a toolchain — as there.
func awaitClosed(ctx context.Context, nodeRef, step string) {
	GinkgoHelper()
	Eventually(func() bool {
		for _, d := range attempts(tryInspect(ctx, nodeRef), step) {
			if d.GetStepRun().GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED {
				return true
			}
		}
		return false
	}, deliveredTimeout, 2*time.Second).Should(BeTrue(), "the run's %s step closed", step)
}

// carryWorkingTree copies what the working tree at root has that its HEAD
// does not into a clone of that HEAD: the tracked files' changes as one
// patch, and every untracked file git does not ignore. It is how a machine
// bound to "a checkout of mill" runs the suite of the tree being worked on.
func carryWorkingTree(ctx context.Context, root, clone string) {
	GinkgoHelper()
	patch := gitOutput(ctx, root, "diff", "--binary", "HEAD")
	if patch != "" {
		apply := exec.CommandContext(ctx, "git", "-C", clone, "apply", "--whitespace=nowarn", "-") //nolint:gosec // the suite's own git
		apply.Stdin = strings.NewReader(patch)
		out, err := apply.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "applying the working tree's changes: %s", out)
	}
	for path := range strings.SplitSeq(gitOutput(ctx, root, "ls-files", "--others", "--exclude-standard", "-z"), "\x00") {
		if path == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, path)) //nolint:gosec // this checkout's own files
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(filepath.Dir(filepath.Join(clone, path)), 0o755)).To(Succeed()) //nolint:gosec // a checkout
		Expect(os.WriteFile(filepath.Join(clone, path), body, 0o644)).To(Succeed())        //nolint:gosec // a checkout
	}
}

// gitOutput is one git command's standard output, in a directory.
func gitOutput(ctx context.Context, dir string, args ...string) string {
	GinkgoHelper()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // the suite's own git
	out, err := command.Output()
	Expect(err).NotTo(HaveOccurred(), "git %s", strings.Join(args, " "))
	return string(out)
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

// leaveBehind starts a marker process under the run's step slice in flight
// and returns that step's ordinal. It reads the slice off run.json, and if
// the step ended between the read and the exec — the marker would then sit
// in a slice the run has already stopped and will not stop again — it tries
// again under the next one.
func leaveBehind(ctx context.Context, runID, machine string) int {
	GinkgoHelper()
	for range 10 {
		var record sandboxRecord
		Eventually(func() bool {
			var ok bool
			record, ok = readRecord(runID)
			return ok && record.Slice != ""
		}, deliveredTimeout, 200*time.Millisecond).Should(BeTrue(), "the run has a step in flight")
		_, status := inMachineUnder(ctx, machine, record.Slice, "sh", "-c", "setsid sleep 3600 >/dev/null 2>&1 &")
		Expect(status).To(BeZero())
		if after, ok := readRecord(runID); ok && after.Slice == record.Slice {
			return record.Ordinal
		}
	}
	Fail("no step held still long enough to leave a process behind in")
	return 0
}

// lineageNamed is the ledger's lineage of that name on a run's sandbox, or
// nil: `go-build` for the repo-scoped go build cache a go repository restores
// and promotes (docs/sandbox.md §Caching).
func lineageNamed(sandbox *millv1.Sandbox, name string) *millv1.SandboxLineage {
	for _, lineage := range sandbox.GetLineages() {
		if lineage.GetName() == name {
			return lineage
		}
	}
	return nil
}

// buildDuration is how long a run's toolchain step took, from the ledger's
// own timestamps. It is the deterministic step the cache lineages warm —
// `mise install` against the mise lineage and, through the fixture's
// postinstall hook, `go vet` against the go-build lineage — so its
// cold-to-warm difference is the cache working, read off the ledger rather
// than a wall clock the suite kept (docs/sandbox.md §Caching).
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
func startMachineWorker(ctx context.Context, s *staged) *worker {
	GinkgoHelper()
	// The worker reads a configuration file where `mill run` reads none:
	// the operator's, by default, which on core names the deployed dagger,
	// the mill project and the label its own worker claims under. It is
	// given an empty one of the suite's own, so every value it runs on is
	// on this line. What it does after reconciling is claim the node the
	// kill left claimable and resume the run in the machine the kill left —
	// a live run with its machine is resumable, not an orphan (docs/
	// sandbox.md §Reconciliation) — and its finish is what takes the
	// machine and the run directory away.
	config := filepath.Join(GinkgoT().TempDir(), "config.yaml")
	Expect(os.WriteFile(config, []byte("# the acceptance suite's worker: the scratch budget, which `work` takes from\n"+
		"# no flag; everything else is on the command line. A zero budget so the\n"+
		"# sweep reaches every scratch project by name at once (docs/sandbox.md\n"+
		"# §\"External state\").\n"+
		"scratch-budget: 0s\n"), 0o600)).To(Succeed())
	args := []string{
		"work",
		"--config", config,
		"--dagger-socket", env.DaggerSocket,
		"--project", s.Graph.Slug,
		"--workflow", "quick-fix",
		"--workflow-dir", workflowDir("quick-fix"),
		"--origin", s.Origin,
		"--ref", s.Ref,
		"--workspace", "machine",
		"--sandbox-root", sandboxRoot,
		"--gateway",
		"--model-credential-file", os.Getenv(sandboxModelCredentialVar),
		"--workspace-dir", GinkgoT().TempDir(),
		"--log-dir", GinkgoT().TempDir(),
		"--sidecar", env.Sidecar,
	}
	command := exec.CommandContext(ctx, env.Mill, args...) //nolint:gosec // the suite's own binary
	// The service's socket reaches `work` as $MILL_SOCKET: the standalone
	// worker declares no --socket flag, and a `socket:` line in its file is
	// read against that flag and so never applied — `serve --work`, which
	// declares it, is the face a deployment runs.
	command.Env = append(os.Environ(),
		"MILL_SOCKET="+suite.service.Socket,
		"MILL_FORGE_TOKEN="+env.ForgeToken,
		"MILL_FORGE_USER="+env.ForgeUser)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &lockedBuffer{}
	command.Stdout, command.Stderr = teeTo(output), teeTo(output)
	Expect(command.Start()).To(Succeed(), "mill work started")
	w := &worker{command: command, output: output, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		err := command.Wait()
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
	}()
	DeferCleanup(func() { w.kill() })
	return w
}
