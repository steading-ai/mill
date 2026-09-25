package acceptance_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/workflow"
)

// The scripted-feature fixture (mill#21's step 5) is a workflow the loader
// accepts once $MILL_ACCEPTANCE admits its harness.scripted steps, and
// refuses otherwise — acceptance criterion 3, checked here at the loader
// itself rather than through the CLI, since neither direction needs this
// package's suite (no dagger, no forge, no sidecar), only the loader and the
// definition on disk.
var _ = Describe("The scripted-feature workflow fixture", Label("unit"), func() {
	const dir = "testdata/workflows/scripted-feature"

	It("loads and validates once MILL_ACCEPTANCE admits its harness.scripted steps", func() {
		GinkgoT().Setenv("MILL_ACCEPTANCE", "1")

		w, skills, err := workflow.Load(dir)

		Expect(err).NotTo(HaveOccurred())
		Expect(workflow.Validate(w)).To(Succeed())
		Expect(skills).NotTo(BeEmpty(), "scripted-feature has no skills, but every skill step names one")
	})

	It("is refused without MILL_ACCEPTANCE", func() {
		GinkgoT().Setenv("MILL_ACCEPTANCE", "")

		_, _, err := workflow.Load(dir)

		Expect(err).To(MatchError(ContainSubstring("MILL_ACCEPTANCE")))
	})
})
