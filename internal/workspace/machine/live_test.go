package machine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
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
		real   driver.Driver
		base   image.Digest
		skill  string
		origin string
		config machine.Config
		nonce  string
	)

	BeforeAll(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine.")
		}
		if _, _, status := host("sudo", "-n", "true"); status != 0 {
			Skip("sudo -n is refused; the helper and the image export need it")
		}
		real = driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: "sudo"})

		By("the host's prerequisites: the skill directory the helper's policy admits, and the run-state directory")
		home, err := os.UserHomeDir()
		Expect(err).NotTo(HaveOccurred())
		skill = filepath.Join(home, ".local", "share", "mill", "skill")
		Expect(os.MkdirAll(skill, 0o755)).To(Succeed())
		if _, err := os.Stat(filepath.Join(skill, "SKILL.md")); err != nil {
			Expect(os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("# mill\n\nPlaceholder written by the live spec.\n"), 0o644)).To(Succeed()) //nolint:gosec // a placeholder skill
		}

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
			Skill:        skill,
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
})

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
