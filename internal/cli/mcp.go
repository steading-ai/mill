package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/version"
)

// `mill mcp` is the CLI's client wrapped for a session: an MCP server on
// stdio whose tools are the read commands and the operate ones, taking the same
// references and answering with the same fields. It is dagger's `dg mcp`
// with mill's verbs — deliberately so, because an agent that knows one
// should not have to learn the other (docs/architecture.md §"One binary, two
// faces").
//
// `run` is not here and will not be: it executes in the calling process and
// is local to the mill's host and account. A session elsewhere that wants
// work done files a dagger node (docs/prd.md R-55).
//
// The protocol is JSON-RPC 2.0, one message per line, as the MCP stdio
// transport specifies:
// https://modelcontextprotocol.io/specification/2025-06-18/basic/transports

// mcpProtocolVersion is the newest protocol revision this server speaks. A
// client asking for an older one it also speaks gets that one back.
const mcpProtocolVersion = "2025-06-18"

var mcpProtocolVersions = []string{mcpProtocolVersion, "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	jsonrpcParseError     = -32700
	jsonrpcInvalidRequest = -32600
	jsonrpcMethodNotFound = -32601
	jsonrpcInvalidParams  = -32602
	jsonrpcInternalError  = -32603
)

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string { return e.Message }

func rpcError(code int, format string, args ...any) *jsonrpcError {
	return &jsonrpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// The MCP messages the server exchanges. Only the fields it reads or writes
// are modelled.

type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type mcpInitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

type mcpCapabilities struct {
	Tools struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// mcpTool is one tool: its advertisement and the call behind it. call
// returns whatever the tool answers, rendered as JSON, or an error the model
// reads.
type mcpTool struct {
	name        string
	description string
	schema      map[string]any
	call        func(ctx context.Context, args json.RawMessage) (any, error)
}

// mcpServer is one `mill mcp` process: the client, and the tools over it.
type mcpServer struct {
	opts readOptions
	log  *slog.Logger

	in  io.Reader
	out io.Writer
	// outMu serialises responses; tool calls run concurrently.
	outMu sync.Mutex

	tools []*mcpTool

	// calls counts in-flight requests, drained before the process ends.
	calls sync.WaitGroup
}

func newMCPCommand() *cobra.Command {
	opts := readOptions{}
	var describe bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the read and operate tools as an MCP server over stdio",
		Long: `Serve mill to a session such as Claude Code as an MCP server on stdin and
stdout. Each tool is one CLI command: same run references, same fields, same
answers as JSON.

The read tools return ledger data and never transcript content. "cancel",
"rerun" and "workspace_clean" are the operate tools, and what they may do is
decided by the unix account this process runs as, not by anything a session
says: the store maps that account's uid to a role, and a reader is refused.

"run" is deliberately absent. It executes in the calling process and is
local to the mill's host and account; a session that wants work done files a
dagger node instead.

--describe prints the tool list as JSON and connects to nothing, which is
how the plugin's reference document is checked against this server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if describe {
				return describeMCP(cmd, opts)
			}
			return runMCP(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&describe, "describe", false, "print the tool list as JSON and exit, without serving")
	addReadFlags(cmd, &opts)
	addDaggerFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces live under")
	return cmd
}

// describeMCP prints what `tools/list` would answer, and connects to
// nothing. It is how the plugin's reference document is checked against the
// server it documents (plugin/mill/skills/mill/references/tools.md), so its
// shape is that answer verbatim: a "tools" array of name, description and
// inputSchema.
func describeMCP(cmd *cobra.Command, opts readOptions) error {
	server := newMCPServer(opts, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(map[string]any{"tools": server.toolInfos()})
}

func runMCP(cmd *cobra.Command, opts readOptions) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return newMCPServer(opts, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()).serve(ctx)
}

// newMCPServer builds the server. It is exported to the package rather than
// to the world so a spec can drive it over two pipes.
func newMCPServer(opts readOptions, in io.Reader, out, diag io.Writer) *mcpServer {
	s := &mcpServer{
		opts: opts,
		log:  slog.New(slog.NewTextHandler(diag, &slog.HandlerOptions{Level: slog.LevelInfo})),
		in:   in,
		out:  out,
	}
	s.tools = s.toolset()
	return s
}

// serve runs until stdin closes or ctx is cancelled.
func (s *mcpServer) serve(ctx context.Context) error {
	s.log.Info("mill mcp starting", "version", version.String(), "socket", s.opts.socket)

	callCtx, cancelCalls := context.WithCancel(ctx)
	defer cancelCalls()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(s.in)
		for {
			line, err := r.ReadBytes('\n')
			if len(strings.TrimSpace(string(line))) > 0 {
				lines <- line
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	how := ""
loop:
	for {
		select {
		case <-ctx.Done():
			how = "signal received"
			break loop
		case err := <-readErr:
			how = "stdin closed"
			if !errors.Is(err, io.EOF) {
				s.log.Error("read stdin", "error", err)
			}
			break loop
		case line := <-lines:
			s.dispatch(callCtx, line)
		}
	}

	s.log.Info("mill mcp shutting down", "cause", how)
	cancelCalls()
	s.calls.Wait()
	return nil
}

// dispatch parses one line and answers it. Requests run concurrently so that
// a `logs --follow` does not stall a `status`.
func (s *mcpServer) dispatch(ctx context.Context, line []byte) {
	var req jsonrpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(nil, nil, rpcError(jsonrpcParseError, "parse error: %v", err))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.reply(req.ID, nil, rpcError(jsonrpcInvalidRequest, "invalid request: jsonrpc must be \"2.0\" and method must be set"))
		return
	}
	// A notification carries no id and gets no reply.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	s.calls.Add(1)
	go func() {
		defer s.calls.Done()
		result, err := s.handle(ctx, req)
		s.reply(req.ID, result, err)
	}()
}

// reply writes one response. A nil id answers a message that could not be
// parsed, which JSON-RPC requires to carry a null id.
func (s *mcpServer) reply(id json.RawMessage, result any, err *jsonrpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	res := jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: err}
	raw, marshalErr := json.Marshal(res)
	if marshalErr != nil {
		raw, _ = json.Marshal(jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: rpcError(jsonrpcInternalError, "encode response: %v", marshalErr)})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(raw, '\n')); err != nil {
		s.log.Error("write stdout", "error", err)
	}
}

func (s *mcpServer) handle(ctx context.Context, req jsonrpcRequest) (any, *jsonrpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolInfos()}, nil
	case "tools/call":
		var params mcpCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, rpcError(jsonrpcInvalidParams, "tools/call: %v", err)
		}
		for _, t := range s.tools {
			if t.name == params.Name {
				return s.callTool(ctx, t, params.Arguments), nil
			}
		}
		return nil, rpcError(jsonrpcInvalidParams, "unknown tool %q; call tools/list for the tools this server offers", params.Name)
	default:
		return nil, rpcError(jsonrpcMethodNotFound, "method %q is not supported", req.Method)
	}
}

// toolInfos advertises every registered tool. One list answers both
// `tools/list` and `--describe`, so a tool cannot be offered to a session and
// missing from the description, or the other way round.
func (s *mcpServer) toolInfos() []mcpToolInfo {
	infos := make([]mcpToolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		infos = append(infos, mcpToolInfo{Name: t.name, Description: t.description, InputSchema: t.schema})
	}
	return infos
}

func (s *mcpServer) initialize(raw json.RawMessage) (any, *jsonrpcError) {
	var params mcpInitializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, rpcError(jsonrpcInvalidParams, "initialize: %v", err)
		}
	}
	protocol := mcpProtocolVersion
	for _, v := range mcpProtocolVersions {
		if v == params.ProtocolVersion {
			protocol = v
		}
	}
	return mcpInitializeResult{
		ProtocolVersion: protocol,
		ServerInfo:      mcpServerInfo{Name: "mill", Version: version.String()},
		Instructions: `mill is a workflow runner over dagger's work graph. A run is one dagger
node being worked: a workflow's steps in order, each closed by evidence the
mill verified.

Typical flow: status to see what is live, explain <run> to see where one is
and what it is waiting on, inspect <run> for every attempt and its evidence,
logs for the redacted per-step log, history for the audit trail.

A run is named by its node reference — mill#18 — or by the run id status
prints. Views size an answer: refs is identifiers, summary is the default,
full is everything the ledger holds.

A run that is waiting or parked is blocked on a dagger gate, and only a
person can answer it: say what you think should happen, do not try to
unblock it yourself.

Three tools change something. cancel stops a run's current step cleanly and
leaves it resumable. rerun redoes a run from a named step, keeping the
generation, the branch and the sessions — restart, which does not, is not
offered here. workspace_clean removes the checkouts of runs that have ended.
The last two need the operator role; the unix account this server runs as
decides whether you hold it.

Nothing here returns transcript content. The ledger holds evidence, not
words.`,
	}, nil
}

// callTool runs one tool. Every failure, whether a bad argument or the
// service's answer, is a tool error the model reads and acts on.
func (s *mcpServer) callTool(ctx context.Context, t *mcpTool, args json.RawMessage) mcpCallResult {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	value, err := t.call(ctx, args)
	if err != nil {
		return mcpCallResult{IsError: true, Content: []mcpContent{{Type: "text", Text: toolErrorText(t.name, err)}}}
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return mcpCallResult{IsError: true, Content: []mcpContent{{Type: "text", Text: t.name + ": encode response: " + err.Error()}}}
	}
	return mcpCallResult{Content: []mcpContent{{Type: "text", Text: string(raw)}}}
}

// toolErrorText renders an error for a model: the Connect code, the message,
// and for the codes a caller can do something about, what to do.
func toolErrorText(tool string, err error) string {
	var b strings.Builder
	code := connect.CodeOf(err)
	fmt.Fprintf(&b, "%s failed (%s): %s", tool, code, err.Error())
	switch code {
	case connect.CodeInvalidArgument:
		b.WriteString("\nCheck the arguments against the tool description and try again.")
	case connect.CodeNotFound:
		b.WriteString("\nNothing matched that reference. A run is named by its node reference, such as mill#18, or by the run id status prints.")
	case connect.CodePermissionDenied:
		b.WriteString("\nThe unix account this server runs as does not hold the role this needs. A person must grant it or do it themselves.")
	case connect.CodeUnauthenticated:
		b.WriteString("\nThe store does not recognise the unix account this server runs as. A person must add it to the role map.")
	case connect.CodeUnavailable, connect.CodeUnknown:
		b.WriteString("\nThe service could not be reached. Check that `mill serve` is running and that --socket points at it.")
	}
	return b.String()
}
