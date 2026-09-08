// Package redact scrubs secrets from everything mill logs.
//
// A redactor sits in front of every log the mill writes
// (docs/architecture.md §"Identity and credentials"): seeded from the step's
// credentials before they touch any environment, extended at runtime by the
// `redact` verb a check or a session calls, and flushed at step boundaries.
//
// It is line-oriented on purpose. A secret split across two Write calls is
// still one secret on one line, so [Redactor.Writer] holds a partial line
// until its newline arrives and scrubs the whole line at once.
package redact

import (
	"bytes"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Mask is what a registered value is replaced by.
const Mask = "[redacted]"

// minLength is the shortest value worth registering. A one or two character
// secret would mask most of a log line and protect nothing, and the values
// mill registers — bearers, tokens, keys — are long.
const minLength = 4

// Redactor holds the values to scrub. The zero value is usable and empty; it
// is safe for concurrent use, because the `redact` verb arrives on the socket
// while a step's output is being written.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// New returns a redactor seeded with values.
func New(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers values to scrub from here on. Values shorter than four
// characters and values already registered are ignored.
func (r *Redactor) Add(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		if len(value) < minLength {
			continue
		}
		if slices.Contains(r.values, value) {
			continue
		}
		r.values = append(r.values, value)
	}
	// Longest first, so a value that contains another is masked whole rather
	// than left as a recognisable remainder.
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// Values is what is registered, longest first.
func (r *Redactor) Values() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.values...)
}

// String scrubs every registered value out of s.
func (r *Redactor) String(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, value := range r.values {
		s = strings.ReplaceAll(s, value, Mask)
	}
	return s
}

// Bytes scrubs every registered value out of b, returning a new slice.
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil || len(b) == 0 {
		return b
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, value := range r.values {
		b = bytes.ReplaceAll(b, []byte(value), []byte(Mask))
	}
	return b
}

// Writer wraps w so that every complete line written through it is scrubbed
// first. The returned writer buffers a partial line; Close flushes it.
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	return &writer{redactor: r, out: w}
}

type writer struct {
	redactor *Redactor
	out      io.Writer

	mu      sync.Mutex
	partial []byte
}

// Write scrubs and forwards every complete line, holding back whatever
// follows the last newline until the rest of that line arrives.
func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.partial = append(w.partial, p...)
	end := bytes.LastIndexByte(w.partial, '\n')
	if end < 0 {
		return len(p), nil
	}
	line := w.partial[:end+1]
	w.partial = append([]byte(nil), w.partial[end+1:]...)
	if _, err := w.out.Write(w.redactor.Bytes(line)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes a trailing line that never got its newline.
func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) == 0 {
		return nil
	}
	line := w.partial
	w.partial = nil
	_, err := w.out.Write(w.redactor.Bytes(line))
	return err
}
