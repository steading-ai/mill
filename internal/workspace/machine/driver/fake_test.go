package driver_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

var _ = Describe("The fake driver", Label("unit"), func() {
	var fake *driver.FakeDriver

	BeforeEach(func() { fake = driver.NewFake() })

	It("records every call with its arguments, in order", func(ctx context.Context) {
		request := driver.ExecRequest{Machine: "mill-01RUN", Slice: "mill-step-1.slice", Argv: []string{"true"}}

		Expect(fake.StartUnit(ctx, "mill-machine@01RUN.service")).To(Succeed())
		Expect(fake.StopUnit(ctx, "mill-01RUN", "mill-step-1.slice")).To(Succeed())
		_, err := fake.ShowMachine(ctx, "mill-01RUN")
		Expect(err).NotTo(HaveOccurred())
		process, err := fake.Exec(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		drain(process)
		_, err = fake.ListMachines(ctx)
		Expect(err).NotTo(HaveOccurred())
		src := filepath.Join(GinkgoT().TempDir(), "src")
		dst := filepath.Join(GinkgoT().TempDir(), "dst")
		Expect(os.Mkdir(src, 0o755)).To(Succeed())
		Expect(fake.Snapshot(ctx, src, dst, true)).To(Succeed())
		Expect(fake.DeleteSubvolume(ctx, dst, false)).To(Succeed())
		_, err = fake.Statfs("/var/lib/mill")
		Expect(err).NotTo(HaveOccurred())

		Expect(fake.Calls()).To(Equal([]driver.Call{
			{Method: driver.StartUnit, Args: []any{"mill-machine@01RUN.service"}},
			{Method: driver.StopUnit, Args: []any{"mill-01RUN", "mill-step-1.slice"}},
			{Method: driver.ShowMachine, Args: []any{"mill-01RUN"}},
			{Method: driver.Exec, Args: []any{request}},
			{Method: driver.ListMachines, Args: nil},
			{Method: driver.Snapshot, Args: []any{src, dst, true}},
			{Method: driver.DeleteSubvolume, Args: []any{dst, false}},
			{Method: driver.Statfs, Args: []any{"/var/lib/mill"}},
		}))
		Expect(fake.CallsTo(driver.Snapshot)).To(HaveLen(1))
	})

	It("scripts a result per call, consumed in order, and succeeds once the script runs out", func(ctx context.Context) {
		fake.Script(driver.ShowMachine,
			driver.Machine{Name: "mill-01RUN", State: "opening"},
			driver.Machine{Name: "mill-01RUN", State: driver.StateRunning},
			errors.New("machined went away"))
		fake.Script(driver.StartUnit, errors.New("polkit refused"))
		fake.Script(driver.Statfs, driver.Filesystem{Type: "xfs", Free: 1, Total: 2})
		fake.Script(driver.ListMachines, []driver.Machine{{Name: "mill-01RUN"}})

		first, err := fake.ShowMachine(ctx, "mill-01RUN")
		Expect(err).NotTo(HaveOccurred())
		Expect(first.State).To(Equal("opening"))
		second, err := fake.ShowMachine(ctx, "mill-01RUN")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.State).To(Equal(driver.StateRunning))
		_, err = fake.ShowMachine(ctx, "mill-01RUN")
		Expect(err).To(MatchError("machined went away"))
		fourth, err := fake.ShowMachine(ctx, "mill-01RUN")
		Expect(err).NotTo(HaveOccurred(), "an exhausted script succeeds")
		Expect(fourth).To(Equal(driver.Machine{}))

		Expect(fake.StartUnit(ctx, "mill-machine@01RUN.service")).To(MatchError("polkit refused"))
		Expect(fake.StartUnit(ctx, "mill-machine@01RUN.service")).To(Succeed())

		fs, err := fake.Statfs("/var/lib/mill")
		Expect(err).NotTo(HaveOccurred())
		Expect(fs).To(Equal(driver.Filesystem{Type: "xfs", Free: 1, Total: 2}))
		Expect(fs.Btrfs()).To(BeFalse())

		listed, err := fake.ListMachines(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(listed).To(ConsistOf(driver.Machine{Name: "mill-01RUN"}))
	})

	It("runs a scripted exec as a real process: stdio streams and the status is what was scripted", func(ctx context.Context) {
		fake.Script(driver.Exec, driver.Output{Stdout: "out\n", Stderr: "err\n", Status: 3})

		process, err := fake.Exec(ctx, driver.ExecRequest{Machine: "mill-01RUN", Slice: "mill-step-1.slice", Argv: []string{"go", "test"}})
		Expect(err).NotTo(HaveOccurred())
		stdout, stderr, result := drain(process)

		Expect(stdout).To(Equal("out\n"))
		Expect(stderr).To(Equal("err\n"))
		Expect(result).To(Equal(workspace.Result{Status: 3}))
	})

	It("returns a scripted exec error instead of a process", func(ctx context.Context) {
		fake.Script(driver.Exec, errors.New("Failed to open namespace of PID 1: Permission denied"))

		process, err := fake.Exec(ctx, driver.ExecRequest{Machine: "mill-01RUN", Slice: "mill-step-1.slice", Argv: []string{"true"}})

		Expect(process).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("Permission denied")))
	})

	It("surfaces a script of the wrong type as the call's error rather than swallowing it", func(ctx context.Context) {
		fake.Script(driver.StopUnit, driver.Machine{})

		Expect(fake.StopUnit(ctx, "", "x")).To(MatchError(ContainSubstring("cannot return a scripted driver.Machine")))
	})

	It("is ready by default and reports a scripted reason, recording the root and machines it was asked about", func(ctx context.Context) {
		reason, err := fake.SandboxReady(ctx, "/var/lib/mill", "/var/lib/machines")
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(BeEmpty(), "the fake is a sandbox-capable host unless told otherwise")

		fake.Script(driver.SandboxReady, "systemd-nspawn is not on PATH")
		reason, err = fake.SandboxReady(ctx, "/var/lib/mill", "/var/lib/machines")
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal("systemd-nspawn is not on PATH"))

		Expect(fake.CallsTo(driver.SandboxReady)).To(Equal([]driver.Call{
			{Method: driver.SandboxReady, Args: []any{"/var/lib/mill", "/var/lib/machines"}},
			{Method: driver.SandboxReady, Args: []any{"/var/lib/mill", "/var/lib/machines"}},
		}))
	})

	It("is a Driver", func() {
		var _ driver.Driver = fake
		process, err := fake.Exec(context.Background(), driver.ExecRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stdout)).Error().NotTo(HaveOccurred())
		Expect(io.Copy(io.Discard, process.Stderr)).Error().NotTo(HaveOccurred())
		Expect(process.Wait().Status).To(BeZero())
	})
})
