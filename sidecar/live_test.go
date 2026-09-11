package sidecar_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
)

// The live specs spend a real session against the Claude Agent SDK, using
// the credentials of the account the sidecar runs under — never anything
// from the request. They are skipped unless MILL_SIDECAR_LIVE is set,
// because they cost tokens and need a login; everything they cover about the
// sidecar's own behaviour is covered deterministically in sidecar_test.go.
// What they add is the part no fake can assert: that the SDK really does
// what docs/spikes/sdk-sessions.md observed it doing.
var _ = Describe("A live session", Label("live"), Serial, func() {
	const liveTimeout = 5 * time.Minute

	var (
		sessionID string
		workdir   string
	)

	BeforeEach(func() {
		live()
		sessionID = newSessionID()
		workdir = GinkgoT().TempDir()
	})

	It("runs a trivial prompt to a terminal record", func() {
		By("given a Start request with a trivial prompt and no tools")
		line := request(&harnessv1.Request{
			Id: "live-trivial",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Reply with exactly the word READY and nothing else.",
				Cwd:       workdir,
				Settings:  settings(map[string]any{"tools": map[string]any{"deny": []any{"Bash", "Write", "Edit"}}}),
			}},
		})

		By("when the sidecar runs it")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then it wrote at least one event and exactly one terminal record, and exited 0")
		Expect(len(s.events())).To(BeNumerically(">=", 1))
		Expect(s.terminals()).To(HaveLen(1))
		Expect(s.exitCode()).To(Equal(0))
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
		Expect(s.terminal().GetFinalMessage()).To(ContainSubstring("READY"))

		By("and the usage the SDK reported reached the wire, keyed by model")
		Expect(s.terminal().GetUsage().GetByModel()).NotTo(BeEmpty())
		for model, counts := range s.terminal().GetUsage().GetByModel() {
			Expect(model).NotTo(BeEmpty())
			Expect(counts.GetInput() + counts.GetOutput() + counts.GetCacheRead() + counts.GetCacheWrite()).
				To(BeNumerically(">", 0))
		}
	})

	It("resumes the same session from a fresh process and sees the prior turn", func() {
		By("given a session that has been told a codeword")
		first := request(&harnessv1.Request{
			Id: "live-start",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Remember this codeword: TANGERINE-4417. Reply with OK and nothing else.",
				Cwd:       workdir,
				Settings:  settings(map[string]any{"tools": map[string]any{"deny": []any{"Bash", "Write", "Edit"}}}),
			}},
		})
		started := runFor(entrypoint, nil, first, liveTimeout)
		Expect(started.terminal().GetExit().GetStatus()).To(BeZero())

		By("when a fresh process resumes it by the same session id")
		second := request(&harnessv1.Request{
			Id: "live-resume",
			Body: &harnessv1.Request_Resume{Resume: &harnessv1.Resume{
				SessionId: sessionID,
				Prompt:    "What codeword did I give you earlier? Reply with the codeword and nothing else.",
				Cwd:       workdir,
				Settings:  settings(map[string]any{"tools": map[string]any{"deny": []any{"Bash", "Write", "Edit"}}}),
			}},
		})
		resumed := runFor(entrypoint, nil, second, liveTimeout)

		By("then the turn references the prior turn's content")
		Expect(resumed.terminal().GetExit().GetStatus()).To(BeZero())
		Expect(resumed.terminal().GetFinalMessage()).To(ContainSubstring("TANGERINE-4417"))
	})

	It("applies a policy that excludes Write and Edit", func() {
		By("given a step whose policy withholds every write path")
		target := filepath.Join(workdir, "escape.txt")
		line := request(&harnessv1.Request{
			Id: "live-policy",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Create the file " + target + " with the exact contents ESCAPED. If you cannot, say why.",
				Cwd:       workdir,
				Settings: settings(map[string]any{"tools": map[string]any{
					"allow": []any{"Read", "Glob", "Grep"},
					"deny":  []any{"Write", "Edit", "NotebookEdit", "Bash", "Task"},
				}}),
			}},
		})

		By("when the session is asked to write a file")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then the file was not written and the session never called a write tool")
		_, err := os.Stat(target)
		Expect(os.IsNotExist(err)).To(BeTrue(), "the policy did not stop the write")
		for _, event := range s.events() {
			if call := event.GetToolCall(); call != nil {
				Expect(call.GetName()).NotTo(BeElementOf("Write", "Edit", "NotebookEdit", "Bash"))
			}
		}

		By("and the transcript shows the refusal")
		GinkgoWriter.Println(s.terminal().GetFinalMessage())
		Expect(s.terminal().GetFinalMessage()).To(MatchRegexp(`(?i)can'?t|cannot|unable|not (available|permitted|allowed)|no write|denied|polic`))
	})

	It("The workspace's network decisions reach the session's Bash tool", func() {
		By("given a proxy and a credential gateway on this host, as a machine's workspace decides them")
		// The gateway swaps the step token for the subscription's own
		// `claude setup-token`, as docs/sandbox.md §Credentials has it and
		// as the E14 spike proved the upstream accepts. The session itself
		// is never given the setup-token: it is in this process's
		// environment, and the sidecar's floor stops it.
		upstream := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
		if upstream == "" {
			Skip("set CLAUDE_CODE_OAUTH_TOKEN to a `claude setup-token` to run the spec that reaches the model through a gateway")
		}
		const stepToken = "step-token-live"
		proxy := connectProxy()
		gateway, swapped := credentialGateway(stepToken, upstream)
		decided := map[string]string{
			"HTTPS_PROXY":          proxy.URL,
			"HTTP_PROXY":           proxy.URL,
			"NO_PROXY":             "127.0.0.1,localhost",
			"ANTHROPIC_BASE_URL":   gateway.URL,
			"ANTHROPIC_AUTH_TOKEN": stepToken,
		}

		By("when a session runs `env` through its Bash tool")
		line := request(&harnessv1.Request{
			Id: "live-egress",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId: sessionID,
				Prompt:    "Run the shell command `env | sort` with the Bash tool, exactly once, then reply with exactly the word DONE and nothing else.",
				Cwd:       workdir,
				Settings: settings(map[string]any{"tools": map[string]any{
					"allow": []any{"Bash"},
					"deny":  []any{"Write", "Edit", "NotebookEdit", "Task", "WebFetch", "WebSearch"},
				}}),
			}},
		})
		s := runFor(entrypoint, decided, line, liveTimeout)

		By("then HTTPS_PROXY, HTTP_PROXY, NO_PROXY, ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN are present")
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero(), s.terminal().GetFinalMessage())
		printed := ""
		for _, event := range s.events() {
			if result := event.GetToolResult(); result != nil {
				printed += result.GetContent()
			}
		}
		for name, value := range decided {
			Expect(printed).To(ContainSubstring(name+"="+value), "the Bash tool did not see %s", name)
		}

		By("and the session reached the model through the gateway, on the step token")
		Expect(swapped.Load()).To(BeNumerically(">", 0), "no request reached the gateway with the step token")

		By("and the host's own model credential did not reach the session")
		Expect(printed).NotTo(ContainSubstring("CLAUDE_CODE_OAUTH_TOKEN"))
		Expect(printed).NotTo(ContainSubstring(upstream))
	})

	It("carries structured output that validates against the step's schema", func() {
		By("given a step that declares a reviewer's output schema")
		line := request(&harnessv1.Request{
			Id: "live-structured",
			Body: &harnessv1.Request_Start{Start: &harnessv1.Start{
				SessionId:    sessionID,
				Prompt:       "Judge whether the criterion 'the CLI exits non-zero on an unknown flag' holds. You have no code to read; decide from the statement alone and answer in the required format.",
				Cwd:          workdir,
				OutputSchema: reviewSchema,
				Settings:     settings(map[string]any{"tools": map[string]any{"deny": []any{"Bash", "Write", "Edit"}}}),
			}},
		})

		By("when the session ends")
		s := runFor(entrypoint, nil, line, liveTimeout)

		By("then the terminal record's structured output validates against the schema")
		Expect(s.terminal().GetExit().GetStatus()).To(BeZero())
		output := structuredOutput(s.terminal())
		Expect(output["decision"]).To(BeElementOf("approve", "revise", "needs_human"))
		Expect(output["findings"]).To(BeAssignableToTypeOf([]any{}))
		summary, ok := output["summary"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(summary["headline"]).To(BeAssignableToTypeOf(""))
		Expect(summary["confidence"]).To(BeAssignableToTypeOf(float64(0)))
	})
})

// connectProxy is a forward proxy of the plainest kind — CONNECT passed
// through, nothing intercepted — which is what the host runs for a machine
// (docs/sandbox.md §Network). It stops with the spec.
func connectProxy() *httptest.Server {
	GinkgoHelper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		origin, err := net.DialTimeout("tcp", r.Host, 30*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = origin.Close()
			http.Error(w, "cannot hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			_ = origin.Close()
			return
		}
		if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			_ = client.Close()
			_ = origin.Close()
			return
		}
		go func() {
			_, _ = io.Copy(origin, client)
			_ = origin.Close()
		}()
		_, _ = io.Copy(client, origin)
		_ = client.Close()
	}))
	DeferCleanup(server.Close)
	return server
}

// credentialGateway is the gateway of docs/sandbox.md §Credentials, reduced
// to what this spec needs: a request bearing the step token is forwarded to
// the API with the subscription credential in its place, and counted; any
// other bearer is refused. It stops with the spec.
func credentialGateway(stepToken, upstream string) (*httptest.Server, *atomic.Int64) {
	GinkgoHelper()

	api, err := url.Parse("https://api.anthropic.com")
	Expect(err).NotTo(HaveOccurred())
	swapped := &atomic.Int64{}
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(api)
			r.Out.Host = api.Host
			r.Out.Header.Set("Authorization", "Bearer "+upstream)
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != stepToken {
			http.Error(w, "not this step's token", http.StatusUnauthorized)
			return
		}
		swapped.Add(1)
		forward.ServeHTTP(w, r)
	}))
	DeferCleanup(server.Close)
	return server, swapped
}
