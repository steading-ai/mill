package scripted_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/scripted"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
	"github.com/steading-ai/mill/internal/workspace/redact"
	"github.com/steading-ai/mill/internal/workspace/socket"
)

// A prompt shaped like internal/run/artifact.go's artifactBrief: the one
// place mill tells a session where its step's artifact belongs.
const dispatchPrompt = "Do the work. Write it to `notes/plan.md` in the checkout. When the session ends the mill reads that file back."

var _ = Describe("The scripted adapter's dispatch", Label("unit"), func() {
	var (
		ws     *local.Workspace
		handle *workspace.Handle
		bearer = "the-dispatch-token"
	)

	BeforeEach(func(ctx context.Context) {
		root := filepath.Join(GinkgoT().TempDir(), "workspaces")
		ws = local.New(local.Options{
			Root:     root,
			Origin:   origin(),
			Redactor: redact.New(),
			Identity: "mill@example.test",
			Name:     "mill",
		})
		made, err := ws.Create(ctx, workspace.Spec{RunID: "run-01", NodeRef: "mill#21"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { Expect(ws.Destroy(ctx)).To(Succeed()) })
		handle = made

		Expect(ws.Rotate(ctx, workspace.TokenFor(bearer, workspace.Read, workspace.Write))).To(Succeed())
	})

	It("writes the artifact and commits it for real, against the run's own checkout", func(ctx context.Context) {
		adapter := scripted.NewAdapter("testdata")
		session := harness.Session{
			ID:      "implement",
			Options: map[string]any{"script": "dispatch.yaml"},
			Workspace: harness.Workspace{
				Socket: handle.Socket,
				Token:  bearer,
			},
		}
		prompt := harness.Prompt{Text: dispatchPrompt}

		turn, err := adapter.Start(ctx, session, prompt, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err := turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("wrote it"))

		written, err := os.ReadFile(filepath.Join(handle.Root, "notes", "plan.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(written)).To(Equal("written by the scripted harness\n"),
			"the placeholder ${artifact_path} must resolve to the path artifactBrief named in the prompt")

		turn, err = adapter.Resume(ctx, session, prompt, nil)
		Expect(err).NotTo(HaveOccurred())
		terminal, err = turn.Wait(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(terminal.FinalMessage).To(Equal("committed it"))

		Expect(git(handle.Root, "log", "-1", "--format=%s")).To(Equal("mill#21: scripted dispatch"),
			"the dispatch must land a real commit on the run's own checkout, not a mocked one")

		client := socket.Dial(handle.Socket, bearer)
		log, err := client.GitLog(ctx, workspace.LogRequest{Limit: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(log).To(HaveLen(1))
		Expect(log[0].Subject).To(Equal("mill#21: scripted dispatch"))
	})
})
