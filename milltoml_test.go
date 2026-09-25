package mill_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workspace/machine/config"
)

// The repository's own declaration (docs/sandbox.md §Self-hosting, step 3):
// mill's sessions run in a machine that may boot machines, and what the
// worker reads from the checkout is this file, so it has to load without a
// finding — a finding would reach the first session of every run of mill
// as something to fix.
var _ = Describe("The repository's mill.toml", Label("unit"), func() {
	It("requests nested and declares btrfs-progs, with no finding", func() {
		loaded, findings, err := config.Load(".")
		Expect(err).NotTo(HaveOccurred())
		Expect(findings).To(BeEmpty())
		Expect(loaded.Machine.Nested).To(BeTrue())
		Expect(loaded.Image.Packages).To(ContainElement("btrfs-progs"))
	})
})
