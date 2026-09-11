package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/workspace"
)

// The specs drive the adapter against stub sidecars: shell scripts that
// write the wire's own lines, or refuse to, or refuse to die. What is under
// test is the adapter's half of the contract — framing, cancellation timing,
// and the terminal record it hands back — and a stub says exactly what a
// scenario needs, at a timing the scenario chooses.

// stub writes a shell script into a temporary directory and returns its
// path, with placeholders replaced.
func stub(body string, replacements ...string) string {
	GinkgoHelper()

	dir := GinkgoT().TempDir()
	path := filepath.Join(dir, "sidecar.sh")
	script := strings.NewReplacer(replacements...).Replace(body)
	Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700)).To(Succeed()) //nolint:gosec // a spec's own scratch script
	return path
}

// scratch is a path in a temporary directory a stub can write to.
func scratch(name string) string {
	GinkgoHelper()

	return filepath.Join(GinkgoT().TempDir(), name)
}

// recorder collects a turn's events as the worker's step log would, and
// notes whether the turn had already ended when each arrived.
type recorder struct {
	mu     sync.Mutex
	events []*harnessv1.Event
	turn   *harness.Turn
	late   int
}

func (r *recorder) sink() harness.Sink {
	return func(event *harnessv1.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.turn != nil {
			select {
			case <-r.turn.Done():
				r.late++
			default:
			}
		}
		r.events = append(r.events, event)
	}
}

// watch tells the recorder which turn to compare arrivals against.
func (r *recorder) watch(turn *harness.Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn = turn
}

func (r *recorder) collected() []*harnessv1.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*harnessv1.Event{}, r.events...)
}

// afterTerminal is how many events arrived after the turn had already ended.
func (r *recorder) afterTerminal() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.late
}

// watching records the commands the adapter asked the workspace to spawn,
// and spawns them for real.
type watching struct {
	workspace.Execer

	mu        sync.Mutex
	commands  []workspace.Command
	processes []*workspace.Process
	last      *workspace.Process
}

func (w *watching) Exec(ctx context.Context, command workspace.Command) (*workspace.Process, error) {
	process, err := w.Execer.Exec(ctx, command)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commands = append(w.commands, command)
	if err == nil {
		w.processes = append(w.processes, process)
		w.last = process
	}
	return process, err
}

// pids are the process groups of every sidecar the adapter spawned, in
// order.
func (w *watching) pids() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	pids := make([]int, 0, len(w.processes))
	for _, process := range w.processes {
		pids = append(pids, process.Pid())
	}
	return pids
}

func (w *watching) spawned() []workspace.Command {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]workspace.Command{}, w.commands...)
}

// pid is the process group the adapter last spawned.
func (w *watching) pid() int {
	GinkgoHelper()

	w.mu.Lock()
	defer w.mu.Unlock()
	Expect(w.last).NotTo(BeNil(), "the adapter spawned nothing")
	return w.last.Pid()
}

// gone reports whether a process — or, given a negative pid, a whole process
// group — has no members left.
func gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// readPid reads a pid a stub wrote to a file.
func readPid(path string) int {
	GinkgoHelper()

	raw, err := os.ReadFile(path) //nolint:gosec // a spec's own scratch file
	Expect(err).NotTo(HaveOccurred())
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	Expect(err).NotTo(HaveOccurred())
	return pid
}
