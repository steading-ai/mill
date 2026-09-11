package nspawn_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// liveVar turns on the specs that boot a real machine. Off by default:
// `make test` must not need root, nspawn or the spike image.
const liveVar = "MILL_SANDBOX_HOST"

const (
	image     = "/var/lib/machines/mill-image-spike"
	template  = "/etc/systemd/system/mill-machine@.service"
	rule      = "/etc/polkit-1/rules.d/50-mill-machine-spike.rules"
	hello     = "quay.io/podman/hello"
	sleeper   = "docker.io/library/alpine"
	bootFlags = "--boot --quiet --directory=" + image + " --ephemeral --private-users=pick --private-users-ownership=auto --network-veth --settings=false"
)

// run executes argv on the host and returns stdout, stderr and the exit
// status; a non-zero status is not an error, because the scenarios assert on
// it. Anything else that stops the command from running is.
func run(stdin []byte, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		Fail(fmt.Sprintf("%s: %v", strings.Join(argv, " "), err))
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

// execIn is docs/sandbox.md §Exec: systemd-run -M as root, with the
// properties the caller names, its main process's status propagated.
func execIn(machine string, props []string, argv ...string) (stdout, stderr string, status int) {
	GinkgoHelper()
	cmd := []string{"sudo", "systemd-run", "-M", machine, "--pipe", "--wait", "--quiet", "--collect"}
	for _, p := range props {
		cmd = append(cmd, "-p", p)
	}
	cmd = append(cmd, "--")
	return run(nil, append(cmd, argv...)...)
}

// boot starts an ephemeral machine under a transient unit with extra nspawn
// flags, waits until its bus answers and it reports running, and stacks the
// pristine proc the write-up's E2 found rootless podman needs. It is torn
// down with the spec.
func boot(machine string, extra ...string) {
	GinkgoHelper()
	unit := "mill-run-" + machine
	argv := append([]string{"sudo", "systemd-run", "--unit=" + unit, "--collect", "-p", "KillMode=mixed",
		"systemd-nspawn", "--machine=" + machine}, strings.Fields(bootFlags)...)
	_, stderr, status := run(nil, append(argv, extra...)...)
	Expect(status).To(BeZero(), stderr)
	DeferCleanup(func() {
		run(nil, "sudo", "systemctl", "stop", unit)
	})

	Eventually(func() int {
		_, _, status := execIn(machine, nil, "true")
		return status
	}).WithTimeout(30*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(), "the machine's bus never answered")
	state, _, _ := execIn(machine, nil, "systemctl", "is-system-running", "--wait")
	Expect(strings.TrimSpace(state)).To(Equal("running"))

	_, stderr, status = execIn(machine, nil, "mount", "-t", "proc", "-o", "nosuid,nodev,noexec", "proc", "/proc")
	Expect(status).To(BeZero(), stderr)
}

// load pulls an image on the host straight from its registry — Harbor is
// not assumed reachable — and pipes it into agent's store in the machine.
func load(machine, ref string) {
	GinkgoHelper()
	pull := exec.Command("podman", "pull", "-q", ref)
	pull.Env = append(os.Environ(), "CONTAINERS_REGISTRIES_CONF=/dev/null")
	pull.Stderr = GinkgoWriter
	Expect(pull.Run()).To(Succeed(), "pulling %s on the host", ref)

	tar, _, status := run(nil, "podman", "save", ref)
	Expect(status).To(BeZero())
	_, stderr, status := run([]byte(tar), "sudo", "systemd-run", "-M", machine, "--pipe", "--wait", "--quiet", "--collect", "-p", "User=agent", "--", "podman", "load")
	Expect(status).To(BeZero(), stderr)
}

var _ = Describe("The per-run sandbox on a real machine", Label("live"), Serial, func() {
	BeforeEach(func() {
		if os.Getenv(liveVar) == "" {
			Skip("set " + liveVar + "=1 on a host with root, nspawn and btrfs to run the spec that boots a real machine")
		}
		// /var/lib/machines is root's and 0700, so the check is root's too.
		if _, _, status := run(nil, "sudo", "test", "-d", image); status != 0 {
			Skip(image + " is missing; spikes/nspawn/build-image.sh builds it")
		}
	})

	It("A machine starts and stops from the operator's uid", func() {
		By("the template unit and the polkit rule are installed")
		if _, _, status := run(nil, "sudo", "test", "-f", template); status != 0 {
			Skip(template + " is not installed; docs/spikes/nspawn-per-run-sandbox.md §E0 has it")
		}
		if _, _, status := run(nil, "sudo", "test", "-f", rule); status != 0 {
			Skip(rule + " is not installed; docs/spikes/nspawn-per-run-sandbox.md §E0 has it")
		}

		By("the operator starts mill-machine@mill-e0 without sudo, execs a command returning 42, and stops it")
		_, stderr, status := run(nil, "systemctl", "start", "mill-machine@mill-e0.service")
		DeferCleanup(func() {
			// A stop that lands before the container's init has its signal
			// handlers hangs the unit and orphans the payload in mill-e0.scope
			// (the write-up's E0); terminate clears whatever stop left.
			run(nil, "sudo", "systemctl", "stop", "mill-machine@mill-e0.service")
			run(nil, "sudo", "machinectl", "terminate", "mill-e0")
		})
		Expect(status).To(BeZero(), "start as the operator: %s", stderr)

		Eventually(func() string {
			state, _, _ := run(nil, "machinectl", "show", "mill-e0", "-p", "State")
			return strings.TrimSpace(state)
		}).WithTimeout(30 * time.Second).Should(Equal("State=running"))
		// State=running precedes the machine's bus by ~200 ms, and a stop in
		// that window is lost; wait for the bus before the operator's verbs.
		Eventually(func() int {
			_, _, status := execIn("mill-e0", nil, "true")
			return status
		}).WithTimeout(30*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(), "the machine's bus never answered")

		stdout, stderr, execStatus := run(nil, "systemd-run", "-M", "mill-e0", "--pipe", "--wait", "--quiet", "--collect", "--", "sh", "-c", "echo out; exit 42")

		_, stopErr, stopStatus := run(nil, "systemctl", "stop", "mill-machine@mill-e0.service")
		Expect(stopStatus).To(BeZero(), "stop as the operator: %s", stopErr)

		By("all three succeed and the exit status is 42")
		Expect(stdout).To(Equal("out\n"), "exec as the operator: %s", stderr)
		Expect(execStatus).To(Equal(42), "exec as the operator: %s", stderr)

		By("starting a unit outside the template is refused")
		_, stderr, status = run(nil, "systemctl", "start", "mill-run-anything-else.service")
		Expect(status).NotTo(BeZero())
		Expect(stderr).To(ContainSubstring("Access denied"))
	})

	It("Podman runs inside a booted machine", func() {
		By("an ephemeral machine booted from the spike image")
		boot("mill-espec")
		load("mill-espec", hello)

		By("agent runs podman info and quay.io/podman/hello")
		info, stderr, status := execIn("mill-espec", []string{"User=agent"}, "podman", "info", "--format", "{{.Store.GraphDriverName}} {{.Host.CgroupManager}}")
		Expect(status).To(BeZero(), stderr)
		out, stderr, helloStatus := execIn("mill-espec", []string{"User=agent"}, "podman", "run", "--rm", hello)

		By("the driver is overlay, the cgroup manager is systemd, and hello prints")
		Expect(strings.TrimSpace(info)).To(Equal("overlay systemd"))
		Expect(helloStatus).To(BeZero(), stderr)
		Expect(out).To(ContainSubstring("Hello Podman World"))
	})

	It("Stopping the step slice reaps the step", func() {
		By("a step that left a nohup sleep and a detached container")
		boot("mill-espec")
		load("mill-espec", sleeper)
		// Delegate= lets agent's podman make sub-cgroups under the exec unit;
		// KillMode=mixed SIGKILLs what the step leaves behind instead of
		// waiting TimeoutStopSec for a SIGTERM the container's pid 1 ignores.
		step := []string{"User=agent", "Slice=mill-step-1.slice", "Delegate=yes", "KillMode=mixed"}
		_, stderr, status := execIn("mill-espec", step, "sh", "-c",
			"nohup sleep 3600 >/dev/null 2>&1 & podman run --cgroups=split -d --name lingering "+sleeper+" sleep 3600 && sleep 2")
		Expect(status).To(BeZero(), stderr)

		By("the step's slice is stopped")
		_, stderr, status = execIn("mill-espec", nil, "systemctl", "stop", "mill-step-1.slice")
		Expect(status).To(BeZero(), stderr)

		By("neither process nor container exists in the next step")
		survivors, _, _ := execIn("mill-espec", []string{"User=agent", "Slice=mill-step-2.slice"}, "sh", "-c",
			`for c in /proc/[0-9]*/comm; do grep -q "^sleep" "$c" && echo "$c"; done; `+
				`pid=$(podman inspect lingering --format "{{.State.Pid}}" 2>/dev/null); [ -n "$pid" ] && [ -d /proc/$pid ] && echo "container pid $pid alive"; true`)
		Expect(strings.TrimSpace(survivors)).To(BeEmpty(), "left running after the slice stopped:\n%s", survivors)
	})

	It("The workspace bind keeps the owner's uid", func() {
		By("a host directory owned by uid 1000 bound at /work")
		workspace := "/var/lib/mill/runs/espec/workspace"
		_, stderr, status := run(nil, "sudo", "sh", "-c", "mkdir -p "+workspace+" && chown 1000:1000 "+workspace)
		Expect(status).To(BeZero(), stderr)
		DeferCleanup(func() {
			run(nil, "sudo", "sh", "-c", "rm -rf /var/lib/mill/runs/espec; rmdir /var/lib/mill/runs 2>/dev/null; true")
		})
		boot("mill-espec", "--bind="+workspace+":/work:idmap")

		By("agent commits inside and the host reads the commit")
		inside, stderr, status := execIn("mill-espec", []string{"User=agent", "WorkingDirectory=/work"}, "sh", "-c",
			"git init -q . && git -c user.name=e4 -c user.email=e4@example.invalid commit -q --allow-empty -m e4 && git rev-parse HEAD")
		Expect(status).To(BeZero(), stderr)
		host, stderr, status := run(nil, "sudo", "-u", "#1000", "git", "-C", workspace, "rev-parse", "HEAD")
		Expect(status).To(BeZero(), stderr)
		owner, _, _ := run(nil, "stat", "-c", "%u", workspace+"/.git/HEAD")

		By("both see the same SHA and the host sees owner 1000")
		Expect(strings.TrimSpace(inside)).To(HaveLen(40))
		Expect(strings.TrimSpace(host)).To(Equal(strings.TrimSpace(inside)))
		Expect(strings.TrimSpace(owner)).To(Equal("1000"))
	})
})
