package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// Usage is what one response billed, as the upstream's `usage` block says.
// Counts, never currency, in the ledger's shape.
type Usage struct {
	// Model is the model the upstream answered as, which is the resolved
	// id rather than the alias the request named.
	Model string
	// Input, Output, CacheRead and CacheWrite are the block's four counts.
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Tokens is every token the response billed, which is what a step's budget
// is counted in.
func (u Usage) Tokens() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// usageBlock is the upstream's `usage` object, in both the message and the
// stream's message_start and message_delta events. Fields it does not
// carry on a given event decode as nil and leave the count alone.
type usageBlock struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// apply overlays the block's counts. A later block is cumulative — the
// stream's message_delta carries the final counts — so what it names wins.
func (b usageBlock) apply(u *Usage) {
	if b.InputTokens != nil {
		u.Input = *b.InputTokens
	}
	if b.OutputTokens != nil {
		u.Output = *b.OutputTokens
	}
	if b.CacheReadInputTokens != nil {
		u.CacheRead = *b.CacheReadInputTokens
	}
	if b.CacheCreationInputTokens != nil {
		u.CacheWrite = *b.CacheCreationInputTokens
	}
}

// message is the non-streamed response, or the stream's message_start
// envelope, reduced to what the gateway reads.
type message struct {
	Model string      `json:"model"`
	Usage *usageBlock `json:"usage"`
}

// event is one streamed event's data, reduced to what the gateway reads.
type event struct {
	Type    string      `json:"type"`
	Message *message    `json:"message"`
	Usage   *usageBlock `json:"usage"`
}

// maxBuffered bounds what the observer holds of a non-streamed body before
// giving up on parsing it. A Messages response is kilobytes; anything past
// this is not one the gateway understands, and the client gets the bytes
// regardless.
const maxBuffered = 8 << 20

// observer wraps an upstream response body: every byte goes to the client
// unchanged, and the observer reads the usage off them as they pass. It is
// two readers in one — a line scanner for `text/event-stream`, a buffer for
// a JSON document — and it reports once, at the end of the body or when the
// client abandons it, whichever comes first.
type observer struct {
	body      io.ReadCloser
	streaming bool
	done      func(Usage, bool)

	once    sync.Once
	usage   Usage
	seen    bool
	pending bytes.Buffer
	dropped bool
}

// newObserver observes body, calling done exactly once with what it saw and
// whether it saw a usage block at all.
func newObserver(body io.ReadCloser, streaming bool, done func(Usage, bool)) *observer {
	return &observer{body: body, streaming: streaming, done: done}
}

func (o *observer) Read(p []byte) (int, error) {
	n, err := o.body.Read(p)
	if n > 0 {
		o.observe(p[:n])
	}
	if err == io.EOF {
		o.finish()
	}
	return n, err
}

// Close closes the upstream body and reports what was seen. A client that
// abandons a stream midway — the CLI's parallel title request does — lands
// here without an EOF, and what the stream had said by then is what is
// recorded.
func (o *observer) Close() error {
	o.finish()
	return o.body.Close()
}

func (o *observer) observe(chunk []byte) {
	if o.dropped {
		return
	}
	if o.pending.Len()+len(chunk) > maxBuffered {
		o.dropped = true
		o.pending.Reset()
		return
	}
	o.pending.Write(chunk)
	if !o.streaming {
		return
	}
	for {
		line, err := o.pending.ReadString('\n')
		if err != nil {
			// A partial line: put it back and wait for the rest.
			o.pending.Reset()
			o.pending.WriteString(line)
			return
		}
		o.line(strings.TrimRight(line, "\r\n"))
	}
}

// line reads one SSE line. Only `data:` lines carry anything, and only the
// message_start and message_delta events carry usage.
func (o *observer) line(line string) {
	data, found := strings.CutPrefix(line, "data:")
	if !found {
		return
	}
	var ev event
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			o.usage.Model = ev.Message.Model
			if ev.Message.Usage != nil {
				ev.Message.Usage.apply(&o.usage)
				o.seen = true
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			ev.Usage.apply(&o.usage)
			o.seen = true
		}
	}
}

func (o *observer) finish() {
	o.once.Do(func() {
		if !o.streaming && !o.dropped {
			var m message
			if err := json.Unmarshal(o.pending.Bytes(), &m); err == nil && m.Usage != nil {
				o.usage.Model = m.Model
				m.Usage.apply(&o.usage)
				o.seen = true
			}
		}
		o.done(o.usage, o.seen)
	})
}
