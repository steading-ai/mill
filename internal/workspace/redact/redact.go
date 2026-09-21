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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

// longest is the length of the longest registered value: values[0], since
// Add keeps the slice sorted longest first.
func (r *Redactor) longest() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.values) == 0 {
		return 0
	}
	return len(r.values[0])
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
	return &writer{out: w, chunker: NewChunker(r)}
}

type writer struct {
	out io.Writer

	mu      sync.Mutex
	chunker *Chunker
}

// Write scrubs and forwards every complete line, holding back whatever
// follows the last newline until the rest of that line arrives.
func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	line := w.chunker.Write(p)
	if line == nil {
		return len(p), nil
	}
	if _, err := w.out.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes a trailing line that never got its newline.
func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := w.chunker.Close()
	if line == nil {
		return nil
	}
	_, err := w.out.Write(line)
	return err
}

// Message walks m's string fields — including a nested
// google.protobuf.Struct or Value, which proto reflection sees as ordinary
// message and map fields — and replaces each with the redactor's scrub of
// it. It mutates m in place, so a body built once and about to be written
// is redacted whole rather than a hand-kept list of which of its fields can
// carry a secret.
func (r *Redactor) Message(m proto.Message) {
	if r == nil || m == nil {
		return
	}
	redactMessage(r, m.ProtoReflect())
}

// redactMessage is [Redactor.Message]'s recursion. Every proto3 message,
// including google.protobuf.Struct and Value, is walked the same way:
// string fields are scrubbed, message fields (and the message elements of a
// list or a map) are recursed into.
func redactMessage(r *Redactor, m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			redactMap(r, fd, v.Map())
		case fd.IsList():
			redactList(r, fd, v.List())
		case fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(r.String(v.String())))
		case fd.Kind() == protoreflect.MessageKind:
			redactMessage(r, v.Message())
		}
		return true
	})
}

func redactMap(r *Redactor, fd protoreflect.FieldDescriptor, m protoreflect.Map) {
	switch fd.MapValue().Kind() {
	case protoreflect.StringKind:
		m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
			m.Set(k, protoreflect.ValueOfString(r.String(v.String())))
			return true
		})
	case protoreflect.MessageKind:
		m.Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
			redactMessage(r, v.Message())
			return true
		})
	}
}

func redactList(r *Redactor, fd protoreflect.FieldDescriptor, l protoreflect.List) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		for i := 0; i < l.Len(); i++ {
			l.Set(i, protoreflect.ValueOfString(r.String(l.Get(i).String())))
		}
	case protoreflect.MessageKind:
		for i := 0; i < l.Len(); i++ {
			redactMessage(r, l.Get(i).Message())
		}
	}
}

// Chunker holds back one stream's incomplete trailing line, so a value
// split across two reads is scrubbed whole rather than missed by both
// halves — [Redactor.Writer]'s rule, restated for a caller that wants
// discrete redacted chunks back instead of an io.Writer, such as a check or
// a verb streaming its output to the activity record. One Chunker is kept
// per stream (`stdout`, `stderr`, or a verb's own key).
type Chunker struct {
	redactor *Redactor
	partial  []byte
}

// NewChunker returns a Chunker that scrubs against r.
func NewChunker(r *Redactor) *Chunker {
	return &Chunker{redactor: r}
}

// Write returns the complete, redacted lines newly available in data,
// holding back whatever follows the last newline until it arrives. It
// returns nil when data completed no line.
func (c *Chunker) Write(data []byte) []byte {
	c.partial = append(c.partial, data...)
	end := bytes.LastIndexByte(c.partial, '\n')
	if end < 0 {
		return nil
	}
	line := c.partial[:end+1]
	c.partial = append([]byte(nil), c.partial[end+1:]...)
	return c.redactor.Bytes(line)
}

// Close flushes a trailing chunk that never got its newline. It returns nil
// when nothing was held back.
func (c *Chunker) Close() []byte {
	if len(c.partial) == 0 {
		return nil
	}
	line := c.partial
	c.partial = nil
	return c.redactor.Bytes(line)
}

// DeltaHoldback redacts a stream of text deltas — `text` and `thinking`
// blocks, which have no newline to hold back on — by keeping the last L−1
// bytes of each block's accumulated text unreleased, L the longest
// currently-registered value ([Redactor.Values] already exposes lengths),
// and releasing them once the next delta or `final` has enough bytes to
// prove no registered value spans the boundary.
type DeltaHoldback struct {
	redactor *Redactor

	mu   sync.Mutex
	held map[string]string
}

// NewDeltaHoldback returns a DeltaHoldback that scrubs against r.
func NewDeltaHoldback(r *Redactor) *DeltaHoldback {
	return &DeltaHoldback{redactor: r, held: map[string]string{}}
}

// Add appends delta to blockID's accumulated text and returns the redacted
// text ready to release. On final, everything held for blockID is released
// and the block is forgotten; otherwise the last `holdLength` bytes stay
// held back for the next call.
func (h *DeltaHoldback) Add(blockID, delta string, final bool) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	text := h.redactor.String(h.held[blockID] + delta)
	if final {
		delete(h.held, blockID)
		return text
	}
	hold := h.holdLength()
	if hold > 0 && len(text) <= hold {
		h.held[blockID] = text
		return ""
	}
	release := text[:len(text)-hold]
	h.held[blockID] = text[len(text)-hold:]
	return release
}

// holdLength is one byte short of the longest registered value: enough that
// a value ending at the boundary between one release and the next is never
// split across them undetected.
func (h *DeltaHoldback) holdLength() int {
	longest := h.redactor.longest()
	if longest <= 1 {
		return 0
	}
	return longest - 1
}
