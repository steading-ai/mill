package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/steading-ai/mill/internal/run"
)

// terminalAnswerer puts a gate's ask to the person running `mill run` and
// reads their answer from the terminal.
//
// This is the only place a gate is answered inside mill's own process, and
// it exists because `mill run` is a person watching one node go by: filing a
// gate and telling them to go and answer it elsewhere would be ceremony. The
// gate is still filed in dagger first, and the answer is still written to
// dagger as *their* act, not the mill's (docs/domain-model.md §Principal) —
// what this saves is the walk to another window, not the record.
//
// A worker never has one of these: `mill work` has nobody at its terminal,
// so it files the gate, releases the lease, and goes back to ClaimNext.
type terminalAnswerer struct {
	in  io.Reader
	out io.Writer
}

// Answer prints the ask and reads a yes or a no. Anything that is not an
// approval is a denial, including end of input: a gate nobody answered is
// not a gate that was approved.
func (t *terminalAnswerer) Answer(_ context.Context, ask run.Ask) (bool, error) {
	_, _ = fmt.Fprintf(t.out, "\n%s  %s\n\n%s\n\n", ask.GateRef, ask.Title, strings.TrimSpace(ask.Body))
	_, _ = fmt.Fprintf(t.out, "Approve %s? [y/N] ", ask.GateRef)
	line, err := bufio.NewReader(t.in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

var _ run.Answerer = (*terminalAnswerer)(nil)

// NewTerminalAnswerer is the person at a terminal, reading from in and
// writing to out. `mill run` builds one when it is not told to leave its
// gates for somebody elsewhere.
func NewTerminalAnswerer(in io.Reader, out io.Writer) run.Answerer {
	return &terminalAnswerer{in: in, out: out}
}
