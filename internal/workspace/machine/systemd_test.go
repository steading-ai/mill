package machine_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
)

// floorMark is a variable only the spec's spawner carries on its floor. A
// stub that sees it was run through that spawner; the mill's own environment
// never has it.
const floorMark = "MILL_SPEC_FLOOR"

// host is a directory of stub host commands and the spawner that runs them.
// A stub records its arguments, one per line, to a file beside it, so a spec
// reads what the driver ran; the driver is told each stub by path, because
// the spawner resolves a bare name on the mill's PATH and never on its
// floor. Stubs are never on that PATH: a driver that reached for the real
// tool would be running the real thing on this host.
type host struct {
	dir      string
	spawner  workspace.Spawner
	commands machine.Commands
}

func newHost() *host {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	stub := func(name string) string { return filepath.Join(dir, name) }
	return &host{
		dir:     dir,
		spawner: workspace.Spawner{Floor: []string{"PATH=/usr/bin:/bin", floorMark + "=through-the-spawner"}},
		commands: machine.Commands{
			Systemctl:  stub("systemctl"),
			Machinectl: stub("machinectl"),
			SystemdRun: stub("systemd-run"),
			Btrfs:      stub("btrfs"),
			Podman:     stub("podman"),
			Tar:        stub("tar"),
			Setcap:     stub("setcap"),
			Du:         stub("du"),
		},
	}
}

// stub installs a command that appends its arguments, one per line, to
// <name>.calls, notes the floor it ran under in <name>.floor, and then runs
// body with them.
func (h *host) stub(name, body string) {
	GinkgoHelper()
	script := "#!/bin/sh\nhere=\"$(dirname \"$0\")\"\n" +
		"printf '%s\\n' \"$@\" >> \"$here/" + name + ".calls\"\n" +
		"printf '%s' \"$" + floorMark + "\" > \"$here/" + name + ".floor\"\n" + body
	Expect(os.WriteFile(filepath.Join(h.dir, name), []byte(script), 0o700)).To(Succeed()) //nolint:gosec // a spec's own stub
}

// floor is the mark a stub saw on its environment, or nothing.
func (h *host) floor(name string) string {
	raw, err := os.ReadFile(filepath.Join(h.dir, name+".floor")) //nolint:gosec // a spec's own scratch file
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// calls is the arguments a stub was run with, as lines.
func (h *host) calls(name string) []string {
	raw, err := os.ReadFile(filepath.Join(h.dir, name+".calls")) //nolint:gosec // a spec's own scratch file
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// drain reads a process to its end and reports its output and how it ended.
func drain(process *workspace.Process) (stdout, stderr string, result workspace.Result) {
	GinkgoHelper()
	_ = process.Stdin.Close()
	out, err := io.ReadAll(process.Stdout)
	Expect(err).NotTo(HaveOccurred())
	errOut, err := io.ReadAll(process.Stderr)
	Expect(err).NotTo(HaveOccurred())
	return string(out), string(errOut), process.Wait()
}

var _ = Describe("The systemd driver", Label("unit"), func() {
	var (
		h      *host
		driver machine.Driver
	)

	BeforeEach(func() {
		h = newHost()
		driver = machine.NewSystemd(h.spawner, h.commands)
	})

	// Scenario: The driver is the only door to the host
	//   Given the systemd driver
	//   When it starts a unit
	//   Then the command goes through Spawner.Exec
	It("The driver is the only door to the host", func(ctx context.Context) {
		By("given the systemd driver")
		// The spawner's floor carries a mark nothing else does. A command
		// run through anything but Spawner.Exec — the mill's own
		// environment, say — would not see it.
		h.stub("systemctl", "exit 0")
		Expect(os.Getenv(floorMark)).To(BeEmpty())

		By("when it starts a unit")
		Expect(driver.StartUnit(ctx, "mill-machine@01RUN.service")).To(Succeed())

		By("then the command goes through Spawner.Exec")
		Expect(h.calls("systemctl")).To(Equal([]string{"start", "mill-machine@01RUN.service"}))
		Expect(h.floor("systemctl")).To(Equal("through-the-spawner"))
	})

	It("carries a failed command's exit status and what it said on stderr", func(ctx context.Context) {
		h.stub("systemctl", `echo "Failed to start mill-machine@01RUN.service: Access denied" >&2; exit 4`)

		err := driver.StartUnit(ctx, "mill-machine@01RUN.service")

		Expect(err).To(MatchError(ContainSubstring("exited 4")))
		Expect(err).To(MatchError(ContainSubstring("Access denied")))
	})

	It("stops a unit on the host, or inside a machine when one is named", func(ctx context.Context) {
		h.stub("systemctl", "exit 0")

		Expect(driver.StopUnit(ctx, "", "mill-machine@01RUN.service")).To(Succeed())
		Expect(driver.StopUnit(ctx, "mill-01RUN", "mill-step-3.slice")).To(Succeed())

		Expect(h.calls("systemctl")).To(Equal([]string{
			"stop", "mill-machine@01RUN.service",
			"--machine=mill-01RUN", "stop", "mill-step-3.slice",
		}))
	})

	It("reads a machine as machined shows it", func(ctx context.Context) {
		h.stub("machinectl", "printf 'Name=mill-01RUN\\nState=running\\nClass=container\\nService=systemd-nspawn\\n'")

		shown, err := driver.ShowMachine(ctx, "mill-01RUN")

		Expect(err).NotTo(HaveOccurred())
		Expect(shown).To(Equal(machine.Machine{
			Name: "mill-01RUN", State: machine.StateRunning, Class: "container", Service: "systemd-nspawn",
		}))
		Expect(h.calls("machinectl")[:2]).To(Equal([]string{"show", "mill-01RUN"}))
	})

	It("reports a machine machined does not know as ErrNoMachine", func(ctx context.Context) {
		h.stub("machinectl", `echo "Could not get path to machine: No machine 'mill-01RUN' known" >&2; exit 1`)

		_, err := driver.ShowMachine(ctx, "mill-01RUN")

		Expect(err).To(MatchError(machine.ErrNoMachine))
		Expect(err).To(MatchError(ContainSubstring("mill-01RUN")))
	})

	It("lists the machines on the host from machinectl's columns", func(ctx context.Context) {
		h.stub("machinectl", "printf 'mill-01RUN container systemd-nspawn fedora 44 -\\nqemu-1-guest vm libvirt-qemu - - -\\n'")

		listed, err := driver.ListMachines(ctx)

		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(Equal([]machine.Machine{
			{Name: "mill-01RUN", Class: "container", Service: "systemd-nspawn"},
			{Name: "qemu-1-guest", Class: "vm", Service: "libvirt-qemu"},
		}))
		Expect(h.calls("machinectl")).To(ContainElements("list", "--no-legend"))
	})

	Describe("exec", func() {
		// The stub is systemd-run as the driver sees it: it reports its
		// arguments on stderr, copies its stdin to stdout, and exits as
		// the command under it would — which is what `--pipe --wait` does.
		BeforeEach(func() {
			h.stub("systemd-run", `printf '%s\n' "$@" >&2; cat; exit 42`)
		})

		It("streams stdin, stdout and stderr and reports the command's exit status", func(ctx context.Context) {
			process, err := driver.Exec(ctx, machine.ExecRequest{
				Machine: "mill-01RUN",
				Unit:    "mill-exec-0001",
				Slice:   "mill-step-2.slice",
				Dir:     "/work",
				User:    "agent",
				Env:     map[string]string{"STEP_TOKEN": "t", "HOME": "/home/agent", "NO_PROXY": "a b"},
				Argv:    []string{"sh", "-c", "cat"},
			})
			Expect(err).NotTo(HaveOccurred())

			go func() {
				defer GinkgoRecover()
				_, err := io.WriteString(process.Stdin, "piped in\n")
				Expect(err).NotTo(HaveOccurred())
				Expect(process.Stdin.Close()).To(Succeed())
			}()
			out, err := io.ReadAll(process.Stdout)
			Expect(err).NotTo(HaveOccurred())
			errOut, err := io.ReadAll(process.Stderr)
			Expect(err).NotTo(HaveOccurred())
			result := process.Wait()

			By("stdin reaches the command and its stdout comes back")
			Expect(out).To(Equal([]byte("piped in\n")))
			By("the exit status is the command's own")
			Expect(result).To(Equal(workspace.Result{Status: 42}))
			By("the invocation is the one docs/sandbox.md §Exec names")
			Expect(strings.Split(strings.TrimSpace(string(errOut)), "\n")).To(Equal([]string{
				"--machine=mill-01RUN",
				"--pipe", "--wait", "--quiet", "--collect",
				"--property=Slice=mill-step-2.slice",
				"--unit=mill-exec-0001",
				"--property=WorkingDirectory=/work",
				"--property=User=agent",
				"--setenv=HOME=/home/agent",
				"--setenv=NO_PROXY=a b",
				"--setenv=STEP_TOKEN=t",
				"--",
				"sh", "-c", "cat",
			}))
		})

		It("refuses an exec under no step slice, because nothing would reap it", func(ctx context.Context) {
			_, err := driver.Exec(ctx, machine.ExecRequest{Machine: "mill-01RUN", Argv: []string{"true"}})

			Expect(err).To(MatchError(ContainSubstring("step slice")))
			Expect(h.calls("systemd-run")).To(BeEmpty(), "nothing was run")
		})

		It("refuses an exec that names no machine or nothing to run", func(ctx context.Context) {
			_, err := driver.Exec(ctx, machine.ExecRequest{Slice: "mill-step-1.slice", Argv: []string{"true"}})
			Expect(err).To(MatchError(ContainSubstring("no machine")))

			_, err = driver.Exec(ctx, machine.ExecRequest{Machine: "mill-01RUN", Slice: "mill-step-1.slice"})
			Expect(err).To(MatchError(ContainSubstring("nothing to run")))
		})
	})

	It("snapshots and deletes subvolumes with btrfs", func(ctx context.Context) {
		h.stub("btrfs", "exit 0")

		Expect(driver.Snapshot(ctx, "/var/lib/mill/cache/global/go-mod/gen-3", "/var/lib/mill/runs/01RUN/cache/go-mod", false)).To(Succeed())
		Expect(driver.Snapshot(ctx, "/var/lib/mill/runs/01RUN/cache/go-mod", "/var/lib/mill/runs/01RUN/cache/go-mod.candidate", true)).To(Succeed())
		Expect(driver.DeleteSubvolume(ctx, "/var/lib/mill/runs/01RUN/cache/go-mod", false)).To(Succeed())
		Expect(driver.SetReadOnly(ctx, "/var/lib/mill/cache/global/go-mod/gen-1", false)).To(Succeed())
		Expect(driver.DeleteSubvolume(ctx, "/var/lib/mill/cache/global/go-mod/gen-1", true)).To(Succeed())

		Expect(h.calls("btrfs")).To(Equal([]string{
			"subvolume", "snapshot", "/var/lib/mill/cache/global/go-mod/gen-3", "/var/lib/mill/runs/01RUN/cache/go-mod",
			"subvolume", "snapshot", "-r", "/var/lib/mill/runs/01RUN/cache/go-mod", "/var/lib/mill/runs/01RUN/cache/go-mod.candidate",
			"subvolume", "delete", "/var/lib/mill/runs/01RUN/cache/go-mod",
			"property", "set", "-ts", "/var/lib/mill/cache/global/go-mod/gen-1", "ro", "false",
			"subvolume", "delete", "/var/lib/mill/cache/global/go-mod/gen-1",
			"filesystem", "sync", "/var/lib/mill/cache/global/go-mod",
		}))
	})

	It("estimates a tree's size with du, in bytes", func(ctx context.Context) {
		h.stub("du", `printf '2834100\t%s\n' "$2"`)

		size, err := driver.DiskUsage(ctx, "/var/lib/mill/cache/repo/r/go-build/gen-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(Equal(uint64(2834100 * 1024)))
		Expect(h.calls("du")).To(Equal([]string{"-sk", "/var/lib/mill/cache/repo/r/go-build/gen-2"}))
	})

	It("reports the filesystem under a path by name, and nothing for a path that is not there", func() {
		fs, err := driver.Statfs(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.Type).NotTo(BeEmpty())
		Expect(fs.Total).To(BeNumerically(">", 0))

		_, err = driver.Statfs(filepath.Join(GinkgoT().TempDir(), "absent"))
		Expect(err).To(MatchError(ContainSubstring("absent")))
	})

	It("drains a command that says nothing and exits zero", func(ctx context.Context) {
		h.stub("btrfs", "exit 0")
		process, err := h.spawner.Exec(ctx, workspace.Command{Path: h.commands.Btrfs, Args: []string{"subvolume", "sync", "/x"}})
		Expect(err).NotTo(HaveOccurred())
		stdout, _, result := drain(process)
		Expect(stdout).To(BeEmpty())
		Expect(result.Status).To(BeZero())
	})
})
