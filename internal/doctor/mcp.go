package doctor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"syscall"

	"github.com/ivy/mill/internal/workspace"
)

// mcpMessage is one line of the newline-delimited JSON-RPC a stdio MCP
// server answers on — the framing the SDK's own stdio transport uses
// (spikes/mcp/workspace-server.ts's LineTransport frames a socket the same
// way).
type mcpMessage struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Mcp starts a stdio MCP server, sends it initialize and tools/list, and
// stops it. Nothing short of the exchange proves it runs: the binary may be
// missing, it may exit before answering, or it may never answer at all.
//
// name is only for the reported line — the server is not told it, the way
// [Sidecar]'s entrypoint names it in the line without the sidecar being
// asked its own name.
func Mcp(ctx context.Context, name, command string, args []string, env map[string]string) Result {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	process, err := workspace.Spawner{}.Exec(ctx, workspace.Command{Path: command, Args: args, Env: env})
	if err != nil {
		return fail("%s did not start: %v", command, err)
	}
	defer func() {
		_ = process.Signal(syscall.SIGKILL)
		process.Wait()
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(process.Stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	read := func() (mcpMessage, error) {
		select {
		case line, ok := <-lines:
			if !ok {
				return mcpMessage{}, fmt.Errorf("exited with no answer")
			}
			var msg mcpMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				return mcpMessage{}, fmt.Errorf("unreadable response: %w", err)
			}
			if msg.Error != nil {
				return mcpMessage{}, fmt.Errorf("refused: %s", msg.Error.Message)
			}
			return msg, nil
		case <-ctx.Done():
			return mcpMessage{}, ctx.Err()
		}
	}

	if err := mcpRequest(process, 1, "initialize"); err != nil {
		return fail("%s: %v", command, err)
	}
	if _, err := read(); err != nil {
		return fail("%s did not answer initialize: %v", command, err)
	}
	if err := mcpRequest(process, 2, "tools/list"); err != nil {
		return fail("%s: %v", command, err)
	}
	msg, err := read()
	if err != nil {
		return fail("%s did not answer tools/list: %v", command, err)
	}
	var tools struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(msg.Result, &tools); err != nil {
		return fail("%s: unexpected tools/list response: %v", command, err)
	}
	return pass("%s: %d tools", name, len(tools.Tools))
}

// mcpRequest writes one newline-delimited JSON-RPC request to a mounted
// server's stdin.
func mcpRequest(process *workspace.Process, id int, method string) error {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
	if err != nil {
		return err
	}
	_, err = process.Stdin.Write(append(line, '\n'))
	return err
}
