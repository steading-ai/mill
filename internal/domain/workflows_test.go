package domain_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/domain"
)

var _ = Describe("A workflow's mcp-servers against the host's own", Label("unit"), func() {
	It("is refused when a name collides", func() {
		workflow := &millv1.Workflow{
			McpServers: map[string]*millv1.WorkflowMcpServer{"playwright": {Command: "npx"}},
		}
		err := domain.CheckMcpCollision(workflow, map[string]struct{}{"playwright": {}})
		var collision *domain.McpCollisionError
		Expect(err).To(BeAssignableToTypeOf(collision))
		Expect(err).To(MatchError(ContainSubstring("playwright")))
	})

	It("passes when no name collides", func() {
		workflow := &millv1.Workflow{
			McpServers: map[string]*millv1.WorkflowMcpServer{"docs": {Command: "npx"}},
		}
		Expect(domain.CheckMcpCollision(workflow, map[string]struct{}{"playwright": {}})).To(Succeed())
	})

	It("passes when the host configures none", func() {
		workflow := &millv1.Workflow{
			McpServers: map[string]*millv1.WorkflowMcpServer{"playwright": {Command: "npx"}},
		}
		Expect(domain.CheckMcpCollision(workflow, nil)).To(Succeed())
	})
})
