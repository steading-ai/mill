package doctor_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/doctor"
)

// fixture is the fake stdio MCP server sidecar/sidecar_test.go also mounts,
// real enough to answer initialize and tools/list without the real
// Playwright server, which is stead's to provide (mill#19 Notes).
func fixture() string {
	GinkgoHelper()
	path, err := filepath.Abs(filepath.Join("..", "..", "sidecar", "spec", "fixtures", "fake-mcp-server.mjs"))
	Expect(err).NotTo(HaveOccurred())
	return path
}

var _ = Describe("The mcp probe", Label("unit"), func() {
	It("starts a configured server, counts its tools, and stops it", func(ctx SpecContext) {
		result := doctor.Mcp(ctx, "playwright", "node", []string{fixture()}, nil)
		Expect(result.OK).To(BeTrue(), result.Reason)
		Expect(result.Reason).To(Equal("playwright: 1 tools"))
	})

	It("names the missing command when the server does not start", func(ctx SpecContext) {
		result := doctor.Mcp(ctx, "playwright", "mill-doctor-mcp-nonexistent", nil, nil)
		Expect(result.OK).To(BeFalse())
		Expect(result.Reason).To(ContainSubstring("mill-doctor-mcp-nonexistent"))
	})
})
