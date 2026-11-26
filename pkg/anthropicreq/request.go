// SPDX-License-Identifier: Apache-2.0

// Package anthropicreq decodes one Anthropic Messages request body ONCE and
// answers, from that one decode, every question rafiki's proxy and capture
// paths ask of it.
//
// A request body carries the whole conversation, so it is the largest thing the
// proxy handles. Each consumer used to decode it independently — routing's
// prefix hash, the three Claude Code thread signals, capture's decomposition
// and its cache-breakpoint scan, the stream flag, and three rewrites — so one
// proxied turn parsed the same megabytes about nine times. Parse performs the
// single top-level decode, and each accessor lazily decodes just the key it
// needs, at most once per Request.
//
// Nothing here decides what a request MEANS beyond restating rules that already
// had exactly one owner; the billing type and its header parser stay in
// pkg/claudethread and are imported, not re-implemented. The package is
// stdlib-only apart from that.
package anthropicreq

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"go.graveland.dev/rafiki/pkg/claudethread"
)

// DecodeCount counts the full-body decodes this package performs: a test hook
// for "the proxy parses each body once". Top counts Parse's single top-level
// decode; Messages counts a messages[] array decode (the strict one, plus the
// content-only fallback EnvelopeAndBreakpoints takes when the strict one fails).
// Per-key value decodes are not counted — they are bounded by one key, not by
// the size of the conversation.
var DecodeCount = struct {
	Top      atomic.Int64
	Messages atomic.Int64
}{}

// Message is one entry of a request's messages[] array, its content left as the
// raw JSON the caller will store or forward.
type Message struct {
	Role    string
	Content json.RawMessage
}

// Request is one Anthropic Messages request body, decoded once.
//
// Every accessor is safe for concurrent use and computed at most once per
// Request: they are pure functions of the body, which no method alters.
type Request struct {
	raw    []byte
	fields map[string]json.RawMessage

	// parseErr is the top-level decode's failure. A body that does not decode is
	// still a Request (see Parse): rafiki forwards such a body verbatim, so
	// there has to be something to thread through the forward path, and every
	// accessor answers the same zero result the pre-parse helpers answered.
	parseErr error

	stream      lazy[bool]
	model       lazy[string]
	prevMsgID   lazy[string]
	billing     lazy[billingResult]
	clientTools lazy[bool]
	messages    lazy[messagesResult]
	envelope    lazy[envelopeResult]
	prefix      lazy[string]
}

type billingResult struct {
	billing claudethread.Billing
	ok      bool
}

type messagesResult struct {
	msgs []Message
	err  error
}

type envelopeResult struct {
	envelope    []byte
	breakpoints []byte
	err         error
}

// lazy is a value computed at most once, on first use.
type lazy[T any] struct {
	once sync.Once
	val  T
}

func (l *lazy[T]) get(compute func() T) T {
	l.once.Do(func() { l.val = compute() })
	return l.val
}

// Parse decodes body once.
//
// A body that does not decode as a JSON object returns the decode error AND a
// usable *Request: Raw answers body verbatim, every parse-derived accessor
// answers its zero result, and Messages/EnvelopeAndBreakpoints report the
// failure. That is deliberate — a body rafiki cannot parse is still forwarded
// upstream, which is the authority on what it will accept — so a caller always
// has something to thread through the forwarding path; the error is returned so
// it can say why nothing was derived.
func Parse(body []byte) (*Request, error) {
	DecodeCount.Top.Add(1)
	r := &Request{raw: body}
	if err := json.Unmarshal(body, &r.fields); err != nil {
		// A partial decode would leave half a body in the map; the body is
		// treated as unreadable in full, exactly as the whole-body decode each
		// helper used to do.
		r.fields = nil
		r.parseErr = err
		return r, err
	}
	return r, nil
}

// Raw returns the exact bytes this Request carries: the bytes to forward
// upstream and to capture. After WithFields it is the rewritten body, never the
// inbound one.
func (r *Request) Raw() []byte { return r.raw }

// Parsed reports whether the body decoded as a JSON object. A Request that did
// not still forwards Raw() verbatim and answers every parse-derived accessor
// with its zero result, but a caller that would REWRITE the body must leave it
// alone: nothing was resolved from it, so nothing can be re-encoded correctly.
func (r *Request) Parsed() bool { return r.parseErr == nil }

// Field returns the raw JSON of one top-level key, nil when the body does not
// carry it.
func (r *Request) Field(key string) json.RawMessage { return r.fields[key] }

// Fields returns a copy of the top-level map, for a rewrite that edits several
// keys and hands the result back through WithFields (see resolveAndAdapt). The
// values are shared and treated as immutable.
//
// A body that decoded as JSON null (no object) yields an empty map rather than
// a nil one: a rewrite cannot edit what is not an object, and an assignment to
// a nil map would panic.
func (r *Request) Fields() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(r.fields))
	for k, v := range r.fields {
		out[k] = v
	}
	return out
}

// WithFields returns a NEW Request carrying fields marshalled into their new
// bytes: this is how a rewrite (resolveAndAdapt, clampEffortBody, doOpenRouter)
// produces the body actually sent upstream. The new Request's lazy caches are
// empty, so every later accessor reads the FORWARDED bytes — exactly what the
// per-field helpers did when they read the rewritten body before this package
// existed.
//
// The map is copied: a caller may keep mutating its own map afterwards without
// changing this Request.
func (r *Request) WithFields(fields map[string]json.RawMessage) (*Request, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	out := &Request{raw: raw}
	if fields != nil {
		out.fields = make(map[string]json.RawMessage, len(fields))
		for k, v := range fields {
			out.fields[k] = v
		}
	}
	return out, nil
}

// Stream reports whether the request body asked for a streamed (SSE) response,
// which determines what Content-Type a well-formed 2xx must carry.
func (r *Request) Stream() bool {
	return r.stream.get(func() bool {
		var s bool
		_ = json.Unmarshal(r.fields["stream"], &s)
		return s
	})
}

// Model returns the model the client asked for, "" when it named none (an
// absent key, a non-string value, or a body that does not decode).
func (r *Request) Model() string {
	return r.model.get(func() string {
		var m string
		_ = json.Unmarshal(r.fields["model"], &m)
		return m
	})
}

// PreviousMessageID returns diagnostics.previous_message_id, the id of the
// assistant message this request's thread last received, or "" when absent.
func (r *Request) PreviousMessageID() string {
	return r.prevMsgID.get(func() string {
		return claudethread.PreviousMessageIDFrom(r.fields["diagnostics"])
	})
}

// Billing returns the parsed x-anthropic-billing-header line from system block
// 0. Every shape other than "an array whose first element has a text field"
// answers ok=false rather than failing.
func (r *Request) Billing() (claudethread.Billing, bool) {
	res := r.billing.get(func() billingResult {
		b, ok := claudethread.BillingFromSystem(r.fields["system"])
		return billingResult{billing: b, ok: ok}
	})
	return res.billing, res.ok
}

// DeclaresClientTools reports whether the request declares at least one tool
// the CLIENT must execute (an entry carrying an input_schema). A literal null
// input_schema is not one.
func (r *Request) DeclaresClientTools() bool {
	return r.clientTools.get(func() bool {
		return claudethread.DeclaresClientToolsFrom(r.fields["tools"])
	})
}

// Messages returns the request's messages[] with each content left as raw JSON.
//
// An absent or null messages key is no messages (nil, no error). A messages
// value that is not an array of objects is an error wrapping "decompose:
// unmarshal request:" — that prefix is capture's own error vocabulary, restated
// here so an operator's log line is unchanged.
func (r *Request) Messages() ([]Message, error) {
	res := r.messages.get(func() messagesResult {
		if r.parseErr != nil {
			return messagesResult{err: fmt.Errorf("decompose: unmarshal request: %w", r.parseErr)}
		}
		raw, ok := r.fields["messages"]
		if !ok {
			return messagesResult{}
		}
		DecodeCount.Messages.Add(1)
		var msgs []Message
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return messagesResult{err: fmt.Errorf("decompose: unmarshal request: %w", err)}
		}
		return messagesResult{msgs: msgs}
	})
	return res.msgs, res.err
}

// EnvelopeAndBreakpoints returns (request-minus-messages JSONB, cache_control
// breakpoint ordinals JSONB). Breakpoints are the indices of messages carrying a
// cache_control marker, as a JSON array; envelope is the request with "messages"
// removed.
//
// An unparseable body is an error wrapping "split envelope: unmarshal request:".
func (r *Request) EnvelopeAndBreakpoints() (envelope, breakpoints []byte, err error) {
	res := r.envelope.get(r.envelopeAndBreakpoints)
	return res.envelope, res.breakpoints, res.err
}

func (r *Request) envelopeAndBreakpoints() envelopeResult {
	if r.parseErr != nil {
		return envelopeResult{err: fmt.Errorf("split envelope: unmarshal request: %w", r.parseErr)}
	}
	idxs := []int{}
	if msgs, merr := r.Messages(); merr == nil {
		for i, m := range msgs {
			if messageHasCacheControl(m.Content) {
				idxs = append(idxs, i)
			}
		}
	} else if raw, ok := r.fields["messages"]; ok {
		// The strict decode failed: a message whose role is not a string. The
		// splitter never required one — it read only each message's content — so
		// fall back to that shape rather than change what breakpoints a turn
		// records. Only a body the Messages accessor refuses gets here, which is
		// a body capture fails on anyway.
		DecodeCount.Messages.Add(1)
		var tolerant []struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &tolerant); err == nil {
			for i, m := range tolerant {
				if messageHasCacheControl(m.Content) {
					idxs = append(idxs, i)
				}
			}
		}
	}
	// A body that decoded as JSON null keeps the null envelope: the pre-parse
	// splitter marshalled the nil map it was handed.
	var m map[string]json.RawMessage
	if r.fields != nil {
		m = make(map[string]json.RawMessage, len(r.fields))
		for k, v := range r.fields {
			if k == "messages" {
				continue
			}
			m[k] = v
		}
	}
	envelope, err := json.Marshal(m)
	if err != nil {
		return envelopeResult{err: fmt.Errorf("split envelope: marshal envelope: %w", err)}
	}
	breakpoints, err := json.Marshal(idxs)
	if err != nil {
		return envelopeResult{err: fmt.Errorf("split envelope: marshal breakpoints: %w", err)}
	}
	return envelopeResult{envelope: envelope, breakpoints: breakpoints}
}

// PrefixHash returns the sha256 of the request's static cache-prefix: the
// request with the volatile "messages" list removed and keys canonicalized (Go
// marshals map keys in sorted order). The same model + system prompt + tools
// therefore hashes identically across a conversation's turns.
//
// It is stored on every turn and compared turn to turn to flag a cache break, so
// its output must stay byte-identical to routing.PrefixHash's: every non-messages
// value is decoded through `any` before the canonical marshal, which is what
// rounds integers past 2^53 and re-spaces nested JSON the way a whole-body
// round trip used to. Returns "" when the request could not be parsed (the column
// is nullable; a turn with no prefix hash simply never flags a break).
func (r *Request) PrefixHash() string {
	return r.prefix.get(func() string {
		if r.parseErr != nil {
			return ""
		}
		var m map[string]any
		if r.fields != nil {
			m = make(map[string]any, len(r.fields))
		}
		for k, raw := range r.fields {
			if k == "messages" {
				continue
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return ""
			}
			m[k] = v
		}
		canonical, err := json.Marshal(m)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(canonical)
		return hex.EncodeToString(sum[:])
	})
}

// messageHasCacheControl reports whether a message's content carries a
// cache_control field on any content block. It decodes content as an array of
// blocks (the shape cache_control actually appears in) and inspects each
// block's cache_control field directly, so a message whose text merely
// contains the literal substring "cache_control" is not a false positive.
// Plain-string content carries no cache_control block, so it is always false —
// even when the string text embeds the literal token. Only a genuinely
// unexpected shape falls back to a conservative substring check.
func messageHasCacheControl(content json.RawMessage) bool {
	var blocks []struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			if len(b.CacheControl) > 0 {
				return true
			}
		}
		return false
	}
	// A plain string message has no per-block cache_control; decoding it as a
	// string (rather than substring-matching) avoids flagging text that merely
	// mentions the token.
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return false
	}
	// Genuinely unexpected shape — conservative substring check so capture never fails.
	return bytes.Contains(content, []byte(`"cache_control"`))
}
