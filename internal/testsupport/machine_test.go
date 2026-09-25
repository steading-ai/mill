package testsupport_test

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/testsupport"
)

// Where a spec process runs decides which of the live tier's specs can run:
// inside a machine, the ones that need the host skip and say so.
var _ = Describe("Where the live tier runs", Label("unit"), func() {
	It("reads the container manager systemd recorded, and is a host when none was", func() {
		content, err := os.ReadFile("/run/systemd/container")
		switch {
		case err == nil && strings.TrimSpace(string(content)) == "systemd-nspawn":
			Expect(testsupport.InsideMachine()).To(BeTrue())
		case err == nil:
			Expect(testsupport.InsideMachine()).To(BeFalse(), "some other container manager is not a machine: %q", content)
		default:
			Expect(testsupport.InsideMachine()).To(BeFalse())
		}
	})

	It("names what a host-only spec needs when it skips", func() {
		Expect(testsupport.HostOnlySkip("the template unit")).To(SatisfyAll(
			HavePrefix("inside a machine: "),
			ContainSubstring("the template unit"),
			ContainSubstring("docs/sandbox.md §Self-hosting")))
	})
})
