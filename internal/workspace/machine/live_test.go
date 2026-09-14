package machine_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/skill"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/version"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	machineconfig "github.com/ivy/mill/internal/workspace/machine/config"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// The live tier: a real machine on `core` through the real driver, as the
// operator's uid and never as root — the unit through polkit, the helper
// through sudo, exactly as the worker runs (docs/sandbox.md §Privilege).
// Off unless MILL_SANDBOX_HOST says so (§Testing).
//
// The run directory is the real one, /var/lib/mill/runs/<id>, because the
// helper's policy admits binds under it and nowhere else; the id carries a
// nonce and the machine is named as §External state has a suite name its
// machines, so a sweep finds it after a kill.

const (
	liveVar      = "MILL_SANDBOX_HOST"
	liveRoot     = machine.DefaultRoot
	liveMachines = machine.DefaultMachines
	liveRunState = machine.DefaultRunState
	liveHelper   = driver.DefaultHelper
)

// host runs argv on the host and returns what it said and its status.
func host(argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		Fail(fmt.Sprintf("%s: %v", strings.Join(argv, " "), err))
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

// must runs argv on the host and fails the spec unless it exits zero.
func must(argv ...string) string {
	GinkgoHelper()
	out, stderr, status := host(argv...)
	Expect(status).To(BeZero(), "%s: %s%s", strings.Join(argv, " "), out, stderr)
	return out
}

// inMachine runs a command in the machine through the helper, as the
// worker would once the exec node lands, and returns stdout and status.
func inMachine(name, user string, argv ...string) (string, int) {
	GinkgoHelper()
	args := []string{"-n", liveHelper, "exec", name, "-p", "Slice=mill-step-spec.slice", "-p", "WorkingDirectory=/work"}
	if user != "" {
		args = append(args, "-p", "User="+user)
	}
	args = append(args, "--")
	args = append(args, argv...)
	out, stderr, status := host(append([]string{"sudo"}, args...)...)
	return out + stderr, status
}

// subvolumes lists the subvolumes under a mounted btrfs path, as root,
// relative to the mount: btrfs prints them under the mounted subvolume's
// own name (`@mill/...`), which is stripped.
func subvolumes(mount string) []string {
	GinkgoHelper()
	out := must("sudo", "-n", "btrfs", "subvolume", "list", "-o", mount)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[len(fields)-1]
		if strings.HasPrefix(name, "@") {
			_, name, _ = strings.Cut(name, "/")
		}
		names = append(names, name)
	}
	return names
}

// repoRoot is this repository's root, for images/base.
func repoRoot() string {
	GinkgoHelper()
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no go.mod above the spec")
		dir = parent
	}
}

// failingProbe is the real driver with its execs answered by a process
// that exits non-zero: an image whose init never reaches running, without
// building one.
type failingProbe struct{ driver.Driver }

func (failingProbe) Exec(ctx context.Context, _ driver.ExecRequest) (*workspace.Process, error) {
	return workspace.Spawner{Floor: []string{}}.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "echo starting; exit 1"}})
}

var _ = Describe("A machine on a real host", Label("live"), Serial, Ordered, func() {
	var (
		real       driver.Driver
		base       image.Digest
		skillDir   string
		sidecarDir string
		origin     string
		config     machine.Config
		nonce      string
	)

	BeforeAll(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine.")
		}
		if testsupport.InsideMachine() {
			Skip(testsupport.HostOnlySkip("the host's template unit, helper and image volume to boot a machine"))
		}
		if _, _, status := host("sudo", "-n", "true"); status != 0 {
			Skip("sudo -n is refused; the helper and the image export need it")
		}
		real = driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: "sudo"})

		By("the host's prerequisites: the skill directory the helper's policy admits, and the run-state directory")
		// The skill is the one the worker writes at its preflight, from
		// its own embed and with its version beside it (internal/skill,
		// internal/cli/run.go checkWorkspace).
		skillDir = machine.Config{}.SkillDir()
		Expect(skill.Materialise(skillDir, version.String())).To(Succeed())
		// The sidecar rides in beside the skill: this binary's, copied from
		// the built tree where the worker keeps it (internal/skill,
		// internal/cli/run.go checkWorkspace). It must be built — `make
		// sidecar-build`, which the acceptance suite runs (docs/runbook.md).
		// The copy is gated on the version breadcrumb, and a development
		// binary's version does not change with its build, so the tree is
		// removed first: the suite then stands where a worker with a new
		// binary version does, and what rides in is this checkout's build.
		sidecarDir = machine.Config{}.SidecarDir()
		Expect(os.RemoveAll(sidecarDir)).To(Succeed())
		Expect(skill.MaterialiseSidecar(filepath.Join(repoRoot(), "sidecar"), sidecarDir, version.String())).To(Succeed())

		By("building the base from images/base and exporting it where machines boot from")
		images := image.New(real, image.Config{Root: liveRoot, Machines: liveMachines, BuildBudget: 15 * time.Minute})
		result, err := images.BuildBase(ctx, image.BaseBuild{Context: filepath.Join(repoRoot(), "images", "base")})
		Expect(err).NotTo(HaveOccurred())
		base = result.Digest
		digest, finding, err := images.Ensure(ctx, base, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())
		Expect(digest).To(Equal(base))

		raw := make([]byte, 4)
		_, _ = rand.Read(raw)
		nonce = hex.EncodeToString(raw)
		config = machine.Config{
			Root:         liveRoot,
			Machines:     liveMachines,
			RunState:     liveRunState,
			Skill:        skillDir,
			Sidecar:      sidecarDir,
			Images:       image.Config{Pin: string(base)},
			CacheDriver:  driver.NewSystemd(workspace.Spawner{}, driver.Commands{}),
			Limits:       machine.Limits{MemoryMax: "4G", CPUQuota: "400%", TasksMax: "4096"},
			ReadyTimeout: 2 * time.Minute,
			Log:          GinkgoWriter,
		}
		Expect(machine.Check(real, config)).To(Succeed())
	})

	BeforeEach(func() {
		dir := filepath.Join(GinkgoT().TempDir(), "origin")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		git(dir, "init", "--quiet", "--initial-branch=main")
		Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("live\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\ngo = \"1.25\"\n"), 0o600)).To(Succeed())
		git(dir, "add", "--all")
		git(dir, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
		origin = dir
		config.Origin = origin
	})

	// sweep removes whatever a failed spec left, by name.
	sweep := func(id string) {
		host("sudo", "-n", liveHelper, "terminate", "mill-"+id)
		host("systemctl", "reset-failed", "mill-machine@"+id+".service")
		for _, copy := range []string{"cache/mise", "cache/go-mod", "cache/go-build", "candidate/mise", "candidate/go-mod", "candidate/go-build"} {
			host("btrfs", "subvolume", "delete", filepath.Join(liveRoot, "runs", id, copy))
		}
		host("rm", "-rf", filepath.Join(liveRoot, "runs", id), filepath.Join(liveRunState, id))
	}

	// Scenario: A machine is a transient unit booting an ephemeral snapshot
	It("A machine is a transient unit booting an ephemeral snapshot", func(ctx context.Context) {
		id := "acceptance-unit-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)

		By("when a run creates its machine")
		handle, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#61"})
		Expect(err).NotTo(HaveOccurred())

		By("then mill-machine@<run-id>.service is active")
		Expect(strings.TrimSpace(must("systemctl", "is-active", "mill-machine@"+id+".service"))).To(Equal("active"))
		Expect(handle.Sandbox.Unit).To(Equal("mill-machine@" + id + ".service"))

		By("and machinectl lists mill-<run-id> running")
		Expect(strings.TrimSpace(must("machinectl", "show", "mill-"+id, "-p", "State"))).To(Equal("State=running"))
		Expect(must("machinectl", "list", "--no-legend")).To(ContainSubstring("mill-" + id))
		Expect(handle.Sandbox.Veth).NotTo(BeEmpty(), "the veth was recorded")
		_, _, status := host("ip", "link", "show", handle.Sandbox.Veth)
		Expect(status).To(BeZero(), "the recorded veth %s is a host interface", handle.Sandbox.Veth)

		By("and the machine's root is a snapshot that is gone after destroy")
		shown := must("machinectl", "status", "--no-pager", "-n", "0", "mill-"+id)
		Expect(shown).To(ContainSubstring("Root: " + liveMachines + "/.#machine.mill-image-" + string(base)))
		Expect(subvolumes(liveMachines)).To(ContainElement(HavePrefix(".#machine.mill-image-" + string(base))))
		Expect(ws.Destroy(ctx)).To(Succeed())
		Expect(subvolumes(liveMachines)).NotTo(ContainElement(HavePrefix(".#machine.mill-image-" + string(base))))
		_, _, status = host("machinectl", "show", "mill-"+id)
		Expect(status).NotTo(BeZero(), "no machine is registered")
	})

	// Scenario: Readiness is systemd's
	It("Readiness is systemd's", func(ctx context.Context) {
		id := "acceptance-ready-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)

		By("when a run creates its machine")
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#61"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })

		By("then no step exec is issued before is-system-running returns: the machine is already there when Create returns")
		out, status := inMachine("mill-"+id, "", "systemctl", "is-system-running")
		Expect(strings.TrimSpace(out)).To(BeElementOf("running", "degraded"), "status %d", status)
		By("and the proxy reached agent's user manager at boot")
		out, status = inMachine("mill-"+id, "", "sh", "-c", `systemctl --user --machine=agent@.host show-environment`)
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("HTTPS_PROXY=" + machine.DefaultProxy))
		Expect(out).To(ContainSubstring("ANTHROPIC_BASE_URL=" + machine.DefaultGateway))
	})

	It("A machine that never reaches ready is stopped and Create returns an error carrying the timeout and the digest; nothing is left in machinectl list", func(ctx context.Context) {
		id := "acceptance-unready-" + nonce
		DeferCleanup(func() { sweep(id) })
		unready := config
		unready.ReadyTimeout = 3 * time.Second
		ws := machine.New(failingProbe{real}, unready)

		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#61"})

		var timeout *machine.ReadyTimeoutError
		Expect(errors.As(err, &timeout)).To(BeTrue(), "%v", err)
		Expect(timeout.Timeout).To(Equal(3 * time.Second))
		Expect(timeout.Image).To(Equal(string(base)))
		Expect(must("machinectl", "list", "--no-legend")).NotTo(ContainSubstring("mill-" + id))
		active, _, _ := host("systemctl", "is-active", "mill-machine@"+id+".service")
		Expect(strings.TrimSpace(active)).NotTo(Equal("active"))
		Expect(filepath.Join(liveRoot, "runs", id)).NotTo(BeADirectory())
		Expect(subvolumes(liveMachines)).NotTo(ContainElement(HavePrefix(".#machine.mill-image-" + string(base))))
	})

	// Scenario: A run leaves nothing behind
	It("A run leaves nothing behind", func(ctx context.Context) {
		id := "acceptance-nothing-" + nonce
		DeferCleanup(func() { sweep(id) })
		before := subvolumes(liveRoot)
		ws := machine.New(real, config)

		By("given a run that wrote everywhere inside its machine")
		created, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#61"})
		Expect(err).NotTo(HaveOccurred())
		name := "mill-" + id
		out, status := inMachine(name, "agent", "sh", "-c",
			`echo work > /work/marker && echo home > /home/agent/marker && echo tmp > /tmp/marker && mkdir -p /home/agent/.cache/go-build && echo built > /home/agent/.cache/go-build/marker && id -u`)
		Expect(status).To(BeZero(), out)
		Expect(strings.TrimSpace(out)).To(Equal("1000"))
		Expect(filepath.Join(created.Root, "marker")).To(BeAnExistingFile(), "the workspace bind is the host's directory")
		info, err := os.Stat(filepath.Join(created.Root, "marker"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Sys()).To(HaveField("Uid", uint32(os.Getuid())), "uid 1000 inside is the operator outside: the bind is idmapped")
		Expect(ws.Candidate(ctx, "test")).To(Succeed())

		By("and Attach reopens it for a resumed run")
		attached, err := machine.New(real, config).Attach(ctx, workspace.Spec{RunID: id, NodeRef: "mill#61"})
		Expect(err).NotTo(HaveOccurred())
		Expect(attached.Sandbox).To(Equal(created.Sandbox))
		Expect(attached.Socket).To(Equal(created.Socket))

		By("when it is destroyed")
		Expect(ws.Destroy(ctx)).To(Succeed())

		By("then no machine, run directory, or unpromoted subvolume remains")
		Expect(must("machinectl", "list", "--no-legend")).NotTo(ContainSubstring(name))
		Expect(filepath.Join(liveRoot, "runs", id)).NotTo(BeADirectory())
		Expect(filepath.Join(liveRunState, id)).NotTo(BeADirectory())
		after := subvolumes(liveRoot)
		var added []string
		for _, name := range after {
			if !contains(before, name) {
				added = append(added, name)
			}
		}
		for _, name := range added {
			Expect(name).To(MatchRegexp(`^cache/.*/gen-[0-9]+$`), "only promoted generations are new: %v", added)
		}
		Expect(added).To(ContainElement(HaveSuffix("/go-build/gen-1")), "the evidence-closed step's cache was promoted")
		for _, name := range added {
			host("btrfs", "property", "set", "-ts", filepath.Join(liveRoot, name), "ro", "false")
			host("btrfs", "subvolume", "delete", filepath.Join(liveRoot, name))
		}
	})

	// Scenario: The skill is mounted read-only at the harness's path
	It("The skill is mounted read-only at the harness's path", func(ctx context.Context) {
		id := "acceptance-skill-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#65"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("when a session lists ~/.claude/skills/mill")
		out, status := inMachine(name, "agent", "ls", machineconfig.SkillPath)
		Expect(status).To(BeZero(), out)

		By("then the skill is present and cannot be written")
		Expect(strings.Fields(out)).To(ContainElements("SKILL.md", skill.VersionFile, "references"))
		shipped, err := os.ReadFile(filepath.Join(repoRoot(), "plugin", "mill", "skills", "mill", "SKILL.md"))
		Expect(err).NotTo(HaveOccurred())
		out, status = inMachine(name, "agent", "cat", machineconfig.SkillPath+"/SKILL.md")
		Expect(status).To(BeZero(), out)
		Expect(out).To(Equal(string(shipped)), "the mounted skill is the plugin's, byte for byte")
		out, status = inMachine(name, "agent", "sh", "-c", "echo scribble > "+machineconfig.SkillPath+"/scribble")
		Expect(status).NotTo(BeZero())
		Expect(out).To(ContainSubstring("Read-only file system"))

		By("and the rest of ~/.claude can be")
		out, status = inMachine(name, "agent", "sh", "-c",
			`echo '{}' > /home/agent/.claude/settings.local.json && mkdir -p /home/agent/.claude/projects && cat /home/agent/.claude/settings.local.json`)
		Expect(status).To(BeZero(), out)
		Expect(strings.TrimSpace(out)).To(Equal("{}"))

		By("and the breadcrumb beside SKILL.md is what mill version prints")
		out, status = inMachine(name, "agent", "cat", machineconfig.SkillPath+"/"+skill.VersionFile)
		Expect(status).To(BeZero(), out)
		binary, err := gexec.Build("github.com/ivy/mill/cmd/mill")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gexec.CleanupBuildArtifacts)
		Expect(strings.TrimSpace(must(binary, "version"))).To(Equal("mill version " + strings.TrimSpace(out)))
	})

	// Scenario: The sidecar rides in read-only
	It("The sidecar rides in read-only", func(ctx context.Context) {
		id := "acceptance-sidecar-ro-" + nonce
		DeferCleanup(func() { sweep(id) })

		By("given the worker started with a new binary version")
		// The sidecar under the share root is this binary's, materialised in
		// BeforeAll and gated on the version breadcrumb.
		Expect(skill.Version(sidecarDir)).To(Equal(version.String()))

		By("when the first machine boots")
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#82"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("then the sidecar under the share root matches the binary and is mounted read-only at its fixed path")
		out, status := inMachine(name, "agent", "findmnt", "-no", "OPTIONS", machineconfig.SidecarPath)
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("ro"))
		out, status = inMachine(name, "agent", "sh", "-c", "echo scribble > "+machineconfig.SidecarPath+"/scribble")
		Expect(status).NotTo(BeZero())
		Expect(out).To(ContainSubstring("Read-only file system"))
		shipped, err := os.ReadFile(filepath.Join(sidecarDir, "dist", "main.js"))
		Expect(err).NotTo(HaveOccurred())
		out, status = inMachine(name, "agent", "cat", machineconfig.SidecarPath+"/dist/main.js")
		Expect(status).To(BeZero(), out)
		Expect(out).To(Equal(string(shipped)), "the mounted sidecar is the worker's, byte for byte")
	})

	// hostOutsideMachine is the node processes matching pattern on the host
	// that are not under the machine's step slice, each with its command
	// name and cgroup: the exec is one like any other, so nothing the
	// session runs escapes the machine's cgroup. The helper's own client —
	// `sudo mill-machine exec … node …` and the `systemd-run --pipe` it
	// holds open — carries the argv on the host too; it is the exec's
	// conduit, not a sidecar, and is told apart by its command name.
	hostOutsideMachine := func(pattern string) []string {
		GinkgoHelper()
		out, _, _ := host("pgrep", "-f", pattern)
		var outside []string
		for _, pid := range strings.Fields(out) {
			comm, _, _ := host("cat", "/proc/"+pid+"/comm")
			if strings.TrimSpace(comm) != "node" {
				continue
			}
			cg, _, _ := host("cat", "/proc/"+pid+"/cgroup")
			if !strings.Contains(cg, "mill-step-") {
				outside = append(outside, pid+" "+strings.TrimSpace(comm)+" "+strings.TrimSpace(cg))
			}
		}
		return outside
	}

	// sidecarMain is the sidecar entrypoint inside a machine: the in-machine
	// node running the mounted sidecar, exactly as the harness adapter spawns
	// it. Started with no stdin it loads and waits for its request, so it
	// stays alive under the step slice for the spec to observe — which is the
	// lifecycle a real session has while its turn runs.
	sidecarArgs := []string{machine.SidecarNode, machineconfig.SidecarPath + "/dist/main.js"}
	sidecarPattern := machineconfig.SidecarPath + "/dist/main.js"

	// sidecarInside is the pids of the sidecar's node inside the machine.
	// pgrep runs directly through the helper, not under a shell, so the only
	// command line carrying the pattern is pgrep's own, which it excludes.
	sidecarInside := func(name string) []string {
		GinkgoHelper()
		out, _ := inMachine(name, "", "pgrep", "-f", sidecarPattern)
		return strings.Fields(out)
	}

	// startSidecar execs the sidecar through the workspace and drains what it
	// says, so a sidecar that exits instead of waiting for its request
	// explains itself in the failure. The returned func is its output so far.
	startSidecar := func(ctx context.Context, ws *machine.Workspace) func() string {
		GinkgoHelper()
		process, err := ws.Exec(ctx, workspace.Command{Path: sidecarArgs[0], Args: sidecarArgs[1:]})
		Expect(err).NotTo(HaveOccurred())
		var mu sync.Mutex
		var said bytes.Buffer
		drain := func(r io.Reader) {
			defer GinkgoRecover()
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				mu.Lock()
				said.Write(buf[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}
		go drain(process.Stdout)
		go drain(process.Stderr)
		// Stdin stays open and unwritten: the sidecar blocks on its request,
		// as a real session's does while its turn runs, until the step's
		// slice stops and reaps it.
		return func() string {
			mu.Lock()
			defer mu.Unlock()
			return said.String()
		}
	}

	// cgroups is the machine's control-group tree with nothing ellipsized,
	// so a command line as long as the sidecar's is shown whole.
	cgroups := func(name string) string {
		GinkgoHelper()
		out, _, _ := host("sudo", "-n", "systemd-cgls", "--full", "--no-pager", "-M", name)
		return out
	}

	// Scenario: The session is an exec inside the machine
	It("The session is an exec inside the machine", func(ctx context.Context) {
		id := "acceptance-session-exec-" + nonce
		DeferCleanup(func() { sweep(id) })

		By("given a run with the machine backend")
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#82"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("when a session step starts")
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		said := startSidecar(ctx, ws)

		By("then the sidecar is started through the workspace under the step slice as agent in /work")
		var pids []string
		Eventually(func() []string {
			pids = sidecarInside(name)
			return pids
		}, 30*time.Second, 300*time.Millisecond).ShouldNot(BeEmpty(), "the sidecar's node runs inside the machine: %s", said())
		pid := pids[0]
		Expect(cgroups(name)).To(MatchRegexp(`mill-step-1\.slice(?s:.*)mill-exec-[0-9a-f]+\.service(?s:.*)`+regexp.QuoteMeta(sidecarPattern)),
			"the sidecar runs under the step's slice: %s", said())
		owner, _ := inMachine(name, "", "stat", "-c", "%U", "/proc/"+pid)
		Expect(strings.TrimSpace(owner)).To(Equal(machine.SessionUser), "the sidecar said: %s", said())
		cwd, _ := inMachine(name, "", "readlink", "/proc/"+pid+"/cwd")
		Expect(strings.TrimSpace(cwd)).To(Equal("/work"))

		By("and nothing is spawned on the host")
		Expect(hostOutsideMachine(sidecarPattern)).To(BeEmpty(), "the sidecar is only under the machine's step slice")

		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
	})

	// Scenario: The checkout refuses a write under a read-only token
	It("The checkout refuses a write under a read-only token", func(ctx context.Context) {
		id := "acceptance-read-only-" + nonce
		DeferCleanup(func() { sweep(id) })

		By("given a live machine and a token that grants read and not write")
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#91"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		Expect(ws.Rotate(ctx, workspace.TokenFor("step-token-"+nonce, workspace.Read, workspace.Exec))).To(Succeed())

		By("when the workspace execs touch /work/x")
		process, err := ws.Exec(ctx, workspace.Command{Path: "touch", Args: []string{"/work/x"}})
		Expect(err).NotTo(HaveOccurred())
		_, stderr, result := drainProcess(process)

		By("then the process exits non-zero and its stderr says the file system is read-only")
		Expect(result.Status).NotTo(BeZero())
		Expect(stderr).To(ContainSubstring("Read-only file system"))

		By("and an exec of touch /tmp/x under the same token exits zero")
		process, err = ws.Exec(ctx, workspace.Command{Path: "touch", Args: []string{"/tmp/x"}})
		Expect(err).NotTo(HaveOccurred())
		_, _, result = drainProcess(process)
		Expect(result.Status).To(BeZero())

		By("and under a token that grants write, touch /work/x exits zero")
		Expect(ws.Rotate(ctx, workspace.TokenFor("step-token-w-"+nonce, workspace.Read, workspace.Write, workspace.Exec))).To(Succeed())
		process, err = ws.Exec(ctx, workspace.Command{Path: "touch", Args: []string{"/work/x"}})
		Expect(err).NotTo(HaveOccurred())
		_, stderr, result = drainProcess(process)
		Expect(result.Status).To(BeZero(), stderr)

		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
	})

	// Scenario: The session dies with its step
	It("The session dies with its step", func(ctx context.Context) {
		id := "acceptance-session-dies-" + nonce
		DeferCleanup(func() { sweep(id) })

		By("given a session running inside the machine")
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#82"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		said := startSidecar(ctx, ws)
		Eventually(func() []string { return sidecarInside(name) },
			30*time.Second, 300*time.Millisecond).ShouldNot(BeEmpty(), "the sidecar said: %s", said())

		By("when the step ends")
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())

		By("then no sidecar process remains")
		Expect(ws.BeginStep(ctx, 2)).To(Succeed())
		Eventually(func() []string { return sidecarInside(name) },
			20*time.Second, 300*time.Millisecond).Should(BeEmpty())

		By("and the next step starts its own")
		said = startSidecar(ctx, ws)
		Eventually(func() string { return cgroups(name) },
			30*time.Second, 300*time.Millisecond).Should(MatchRegexp(`mill-step-2\.slice(?s:.*)`+regexp.QuoteMeta(sidecarPattern)),
			"the fresh session runs under step 2's slice: %s", said())
		Expect(ws.EndStep(ctx, 2, false)).To(Succeed())
	})

	// Scenario: Stdout, stderr and the exit code come back intact and streamed
	It("Stdout, stderr and the exit code come back intact and streamed", func(ctx context.Context) {
		id := "acceptance-exec-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#62"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })

		By("when mill execs a command that writes to both streams and exits 42")
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		process, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 42"}})
		Expect(err).NotTo(HaveOccurred())
		stdout, stderr, result := drainProcess(process)

		By("then the streams arrive separately and the exit code is 42")
		Expect(strings.TrimSpace(stdout)).To(Equal("out"))
		Expect(strings.TrimSpace(stderr)).To(Equal("err"))
		Expect(result.Status).To(BeEquivalentTo(42))

		By("and a process that prints, sleeps five seconds, then prints delivers the first line before the second")
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
		Expect(ws.BeginStep(ctx, 2)).To(Succeed())
		streamed, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "echo first; sleep 5; echo second"}})
		Expect(err).NotTo(HaveOccurred())
		reader := bufio.NewReader(streamed.Stdout)
		start := time.Now()
		line, err := reader.ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(line)).To(Equal("first"))
		Expect(time.Since(start)).To(BeNumerically("<", 4*time.Second), "the first line arrived before the sleep ended")
		_, _ = io.ReadAll(reader)
		_, _ = io.ReadAll(streamed.Stderr)
		streamed.Wait()
		Expect(ws.EndStep(ctx, 2, false)).To(Succeed())

		By("and an exec's working directory, user and environment are its own")
		Expect(ws.BeginStep(ctx, 3)).To(Succeed())
		marked, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "pwd; id -u; echo ${MARK:-unset}"}, Dir: "/work", Env: map[string]string{"MARK": "1"}})
		Expect(err).NotTo(HaveOccurred())
		out, _, res := drainProcess(marked)
		Expect(res.Status).To(BeEquivalentTo(0))
		Expect(strings.Fields(out)).To(Equal([]string{"/work", "1000", "1"}))
		unmarked, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "echo ${MARK:-unset}"}})
		Expect(err).NotTo(HaveOccurred())
		out, _, _ = drainProcess(unmarked)
		Expect(strings.TrimSpace(out)).To(Equal("unset"), "the next exec's environment does not carry the previous one's MARK")
		Expect(ws.EndStep(ctx, 3, false)).To(Succeed())
	})

	// Scenario: An exec without a step slice is refused
	It("An exec without a step slice is refused before any command runs", func(ctx context.Context) {
		id := "acceptance-noslice-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#62"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })

		_, err = ws.Exec(ctx, workspace.Command{Path: "/bin/true"})
		Expect(err).To(MatchError(ContainSubstring("no step slice")))
	})

	// Scenario: Cancellation stops the unit, not the client
	It("Cancellation stops the unit, not the client", func(ctx context.Context) {
		id := "acceptance-cancel-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#62"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("given an exec of sleep 300 in flight")
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		execCtx, cancel := context.WithCancel(ctx)
		process, err := ws.Exec(execCtx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "sleep 300"}})
		Expect(err).NotTo(HaveOccurred())
		done := make(chan struct{})
		go func() { defer close(done); _, _, _ = drainProcess(process) }()
		Eventually(func() int {
			out, _ := inMachine(name, "", "sh", "-c", "pgrep -c -x sleep || true")
			n, _ := strconv.Atoi(strings.TrimSpace(out))
			return n
		}, 30*time.Second, 200*time.Millisecond).Should(BeNumerically(">", 0), "the sleep is running in the machine")

		By("when the exec is cancelled")
		cancel()
		Eventually(done, 30*time.Second).Should(BeClosed(), "the client returned once the unit stopped")

		By("then the unit is inactive and no sleep remains")
		Eventually(func() string {
			out, _ := inMachine(name, "", "sh", "-c", "pgrep -x sleep || echo none")
			return strings.TrimSpace(out)
		}, 20*time.Second, 200*time.Millisecond).Should(Equal("none"))
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
	})

	// Scenario: A background process does not survive its step
	It("A background process does not survive its step", func(ctx context.Context) {
		id := "acceptance-reap-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#62"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("given step 1 started a process with nohup and a detached container")
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		bg, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "nohup sleep 3600 >/dev/null 2>&1 & echo backgrounded"}})
		Expect(err).NotTo(HaveOccurred())
		out, _, res := drainProcess(bg)
		Expect(res.Status).To(BeEquivalentTo(0), out)
		// alpine has a sleep and every file root's; quay.io/podman/hello
		// is one binary from scratch with no sleep, and busybox's tree
		// has a uid past agent's 55536 subordinates. The pull goes
		// through Harbor's proxy project as registries.conf routes
		// docker.io.
		container, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c",
			"podman run -d --name lingering docker.io/library/alpine sleep 3600"}})
		Expect(err).NotTo(HaveOccurred())
		cout, cerr, res := drainProcess(container)
		Expect(res.Status).To(BeEquivalentTo(0), "podman run: %s%s", cout, cerr)

		By("and the container's cgroup is under the step's slice (--cgroups=split; the spike's E3)")
		// systemd-cgls shows the machine's tree; with --cgroups=split the
		// container payload sits in a sub-cgroup of the exec unit, itself
		// under mill-step-1.slice. That the slice reaps the container when
		// it stops — the pgrep below after EndStep — is the proof it was
		// under the slice and not under agent's user manager, which the
		// slice stop would not reach.
		Eventually(func() string {
			cgls, _, _ := host("sudo", "-n", "systemd-cgls", "-M", name, "--no-pager")
			return cgls
		}, 20*time.Second, 200*time.Millisecond).Should(MatchRegexp(`mill-step-1\.slice(?s:.*)sleep`),
			"the container's sleep runs under the step's slice")

		By("when step 1 ends")
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())

		By("then neither the process nor the container exists in the machine, under step 2's slice")
		Expect(ws.BeginStep(ctx, 2)).To(Succeed())
		// One pgrep covers both: the nohup and the container each run a
		// `sleep`, so none remaining is both gone. `podman ps` is stale
		// after a slice kill (the spike's E3), so it is not the evidence.
		Eventually(func() string {
			out, _ := inMachine(name, "", "sh", "-c", "pgrep -x sleep || echo none")
			return strings.TrimSpace(out)
		}, 20*time.Second, 200*time.Millisecond).Should(Equal("none"))

		By("and step 2's slice was not created until step 1's was inactive")
		active, _, _ := host("sudo", "-n", "systemctl", "--machine="+name, "is-active", "mill-step-1.slice")
		Expect(strings.TrimSpace(active)).NotTo(Equal("active"))
		Expect(ws.EndStep(ctx, 2, false)).To(Succeed())
	})

	// Scenario: An evidence-closed step leaves a candidate
	It("An evidence-closed step leaves a candidate", func(ctx context.Context) {
		id := "acceptance-candidate-" + nonce
		DeferCleanup(func() { sweep(id) })
		local := config
		ws := machine.New(real, local)
		origin := filepath.Join(GinkgoT().TempDir(), "cache-origin")
		Expect(os.MkdirAll(origin, 0o700)).To(Succeed())
		git(origin, "init", "--quiet", "--initial-branch=main")
		Expect(os.WriteFile(filepath.Join(origin, "mise.toml"), []byte("[tools]\ngo = \"1.25\"\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(origin, "mill.toml"), []byte("[cache.go-build]\npath = \"/home/agent/.cache/go-build\"\n"), 0o600)).To(Succeed())
		git(origin, "add", "--all")
		git(origin, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
		local.Origin = origin

		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#62", Origin: origin})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		name := "mill-" + id

		By("given step 1 wrote to the run's cache path")
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		wrote, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "mkdir -p /home/agent/.cache/go-build && echo built > /home/agent/.cache/go-build/marker"}})
		Expect(err).NotTo(HaveOccurred())
		_, stderr, res := drainProcess(wrote)
		Expect(res.Status).To(BeEquivalentTo(0), stderr)

		By("when step 1 closes on evidence")
		Expect(ws.EndStep(ctx, 1, true)).To(Succeed())

		By("then a read-only snapshot of the run's cache copy exists as the candidate")
		candidate := filepath.Join(liveRoot, "runs", id, "candidate", "go-build")
		Expect(candidate).To(BeADirectory())
		Expect(filepath.Join(candidate, "marker")).To(BeAnExistingFile())
		roOut := must("btrfs", "property", "get", "-ts", candidate, "ro")
		Expect(strings.TrimSpace(roOut)).To(HaveSuffix("ro=true"))

		By("and it was taken before the slice stopped")
		active, _, _ := host("sudo", "-n", "systemctl", "--machine="+name, "is-active", "mill-step-1.slice")
		Expect(strings.TrimSpace(active)).NotTo(Equal("active"))
	})

	// Scenario: An orphan machine is stopped and its run directory removed
	It("An orphan machine is stopped and its run directory removed", func(ctx context.Context) {
		id := "acceptance-orphan-" + nonce
		DeferCleanup(func() { sweep(id) })
		name := "mill-" + id

		By("given a registered machine with no live run")
		_, err := machine.New(real, config).Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#63"})
		Expect(err).NotTo(HaveOccurred())
		Expect(must("machinectl", "list", "--no-legend")).To(ContainSubstring(name))
		ledger := &fakeLedger{}

		By("when the worker starts")
		report, err := machine.NewReconciler(real, config).Reconcile(ctx, ledger)
		Expect(err).NotTo(HaveOccurred())

		By("then the machine is gone and its run directory is deleted")
		Expect(must("machinectl", "list", "--no-legend")).NotTo(ContainSubstring(name))
		active, _, _ := host("systemctl", "is-active", "mill-machine@"+id+".service")
		Expect(strings.TrimSpace(active)).NotTo(Equal("active"))
		Expect(filepath.Join(liveRoot, "runs", id)).NotTo(BeADirectory())
		Expect(filepath.Join(liveRunState, id)).NotTo(BeADirectory())

		By("and the reconciliation is recorded")
		Expect(ledger.recorded()).NotTo(BeNil())
		orphans := report.Of(machine.FindingOrphan)
		Expect(orphans).To(HaveLen(1))
		Expect(orphans[0].Name).To(Equal(name))
		Expect(orphans[0].Action).To(Equal("stopped; run directory removed"))
	})

	// Scenario: A live run resumes at its step with a fresh slice
	It("A live run resumes at its step with a fresh slice", func(ctx context.Context) {
		id := "acceptance-resume-" + nonce
		DeferCleanup(func() { sweep(id) })
		name := "mill-" + id
		first := machine.New(real, config)
		_, err := first.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#63"})
		Expect(err).NotTo(HaveOccurred())

		By("given a live run at step 2 with a process still in its slice")
		Expect(first.BeginStep(ctx, 1)).To(Succeed())
		Expect(first.EndStep(ctx, 1, false)).To(Succeed())
		Expect(first.BeginStep(ctx, 2)).To(Succeed())
		lingering, err := first.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "sleep 3600"}})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			out, _ := inMachine(name, "", "sh", "-c", "pgrep -x sleep || echo none")
			return strings.TrimSpace(out)
		}, 20*time.Second, 200*time.Millisecond).ShouldNot(Equal("none"), "the process is in the slice")
		// The worker dies here: its context ends, but nothing ends the
		// step, and the exec's own cancellation stop is what a dead worker
		// never issues.
		ledger := &fakeLedger{live: []machine.LiveRun{{RunID: id, NodeRef: "mill#63", Step: "implement"}}}

		By("when the worker starts")
		report, err := machine.NewReconciler(real, config).Reconcile(ctx, ledger)
		Expect(err).NotTo(HaveOccurred())

		By("then the slice is stopped")
		resumable := report.Of(machine.FindingResumable)
		Expect(resumable).To(HaveLen(1))
		Expect(resumable[0].Action).To(Equal("stopped mill-step-2.slice"))
		Eventually(func() string {
			out, _ := inMachine(name, "", "sh", "-c", "pgrep -x sleep || echo none")
			return strings.TrimSpace(out)
		}, 20*time.Second, 200*time.Millisecond).Should(Equal("none"), "nothing of the step survives")
		Expect(must("machinectl", "list", "--no-legend")).To(ContainSubstring(name), "the machine itself is untouched")
		_, _, _ = drainProcess(lingering)

		By("and recreated, and the run resumes at step 2")
		second := machine.New(real, config)
		_, err = second.Attach(ctx, workspace.Spec{RunID: id, NodeRef: "mill#63"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(second.Destroy(ctx)).To(Succeed()) })
		Expect(second.NextOrdinal()).To(Equal(2))
		Expect(second.BeginStep(ctx, 2)).To(Succeed())
		fresh, err := second.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "cat /proc/self/cgroup"}})
		Expect(err).NotTo(HaveOccurred())
		out, stderr, res := drainProcess(fresh)
		Expect(res.Status).To(BeEquivalentTo(0), stderr)
		Expect(out).To(ContainSubstring("mill-step-2.slice"), "the fresh slice carries the same ordinal")
		Expect(second.EndStep(ctx, 2, false)).To(Succeed())
	})

	// The shim's report, taken apart from dnf: the socket directory is bound
	// at boot, `agent` inside is the worker outside, and the verb answers a
	// bearer the sidecar would have put in the session's environment. This is
	// what the dnf scenario below relies on, proven without a mirror.
	It("The shim's report reaches the workspace socket from inside the machine", func(ctx context.Context) {
		id := "acceptance-report-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#64"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		const bearer = "report-step-bearer"
		Expect(ws.Rotate(ctx, workspace.TokenFor(bearer, workspace.Exec))).To(Succeed())
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		defer func() { _ = ws.EndStep(ctx, 1, false) }()

		By("when the shim's own curl line runs as agent, with the step token in its environment")
		report, err := ws.Exec(ctx, workspace.Command{
			Path: "/bin/sh", Args: []string{"-c", `test -S /run/mill/ws.sock && curl -sS --max-time 5 --unix-socket /run/mill/ws.sock -X POST ` +
				`-H "Authorization: Bearer $STEP_TOKEN" -H 'Content-Type: application/json' ` +
				`--data '{"packages":["vips-devel"]}' -o /dev/null -w '%{http_code}' http://mill/v1/report.install`},
			Env: map[string]string{"STEP_TOKEN": bearer},
		})
		Expect(err).NotTo(HaveOccurred())
		stdout, stderr, result := drainProcess(report)
		Expect(result.Status).To(BeEquivalentTo(0), stderr)
		Expect(strings.TrimSpace(stdout)).To(Equal("200"))

		By("then the workspace holds the report, and a bearer that is not the step's is refused")
		Expect(ws.Installs()).To(Equal([]workspace.Install{{Packages: []string{"vips-devel"}}}))
		forged, err := ws.Exec(ctx, workspace.Command{
			Path: "/bin/sh", Args: []string{"-c", `curl -sS --max-time 5 --unix-socket /run/mill/ws.sock -X POST ` +
				`-H "Authorization: Bearer not-the-token" --data '{"packages":["gcc"]}' -o /dev/null -w '%{http_code}' http://mill/v1/report.install`},
		})
		Expect(err).NotTo(HaveOccurred())
		stdout, _, _ = drainProcess(forged)
		Expect(strings.TrimSpace(stdout)).To(Equal("401"))
		Expect(ws.Installs()).To(BeEmpty())
	})

	// Scenario: A runtime install is reported and not remembered
	//   Given a session installs a package with the shim and does not change mill.toml
	//   When the run's provenance is read and the next run starts
	//   Then the install is recorded as undeclared
	//   And the package is absent in the next machine
	//
	// The session's half is an exec as a session's shell would make it: as
	// `agent`, under a step slice, with the step token in its environment
	// the way the sidecar sets it. The shim reports over `/run/mill/ws.sock`,
	// the socket directory bound at boot, and the workspace holds the report
	// for the run loop to write as "installed vips-devel, not declared in
	// mill.toml" (internal/run/preparatory.go; the ledger row is that
	// package's spec).
	It("A runtime install is reported and not remembered", func(ctx context.Context) {
		id := "acceptance-dnf-" + nonce
		DeferCleanup(func() { sweep(id) })
		ws := machine.New(real, config)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#64"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })

		By("given a session installs a package with the shim and does not change mill.toml")
		const bearer = "dnf-step-bearer"
		Expect(ws.Rotate(ctx, workspace.TokenFor(bearer, workspace.Exec))).To(Succeed())
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		install, err := ws.Exec(ctx, workspace.Command{
			Path: "/bin/sh", Args: []string{"-c", "dnf install -y vips-devel"},
			Env: map[string]string{"STEP_TOKEN": bearer},
		})
		Expect(err).NotTo(HaveOccurred())
		stdout, stderr, result := drainProcess(install)
		Expect(result.Status).To(BeEquivalentTo(0), stdout+stderr)

		By("then the hint is the last lines of its output")
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		Expect(len(lines)).To(BeNumerically(">=", 3))
		tail := lines[len(lines)-3:]
		Expect(tail[0]).To(Equal("mill: installed vips-devel into an ephemeral machine; the next run's image will not have it."))
		Expect(tail[1]).To(Equal("mill: declare it in mill.toml so the image does. Under [image], add:"))
		Expect(tail[2]).To(Equal(`mill:   packages = ["vips-devel"]`))
		installed, err := ws.Exec(ctx, workspace.Command{Path: "/usr/bin/rpm", Args: []string{"-q", "vips-devel"}})
		Expect(err).NotTo(HaveOccurred())
		_, _, res := drainProcess(installed)
		Expect(res.Status).To(BeEquivalentTo(0), "the package is in this machine")

		By("and sudo /usr/bin/dnf remove is refused by sudoers")
		remove, err := ws.Exec(ctx, workspace.Command{Path: "/usr/bin/sudo", Args: []string{"-n", "/usr/bin/dnf", "remove", "-y", "vips-devel"}})
		Expect(err).NotTo(HaveOccurred())
		_, refusal, res := drainProcess(remove)
		Expect(res.Status).NotTo(BeEquivalentTo(0))
		Expect(refusal).To(ContainSubstring("password"))

		By("when the run's provenance is read")
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())
		Expect(ws.Installs()).To(Equal([]workspace.Install{{Packages: []string{"vips-devel"}}}),
			"the shim's report reached the socket, for the loop to record as undeclared")
		Expect(ws.Destroy(ctx)).To(Succeed())

		By("and the next run starts, the package is absent in the next machine")
		next := "acceptance-dnf-next-" + nonce
		DeferCleanup(func() { sweep(next) })
		second := machine.New(real, config)
		_, err = second.Create(ctx, workspace.Spec{RunID: next, NodeRef: "mill#64"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = second.Destroy(ctx) })
		Expect(second.BeginStep(ctx, 1)).To(Succeed())
		absent, err := second.Exec(ctx, workspace.Command{Path: "/usr/bin/rpm", Args: []string{"-q", "vips-devel"}})
		Expect(err).NotTo(HaveOccurred())
		out, _, res := drainProcess(absent)
		Expect(res.Status).NotTo(BeEquivalentTo(0), out)
		Expect(out).To(ContainSubstring("not installed"))
		Expect(second.EndStep(ctx, 1, false)).To(Succeed())
	})

	// The verbs run on the host against the workspace bind, so what a
	// session wrote as agent inside is what the worker stages and signs
	// outside (docs/sandbox.md §Verbs).
	//
	// Scenario: A commit is signed on the host for work done inside
	//   Given a session inside a machine that wrote a file in /work
	//   When the workspace commits through its verb
	//   Then the commit verifies with the mill's key on the host and its tree holds the file
	It("A commit is signed on the host for work done inside", func(ctx context.Context) {
		id := "acceptance-commit-" + nonce
		DeferCleanup(func() { sweep(id) })
		signed := config
		signed.SigningDir = filepath.Join(GinkgoT().TempDir(), "signing")
		signed.Name, signed.Identity = "mill", "mill@core.test"
		ws := machine.New(real, signed)

		By("given a session inside a machine that wrote a file in /work")
		handle, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#81"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		wrote, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'written inside\\n' > /work/INSIDE.md && id -un"}})
		Expect(err).NotTo(HaveOccurred())
		out, stderr, res := drainProcess(wrote)
		Expect(res.Status).To(BeEquivalentTo(0), stderr)
		Expect(strings.TrimSpace(out)).To(Equal("agent"))
		Expect(ws.EndStep(ctx, 1, false)).To(Succeed())

		By("when the workspace commits through its verb")
		committed, err := ws.Git().Commit(ctx, workspace.CommitRequest{Message: "mill#81: written inside, signed outside"})
		Expect(err).NotTo(HaveOccurred())

		By("then the commit verifies with the mill's key on the host")
		workspaceDir := filepath.Join(liveRoot, "runs", id, "workspace")
		Expect(handle.Root).To(Equal(workspaceDir))
		key := ws.SigningKey()
		Expect(key).NotTo(BeNil())
		_, stderrOut, status := host("git", "-C", workspaceDir, "-c", "gpg.ssh.allowedSignersFile="+key.AllowedSigners, "verify-commit", "HEAD")
		Expect(status).To(BeZero(), stderrOut)
		Expect(stderrOut).To(ContainSubstring("Good \"git\" signature for mill@core.test"))
		Expect(strings.TrimSpace(must("git", "-C", workspaceDir, "show", "--no-patch", "--format=%an <%ae>", "HEAD"))).To(Equal(ws.Committer().String()))
		Expect(committed.Author).To(Equal("mill <mill@core.test>"))

		By("and its tree holds the file")
		Expect(must("git", "-C", workspaceDir, "show", "HEAD:INSIDE.md")).To(Equal("written inside\n"))
		Expect(strings.TrimSpace(must("git", "-C", workspaceDir, "rev-parse", "HEAD"))).To(Equal(committed.SHA))
		inside, status := inMachine("mill-"+id, "agent", "git", "-C", "/work", "rev-parse", "HEAD")
		Expect(status).To(BeZero(), inside)
		Expect(strings.TrimSpace(inside)).To(Equal(committed.SHA), "the same tree on both sides")
	})

	// No git credential, signing key or forge token is readable inside the
	// machine: the paths a credential would live at, and the environment a
	// session's exec is given, as agent.
	It("No git credential, signing key or forge token is readable inside the machine", func(ctx context.Context) {
		id := "acceptance-nocred-" + nonce
		DeferCleanup(func() { sweep(id) })
		signed := config
		signed.SigningDir = filepath.Join(GinkgoT().TempDir(), "signing")
		ws := machine.New(real, signed)
		_, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#81"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(ws.Rotate(ctx, workspace.TokenFor("step-token-"+nonce, workspace.Forge))).To(Succeed())
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		DeferCleanup(func(ctx context.Context) { Expect(ws.EndStep(ctx, 1, false)).To(Succeed()) })

		assertNoCredentialInside(ctx, ws, ws.SigningKey())
	})

	// Scenario: Push and PR reach the forge without the machine
	//   Given a committed change in a machine-backed run
	//   When the workspace pushes and opens a PR
	//   Then the scratch project shows the branch and the merge request
	//   And nothing inside the machine held a forge credential
	It("Push and PR reach the forge without the machine", func(ctx context.Context) {
		api := scratchForge()
		id := "acceptance-push-" + nonce
		DeferCleanup(func() { sweep(id) })

		By("given a scratch project on the forge, swept by name afterwards")
		project := api.createProject(ctx, "mill-acceptance-push-"+nonce)
		DeferCleanup(func(ctx context.Context) { api.sweepByName(ctx, project.Name) })
		client := gitlab.New(forge.Repo{Kind: forge.KindGitLab, BaseURL: api.url, Path: project.Path, CloneURL: project.CloneURL}, forge.Static(api.user, api.token))
		delivering := config
		delivering.Origin = project.CloneURL
		delivering.Forge = client
		delivering.SigningDir = filepath.Join(GinkgoT().TempDir(), "signing")
		ws := machine.New(real, delivering)

		By("and a committed change in a machine-backed run")
		handle, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#81"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		Expect(ws.Rotate(ctx, workspace.TokenFor("step-token-"+nonce, workspace.Forge))).To(Succeed())
		Expect(ws.BeginStep(ctx, 1)).To(Succeed())
		DeferCleanup(func(ctx context.Context) { Expect(ws.EndStep(ctx, 1, false)).To(Succeed()) })
		wrote, err := ws.Exec(ctx, workspace.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'pushed from a machine\\n' > /work/PUSHED.md"}})
		Expect(err).NotTo(HaveOccurred())
		_, stderr, res := drainProcess(wrote)
		Expect(res.Status).To(BeEquivalentTo(0), stderr)
		committed, err := ws.Git().Commit(ctx, workspace.CommitRequest{Message: "mill#81: pushed from a machine"})
		Expect(err).NotTo(HaveOccurred())

		By("when the workspace pushes and opens a PR")
		pushed, err := ws.Forge().Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pushed).To(Equal(workspace.PushResult{Branch: handle.Branch, SHA: committed.SHA, Remote: project.CloneURL}))
		pr, err := ws.Forge().PR(ctx, workspace.PRRequest{Title: "mill#81: pushed from a machine", Target: project.DefaultBranch})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.Number).NotTo(BeZero())
		checks, err := ws.Forge().Checks(ctx, workspace.ChecksRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks.SHA).To(Equal(committed.SHA))
		Expect(checks.Status).To(BeElementOf(forge.Success, forge.Pending), "a project with no pipeline reads as success")

		By("then the scratch project shows the branch and the merge request")
		Expect(api.branchHead(ctx, project, handle.Branch)).To(Equal(committed.SHA))
		Expect(api.mergeRequestBranches(ctx, project)).To(ContainElement(handle.Branch))

		By("and nothing inside the machine held a forge credential")
		assertNoCredentialInside(ctx, ws, ws.SigningKey())
		env, status := inMachine("mill-"+id, "agent", "sh", "-c", "env; cat /work/.git/config")
		Expect(status).To(BeZero(), env)
		Expect(env).NotTo(ContainSubstring(api.token), "the forge token reached neither the environment nor the checkout's remote")
	})
})

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
