package machine_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/claude"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/machine/driver"
)

// The session's sidecar is one exec like any other: routed through the run's
// machine workspace, so it lands under the step slice as agent in /work and
// dies with the step. These specs prove the adapter and the machine
// workspace fit together over the fake driver; the live tier proves it in a
// real machine.
var _ = Describe("The session's sidecar inside the machine", Label("unit"), func() {
	var (
		f  *fixture
		ws *machine.Workspace
	)

	BeforeEach(func(ctx context.Context) {
		f = newFixture(origin(nil))
		ws = f.workspace()
		_, err := ws.Create(ctx, workspace.Spec{RunID: runID, NodeRef: "mill#82", Origin: f.config.Origin})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = ws.Destroy(ctx) })
		Expect(ws.Rotate(ctx, workspace.TokenFor("the-step-bearer", workspace.Exec))).To(Succeed())
	})

	It("is started through the workspace under the step slice as agent in /work", func(ctx context.Context) {
		// The adapter the machine backend builds: its execer is the run's
		// workspace, its entrypoint the in-machine node and sidecar path, and
		// it takes none of the host's environment as a floor.
		adapter := claude.New(
			claude.WithExecer(ws),
			claude.WithSandbox(),
			claude.WithEntrypoint(machine.SidecarNode, machine.SidecarMount+"/dist/main.js"))

		Expect(ws.BeginStep(ctx, 3)).To(Succeed())
		before := len(f.host.CallsTo(driver.Exec))
		// The fake spawns a process that writes nothing and exits, so the
		// adapter synthesises a terminal; the exec it went through is what
		// this spec reads.
		f.host.Script(driver.Exec, driver.Output{})

		session := harness.Session{
			ID:      "01JSESSION0000000000000000",
			Timeout: time.Minute,
			Cwd:     ws.SessionCwd(),
			Workspace: harness.Workspace{
				Socket: ws.SessionSocket(),
				Token:  "the-step-bearer",
				Env:    ws.Environment(),
			},
		}
		turn, err := adapter.Start(ctx, session, harness.Prompt{Text: "do the work"}, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("the sidecar went through the machine workspace, not a host spawner")
		execs := execRequestsAfter(f.host, before)
		Expect(execs).To(HaveLen(1))
		sidecar := execs[0]

		By("under the step slice, as agent, in /work")
		Expect(sidecar.Slice).To(Equal("mill-step-3.slice"))
		Expect(sidecar.User).To(Equal(machine.SessionUser))
		Expect(sidecar.Dir).To(Equal("/work"))

		By("running the in-machine node against the mounted sidecar")
		Expect(sidecar.Argv).To(Equal([]string{machine.SidecarNode, machine.SidecarMount + "/dist/main.js"}))

		By("with the session's grant and the machine's egress, and no host HOME or PATH")
		Expect(sidecar.Env).To(HaveKeyWithValue("WORKSPACE_SOCKET", "/run/mill/ws.sock"))
		Expect(sidecar.Env).To(HaveKeyWithValue("STEP_TOKEN", "the-step-bearer"))
		Expect(sidecar.Env).To(HaveKeyWithValue("ANTHROPIC_BASE_URL", machine.DefaultGateway))
		Expect(sidecar.Env).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", "the-step-bearer"))
		Expect(sidecar.Env).To(HaveKeyWithValue("HTTPS_PROXY", machine.DefaultProxy))
		Expect(sidecar.Env).NotTo(HaveKey("HOME"))
		Expect(sidecar.Env).NotTo(HaveKey("PATH"))
	})

	It("refuses to start a session outside a step slice, so nothing would reap it", func(ctx context.Context) {
		adapter := claude.New(claude.WithExecer(ws), claude.WithSandbox(),
			claude.WithEntrypoint(machine.SidecarNode, machine.SidecarMount+"/dist/main.js"))
		session := harness.Session{
			ID: "01JSESSION0000000000000001", Timeout: time.Minute, Cwd: ws.SessionCwd(),
			Workspace: harness.Workspace{Socket: ws.SessionSocket(), Token: "the-step-bearer", Env: ws.Environment()},
		}
		_, err := adapter.Start(ctx, session, harness.Prompt{Text: "x"}, nil)
		Expect(err).To(MatchError(ContainSubstring("no step slice")))
	})
})
