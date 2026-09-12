package machine_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
)

// The nested live tier: a machine granted `[machine].nested` on `core`,
// through the real helper, and an inner machine booted inside it exactly as
// docs/sandbox.md §Self-hosting "Nested, exactly" says, as `agent` through
// the sudo the nested image variant bakes. Off unless MILL_SANDBOX_HOST says
// so (§Testing).
//
// The outer boots once; the three scenarios share it in order — the inner
// boots and answers, an inner run promotes under its own root, then the
// outer's stop takes the inner with it — because the third is what happens
// to the first two.

// innerMachine is the inner's name, as the criterion spells it.
const innerMachine = "inner"

// innerImages is where the outer snapshots the read-only bind before the
// inner boots ephemeral from the copy (§Self-hosting, step 1).
const innerImages = "/var/lib/mill-machines"

// inInner runs a command in the inner machine, through the outer as agent
// and its sudo, and returns the combined output and the command's own exit
// status: the helper passes the outer systemd-run's `--wait` status through,
// which is the inner systemd-run's, which is the command's.
func inInner(outer, user string, env map[string]string, argv ...string) (string, int) {
	GinkgoHelper()
	inner := []string{"sudo", "-n", "systemd-run", "-M", innerMachine, "--pipe", "--wait", "--quiet", "--collect"}
	if user != "" {
		inner = append(inner, "-p", "User="+user)
	}
	for name, value := range env {
		inner = append(inner, "-p", "Environment="+name+"="+value)
	}
	inner = append(inner, "--")
	inner = append(inner, argv...)
	return inMachine(outer, "agent", inner...)
}

// generations lists a lineage's generations on the host, by name.
func generations(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "gen-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

var _ = Describe("A nested machine on a real host", Label("live"), Serial, Ordered, func() {
	var (
		real   driver.Driver
		base   image.Digest
		config machine.Config
		origin string
		id     string
		name   string
		ws     *machine.Workspace
	)

	BeforeAll(func(ctx context.Context) {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine.")
		}
		if testsupport.InsideMachine() {
			// The inner boot this spec performs as agent is what a spec
			// inside a nested machine could drive itself; none does yet.
			Skip(testsupport.HostOnlySkip("the host's template unit and helper to boot the outer machine"))
		}
		if _, _, status := host("sudo", "-n", "true"); status != 0 {
			Skip("sudo -n is refused; the helper and the image export need it")
		}
		real = driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: "sudo"})
		info, err := os.Stat(machine.DefaultDaggerCheckout)
		Expect(err).NotTo(HaveOccurred(), "the dagger checkout the helper's policy names as a nested read-only source is not there: homestead's mill_storage puts it at %s", machine.DefaultDaggerCheckout)
		Expect(info.IsDir()).To(BeTrue())

		By("the host's prerequisites: the skill directory the helper's policy admits")
		home, err := os.UserHomeDir()
		Expect(err).NotTo(HaveOccurred())
		skill := filepath.Join(home, ".local", "share", "mill", "skill")
		Expect(os.MkdirAll(skill, 0o755)).To(Succeed())
		if _, err := os.Stat(filepath.Join(skill, "SKILL.md")); err != nil {
			Expect(os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("# mill\n\nPlaceholder written by the live spec.\n"), 0o644)).To(Succeed()) //nolint:gosec // a placeholder skill
		}

		By("building the base from images/base and exporting it where machines boot from")
		images := image.New(real, image.Config{Root: liveRoot, Machines: liveMachines, BuildBudget: 15 * time.Minute})
		result, err := images.BuildBase(ctx, image.BaseBuild{Context: filepath.Join(repoRoot(), "images", "base")})
		Expect(err).NotTo(HaveOccurred())
		base = result.Digest
		_, finding, err := images.Ensure(ctx, base, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(finding).To(BeNil())

		By("given a repository that requests nested, and a worker that grants it")
		dir := filepath.Join(GinkgoT().TempDir(), "origin")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		git(dir, "init", "--quiet", "--initial-branch=main")
		Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("nested\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\ngo = \"1.25\"\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "mill.toml"), []byte("[machine]\nnested = true\n"), 0o600)).To(Succeed())
		git(dir, "add", "--all")
		git(dir, "commit", "--quiet", "--no-gpg-sign", "--message", "seed")
		origin = dir

		raw := make([]byte, 4)
		_, _ = rand.Read(raw)
		id = "acceptance-nested-" + hex.EncodeToString(raw)
		name = "mill-" + id
		config = machine.Config{
			Root:         liveRoot,
			Machines:     liveMachines,
			RunState:     liveRunState,
			Skill:        skill,
			Origin:       origin,
			NestedRepos:  []string{origin},
			Images:       image.Config{Pin: string(base), BuildBudget: 15 * time.Minute},
			CacheDriver:  driver.NewSystemd(workspace.Spawner{}, driver.Commands{}),
			Limits:       machine.Limits{MemoryMax: "6G", CPUQuota: "400%", TasksMax: "8192"},
			ReadyTimeout: 2 * time.Minute,
			Log:          GinkgoWriter,
		}
		Expect(machine.Check(real, config)).To(Succeed())
		DeferCleanup(func() {
			host("sudo", "-n", liveHelper, "terminate", name)
			host("systemctl", "reset-failed", "mill-machine@"+id+".service")
			for _, copy := range []string{"cache/mise", "cache/go-mod", "cache/go-build", "candidate/mise", "candidate/go-mod", "candidate/go-build"} {
				host("btrfs", "subvolume", "delete", filepath.Join(liveRoot, "runs", id, copy))
			}
			host("rm", "-rf", filepath.Join(liveRoot, "runs", id), filepath.Join(liveRunState, id))
		})
	})

	// Scenario: A nested machine boots an inner machine and execs into it
	//   Given a machine provisioned with nested
	//   When a session boots an inner machine from a bound image and execs a command exiting 7
	//   Then the exit code is 7 and the outer's machinectl lists inner
	It("A nested machine boots an inner machine and execs into it", func(ctx context.Context) {
		By("given a machine provisioned with nested")
		ws = machine.New(real, config)
		handle, err := ws.Create(ctx, workspace.Spec{RunID: id, NodeRef: "mill#66"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ws.Findings()).To(BeEmpty(), "the grant leaves no finding")
		manifest, err := machine.ReadManifest(filepath.Join(liveRoot, "runs", id, machine.ManifestName))
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest.Nested).To(BeTrue())
		Expect(manifest.Binds).To(ContainElements(
			machine.Bind{Source: liveMachines, Target: "/var/lib/machines", ReadOnly: true},
			machine.Bind{Source: machine.DefaultDaggerCheckout, Target: machine.DaggerCheckoutTarget, ReadOnly: true}))
		nested := image.Derived(base, nil, image.Nested)
		Expect(handle.Sandbox.Image).To(Equal(string(nested)), "the nested variant boots")
		Expect(strings.TrimSpace(must("machinectl", "show", name, "-p", "State"))).To(Equal("State=running"))

		By("and inside it agent has sudo, nspawn and btrfs-progs, and sees the two binds")
		// The image directory is root's on the host and so inside (the bind
		// is idmapped for the worker's uid, not root's); the outer's sudo
		// reads it, as the snapshot below needs.
		out, status := inMachine(name, "agent", "sh", "-c", "sudo -n id -u && rpm -q systemd-container btrfs-progs && sudo -n ls /var/lib/machines/mill-image-"+string(base)+" >/dev/null && test -d "+machine.DaggerCheckoutTarget+"/internal/store/migrations && echo binds")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("0\n"))
		Expect(out).To(ContainSubstring("binds"))
		out, status = inMachine(name, "agent", "sh", "-c", "sudo -n touch /var/lib/machines/x 2>&1; sudo -n touch "+machine.DaggerCheckoutTarget+"/x 2>&1; true")
		Expect(status).To(BeZero())
		Expect(strings.Count(out, "Read-only file system")).To(Equal(2), "both binds are read-only even to root inside: %s", out)

		By("and agent creates, snapshots and deletes subvolumes in the outer without a further sudo")
		out, status = inMachine(name, "agent", "sh", "-c",
			"cd /home/agent && btrfs subvolume create s && btrfs subvolume snapshot -r s s-ro && btrfs property set -ts s-ro ro false && btrfs subvolume delete s s-ro && echo ok")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("ok"))

		By("when a session boots an inner machine from the bound image: snapshot it into the outer's tree, a fresh tmpfs for nspawn's tunnel, then nspawn under a unit")
		snapshot := fmt.Sprintf("sudo -n mkdir -p %[1]s && sudo -n btrfs subvolume snapshot /var/lib/machines/mill-image-%[2]s %[1]s/mill-image-%[2]s", innerImages, base)
		out, status = inMachine(name, "agent", "sh", "-c", snapshot)
		Expect(status).To(BeZero(), out)
		out, status = inMachine(name, "agent", "sh", "-c", "sudo -n mkdir -p /run/systemd/nspawn && sudo -n mount -t tmpfs tmpfs /run/systemd/nspawn")
		Expect(status).To(BeZero(), out)
		out, status = inMachine(name, "agent", "sh", "-c", "findmnt -no FSTYPE /.hostsys")
		Expect(strings.TrimSpace(out)).To(Equal("sysfs"),
			"the outer has no host sysfs at /.hostsys: the inner's nspawn cannot mount proc or sysfs without one in its tree when it pivots (docs/sandbox.md §Provision). "+
				"Homestead's mill-machine must add --bind-ro=/sys:/.hostsys to the outer's argv when nested is true; status %d: %s", status, out)
		boot := strings.Join([]string{
			"sudo", "-n", "systemd-run", "--unit=" + innerMachine, "--collect",
			"systemd-nspawn", "--boot", "--quiet", "--machine=" + innerMachine,
			"--directory=" + innerImages + "/mill-image-" + string(base), "--ephemeral",
			"--private-users=identity", "--network-veth", "--settings=false",
			"--bind-ro=/proc:/.hostproc", "--bind-ro=/.hostsys:/.hostsys",
		}, " ")
		out, status = inMachine(name, "agent", "sh", "-c", boot)
		Expect(status).To(BeZero(), out)
		Eventually(func() string {
			out, _ := inInner(name, "", nil, "systemctl", "is-system-running", "--wait")
			return strings.TrimSpace(out)
		}, 90*time.Second, time.Second).Should(BeElementOf("running", "degraded"), "the inner reached ready")

		By("then the outer's machinectl lists inner")
		out, status = inMachine(name, "agent", "sudo", "-n", "machinectl", "list", "--no-legend")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring(innerMachine))
		Expect(must("machinectl", "list", "--no-legend")).NotTo(ContainSubstring(innerMachine), "the host's machined never learns of the inner")

		By("and a command exiting 7 in the inner returns 7 through every layer")
		out, status = inInner(name, "", nil, "sh", "-c", "exit 7")
		Expect(status).To(Equal(7), out)
		out, status = inInner(name, "", nil, "sh", "-c", "cat /proc/self/uid_map; systemd-detect-virt --container")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("systemd-nspawn"))
		Expect(strings.Fields(strings.SplitN(out, "\n", 2)[0])).To(Equal([]string{"0", "0", "65536"}), "identity: the inner's root is the outer's root")

		By("and podman inside the inner, as agent, runs a container pulled through the outer's masquerade and the host's proxy")
		proxy := map[string]string{
			workspace.HTTPSProxyVar: machine.DefaultProxy, strings.ToLower(workspace.HTTPSProxyVar): machine.DefaultProxy,
			workspace.HTTPProxyVar: machine.DefaultProxy, strings.ToLower(workspace.HTTPProxyVar): machine.DefaultProxy,
		}
		var hello string
		Eventually(func() int {
			hello, status = inInner(name, "agent", proxy, "podman", "run", "--rm", "quay.io/podman/hello")
			return status
		}, 3*time.Minute, 5*time.Second).Should(BeZero(), func() string { return hello })
		Expect(hello).To(ContainSubstring("Hello Podman World"))
	})

	// Scenario: A nested run does not promote into the outer's lineages
	//   Given an inner run whose step closed on evidence
	//   When the inner run finishes
	//   Then the outer repo's lineage has no new generation
	It("A nested run does not promote into the outer's lineages", func(ctx context.Context) {
		Expect(ws).NotTo(BeNil(), "the outer from the previous scenario")
		outerLineage := filepath.Join(liveRoot, "cache", "repo", cache.RepoID(origin), "go-build")
		before := generations(outerLineage)

		By("given an inner run whose step closed on evidence: the cache package's nested live spec, compiled here and run inside the outer as agent")
		binary := filepath.Join(liveRoot, "runs", id, machine.WorkspaceDir, "cache.test")
		must("go", "test", "-c", "-o", binary, filepath.Join(repoRoot(), "internal", "workspace", "machine", "cache"))
		DeferCleanup(func() { _ = os.Remove(binary) })

		By("when the inner run finishes")
		out, status := inMachine(name, "agent", "sh", "-c",
			"cd /home/agent && "+liveVar+"=1 HOME=/home/agent /work/cache.test --ginkgo.label-filter=live --ginkgo.focus='A nested run promotes under its own root' --ginkgo.no-color 2>&1")
		Expect(status).To(BeZero(), out)
		Expect(out).To(ContainSubstring("1 Passed"))

		By("then the inner's generation is under a root inside the outer's ephemeral root")
		out, status = inMachine(name, "agent", "sh", "-c", "find "+machine.NestedRoot+" -maxdepth 6 -name 'gen-*' -type d; findmnt -no TARGET -T "+machine.NestedRoot)
		Expect(status).To(BeZero(), out)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		Expect(lines).To(HaveLen(2), out)
		Expect(lines[0]).To(HavePrefix(machine.NestedRoot + "/cache-live-nested/cache/"))
		Expect(lines[0]).To(HaveSuffix("/go-build/gen-1"))
		Expect(lines[1]).To(Equal("/"), "the nested root is on the ephemeral root, not on a bind")

		By("and the outer repo's lineage has no new generation")
		Expect(generations(outerLineage)).To(Equal(before))
		newer, _, _ := host("find", filepath.Join(liveRoot, "cache"), "-newer", binary, "-name", "gen-*")
		Expect(strings.TrimSpace(newer)).To(BeEmpty(), "nothing under the host's lineages is newer than the inner run")
	})

	// Scenario: Inner machines die with the outer
	//   Given an outer machine with an inner machine running
	//   When the outer's unit is stopped
	//   Then no inner machine is registered anywhere
	It("Inner machines die with the outer", func(ctx context.Context) {
		Expect(ws).NotTo(BeNil(), "the outer from the first scenario")

		By("given an outer machine with an inner machine running")
		out, status := inMachine(name, "agent", "sudo", "-n", "machinectl", "show", innerMachine, "-p", "State")
		Expect(status).To(BeZero(), out)
		Expect(strings.TrimSpace(out)).To(Equal("State=running"))

		By("when the outer's unit is stopped")
		Expect(ws.Destroy(ctx)).To(Succeed())
		ws = nil

		By("then no inner machine is registered anywhere")
		listed := must("machinectl", "list", "--no-legend")
		Expect(listed).NotTo(ContainSubstring(innerMachine))
		Expect(listed).NotTo(ContainSubstring(name))
		units, _, _ := host("systemctl", "list-units", "--all", "--no-legend", "inner.service")
		Expect(strings.TrimSpace(units)).To(BeEmpty(), "no unit on the host answers to the inner; it was the outer's: %s", units)
		active, _, _ := host("systemctl", "is-active", "mill-machine@"+id+".service")
		Expect(strings.TrimSpace(active)).NotTo(Equal("active"))
		Expect(subvolumes(liveMachines)).NotTo(ContainElement(HavePrefix(".#machine.mill-image-")), "the outer's snapshot, and the inner's inside it, are gone")
		Expect(filepath.Join(liveRoot, "runs", id)).NotTo(BeADirectory())
	})
})
