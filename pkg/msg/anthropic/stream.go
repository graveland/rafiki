package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// maxStreamLine is the largest SSE line the decoder accepts. A provider that
// sends a single line bigger than this is not speaking the protocol any more,
// and buffering it would grow without bound.
const maxStreamLine = 16 << 20

// ErrStreamRead marks a failure to READ the underlying stream, as opposed to a
// malformed event. The distinction matters to a consumer that skips what it
// cannot fold in: a read error is STICKY — bufio.Scanner returns the same one on
// every Scan (a data line over maxStreamLine yields bufio.ErrTooLong forever) —
// so skipping it and calling Next again can never advance. Such a consumer must
// treat this error as terminal and persist what it accumulated. The wrapped
// cause is on the chain, so errors.Is(err, bufio.ErrTooLong) still holds.
var ErrStreamRead = errors.New("anthropic: read stream")

// DecodeResponse decodes a whole (non-streamed) Anthropic message body.
//
// The content blocks go through the same residue-aware decoder as stored
// history, so a caller, a citation, or a server-tool block survives exactly as
// it would from a stored row. Usage is decoded by PRESENCE: a count the body
// does not carry stays nil, because a provider that did not report a cache
// count has not reported zero of them. The stop reason keeps the wire's own
// spelling alongside the modelled enum, so a value this package does not model
// still reaches the record.
func DecodeResponse(body []byte) (*msg.Response, error) {
	var wire struct {
		ID         string                     `json:"id"`
		Model      string                     `json:"model"`
		StopReason string                     `json:"stop_reason"`
		Provider   json.RawMessage            `json:"provider"`
		Content    json.RawMessage            `json:"content"`
		Usage      map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	content, err := decodeContent(wire.Content)
	if err != nil {
		return nil, err
	}
	return &msg.Response{
		ID:            wire.ID,
		Model:         wire.Model,
		Content:       content,
		StopReason:    msg.ParseStopReason(wire.StopReason),
		StopReasonRaw: wire.StopReason,
		Usage:         decodeUsage(wire.Usage),
		Provider:      providerOf(wire.Provider),
	}, nil
}

// decodeUsage reads the token counts a body reports, keyed by presence.
func decodeUsage(raw map[string]json.RawMessage) msg.Usage {
	return msg.Usage{
		Input:      countOf(raw, "input_tokens"),
		Output:     countOf(raw, "output_tokens"),
		CacheRead:  countOf(raw, "cache_read_input_tokens"),
		CacheWrite: countOf(raw, "cache_creation_input_tokens"),
	}
}

// countOf returns the count under key, or nil when the key is absent or is not
// a number. A null count is not a report of zero.
func countOf(raw map[string]json.RawMessage, key string) *int64 {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var n int64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil
	}
	return msg.Ptr(n)
}

// providerOf reads OpenRouter's non-standard top-level "provider" field. It is
// "" for native Anthropic responses and for anything that is not a string.
func providerOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Decoder reads an Anthropic server-sent event stream as native events.
//
// It is the SSE face of this codec: every event the API defines is translated
// into the native event model, and the block JSON it rebuilds from a stream's
// fragments is decoded by the same residue-aware decoder the whole-body and
// stored-message paths use, so a streamed response and its recorded equivalent
// carry identical blocks.
type Decoder struct {
	r       io.ReadCloser
	sc      *bufio.Scanner
	blocks  map[int]*blockState
	invalid map[int]json.RawMessage
	done    bool
}

// NewStreamDecoder wraps r in a decoder. The caller owns r and Close releases
// it.
func NewStreamDecoder(r io.ReadCloser) msg.StreamDecoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	return &Decoder{
		r:       r,
		sc:      sc,
		blocks:  make(map[int]*blockState),
		invalid: make(map[int]json.RawMessage),
	}
}

// InvalidInput returns the raw, unparseable input fragments the decoder buffered
// for the block at index, if any. Every block kind that carries an input is
// covered: a client tool call's finalized input is left to the accumulator's
// repair policy, which substitutes an empty object, while an opaque server-tool
// block is decoded with an empty input of its own type. Either way the original
// bytes are retained here so a proxy can repair or report them.
func (d *Decoder) InvalidInput(index int) (json.RawMessage, bool) {
	raw, ok := d.invalid[index]
	return raw, ok
}

// Close releases the underlying stream.
func (d *Decoder) Close() error { return d.r.Close() }

// Next returns the next native event, or io.EOF once the stream's message_stop
// has been read and the body ends. A body that ends without message_stop —
// including one cut mid-event — is io.ErrUnexpectedEOF: a truncated stream is a
// failed response, not a short one.
func (d *Decoder) Next() (msg.StreamEvent, error) {
	for {
		ev, ok, err := d.next()
		if err != nil {
			if errors.Is(err, io.EOF) && !d.done {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if ok {
			return ev, nil
		}
	}
}

// next reads one SSE event and maps it. ok is false for an event that carries
// no native event of its own: a ping, a comment, a stop marker, a delta folded
// into the block being rebuilt.
func (d *Decoder) next() (msg.StreamEvent, bool, error) {
	name, data, err := d.readEvent()
	if err != nil {
		return nil, false, err
	}
	typ := name
	if typ == "" {
		typ = jsonType(data)
	}
	switch typ {
	case "ping":
		return nil, false, nil
	case "message_start":
		return d.messageStart(data)
	case "content_block_start":
		return d.contentBlockStart(data)
	case "content_block_delta":
		return d.contentBlockDelta(data)
	case "content_block_stop":
		return d.contentBlockStop(data)
	case "message_delta":
		return d.messageDelta(data)
	case "message_stop":
		d.done = true
		return nil, false, nil
	case "error":
		return nil, false, inBandError(data)
	default:
		// An event type this codec does not model carries nothing a caller
		// could act on; the API adds event types over time, so refusing one
		// would break forward compatibility for no gain. Content DELTAS are
		// the exception: dropping one would silently lose model output.
		return nil, false, nil
	}
}

// readEvent reads one SSE event: its event name and its data, which may be
// spread over several data lines. A blank line terminates it.
//
// An event is dispatched only once that blank line arrives, as the SSE spec
// says. A body that stops inside one — after the event name, or part way
// through a data line — was truncated, and reports io.ErrUnexpectedEOF rather
// than handing back a half-read event: a cut stream is a failed response, not a
// short one.
func (d *Decoder) readEvent() (string, []byte, error) {
	var (
		name string
		data []byte
	)
	for d.sc.Scan() {
		line := d.sc.Bytes()
		if len(line) == 0 {
			if name == "" && data == nil {
				continue
			}
			return name, data, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if err := d.sc.Err(); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrStreamRead, err)
	}
	if name != "" || data != nil {
		// The event never got its terminating blank line, so it never completed.
		// Discarding it is what the SSE spec asks for; reporting EOF here would
		// turn a truncated body into a clean end of stream.
		return "", nil, io.ErrUnexpectedEOF
	}
	return "", nil, io.EOF
}

// jsonType returns the "type" of a JSON object, or "".
func jsonType(data []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return ""
	}
	return head.Type
}

func (d *Decoder) messageStart(data []byte) (msg.StreamEvent, bool, error) {
	var wire struct {
		Message struct {
			ID       string                     `json:"id"`
			Model    string                     `json:"model"`
			Provider json.RawMessage            `json:"provider"`
			Usage    map[string]json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, fmt.Errorf("anthropic: decode message_start: %w", err)
	}
	return msg.MessageStart{
		ID:       wire.Message.ID,
		Model:    wire.Message.Model,
		Provider: providerOf(wire.Message.Provider),
		Usage:    decodeUsage(wire.Message.Usage),
	}, true, nil
}

func (d *Decoder) messageDelta(data []byte) (msg.StreamEvent, bool, error) {
	var wire struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, fmt.Errorf("anthropic: decode message_delta: %w", err)
	}
	return msg.MessageDelta{
		StopReason:    msg.ParseStopReason(wire.Delta.StopReason),
		StopReasonRaw: wire.Delta.StopReason,
		Usage:         decodeUsage(wire.Usage),
	}, true, nil
}

func (d *Decoder) contentBlockStart(data []byte) (msg.StreamEvent, bool, error) {
	var wire struct {
		Index        int             `json:"index"`
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, fmt.Errorf("anthropic: decode content_block_start: %w", err)
	}
	block, err := decodeBlock(wire.ContentBlock)
	if err != nil {
		return nil, false, err
	}
	state := &blockState{block: block, raw: make(map[string]json.RawMessage)}
	if err := json.Unmarshal(wire.ContentBlock, &state.raw); err != nil {
		return nil, false, fmt.Errorf("anthropic: read content_block_start block: %w", err)
	}
	d.blocks[wire.Index] = state
	return msg.BlockStart{Index: wire.Index, Block: block}, true, nil
}

func (d *Decoder) contentBlockDelta(data []byte) (msg.StreamEvent, bool, error) {
	var wire struct {
		Index int             `json:"index"`
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, fmt.Errorf("anthropic: decode content_block_delta: %w", err)
	}
	state := d.blocks[wire.Index]
	typ := jsonType(wire.Delta)
	switch typ {
	case "text_delta":
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(wire.Delta, &p); err != nil {
			return nil, false, fmt.Errorf("anthropic: decode text_delta: %w", err)
		}
		if state != nil {
			state.text += p.Text
			state.textSeen = true
		}
		return msg.BlockDelta{Index: wire.Index, Kind: msg.DeltaText, Text: p.Text}, true, nil
	case "thinking_delta":
		var p struct {
			Thinking string `json:"thinking"`
		}
		if err := json.Unmarshal(wire.Delta, &p); err != nil {
			return nil, false, fmt.Errorf("anthropic: decode thinking_delta: %w", err)
		}
		if state != nil {
			state.thinking += p.Thinking
			state.thinkingSeen = true
		}
		return msg.BlockDelta{Index: wire.Index, Kind: msg.DeltaThinking, Text: p.Thinking}, true, nil
	case "signature_delta":
		var p struct {
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(wire.Delta, &p); err != nil {
			return nil, false, fmt.Errorf("anthropic: decode signature_delta: %w", err)
		}
		if state != nil {
			state.signature += p.Signature
			state.signatureSeen = true
		}
		return msg.BlockDelta{Index: wire.Index, Kind: msg.DeltaSignature, Text: p.Signature}, true, nil
	case "input_json_delta":
		var p struct {
			PartialJSON string `json:"partial_json"`
		}
		if err := json.Unmarshal(wire.Delta, &p); err != nil {
			return nil, false, fmt.Errorf("anthropic: decode input_json_delta: %w", err)
		}
		if state == nil {
			return msg.BlockDelta{Index: wire.Index, Kind: msg.DeltaInputJSON, Text: p.PartialJSON}, true, nil
		}
		state.input += p.PartialJSON
		state.inputSeen = true
		if _, opaque := state.block.(msg.Opaque); opaque {
			// A server-tool block is opaque: its arguments are folded into the
			// block's own JSON, and the accumulator has no block to append them
			// to.
			return nil, false, nil
		}
		return msg.BlockDelta{Index: wire.Index, Kind: msg.DeltaInputJSON, Text: p.PartialJSON}, true, nil
	case "citations_delta":
		var p struct {
			Citation json.RawMessage `json:"citation"`
		}
		if err := json.Unmarshal(wire.Delta, &p); err != nil {
			return nil, false, fmt.Errorf("anthropic: decode citations_delta: %w", err)
		}
		if state != nil && len(p.Citation) > 0 {
			state.citations = append(state.citations, p.Citation)
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("anthropic: unsupported content block delta %q at index %d", typ, wire.Index)
}

func (d *Decoder) contentBlockStop(data []byte) (msg.StreamEvent, bool, error) {
	var wire struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, fmt.Errorf("anthropic: decode content_block_stop: %w", err)
	}
	state, ok := d.blocks[wire.Index]
	if !ok {
		return msg.BlockStop{Index: wire.Index}, true, nil
	}
	delete(d.blocks, wire.Index)
	block, err := state.finalize()
	if err != nil {
		return nil, false, err
	}
	if state.invalidInput != nil {
		d.invalid[wire.Index] = state.invalidInput
	}
	return msg.BlockStop{Index: wire.Index, Block: block}, true, nil
}

// blockState is one open content block: the block as it started, its own JSON,
// and the fragments that arrived since.
type blockState struct {
	block     msg.Block
	raw       map[string]json.RawMessage
	text      string
	thinking  string
	signature string
	input     string
	citations []json.RawMessage

	textSeen      bool
	thinkingSeen  bool
	signatureSeen bool
	inputSeen     bool

	invalidInput json.RawMessage
}

// finalize rebuilds the block's JSON from its fragments and decodes it with the
// same residue-aware rules a whole body gets. A nil block means the input could
// not be parsed and the accumulator's repair policy should apply instead.
// Unparseable input is retained on the decoder for BOTH block kinds, so a
// caller that records the response can put the fragments where the input was.
func (s *blockState) finalize() (msg.Block, error) {
	for _, f := range []struct {
		key   string
		value string
		seen  bool
	}{
		{"text", s.startString("text") + s.text, s.textSeen},
		{"thinking", s.startString("thinking") + s.thinking, s.thinkingSeen},
		{"signature", s.startString("signature") + s.signature, s.signatureSeen},
	} {
		if !f.seen {
			continue
		}
		enc, err := json.Marshal(f.value)
		if err != nil {
			return nil, fmt.Errorf("anthropic: rebuild block %s: %w", f.key, err)
		}
		s.raw[f.key] = enc
	}
	if s.inputSeen {
		fragments := bytes.TrimSpace([]byte(s.input))
		switch {
		case len(fragments) == 0:
			// Nothing arrived: the block keeps the input it started with.
		case json.Valid(fragments):
			s.raw["input"] = json.RawMessage(fragments)
		case !s.opaque():
			// A client tool call whose arguments did not parse. The block is
			// left to the accumulator, which substitutes an empty object; the
			// raw fragments are kept for a proxy to repair or report.
			s.invalidInput = json.RawMessage(fragments)
			return nil, nil
		default:
			// An opaque server-tool block still has to be sent on with its own
			// type and every other key intact, so it decodes with an empty
			// input. Its fragments are retained exactly as a client tool call's
			// are: the record must say the input was malformed, not that the
			// tool was called with no arguments.
			s.invalidInput = json.RawMessage(fragments)
			s.raw["input"] = json.RawMessage("{}")
		}
	}
	if len(s.citations) > 0 {
		citations, err := appendCitations(s.raw["citations"], s.citations)
		if err != nil {
			return nil, err
		}
		s.raw["citations"] = citations
	}
	raw, err := json.Marshal(s.raw)
	if err != nil {
		return nil, fmt.Errorf("anthropic: rebuild content block: %w", err)
	}
	return decodeBlock(raw)
}

// opaque reports whether the open block is one this codec keeps verbatim.
func (s *blockState) opaque() bool {
	_, ok := s.block.(msg.Opaque)
	return ok
}

// startString returns the string the block started with under key.
func (s *blockState) startString(key string) string {
	raw, ok := s.raw[key]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// appendCitations returns existing with added appended, in arrival order.
func appendCitations(existing json.RawMessage, added []json.RawMessage) (json.RawMessage, error) {
	var citations []json.RawMessage
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &citations); err != nil {
			return nil, fmt.Errorf("anthropic: read block citations: %w", err)
		}
	}
	out, err := json.Marshal(append(citations, added...))
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode block citations: %w", err)
	}
	return out, nil
}

// inBandError turns an SSE "error" event into the API error it reports. Such an
// error arrives after the connection is up, so it has no HTTP status and no
// header; the event's own JSON is the body.
func inBandError(data []byte) error {
	se := parseStreamError(data)
	return &msg.APIError{
		Body:    append([]byte(nil), data...),
		Kind:    classifyInBand(data),
		Dialect: msg.DialectAnthropic,
		Message: se.Message,
	}
}

// streamError is the machine-readable part of an SSE error event.
type streamError struct {
	ErrType string
	Message string
	Code    int
}

// parseStreamError reads either envelope a provider sends: the protocol's
// {"type":"error","error":{...}} or the bare {"type":"timeout_error",...} some
// upstreams emit instead.
func parseStreamError(data []byte) streamError {
	var wire struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   *struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return streamError{}
	}
	if wire.Error == nil {
		return streamError{ErrType: wire.Type, Message: wire.Message}
	}
	se := streamError{ErrType: wire.Error.Type, Message: wire.Error.Message}
	if len(wire.Error.Code) > 0 && string(wire.Error.Code) != "null" {
		if n, err := strconv.Atoi(strings.Trim(string(wire.Error.Code), `"`)); err == nil {
			se.Code = n
		}
	}
	return se
}
