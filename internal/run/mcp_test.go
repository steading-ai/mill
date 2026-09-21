package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// A step's session carries the worker's own MCP servers and the workflow's
// own, merged into one catalog: the sidecar decides what a step actually
// mounts (sidecar/src/session.ts), and mill's own half is handing it a
// complete list to decide from (docs/architecture.md §"Skill steps: the
// harness adapter").
var _ = Describe("A session's MCP catalog", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("carries the worker's global servers and the workflow's own to every step", func() {
		By("given a workflow that declares its own server, alongside a worker config that declares a different one")
		definition := f.variant(ctx, "mcp-fix", func(w *millv1.Workflow) {
			w.McpServers = map[string]*millv1.WorkflowMcpServer{
				"docs": {Command: "docs-server", Args: []string{"--stdio"}},
			}
		})
		f.script(f.quickFixScripts()...)

		By("when the run starts")
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = definition
			o.McpServers = map[string]harness.McpServer{
				"playwright": {Command: "playwright-server", Global: true},
			}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the first step's session carries both, and a directory to mount them under")
		session := f.harness.Calls()[0].Session
		Expect(session.McpServers).To(Equal(map[string]harness.McpServer{
			"playwright": {Command: "playwright-server", Global: true},
			"docs":       {Command: "docs-server", Args: []string{"--stdio"}},
		}))
		Expect(session.McpDir).NotTo(BeEmpty())
	})
})
