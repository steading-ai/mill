package version_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/version"
)

var _ = Describe("version", Label("unit"), func() {
	It("is semver-shaped even without ldflags", func() {
		Expect(version.String()).To(MatchRegexp(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`))
	})
})
