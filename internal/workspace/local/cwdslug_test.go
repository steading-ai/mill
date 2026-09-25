package local

// cwdSlug is pinned directly, in package local rather than local_test,
// because it is deliberately unexported: nothing outside this package needs
// the transcript directory's name, only CaptureTwin's result.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("cwdSlug", Label("unit"), func() {
	It("matches the one example docs/spikes/sdk-sessions.md recorded (§Q1)", func() {
		Expect(cwdSlug("/home/ivy/src/github.com/steading-ai/mill")).To(Equal("-home-ivy-src-github-com-ivy-mill"))
	})
})
