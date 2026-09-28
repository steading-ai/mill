package local

// cwdSlug is pinned directly, in package local rather than local_test,
// because it is deliberately unexported: nothing outside this package needs
// the transcript directory's name, only CaptureTwin's result.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("cwdSlug", Label("unit"), func() {
	// The input is the spike's recorded example, docs/spikes/sdk-sessions.md
	// line 55, not this repo's current module path: the spec pins cwdSlug's
	// output to that one observation, so a future rename of the module
	// (as 5adf25c did) must not touch this line.
	It("matches the one example docs/spikes/sdk-sessions.md recorded (§Q1)", func() {
		Expect(cwdSlug("/home/ivy/src/github.com/ivy/mill")).To(Equal("-home-ivy-src-github-com-ivy-mill"))
	})
})
