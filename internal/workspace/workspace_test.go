package workspace_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/workspace"
)

var _ = Describe("The workspace's vocabulary", Label("unit"), func() {
	DescribeTable("the run's branch is derived from the node ref",
		func(nodeRef, branch string) {
			Expect(workspace.BranchFor(nodeRef)).To(Equal(branch))
		},
		Entry("a dagger ref", "mill#12", "mill/mill-12"),
		Entry("another project", "dagger#4021", "mill/dagger-4021"),
		Entry("characters git forbids collapse to one dash", "mill #12", "mill/mill-12"),
		Entry("a trailing separator is dropped, because git refuses a name ending in one", "mill#12.", "mill/mill-12"),
		Entry("no node at all", "", ""),
	)

	Describe("the egress decisions", func() {
		It("become the five variables a process is given, with the step token as the gateway's bearer", func() {
			decided := workspace.Egress{Proxy: "http://10.99.14.1:3128", NoProxy: "10.99.14.1", Gateway: "http://10.99.14.1:8081"}

			Expect(decided.Environment("step-token-a")).To(Equal(map[string]string{
				"HTTPS_PROXY":          "http://10.99.14.1:3128",
				"HTTP_PROXY":           "http://10.99.14.1:3128",
				"NO_PROXY":             "10.99.14.1",
				"ANTHROPIC_BASE_URL":   "http://10.99.14.1:8081",
				"ANTHROPIC_AUTH_TOKEN": "step-token-a",
				"ENABLE_TOOL_SEARCH":   "true",
			}))
		})

		It("bypass the proxy for the gateway's host, so the model is reached directly and never through squid", func() {
			decided := workspace.Egress{Proxy: "http://10.99.255.1:3128", NoProxy: "localhost,127.0.0.1", Gateway: "http://10.99.255.1:8080"}
			Expect(decided.Environment("t")).To(HaveKeyWithValue("NO_PROXY", "localhost,127.0.0.1,10.99.255.1"))
			By("once, when NoProxy already names it")
			Expect(workspace.Egress{Proxy: "http://p:3128", NoProxy: "localhost,10.99.255.1", Gateway: "http://10.99.255.1:8080"}.Bypass()).
				To(Equal("localhost,10.99.255.1"))
			By("alone, when nothing else bypasses")
			Expect(workspace.Egress{Proxy: "http://p:3128", Gateway: "http://gw:8080"}.Bypass()).To(Equal("gw"))
			By("not at all without a proxy to bypass")
			Expect(workspace.Egress{Gateway: "http://gw:8080"}.Bypass()).To(BeEmpty())
		})

		It("set nothing when nothing was decided, so the host's route and the account's login stand", func() {
			Expect(workspace.Egress{}.Environment("step-token-a")).To(BeEmpty())
		})

		It("name a proxy without a gateway, and a gateway without a proxy", func() {
			Expect(workspace.Egress{Proxy: "http://proxy:3128"}.Environment("t")).To(HaveLen(3))
			Expect(workspace.Egress{Proxy: "http://proxy:3128"}.Environment("t")).NotTo(HaveKey("ANTHROPIC_AUTH_TOKEN"))
			Expect(workspace.Egress{Gateway: "http://gateway:8081"}.Environment("t")).To(Equal(map[string]string{
				"ANTHROPIC_BASE_URL":   "http://gateway:8081",
				"ANTHROPIC_AUTH_TOKEN": "t",
				"ENABLE_TOOL_SEARCH":   "true",
			}))
		})
	})

	Describe("the step token", func() {
		It("authenticates a bearer against the hash the store keeps", func() {
			issued := workspace.TokenFor("the-bearer", workspace.Read)

			Expect(issued.Hash).To(Equal(workspace.HashBearer("the-bearer")))
			Expect(issued.Matches("the-bearer")).To(BeTrue())
			Expect(issued.Matches("another-bearer")).To(BeFalse())
			Expect(issued.Matches("")).To(BeFalse())
		})

		It("authenticates from the hash alone, having never seen the bearer", func() {
			held := workspace.Token{Hash: workspace.HashBearer("the-bearer")}

			Expect(held.Live()).To(BeTrue())
			Expect(held.Matches("the-bearer")).To(BeTrue())
		})

		It("is not live, and matches nothing, when it is the zero token", func() {
			var revoked workspace.Token

			Expect(revoked.Live()).To(BeFalse())
			Expect(revoked.Matches("the-bearer")).To(BeFalse())
		})

		It("grants exactly the capabilities it was issued with", func() {
			issued := workspace.TokenFromProto(&millv1.StepToken{
				Id:        "01J0",
				Token:     "the-bearer",
				TokenHash: workspace.HashBearer("the-bearer"),
				Capabilities: []millv1.Capability{
					millv1.Capability_CAPABILITY_READ,
					millv1.Capability_CAPABILITY_EXEC,
				},
			})

			Expect(issued.ID).To(Equal("01J0"))
			Expect(issued.Capabilities).To(Equal([]workspace.Capability{workspace.Read, workspace.Exec}))
			Expect(issued.Grants(workspace.Read)).To(BeTrue())
			Expect(issued.Grants(workspace.Write)).To(BeFalse())
		})

		It("drops a capability the ledger does not name", func() {
			Expect(workspace.CapabilitiesFromProto([]millv1.Capability{
				millv1.Capability_CAPABILITY_UNSPECIFIED,
				millv1.Capability_CAPABILITY_FORGE,
			})).To(Equal([]workspace.Capability{workspace.Forge}))
		})
	})

	It("names the capability a refused verb needed", func() {
		refusal := &workspace.CapabilityError{Verb: "fs.write", Missing: workspace.Write}

		Expect(refusal.Error()).To(ContainSubstring("fs.write requires the write capability"))
	})
})
