package cli

import (
	"errors"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// errNoSubvolume is what the fake host answers a subvolume create with.
var errNoSubvolume = errors.New("scripted: no subvolume here")

var _ = Describe("The sandbox root of a worker inside a machine", Label("unit"), func() {
	var (
		cmd  *cobra.Command
		root string
	)

	BeforeEach(func() {
		cmd = &cobra.Command{Use: "probe"}
		root = machine.DefaultRoot
		cmd.Flags().StringVar(&root, "sandbox-root", machine.DefaultRoot, "")
	})

	It("defaults to a subvolume under the ephemeral root when systemd-detect-virt reports a machine, and is made as the worker", func() {
		host := driver.NewFake()
		host.Script(driver.Container, driver.ContainerNspawn)
		// A nested root of the spec's own, absent the way a machine's is
		// until its worker makes one — the real one is already there
		// wherever a worker has run, this suite's live specs included, and
		// EnsureNestedRoot rightly asks for nothing then. The fake makes a
		// directory stand in for the subvolume, so the scripted refusal is
		// what proves the create was asked of the driver, at the root it
		// was given, and nothing else.
		nested := filepath.Join(GinkgoT().TempDir(), "mill")
		host.Script(driver.CreateSubvolume, errNoSubvolume)
		Expect(resolveSandboxRootOn(cmd, host, nested, &root, "")).To(MatchError(errNoSubvolume))
		Expect(root).To(Equal(nested))
		Expect(host.CallsTo(driver.CreateSubvolume)).To(Equal([]driver.Call{{Method: driver.CreateSubvolume, Args: []any{nested}}}),
			"the subvolume is created through the sudo-less driver")
	})

	It("keeps the host's default on a host, and whatever the flag or the file named anywhere", func() {
		host := driver.NewFake()
		Expect(resolveSandboxRootOn(cmd, host, machine.NestedRoot, &root, "")).To(Succeed())
		Expect(root).To(Equal(machine.DefaultRoot))

		host.Script(driver.Container, driver.ContainerNspawn)
		Expect(resolveSandboxRootOn(cmd, host, machine.NestedRoot, &root, "/srv/mill")).To(Succeed())
		Expect(root).To(Equal("/srv/mill"), "the file's root stands; Check refuses it if it is off the ephemeral root")

		Expect(cmd.Flags().Set("sandbox-root", "/flag/mill")).To(Succeed())
		host.Script(driver.Container, driver.ContainerNspawn)
		Expect(resolveSandboxRootOn(cmd, host, machine.NestedRoot, &root, "/srv/mill")).To(Succeed())
		Expect(root).To(Equal("/flag/mill"))
		Expect(host.CallsTo(driver.CreateSubvolume)).To(BeEmpty())
	})
})
