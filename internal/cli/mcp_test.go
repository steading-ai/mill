package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/cli"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/server"
	"github.com/steading-ai/mill/internal/server/interceptor"
	"github.com/steading-ai/mill/internal/testsupport"
)

// session is `mill mcp` running over a pair of pipes, spoken to the way a
// harness speaks to it: one JSON-RPC message per line on stdin, one response
// per line on stdout.
type session struct {
	requests *io.PipeWriter
	replies  *bufio.Reader
	stderr   *bytes.Buffer
	done     chan error
	next     int
}

// mcp starts `mill mcp` against a spec's service.
func mcp(ctx context.Context, args ...string) *session {
	GinkgoHelper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &bytes.Buffer{}

	root := cli.NewRootCommand(stdoutW, stderr)
	root.SetIn(stdinR)
	root.SetArgs(append([]string{"mcp"}, args...))

	s := &session{requests: stdinW, replies: bufio.NewReader(stdoutR), stderr: stderr, done: make(chan error, 1)}
	go func() {
		defer GinkgoRecover()
		err := root.ExecuteContext(ctx)
		_ = stdoutW.Close()
		s.done <- err
	}()
	DeferCleanup(func() {
		_ = stdinW.Close()
		Eventually(s.done, 10*time.Second).Should(Receive(BeNil()), stderr.String())
	})
	return s
}

// call sends one request and returns the result of the response to it.
func (s *session) call(method string, params map[string]any) map[string]any {
	GinkgoHelper()
	s.next++
	request := map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method}
	if params != nil {
		request["params"] = params
	}
	raw, err := json.Marshal(request)
	Expect(err).NotTo(HaveOccurred())
	_, err = s.requests.Write(append(raw, '\n'))
	Expect(err).NotTo(HaveOccurred())

	line, err := s.replies.ReadBytes('\n')
	Expect(err).NotTo(HaveOccurred(), s.stderr.String())
	var response struct {
		Result map[string]any  `json:"result"`
		Error  *map[string]any `json:"error"`
	}
	Expect(json.Unmarshal(line, &response)).To(Succeed())
	Expect(response.Error).To(BeNil(), string(line))
	return response.Result
}

// tool calls one tool and returns the JSON it answered with, decoded.
func (s *session) tool(name string, args map[string]any) map[string]any {
	GinkgoHelper()
	result := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	Expect(result["isError"]).To(BeNil(), "%v", result)
	content, ok := result["content"].([]any)
	Expect(ok).To(BeTrue(), "%v", result)
	Expect(content).To(HaveLen(1))
	text, ok := content[0].(map[string]any)["text"].(string)
	Expect(ok).To(BeTrue())

	var decoded map[string]any
	Expect(json.Unmarshal([]byte(text), &decoded)).To(Succeed(), text)
	return decoded
}

// toolNames are the tools the server advertises.
func (s *session) toolNames() []string {
	GinkgoHelper()
	listed := s.call("tools/list", nil)
	tools, ok := listed["tools"].([]any)
	Expect(ok).To(BeTrue(), "%v", listed)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

var _ = Describe("mill mcp", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("lists every read tool and the operate ones, and run is not among them", func() {
		r := openRun(ctx, "millcli#30", nodeID(30))
		s := mcp(ctx, "--socket", r.mill.socket)

		Expect(s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})).
			To(HaveKeyWithValue("protocolVersion", "2025-06-18"))
		Expect(s.toolNames()).To(ConsistOf(
			"status", "inspect", "explain", "logs", "history", "cost",
			"workflow_list", "workflow_show", "doctor", "cancel",
			"rerun", "workspace_show", "workspace_clean"))
		// `run` executes in the calling process and is local to the mill's
		// host and account; a session elsewhere files a skift node.
		Expect(s.toolNames()).NotTo(ContainElement("run"))
	})

	It("answers explain with the same fields the CLI prints", func() {
		r := openRun(ctx, "millcli#31", nodeID(31))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		r.fail(ctx, "implement", 1)
		r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		s := mcp(ctx, "--socket", r.mill.socket)
		s.call("initialize", nil)
		answer := s.tool("explain", map[string]any{"run": "millcli#31"})

		printed := r.cli(ctx, "explain", "millcli#31")
		Expect(answer).To(HaveKeyWithValue("node", "millcli#31"))
		Expect(answer).To(HaveKeyWithValue("status", "running"))
		Expect(answer).To(HaveKeyWithValue("step", "implement"))
		Expect(answer).To(HaveKeyWithValue("step_run_status", "running"))
		Expect(answer).To(HaveKeyWithValue("evidence", "artifact"))
		Expect(answer).To(HaveKeyWithValue("attempts_used", float64(2)))
		Expect(answer).To(HaveKeyWithValue("retries", float64(3)))
		for _, field := range []string{"implement", "artifact", "running"} {
			Expect(printed).To(ContainSubstring(field), "the CLI prints what the tool answered")
		}
		Expect(printed).To(ContainSubstring("attempts  2 of 3 retries"))
	})

	It("sizes a reader's answer with view, as skift's tools do", func() {
		r := openRun(ctx, "millcli#32", nodeID(32))

		s := mcp(ctx, "--socket", r.mill.socket)
		s.call("initialize", nil)

		refs := s.tool("status", map[string]any{"view": "refs"})
		first := refs["runs"].([]any)[0].(map[string]any)
		Expect(first).To(HaveKeyWithValue("node", "millcli#32"))
		Expect(first).NotTo(HaveKey("elapsed"))

		summary := s.tool("status", map[string]any{})
		Expect(summary["runs"].([]any)[0].(map[string]any)).To(HaveKey("elapsed"))

		full := s.tool("status", map[string]any{"view": "full"})
		Expect(full).To(HaveKey("runs"))
		Expect(full["runs"].([]any)[0]).To(HaveKey("run"), "full is the wire's own shape")
	})

	It("cancel over MCP asks the worker to stop", func() {
		r := openRun(ctx, "millcli#33", nodeID(33))
		s := mcp(ctx, "--socket", r.mill.socket)
		s.call("initialize", nil)

		answer := s.tool("cancel", map[string]any{"run": "millcli#33", "reason": "an agent was asked to stop it"})
		Expect(answer["run"].(map[string]any)).To(HaveKeyWithValue("cancel_requested", true))
		Expect(r.reload(ctx).GetCancelRequested()).To(BeTrue())
	})

	It("reads logs over the RPC, tail and the step-the-run-is-on default both honored", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#35", nodeID(35))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)
		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt()),
			textActivity("line one"), textActivity("line two"), textActivity("line three"))

		s := mcp(ctx, "--socket", m.socket)
		s.call("initialize", nil)

		answer := s.tool("logs", map[string]any{"run": "millcli#35", "tail": 2})
		Expect(answer).To(HaveKeyWithValue("step", "implement"))
		Expect(answer).To(HaveKeyWithValue("attempt", float64(1)))
		lines, ok := answer["lines"].([]any)
		Expect(ok).To(BeTrue(), "%v", answer)
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(ContainSubstring("line two"))
		Expect(lines[1]).To(ContainSubstring("line three"))
	})

	It("reports a misspelt argument rather than ignoring it", func() {
		r := openRun(ctx, "millcli#34", nodeID(34))
		s := mcp(ctx, "--socket", r.mill.socket)
		s.call("initialize", nil)

		result := s.call("tools/call", map[string]any{
			"name": "explain", "arguments": map[string]any{"runn": "millcli#34"},
		})
		Expect(result["isError"]).To(BeTrue())
	})

	It("renders a streamed text block's deltas as one accumulated line", func() {
		logDir := GinkgoT().TempDir()
		m := servingWithLogDir(ctx, logDir)
		r := openRunOn(ctx, m, "millcli#36", nodeID(36))
		sr := r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		appendActivity(run.ActivityPath(logDir, r.run.GetId(), "implement", sr.GetAttempt()),
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: "Hello", Delta: true}}},
			&millv1.Activity{Source: millv1.ActivitySource_ACTIVITY_SOURCE_HARNESS,
				Body: &millv1.Activity_Text{Text: &millv1.Text{BlockId: "b1", Text: ", world!", Final: true}}},
		)

		s := mcp(ctx, "--socket", m.socket)
		s.call("initialize", nil)

		result := s.tool("logs", map[string]any{"run": "millcli#36", "step": "implement"})
		lines, ok := result["lines"].([]any)
		Expect(ok).To(BeTrue(), "%v", result)
		Expect(lines).To(ConsistOf(ContainSubstring("text Hello, world!")))
	})

	It("A thicket agent can say why a run is stuck", func() {
		By("given a run parked after three review rounds")
		g := skiftNode(ctx)
		r := openRun(ctx, g.nodeRef, g.nodeID)
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		for round := 0; round < 3; round++ {
			r.fail(ctx, "implement", 1)
		}
		g.fileGate(ctx, r.run.GetId(), "implement", "implement failed three times on "+g.nodeRef)
		r.park(ctx, g.gateID)

		By("when explain is called over MCP")
		s := mcp(ctx, "--socket", r.mill.socket,
			"--skift-socket", g.service.Socket, "--project", skiftProject)
		s.call("initialize", nil)
		answer := s.tool("explain", map[string]any{"run": g.nodeRef})

		By("then the response names the parked step, the park gate, and the attempts used")
		Expect(answer).To(HaveKeyWithValue("status", "parked"))
		Expect(answer).To(HaveKeyWithValue("step", "implement"))
		Expect(answer).To(HaveKeyWithValue("parked_reason", "retries"))
		Expect(answer).To(HaveKeyWithValue("gate", g.gateRef))
		Expect(answer).To(HaveKeyWithValue("gate_title", g.title))
		Expect(answer).To(HaveKeyWithValue("attempts_used", float64(3)))
		Expect(answer).To(HaveKeyWithValue("retries", float64(3)))

		By("and it contains no transcript content")
		// The ledger holds no transcript, and explain reads nothing
		// else: what comes back is the run's shape, the step's contract
		// and the gate's ask.
		raw, err := json.Marshal(answer)
		Expect(err).NotTo(HaveOccurred())
		for key := range answer {
			Expect(key).To(BeElementOf(
				"node", "run", "status", "step", "step_kind", "step_run_status", "exit",
				"evidence", "attempts_used", "retries", "gate", "gate_title",
				"parked_reason", "waiting_on"))
		}
		Expect(string(raw)).NotTo(ContainSubstring("final_message"))
		Expect(string(raw)).NotTo(ContainSubstring("prompt"))
	})
})

// readerSocket serves the ledger on a unix socket this process reaches as a
// `reader`. The role map's service uid is one nothing on this host holds, so
// the peer uid the kernel reports for the spec's own connections resolves to
// the role the map gives it and not to the service (internal/server/interceptor).
func readerSocket(ctx context.Context, m mill) mill {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "reader.sock")
	svc, err := domain.Open(ctx, m.databaseURL)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(svc.Close)
	srv := server.New(server.Config{
		SocketPath: path,
		Logger:     slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Interceptors: []connect.Interceptor{interceptor.NewRoles(
			interceptor.RoleMap{
				// A uid no account on this host holds, so the spec's own is
				// not the service's.
				ServiceUID: -1,
				Roles:      map[int]interceptor.Role{os.Getuid(): interceptor.RoleReader},
			}, server.Requirement())},
	}, server.NewStoreService(svc, server.NewObserver(server.DefaultObserverFallback), GinkgoT().TempDir()), nil)
	Expect(srv.Listen()).To(Succeed())

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		served <- srv.Serve(serveCtx)
	}()
	DeferCleanup(func() {
		stop()
		Eventually(served, 10*time.Second).Should(Receive(BeNil()))
	})
	return mill{socket: path}
}

var _ = Describe("mill mcp under a reader uid", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("a reader reads a workflow and is refused rerun and workspace clean", func() {
		// The ledger is composed over a socket the spec reaches as the
		// service, and then read back over one it reaches as a reader: the
		// two differ in nothing but the role map.
		writer := serving(ctx)
		r := openRunOn(ctx, writer, "millcli#80", nodeID(80))
		reader := readerSocket(ctx, writer)

		s := mcp(ctx, "--socket", reader.socket, "--workspace-dir", GinkgoT().TempDir())
		s.call("initialize", nil)

		// A reader may look at what a run would follow.
		shown := s.tool("workflow_show", map[string]any{"name": "quick-fix"})
		Expect(shown).To(HaveKeyWithValue("name", "quick-fix"))
		Expect(shown["steps"]).To(HaveLen(7))

		// It may not redo one. The store refuses the write, naming the
		// role.
		refused := s.call("tools/call", map[string]any{
			"name": "rerun", "arguments": map[string]any{"run": r.run.GetId(), "from": "implement"},
		})
		Expect(refused["isError"]).To(BeTrue(), "%v", refused)
		Expect(textOf(refused)).To(ContainSubstring(string(interceptor.RoleReader)))

		// Nor may it remove a workspace, which the store holds nothing
		// about: `mill mcp` asks the socket who the caller is and applies
		// the same map.
		refused = s.call("tools/call", map[string]any{
			"name": "workspace_clean", "arguments": map[string]any{"dry_run": true},
		})
		Expect(refused["isError"]).To(BeTrue(), "%v", refused)
		Expect(textOf(refused)).To(ContainSubstring(string(interceptor.RoleReader)))
	})
})

// textOf is the text of a tool result's single content block.
func textOf(result map[string]any) string {
	GinkgoHelper()
	content, ok := result["content"].([]any)
	Expect(ok).To(BeTrue(), "%v", result)
	Expect(content).To(HaveLen(1))
	text, ok := content[0].(map[string]any)["text"].(string)
	Expect(ok).To(BeTrue())
	return text
}
