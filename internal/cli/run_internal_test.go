package cli

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// refusalForHost is the check `mill run` makes before it does anything else:
// whether the worker behind the socket it dialed claims to be on some other
// host than this one.
var _ = Describe("refusalForHost", Label("unit"), func() {
	DescribeTable("when to let `mill run` proceed",
		func(claims *millv1.ClaimsResponse, localHost string) {
			Expect(refusalForHost(claims, localHost)).To(Succeed())
		},
		Entry("no worker answered", nil, "here"),
		Entry("the worker did not say which host it's on", &millv1.ClaimsResponse{}, "here"),
		Entry("the worker's host is this one", &millv1.ClaimsResponse{Host: "here"}, "here"),
	)

	It("refuses, naming both hosts, when the worker's host is not this one", func() {
		err := refusalForHost(&millv1.ClaimsResponse{Host: "there"}, "here")
		Expect(err).To(MatchError(ContainSubstring("there")))
		Expect(err).To(MatchError(ContainSubstring("here")))
		Expect(err).To(MatchError(ContainSubstring("mill run")))
		Expect(err).To(MatchError(ContainSubstring("mill work")))
	})
})
