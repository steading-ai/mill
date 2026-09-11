package acceptance_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The guard on a suite's own state: a derived workflow is one scenario's
// scaffolding and is pushed into that scenario's scratch project, never at
// org scope, so that sweeping the project by name sweeps the definition too
// (docs/sandbox.md §"External state"). The guard runs inside stageWorkflow;
// this spec drives it directly, because a scenario that pushed at org scope
// would be proving the guard by polluting the store.
var _ = Describe("A suite's workflow pushes", func() {
	It("A suite's attempt to push a workflow at org scope fails the scenario that attempted it", func() {
		By("a push at project scope passes the guard")
		Expect(InterceptGomegaFailures(func() {
			requireProjectScope([]string{"--scope", "project", "--project", "mill-acceptance-x-0badcafe"})
		})).To(BeEmpty())

		By("a push at org scope fails the scenario before anything reaches the store")
		failures := InterceptGomegaFailures(func() {
			requireProjectScope([]string{"--scope", "org"})
		})
		Expect(failures).To(HaveLen(1))
		Expect(failures[0]).To(ContainSubstring("project scope only"))

		By("and so does a push that names no scope, which leaves the definition at its own — org, for feature")
		Expect(InterceptGomegaFailures(func() {
			requireProjectScope(nil)
		})).To(HaveLen(1))
	})
})
