// Package socket serves the workspace's verbs on a unix socket.
//
// The socket is how a step is given capability without being given the host
// (docs/domain-model.md §Workspace). Its client is the harness adapter, not
// the session: the sidecar holds the socket and re-exposes the verbs as an
// MCP server hosted in its own process, because no MCP transport the Agent
// SDK offers reaches a unix socket — `docs/spikes/mcp-workspace.md` Q3. So
// this server speaks HTTP and JSON over the socket and does not speak MCP.
//
// Two rules hold for every request. It carries a bearer, which is checked by
// hash against the step token the workspace currently holds, so a rotated or
// revoked token stops working on the request after the rotation. And the verb
// it names must be one the token's capability set grants, which is refused by
// name rather than by silence.
//
// What the socket is not is enforcement of what a spawned process does. `exec`
// can write where `write` was not granted; MVP detects that with the
// `clean-tree` check rather than preventing it (docs/architecture.md §"The
// workspace"). The boundary that holds is the one [workspace.Workspace.Exec]
// sets: cwd, uid and environment.
package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

// Verbs is what the socket exposes: the workspace's verbs without its
// lifecycle. Create, Attach, Destroy and Rotate belong to the worker and are
// unreachable from a session by construction, because they are not here.
type Verbs interface {
	Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error)
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, content []byte) error
	List(ctx context.Context, path string) ([]workspace.Entry, error)
	Git() workspace.Git
	Forge() workspace.ForgeVerbs
	Redact(values ...string)
}

// Verb names, as they appear in the table in docs/architecture.md §"The
// workspace" and on the wire.
const (
	VerbFSRead      = "fs.read"
	VerbFSWrite     = "fs.write"
	VerbFSList      = "fs.list"
	VerbExec        = "exec"
	VerbGitStatus   = "git.status"
	VerbGitDiff     = "git.diff"
	VerbGitLog      = "git.log"
	VerbGitCommit   = "git.commit"
	VerbGitPush     = "git.push"
	VerbForgePR     = "forge.pr"
	VerbForgeChecks = "forge.checks"
	VerbRedact      = "redact"
	// VerbReportInstall is the `dnf` shim's report of what a session
	// installed into its machine (docs/sandbox.md §"The dnf shim"). Any
	// live token may report: the install already happened, and the report
	// is provenance, not a grant.
	VerbReportInstall = "report.install"
)

// required is the capability each verb is gated by. A verb absent from this
// table does not exist; a verb whose capability is empty needs only a live
// token, which is what "any" means for `redact` in the architecture's table.
var required = map[string]workspace.Capability{
	VerbFSRead:        workspace.Read,
	VerbFSList:        workspace.Read,
	VerbFSWrite:       workspace.Write,
	VerbExec:          workspace.Exec,
	VerbGitStatus:     workspace.Read,
	VerbGitDiff:       workspace.Read,
	VerbGitLog:        workspace.Read,
	VerbGitCommit:     workspace.Write,
	VerbGitPush:       workspace.Forge,
	VerbForgePR:       workspace.Forge,
	VerbForgeChecks:   workspace.Forge,
	VerbRedact:        "",
	VerbReportInstall: "",
}

// Error codes on the wire.
const (
	// CodeUnauthorized is a bearer that is not the live step token.
	CodeUnauthorized = "unauthorized"
	// CodeForbidden is a live token without the verb's capability.
	CodeForbidden = "forbidden"
	// CodeUnknownVerb is a verb that does not exist.
	CodeUnknownVerb = "unknown_verb"
	// CodeInvalid is a request the verb cannot read.
	CodeInvalid = "invalid"
	// CodeNotImplemented is a verb this workspace cannot serve: one it does
	// not implement, or a forge verb on a run with no forge.
	CodeNotImplemented = "not_implemented"
	// CodeFailed is the verb refusing on its own terms.
	CodeFailed = "failed"
)

// Error is the body of every response that is not a success. Capability is
// set when the refusal was a capability the token does not carry, so a client
// can tell the session which one to ask its workflow for.
type Error struct {
	Code       string               `json:"code"`
	Message    string               `json:"message"`
	Capability workspace.Capability `json:"capability,omitempty"`
	Verb       string               `json:"verb,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Server serves the verbs over a unix socket. Its zero value is not usable;
// see [New].
type Server struct {
	verbs Verbs

	mu    sync.RWMutex
	token workspace.Token

	logMu sync.Mutex
	log   io.WriteCloser

	listenerMu sync.Mutex
	listener   net.Listener
	http       *http.Server
	path       string
	done       chan struct{}
}

// Option configures a server.
type Option func(*Server)

// WithLog writes one summary line per request to w, through the redactor, so
// that every call at the seam is in the step run's log and no registered
// value survives into it.
func WithLog(w io.Writer, redactor *redact.Redactor) Option {
	return func(s *Server) {
		if w == nil {
			return
		}
		if redactor == nil {
			redactor = redact.New()
		}
		s.log = redactor.Writer(w)
	}
}

// New returns a server for the verbs, holding no token: every request is
// refused until [Server.Rotate] installs one.
func New(verbs Verbs, options ...Option) *Server {
	s := &Server{verbs: verbs}
	for _, option := range options {
		option(s)
	}
	return s
}

// Rotate installs the live step token. The token it replaces stops
// authenticating on the next request; the zero token leaves the socket with
// no live token at all, which is how a step's access ends when the step run
// closes.
func (s *Server) Rotate(token workspace.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

// Token is the live token.
func (s *Server) Token() workspace.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

// Listen creates the socket at path and serves on it until [Server.Close].
// The socket is created inside a directory only the mill's account can read
// and with 0600 of its own, so the filesystem is a second check on the
// bearer.
func (s *Server) Listen(path string) error {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listener != nil {
		return fmt.Errorf("socket: already listening on %s", s.path)
	}
	// Before the directory is made and before bind: a path the kernel
	// cannot hold fails as "invalid argument", which names nothing.
	if err := CheckPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("socket: create %s: %w", filepath.Dir(path), err)
	}
	// A socket left behind by a worker that died would make bind fail; the
	// run directory is this workspace's own, so removing it is safe.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("socket: remove stale %s: %w", path, err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("socket: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("socket: chmod %s: %w", path, err)
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	s.listener, s.path, s.http, s.done = listener, path, server, done
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	return nil
}

// Path is the socket's path, empty until it listens.
func (s *Server) Path() string {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	return s.path
}

// Close stops serving, removes the socket, and flushes the log.
func (s *Server) Close(ctx context.Context) error {
	s.listenerMu.Lock()
	server, listener, path, done := s.http, s.listener, s.path, s.done
	s.http, s.listener, s.path, s.done = nil, nil, "", nil
	s.listenerMu.Unlock()

	var err error
	if server != nil {
		err = server.Shutdown(ctx)
		<-done
	} else if listener != nil {
		err = listener.Close()
	}
	if path != "" {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = removeErr
		}
	}
	s.logMu.Lock()
	if s.log != nil {
		_ = s.log.Close()
	}
	s.logMu.Unlock()
	return err
}

// Handler is the verb table as an HTTP handler, so a caller that would rather
// serve it somewhere else — a spec, a future container workspace — can.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/{verb}", s.serve)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, &Error{Code: CodeUnknownVerb, Message: "socket: no such verb"})
	})
	return mux
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")

	token := s.Token()
	bearer := bearerOf(r)
	if !token.Matches(bearer) {
		s.logf("verb=%s refused=unauthorized", verb)
		writeError(w, http.StatusUnauthorized, &Error{
			Code:    CodeUnauthorized,
			Verb:    verb,
			Message: "workspace: the bearer presented is not this step's live token",
		})
		return
	}

	capability, known := required[verb]
	if !known {
		s.logf("verb=%s refused=unknown_verb", verb)
		writeError(w, http.StatusNotFound, &Error{
			Code:    CodeUnknownVerb,
			Verb:    verb,
			Message: fmt.Sprintf("workspace: no verb %q", verb),
		})
		return
	}
	if capability != "" && !token.Grants(capability) {
		refusal := &workspace.CapabilityError{Verb: verb, Missing: capability}
		s.logf("verb=%s refused=forbidden capability=%s", verb, capability)
		writeError(w, http.StatusForbidden, &Error{
			Code:       CodeForbidden,
			Verb:       verb,
			Capability: capability,
			Message:    refusal.Error(),
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, &Error{Code: CodeInvalid, Verb: verb, Message: "workspace: request body: " + err.Error()})
		return
	}

	response, status, refusal := s.dispatch(r.Context(), verb, body)
	if refusal != nil {
		s.logf("verb=%s refused=%s message=%s", verb, refusal.Code, refusal.Message)
		writeError(w, status, refusal)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// maxRequestBytes caps a request body. A file written through fs.write is the
// largest thing on this wire; anything past this is a mistake or an attack.
const maxRequestBytes = 64 << 20

func (s *Server) dispatch(ctx context.Context, verb string, body []byte) (any, int, *Error) {
	switch verb {
	case VerbFSRead:
		var request PathRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		content, err := s.verbs.Read(ctx, request.Path)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s path=%s bytes=%d", verb, request.Path, len(content))
		return ReadResponse{Content: content}, http.StatusOK, nil

	case VerbFSWrite:
		var request WriteRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if err := s.verbs.Write(ctx, request.Path, request.Content); err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s path=%s bytes=%d", verb, request.Path, len(request.Content))
		return WriteResponse{Path: request.Path, Bytes: len(request.Content)}, http.StatusOK, nil

	case VerbFSList:
		var request PathRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		entries, err := s.verbs.List(ctx, request.Path)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s path=%s entries=%d", verb, request.Path, len(entries))
		return ListResponse{Entries: entries}, http.StatusOK, nil

	case VerbExec:
		var request ExecRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		response, err := s.exec(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s command=%s status=%d out=%s", verb, strings.TrimSpace(request.Path+" "+strings.Join(request.Args, " ")), response.Status, summarize(response.Stdout))
		return response, http.StatusOK, nil

	case VerbGitStatus:
		status, err := s.verbs.Git().Status(ctx)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s branch=%s clean=%t files=%d", verb, status.Branch, status.Clean, len(status.Files))
		return status, http.StatusOK, nil

	case VerbGitDiff:
		var request workspace.DiffRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		diff, err := s.verbs.Git().Diff(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s staged=%t bytes=%d", verb, request.Staged, len(diff))
		return DiffResponse{Diff: diff}, http.StatusOK, nil

	case VerbGitLog:
		var request workspace.LogRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		commits, err := s.verbs.Git().Log(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s commits=%d", verb, len(commits))
		return LogResponse{Commits: commits}, http.StatusOK, nil

	case VerbGitCommit:
		var request workspace.CommitRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		commit, err := s.verbs.Git().Commit(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s sha=%s subject=%s", verb, commit.SHA, commit.Subject)
		return commit, http.StatusOK, nil

	case VerbRedact:
		var request RedactRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		s.verbs.Redact(request.Values...)
		s.logf("verb=%s values=%d", verb, len(request.Values))
		return RedactResponse{Registered: len(request.Values)}, http.StatusOK, nil

	case VerbReportInstall:
		var request ReportInstallRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		reporter, ok := s.verbs.(workspace.Reporter)
		if !ok {
			return nil, http.StatusNotImplemented, &Error{Code: CodeNotImplemented, Verb: verb,
				Message: "workspace: this workspace keeps no record of installs"}
		}
		if err := reporter.ReportInstall(ctx, request.Packages); err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s packages=%s", verb, strings.Join(request.Packages, ","))
		return ReportInstallResponse{Packages: len(request.Packages)}, http.StatusOK, nil

	case VerbGitPush:
		var request workspace.PushRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		result, err := s.verbs.Forge().Push(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s branch=%s sha=%s remote=%s", verb, result.Branch, result.SHA, result.Remote)
		return result, http.StatusOK, nil

	case VerbForgePR:
		var request workspace.PRRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		pr, err := s.verbs.Forge().PR(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s pr=%d state=%s url=%s", verb, pr.Number, pr.State, pr.URL)
		return pr, http.StatusOK, nil

	case VerbForgeChecks:
		var request workspace.ChecksRequest
		if err := decode(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		result, err := s.verbs.Forge().Checks(ctx, request)
		if err != nil {
			return nil, statusFor(err), failure(verb, err)
		}
		s.logf("verb=%s sha=%s status=%s", verb, result.SHA, result.Status)
		return result, http.StatusOK, nil

	default:
		return nil, http.StatusNotFound, &Error{Code: CodeUnknownVerb, Verb: verb, Message: fmt.Sprintf("workspace: no verb %q", verb)}
	}
}

// exec runs a command to completion and returns what it produced. The socket
// is request/response, so a session sees a command's output when it ends;
// streaming is the harness adapter's business, not this wire's.
func (s *Server) exec(ctx context.Context, request ExecRequest) (ExecResponse, error) {
	process, err := s.verbs.Exec(ctx, workspace.Command{
		Path: request.Path,
		Args: request.Args,
		Dir:  request.Dir,
	})
	if err != nil {
		return ExecResponse{}, err
	}
	if request.Stdin != "" {
		if _, err := io.WriteString(process.Stdin, request.Stdin); err != nil {
			return ExecResponse{}, fmt.Errorf("workspace: write stdin: %w", err)
		}
	}
	_ = process.Stdin.Close()

	var stdout, stderr []byte
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		if stderr, err = io.ReadAll(process.Stderr); err != nil {
			readErr = err
		}
	}()
	if stdout, err = io.ReadAll(process.Stdout); err != nil {
		readErr = err
	}
	<-done
	result := process.Wait()
	if readErr != nil {
		return ExecResponse{}, fmt.Errorf("workspace: read output: %w", readErr)
	}
	return ExecResponse{
		Status: result.Status,
		Signal: result.Signal,
		Stdout: string(stdout),
		Stderr: string(stderr),
	}, nil
}

// logf writes one summary line through the redactor. Full tool output is
// never logged; a summary line per result is (docs/architecture.md
// §Observability).
func (s *Server) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.log == nil {
		return
	}
	_, _ = fmt.Fprintf(s.log, format+"\n", args...)
}

// summarize is a result's first line, short enough for a log.
func summarize(output string) string {
	summary := output
	if index := strings.IndexByte(summary, '\n'); index >= 0 {
		summary = summary[:index]
	}
	const limit = 120
	if len(summary) > limit {
		summary = summary[:limit] + "…"
	}
	return summary
}

func bearerOf(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if scheme, value, found := strings.Cut(header, " "); found && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(value)
	}
	return ""
}

func decode(body []byte, into any) *Error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return &Error{Code: CodeInvalid, Message: "workspace: request body: " + err.Error()}
	}
	return nil
}

func failure(verb string, err error) *Error {
	code := CodeFailed
	switch {
	case errors.Is(err, workspace.ErrNotImplemented):
		code = CodeNotImplemented
	case errors.Is(err, workspace.ErrOutsideTree), errors.Is(err, os.ErrNotExist):
		code = CodeInvalid
	case errors.Is(err, workspace.ErrNoForge):
		code = CodeNotImplemented
	}
	refusal := &Error{Code: code, Verb: verb, Message: err.Error()}
	var capability *workspace.CapabilityError
	if errors.As(err, &capability) {
		refusal.Code = CodeForbidden
		refusal.Capability = capability.Missing
	}
	return refusal
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, workspace.ErrNotImplemented), errors.Is(err, workspace.ErrNoForge):
		return http.StatusNotImplemented
	case errors.Is(err, workspace.ErrOutsideTree), errors.Is(err, os.ErrNotExist):
		return http.StatusBadRequest
	}
	var capability *workspace.CapabilityError
	if errors.As(err, &capability) {
		return http.StatusForbidden
	}
	return http.StatusUnprocessableEntity
}

func writeError(w http.ResponseWriter, status int, refusal *Error) {
	writeJSON(w, status, map[string]*Error{"error": refusal})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
