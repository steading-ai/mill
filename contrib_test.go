package mill_test

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The unit file is what a deployment installs, so what it sets is part of
// the design and not an example. The one setting asserted here is the one a
// deployment cannot be allowed to forget: a mill started from a unit that
// carries DATABASE_URL and MILL_FORGE_TOKEN hands both to every session it
// spawns unless it is told not to.
var _ = Describe("The systemd unit", Label("unit"), func() {
	It("runs the worker under the strict environment mode", func() {
		body, err := os.ReadFile("contrib/systemd/mill.service")
		Expect(err).NotTo(HaveOccurred())

		Expect(string(body)).To(ContainSubstring("--env-mode strict"))
	})
})
