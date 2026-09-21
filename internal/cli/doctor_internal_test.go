package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/cache"
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

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Name).To(Equal("workspace"))
		Expect(result.Reason).To(HavePrefix("local:"))
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("fails the machine backend on a sandbox root that is not btrfs, naming the filesystem it found", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, driver.Filesystem{Type: "xfs"})

		result := probeWorkspace(context.Background(), opts, fake)

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

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Reason).To(Equal("machine: sandbox root /var/lib/mill is btrfs"))
	})

	It("fails a sandbox root it cannot stat, with the reason", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, errors.New("statfs /var/lib/mill: no such file or directory"))

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("no such file or directory"))
	})

	It("fails a kind it does not know rather than probing something else", func() {
		opts.workspace = "podman"

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("podman"))
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("fails the machine backend on a host missing what the sandbox needs, naming the reason", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})
		fake.Script(driver.SandboxReady, "systemd-nspawn is not on PATH")

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(Equal("machine: systemd-nspawn is not on PATH"))
	})

	It("does not ask the sandbox is ready from inside a machine", func() {
		opts.workspace = "machine"
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs})
		fake.Script(driver.Container, driver.ContainerNspawn)

		result := probeWorkspace(context.Background(), opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(fake.CallsTo(driver.SandboxReady)).To(BeEmpty())
	})
})

var _ = Describe("The doctor's space probe", Label("unit"), func() {
	var (
		fake *driver.FakeDriver
		opts doctorOptions
	)

	BeforeEach(func() {
		fake = driver.NewFake()
		opts = doctorOptions{workspace: "machine", sandboxRoot: filepath.Join("/var", "lib", "mill")}
	})

	It("names the local backend and asks the host nothing", func() {
		opts.workspace = ""

		result := probeSpace(opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Name).To(Equal("space"))
		Expect(result.Reason).To(HavePrefix("local:"))
		Expect(fake.Calls()).To(BeEmpty())
	})

	It("passes free space above the low watermark, naming both watermarks in bytes", func() {
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 30 << 30})

		result := probeSpace(opts, fake)

		Expect(result.OK).To(BeTrue())
		Expect(result.Warning).To(BeEmpty())
		Expect(result.Reason).To(Equal("30G free of 100G on /var/lib/mill; watermarks high 10% (10G), low 20% (20G)"))
	})

	It("warns between the watermarks and fails below the high one", func() {
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 15 << 30})
		result := probeSpace(opts, fake)
		Expect(result.OK).To(BeTrue())
		Expect(result.Warning).To(ContainSubstring("below the low watermark"))

		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 5 << 30})
		result = probeSpace(opts, fake)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("below the high watermark"))
	})

	It("reads configured watermarks, in bytes or shares, and refuses an inverted pair", func() {
		opts.highWatermark = cache.Watermark{Bytes: 50 << 30}
		opts.lowWatermark = cache.Watermark{Percent: 60}
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 55 << 30})
		result := probeSpace(opts, fake)
		Expect(result.OK).To(BeTrue())
		Expect(result.Warning).To(ContainSubstring("below the low watermark"))
		Expect(result.Reason).To(ContainSubstring("high 50G (50G), low 60% (60G)"))

		opts.lowWatermark = cache.Watermark{Percent: 5}
		fake.Script(driver.Statfs, driver.Filesystem{Type: driver.TypeBtrfs, Total: 100 << 30, Free: 55 << 30})
		result = probeSpace(opts, fake)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("low watermark 5% is below the high watermark 50G"))
	})
})

// mill#70: every probe is named where an operator reads about it.
var _ = Describe("The doctor's probe names", Label("unit"), func() {
	It("are each named by the command's usage and by docs/tools.md §doctor", func() {
		usage := newDoctorCommand().Long
		body, err := os.ReadFile(filepath.Join("..", "..", "docs", "tools.md"))
		Expect(err).NotTo(HaveOccurred())
		doc := string(body)

		for _, name := range ProbeNames {
			Expect(usage).To(MatchRegexp(`(?m)^  %s +\S`, name), "mill doctor --help does not name the %s probe", name)
			Expect(doc).To(ContainSubstring("| `"+name+"` |"), "docs/tools.md §doctor has no row for the %s probe", name)
		}
		Expect(regexp.MustCompile(`(?m)^  ([a-z]+) +\S`).FindAllStringSubmatch(usage, -1)).To(HaveLen(len(ProbeNames)),
			"mill doctor --help names a probe ProbeNames does not")
	})
})
