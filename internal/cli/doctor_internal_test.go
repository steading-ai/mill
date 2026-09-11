package cli

import (
	"errors"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/driver"
)

var _ = Describe("The doctor's workspace probe", Label("unit"), func() {
	var (
		fake *driver.FakeDriver
		opts doctorOptions
	)

	BeforeEach(func() {
		fake = driver.NewFake()
		opts = doctorOptions{sandboxRoot: filepath.Join("/var", "lib", "mill")}
	})

	It("names the local backend and asks the host nothing", func() {
		opts.workspace = ""

		result := probeWorkspace(opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Name).To(Equal("workspace"))
		Expect(result.Reason).To(HavePrefix("local:"))
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("fails the machine backend on a sandbox root that is not btrfs, naming the filesystem it found", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, driver.Filesystem{Type: "xfs"})

		result := probeWorkspace(opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(HavePrefix("machine:"))
		Expect(result.Reason).To(ContainSubstring("/var/lib/mill is xfs, not btrfs"))
		Expect(fake.CallsTo(driver.Statfs)).To(Equal([]driver.Call{
			{Method: driver.Statfs, Args: []any{"/var/lib/mill"}},
		}))
	})

	It("passes the machine backend on a btrfs sandbox root", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})

		result := probeWorkspace(opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Reason).To(Equal("machine: sandbox root /var/lib/mill is btrfs"))
	})

	It("fails a sandbox root it cannot stat, with the reason", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, errors.New("statfs /var/lib/mill: no such file or directory"))

		result := probeWorkspace(opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no such file or directory"))
	})

	It("fails a kind it does not know rather than probing something else", func() {
		opts.workspace = "podman"

		result := probeWorkspace(opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("podman"))
		Expect(fake.Calls()).To(BeEmpty())
	})
})
