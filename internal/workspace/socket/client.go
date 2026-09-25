package socket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/workspace"
)

// Client is a caller of the workspace socket: the harness adapter, a check
// the worker runs, or a spec. It holds a socket path and a bearer, which is
// everything a step is given.
type Client struct {
	http   *http.Client
	bearer string
}

// Dial returns a client for the socket at path, presenting bearer on every
// request. Nothing is connected until a call is made.
func Dial(path, bearer string) *Client {
	return &Client{
		bearer: bearer,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// Call posts a request to a verb and decodes its response. A refusal comes
// back as *[Error], so a caller can read the code and the missing capability
// with errors.As.
func (c *Client) Call(ctx context.Context, verb string, request, response any) error {
	var body io.Reader = http.NoBody
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("socket: encode %s: %w", verb, err)
		}
		body = bytes.NewReader(encoded)
	}
	// The host is ignored: the transport dials the socket whatever it says.
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://workspace/v1/"+verb, body)
	if err != nil {
		return fmt.Errorf("socket: %s: %w", verb, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("socket: %s: %w", verb, err)
	}
	defer func() { _ = httpResponse.Body.Close() }()

	payload, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return fmt.Errorf("socket: %s: %w", verb, err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		var refusal struct {
			Error *Error `json:"error"`
		}
		if err := json.Unmarshal(payload, &refusal); err != nil || refusal.Error == nil {
			return &Error{Code: CodeFailed, Verb: verb, Message: fmt.Sprintf("socket: %s: %s", verb, httpResponse.Status)}
		}
		return refusal.Error
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(payload, response); err != nil {
		return fmt.Errorf("socket: decode %s: %w", verb, err)
	}
	return nil
}

// Read reads a file in the checkout.
func (c *Client) Read(ctx context.Context, path string) ([]byte, error) {
	var response ReadResponse
	err := c.Call(ctx, VerbFSRead, PathRequest{Path: path}, &response)
	return response.Content, err
}

// Write writes a file in the checkout.
func (c *Client) Write(ctx context.Context, path string, content []byte) error {
	return c.Call(ctx, VerbFSWrite, WriteRequest{Path: path, Content: content}, nil)
}

// List lists a directory in the checkout.
func (c *Client) List(ctx context.Context, path string) ([]workspace.Entry, error) {
	var response ListResponse
	err := c.Call(ctx, VerbFSList, PathRequest{Path: path}, &response)
	return response.Entries, err
}

// Exec runs a command to completion in the checkout.
func (c *Client) Exec(ctx context.Context, request ExecRequest) (ExecResponse, error) {
	var response ExecResponse
	err := c.Call(ctx, VerbExec, request, &response)
	return response, err
}

// GitStatus reports the checkout's branch and changed paths.
func (c *Client) GitStatus(ctx context.Context) (workspace.Status, error) {
	var response workspace.Status
	err := c.Call(ctx, VerbGitStatus, struct{}{}, &response)
	return response, err
}

// GitDiff renders a unified diff.
func (c *Client) GitDiff(ctx context.Context, request workspace.DiffRequest) (string, error) {
	var response DiffResponse
	err := c.Call(ctx, VerbGitDiff, request, &response)
	return response.Diff, err
}

// GitLog reads the run branch's history.
func (c *Client) GitLog(ctx context.Context, request workspace.LogRequest) ([]workspace.CommitInfo, error) {
	var response LogResponse
	err := c.Call(ctx, VerbGitLog, request, &response)
	return response.Commits, err
}

// GitCommit commits on the run's branch, signed.
func (c *Client) GitCommit(ctx context.Context, request workspace.CommitRequest) (workspace.CommitInfo, error) {
	var response workspace.CommitInfo
	err := c.Call(ctx, VerbGitCommit, request, &response)
	return response, err
}

// Push puts the run's branch on the forge.
func (c *Client) Push(ctx context.Context, request workspace.PushRequest) (workspace.PushResult, error) {
	var response workspace.PushResult
	err := c.Call(ctx, VerbGitPush, request, &response)
	return response, err
}

// PR is the run branch's pull request, opened if the forge has none.
func (c *Client) PR(ctx context.Context, request workspace.PRRequest) (forge.PullRequest, error) {
	var response forge.PullRequest
	err := c.Call(ctx, VerbForgePR, request, &response)
	return response, err
}

// Checks is what the forge's checks say about a commit.
func (c *Client) Checks(ctx context.Context, request workspace.ChecksRequest) (workspace.ChecksResult, error) {
	var response workspace.ChecksResult
	err := c.Call(ctx, VerbForgeChecks, request, &response)
	return response, err
}

// Redact registers values the workspace must scrub from its logs.
func (c *Client) Redact(ctx context.Context, values ...string) error {
	return c.Call(ctx, VerbRedact, RedactRequest{Values: values}, nil)
}

// ReportInstall reports packages the `dnf` shim installed into the machine.
func (c *Client) ReportInstall(ctx context.Context, packages ...string) error {
	return c.Call(ctx, VerbReportInstall, ReportInstallRequest{Packages: packages}, nil)
}
