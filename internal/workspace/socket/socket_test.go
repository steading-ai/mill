package socket_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/redact"
	"github.com/ivy/mill/internal/workspace/socket"
)

var _ = Describe("The workspace socket", Label("unit"), func() {
	var (
		verbs    *tree
		server   *socket.Server
		log      *bytes.Buffer
		path     string
		redactor *redact.Redactor
	)

	// live installs a token on the socket and returns a client holding its
	// bearer, which is everything a step is ever given.
	live := func(bearer string, capabilities ...workspace.Capability) *socket.Client {
		server.Rotate(workspace.TokenFor(bearer, capabilities...))
		return socket.Dial(path, bearer)
	}

	// refusal is the typed error the socket answers with.
	refusal := func(err error) *socket.Error {
		GinkgoHelper()
		var wire *socket.Error
		Expect(errors.As(err, &wire)).To(BeTrue(), "expected a socket refusal, got %v", err)
		return wire
	}

	BeforeEach(func(ctx context.Context) {
		verbs = newTree()
		log = &bytes.Buffer{}
		redactor = redact.New()
		server = socket.New(verbs, socket.WithLog(log, redactor))
		path = filepath.Join(GinkgoT().TempDir(), "ws.sock")
		Expect(server.Listen(path)).To(Succeed())
		DeferCleanup(func(ctx context.Context) { Expect(server.Close(ctx)).To(Succeed()) })
	})

	It("Capability is enforced at the verb", func(ctx context.Context) {
		// Given a step token with read only
		step := live("read-only-bearer", workspace.Read)
		before := verbs.snapshot()

		// When fs.write is requested
		err := step.Write(ctx, "README.md", []byte("rewritten by a reviewer"))

		// Then the response names write as the missing capability and the
		// tree is unchanged
		wire := refusal(err)
		Expect(wire.Code).To(Equal(socket.CodeForbidden))
		Expect(wire.Capability).To(Equal(workspace.Write))
		Expect(wire.Message).To(ContainSubstring("fs.write requires the write capability"))
		Expect(verbs.snapshot()).To(Equal(before))
	})

	It("serves the read verbs to a token that carries read", func(ctx context.Context) {
		step := live("read-only-bearer", workspace.Read)

		Expect(step.Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))
		Expect(step.List(ctx, ".")).To(ConsistOf(workspace.Entry{Name: "README.md"}))
		Expect(step.GitStatus(ctx)).To(Equal(workspace.Status{Branch: "mill/mill-12", Clean: true}))
		Expect(step.GitDiff(ctx, workspace.DiffRequest{})).To(ContainSubstring("diff --git"))
		Expect(step.GitLog(ctx, workspace.LogRequest{})).To(HaveLen(1))
	})

	It("refuses git.commit to a token without write, naming the capability", func(ctx context.Context) {
		step := live("read-and-exec", workspace.Read, workspace.Exec)

		_, err := step.GitCommit(ctx, workspace.CommitRequest{Message: "sneak"})

		wire := refusal(err)
		Expect(wire.Code).To(Equal(socket.CodeForbidden))
		Expect(wire.Capability).To(Equal(workspace.Write))
		Expect(wire.Message).To(ContainSubstring("git.commit requires the write capability"))
		Expect(verbs.commits).To(BeEmpty())
	})

	It("refuses an unknown bearer", func(ctx context.Context) {
		live("the-live-bearer", workspace.Read)

		err := socket.Dial(path, "a-bearer-nobody-issued").Call(ctx, socket.VerbFSRead, socket.PathRequest{Path: "README.md"}, nil)

		Expect(refusal(err).Code).To(Equal(socket.CodeUnauthorized))
	})

	It("refuses a request carrying no bearer at all", func(ctx context.Context) {
		live("the-live-bearer", workspace.Read)

		err := socket.Dial(path, "").Call(ctx, socket.VerbFSRead, socket.PathRequest{Path: "README.md"}, nil)

		Expect(refusal(err).Code).To(Equal(socket.CodeUnauthorized))
	})

	It("refuses a revoked bearer", func(ctx context.Context) {
		step := live("revoked-bearer", workspace.Read)
		Expect(step.Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))

		// Revocation, as the worker performs it when a step run closes: the
		// socket is left with no live token at all.
		server.Rotate(workspace.Token{})

		_, err := step.Read(ctx, "README.md")
		Expect(refusal(err).Code).To(Equal(socket.CodeUnauthorized))
	})

	It("closes the previous token's access when a new one is rotated in", func(ctx context.Context) {
		implementer := live("the-implementers-bearer", workspace.Read, workspace.Write)
		Expect(implementer.Write(ctx, "notes.md", []byte("done"))).To(Succeed())

		reviewer := live("the-reviewers-bearer", workspace.Read)

		_, err := implementer.Read(ctx, "README.md")
		Expect(refusal(err).Code).To(Equal(socket.CodeUnauthorized))
		Expect(reviewer.Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))
	})

	It("authenticates against the hash the store holds, without ever seeing the bearer", func(ctx context.Context) {
		// What IssueStepToken keeps is the hash; a workspace given only that
		// still authenticates the bearer the session was handed.
		server.Rotate(workspace.Token{
			Hash:         workspace.HashBearer("the-issued-bearer"),
			Capabilities: []workspace.Capability{workspace.Read},
		})

		Expect(socket.Dial(path, "the-issued-bearer").Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))
	})

	It("runs a command for a token that carries exec", func(ctx context.Context) {
		step := live("exec-bearer", workspace.Exec)

		result, err := step.Exec(ctx, socket.ExecRequest{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Status).To(BeEquivalentTo(3))
		Expect(result.Stdout).To(Equal("out\n"))
		Expect(result.Stderr).To(Equal("err\n"))
	})

	It("takes redact from any live token and refuses it from none", func(ctx context.Context) {
		step := live("redact-bearer")

		Expect(step.Redact(ctx, "a-value-only-the-step-knows")).To(Succeed())
		Expect(verbs.redacts).To(ConsistOf("a-value-only-the-step-knows"))

		server.Rotate(workspace.Token{})
		Expect(refusal(step.Redact(ctx, "too-late")).Code).To(Equal(socket.CodeUnauthorized))
	})

	It("serves the forge verbs to a token that carries forge", func(ctx context.Context) {
		api := newRecordedGitLab()
		DeferCleanup(api.server.Close)
		verbs.forge = api.forgeVerbs("mill/mill-12")
		step := live("forge-bearer", workspace.Forge)

		pushed, err := step.Push(ctx, workspace.PushRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pushed.Branch).To(Equal("mill/mill-12"))

		pr, err := step.PR(ctx, workspace.PRRequest{Title: "mill#12"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.URL).To(Equal("https://gitlab.example.test/group/thing/-/merge_requests/7"))

		checks, err := step.Checks(ctx, workspace.ChecksRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks.Status).To(Equal(forge.Success))
	})

	It("answers the forge verbs with a typed not-implemented on a run with no forge, not with silence", func(ctx context.Context) {
		step := live("forge-bearer", workspace.Forge)

		for _, verb := range []string{socket.VerbGitPush, socket.VerbForgePR, socket.VerbForgeChecks} {
			wire := refusal(step.Call(ctx, verb, struct{}{}, nil))
			Expect(wire.Code).To(Equal(socket.CodeNotImplemented), verb)
			Expect(wire.Verb).To(Equal(verb))
		}
	})

	It("The forge credential is scoped to the step", func(ctx context.Context) {
		// Given step token T with forge capability.
		api := newRecordedGitLab()
		DeferCleanup(api.server.Close)
		verbs.forge = api.forgeVerbs("mill/mill-12")
		step := live("the-step-token-T", workspace.Forge)

		pr, err := step.PR(ctx, workspace.PRRequest{Title: "mill#12"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pr.Number).To(Equal(7))
		reached := api.count()
		Expect(reached).To(BeNumerically(">", 0), "the granted request did reach GitLab")

		// When T is revoked and forge.pr is requested again with T.
		server.Rotate(workspace.Token{})
		_, err = step.PR(ctx, workspace.PRRequest{Title: "mill#12"})

		// Then the request is refused before any GitLab call.
		Expect(refusal(err).Code).To(Equal(socket.CodeUnauthorized))
		Expect(api.count()).To(Equal(reached), "a revoked step token reaches no forge")
	})

	It("refuses a verb that does not exist", func(ctx context.Context) {
		step := live("read-bearer", workspace.Read)

		Expect(refusal(step.Call(ctx, "fs.delete", struct{}{}, nil)).Code).To(Equal(socket.CodeUnknownVerb))
	})

	It("writes one summary line per call, through the redactor", func(ctx context.Context) {
		step := live("summary-bearer", workspace.Read, workspace.Exec)
		redactor.Add("a-secret-in-the-output")

		Expect(step.Read(ctx, "README.md")).To(Equal([]byte("as cloned\n")))
		_, err := step.Exec(ctx, socket.ExecRequest{Path: "/bin/sh", Args: []string{"-c", "echo a-secret-in-the-output"}})
		Expect(err).NotTo(HaveOccurred())

		Expect(log.String()).To(ContainSubstring("verb=fs.read path=README.md bytes=10"))
		Expect(log.String()).To(ContainSubstring("out=" + redact.Mask))
		Expect(log.String()).NotTo(ContainSubstring("a-secret-in-the-output"))
	})

	It("creates the socket readable by the mill's account alone, and removes it on close", func(ctx context.Context) {
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))

		Expect(server.Close(ctx)).To(Succeed())
		Expect(os.Stat(path)).Error().To(MatchError(os.ErrNotExist))

		// Close is what DeferCleanup would do; leaving it to run twice would
		// fail on a listener that is already gone.
		server = socket.New(verbs)
	})

	It("refuses a request that is not a POST to a verb", func(ctx context.Context) {
		client := &http.Client{Transport: &http.Transport{DialContext: unixDialer(path)}}
		response, err := client.Get("http://workspace/v1/fs.read") //nolint:noctx // the request is a one-line probe in a spec
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(response.Body.Close()).To(Succeed()) }()

		Expect(response.StatusCode).To(Equal(http.StatusNotFound))
	})
})
