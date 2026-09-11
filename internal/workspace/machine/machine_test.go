package machine_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/machine"
)

var _ = Describe("The machine workspace", Label("unit"), func() {
	var ws *machine.Workspace

	BeforeEach(func() {
		ws = machine.New(machine.NewFake(), machine.Config{Name: "mill", Identity: "mill@core"})
	})

	// notBuilt asserts an error is ErrNotBuilt and names the node.
	notBuilt := func(err error, node string) {
		GinkgoHelper()
		Expect(err).To(MatchError(machine.ErrNotBuilt))
		Expect(err).To(MatchError(ContainSubstring(node)))
		var typed *machine.NotBuiltError
		Expect(err).To(BeAssignableToTypeOf(typed))
	}

	It("answers every verb with ErrNotBuilt naming the node that builds it", func(ctx context.Context) {
		spec := workspace.Spec{RunID: "01RUN", NodeRef: "mill#1"}

		_, err := ws.Create(ctx, spec)
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Attach(ctx, spec)
		notBuilt(err, machine.LifecycleNode)
		notBuilt(ws.Destroy(ctx), machine.LifecycleNode)
		notBuilt(ws.Rotate(ctx, workspace.Token{}), machine.LifecycleNode)
		_, err = ws.Read(ctx, "README.md")
		notBuilt(err, machine.LifecycleNode)
		notBuilt(ws.Write(ctx, "README.md", nil), machine.LifecycleNode)
		_, err = ws.List(ctx, ".")
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Git().Status(ctx)
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Git().Diff(ctx, workspace.DiffRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Git().Log(ctx, workspace.LogRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Git().Commit(ctx, workspace.CommitRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Forge().Push(ctx, workspace.PushRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Forge().PR(ctx, workspace.PRRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.Forge().Checks(ctx, workspace.ChecksRequest{})
		notBuilt(err, machine.LifecycleNode)
		_, err = ws.GitRunner().Run(ctx, nil, "status")
		notBuilt(err, machine.LifecycleNode)

		By("exec belongs to the node after lifecycle")
		_, err = ws.Exec(ctx, workspace.Command{Path: "true"})
		notBuilt(err, machine.ExecNode)
	})

	It("is honest before a claim: Check reports the backend is not built", func() {
		notBuilt(machine.Check(machine.Config{}), machine.LifecycleNode)
	})

	It("has no handle, carries the committer it was configured with, and decides no environment yet", func() {
		Expect(ws.Handle()).To(BeNil())
		Expect(ws.Committer()).To(Equal(workspace.Committer{Name: "mill", Email: "mill@core"}))
		Expect(ws.Environment()).To(BeEmpty())
	})

	It("takes the sandbox's default paths when none are configured", func() {
		Expect(machine.DefaultRoot).To(Equal("/var/lib/mill"))
		Expect(machine.DefaultMachines).To(Equal("/var/lib/machines"))
	})
})
