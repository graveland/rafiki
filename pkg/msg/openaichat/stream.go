package openaichat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"go.graveland.dev/rafiki/pkg/msg"
)

// streamDecoder adapts a Chat Completions text/event-stream body to
// msg.StreamDecoder. Each `data:` line carries one chat.completion.chunk (or
// the `[DONE]` terminator); a chunk translates into zero or more native events,
// so events are queued between pulls — a chunk can open a block and deliver its
// first delta, and a role-only opener translates to nothing.
//
// Native events are emitted directly: no Anthropic event sequence is forged in
// between, so the caller sees the dialect's own structure (one MessageStart,
// dense block indexes, one MessageDelta carrying the stop reason and totals).
type streamDecoder struct {
	rc  io.ReadCloser
	scn *bufio.Scanner

	queue  []msg.StreamEvent
	err    error
	done   bool
	closed bool

	// Translation state.
	started    bool // MessageStart emitted
	finishSeen bool // a finish_reason chunk was processed
	deltaSent  bool // MessageDelta emitted
	stop       msg.StopReason
	stopRaw    string    // the finish_reason exactly as the wire spelled it
	usage      msg.Usage // the latest usage any chunk reported
	haveUsage  bool

	activeValid  bool // a block is currently open
	activeRef    blockRef
	activeIndex  int // index of the open block
	blocksIssued int // content-block indexes handed out so far
}

// blockRef identifies the source a translated block streams from: a text block
// keyed by choice index, or a tool_use block keyed by tool-call index. Only
// choices[0] is translated — rafiki never asks for more than one choice, and
// Encode sends no `n`.
//
// Only one block is ever open at a time: whenever a different source starts
// streaming, the open block is stopped before the new one starts, so every
// delta sits next to its own BlockStart.
type blockRef struct {
	kind  string // blockText or blockTool
	index int
}

const (
	blockText = "text"
	blockTool = "tool"
)

// NewStreamDecoder returns a decoder over a Chat Completions SSE body. The
// caller owns r and must Close the decoder when done with it.
func NewStreamDecoder(r io.ReadCloser) msg.StreamDecoder {
	scn := bufio.NewScanner(r)
	// Long tool-argument and text chunks are the biggest lines in practice; the
	// ceiling matches what the SDK's own SSE decoder allowed.
	scn.Buffer(nil, bufio.MaxScanTokenSize<<9)
	return &streamDecoder{rc: r, scn: scn}
}

// Next returns the next event, io.EOF after the last event of a clean stream, or
// the stream's failure. Events produced before a failure are delivered first,
// so an in-band error surfaces after the content that preceded it.
func (d *streamDecoder) Next() (msg.StreamEvent, error) {
	for len(d.queue) == 0 && !d.done {
		d.pull()
	}
	if len(d.queue) > 0 {
		ev := d.queue[0]
		d.queue = d.queue[1:]
		return ev, nil
	}
	if d.err != nil {
		err := d.err
		d.err = nil
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the stream.
func (d *streamDecoder) Close() error {
	d.closed = true
	return d.rc.Close()
}

// pull consumes ONE SSE event block — the accumulated `data:` lines up to the
// next blank line — and dispatches it. Comment lines (OpenRouter's
// `: OPENROUTER PROCESSING` keep-alives) and non-data fields are skipped; split
// data lines are joined with '\n' as the SSE spec requires.
func (d *streamDecoder) pull() {
	if d.closed {
		d.done = true
		return
	}
	var data []byte
	for d.scn.Scan() {
		line := d.scn.Bytes()
		if len(line) == 0 {
			if len(data) == 0 {
				continue // blank line between events
			}
			d.dispatch(data)
			return
		}
		if line[0] == ':' {
			continue
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if string(name) != "data" {
			continue // "event:" and friends: this dialect's type is in the payload
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		data = append(data, value...)
		data = append(data, '\n')
	}
	// The scanner stopped: a transport error mid-body, or clean EOF — possibly
	// with one last event block that lacked its terminating blank line.
	if err := d.scn.Err(); err != nil {
		if !d.finishSeen {
			d.fail(d.streamError(msg.ErrTransient, nil, "llm: openai: stream transport error: "+err.Error()))
			return
		}
		// After finish_reason the message is complete: a read failure in the
		// trailer must not fail an already-delivered turn, though its stop
		// reason and totals still have to be delivered.
		d.emitMessageDelta()
		d.done = true
		return
	}
	if len(data) > 0 {
		d.dispatch(data)
		return
	}
	d.endOfStream()
}

// endOfStream finishes a body that ended without a [DONE] sentinel. A stream
// with no finish_reason never completed its message: surfacing that beats
// silently persisting a truncated response as a complete turn.
func (d *streamDecoder) endOfStream() {
	if !d.finishSeen {
		d.fail(d.streamError(msg.ErrTransient, nil, "llm: openai: stream ended without a finish_reason (response truncated)"))
		return
	}
	d.emitMessageDelta()
	d.done = true
}

// dispatch translates one SSE data payload: a chat.completion.chunk, the [DONE]
// sentinel, or something that must become a stream failure.
func (d *streamDecoder) dispatch(data []byte) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		// [DONE] without a preceding finish_reason is a truncated stream, not a
		// completed one.
		if !d.finishSeen {
			d.fail(d.streamError(msg.ErrTransient, data, "llm: openai: stream ended ([DONE]) without a finish_reason (response truncated)"))
			return
		}
		d.emitMessageDelta()
		d.done = true
		return
	}
	var chunk streamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		d.fail(d.streamError(msg.ErrTransient, data, "llm: openai: stream chunk failed to parse as a chat.completion.chunk: "+err.Error()))
		return
	}
	if trimmed := bytes.TrimSpace(chunk.Error); len(trimmed) > 0 && string(trimmed) != "null" {
		// An in-band error object: the stream is over, and the payload carries
		// what kind of failure it is.
		d.fail(&msg.APIError{
			Status:  0,
			Body:    data,
			Kind:    Classify(0, nil, data),
			Dialect: msg.DialectOpenAIChat,
			Message: parseErrorPayload(data).message,
		})
		return
	}
	if len(chunk.Choices) == 0 && chunk.Usage == nil {
		// Parses as JSON but is neither a chunk nor an error object: the stream
		// is broken mid-flight, which is a transport-shaped failure, so it is
		// retryable rather than the unclassified ErrOther.
		d.fail(d.streamError(msg.ErrTransient, data, "llm: openai: stream carried a non-chunk payload"))
		return
	}
	d.translate(&chunk)
}

// translate maps one chat.completion.chunk onto native events:
//
//  1. the first chunk received     -> MessageStart
//  2. the first text delta for a   -> BlockStart, then the delta (3)
//     choice index
//  3. every text delta             -> BlockDelta(DeltaText)
//  4. the first arguments delta    -> BlockStart, then the delta (5)
//     for a tool-call index
//  5. every arguments delta        -> BlockDelta(DeltaInputJSON), the fragment
//     forwarded unmodified
//  6. a block's source stopping    -> BlockStop, emitted the moment anything
//     else needs to speak
//  7. the finish_reason chunk      -> BlockStop for the open block, then the
//     MessageDelta (stop reason from the same table DecodeResponse uses)
//  8. anything untranslatable      -> a *msg.APIError from Next
//
// The MessageDelta waits for the usage-only trailer Chat Completions sends
// after the finish_reason chunk, because that is where the totals are. When the
// finish chunk itself carries usage, or no trailer ever arrives, it is emitted
// as soon as the message is complete.
func (d *streamDecoder) translate(chunk *streamChunk) {
	if chunk.Usage != nil {
		d.usage = decodeUsage(chunk.Usage)
		d.haveUsage = true
	}
	if !d.started {
		d.started = true
		d.queue = append(d.queue, msg.MessageStart{ID: chunk.ID, Model: chunk.Model, Provider: chunk.Provider})
	}
	if d.finishSeen {
		if chunk.Usage != nil {
			d.emitMessageDelta()
		}
		return // trailer after the finish chunk: the message is complete
	}
	if len(chunk.Choices) == 0 {
		return // usage-only chunk
	}
	choice := chunk.Choices[0]

	// A present-but-empty content (the role-only opener chunk) contributes
	// nothing, matching DecodeResponse, which emits no block for a zero-length
	// completion.
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		ref := blockRef{kind: blockText, index: choice.Index}
		if !d.activeValid || d.activeRef != ref {
			d.openTextBlock(choice.Index)
		}
		d.queue = append(d.queue, msg.BlockDelta{Index: d.activeIndex, Kind: msg.DeltaText, Text: *choice.Delta.Content})
	}

	for i := range choice.Delta.ToolCalls {
		tc := choice.Delta.ToolCalls[i]
		if tc.Function == nil && tc.ID == "" {
			continue // an entry carrying only an index has nothing to emit
		}
		ref := blockRef{kind: blockTool, index: tc.Index}
		if !d.activeValid || d.activeRef != ref {
			d.openToolBlock(tc)
		}
		if tc.Function != nil && tc.Function.Arguments != "" {
			// Forward the fragment exactly as sent: an arguments delta is an
			// incremental fragment, never a re-emitted snapshot.
			d.queue = append(d.queue, msg.BlockDelta{Index: d.activeIndex, Kind: msg.DeltaInputJSON, Text: tc.Function.Arguments})
		}
	}

	if choice.FinishReason != nil && *choice.FinishReason != "" {
		d.finishSeen = true
		d.closeActiveBlock()
		d.stop = stopReason(*choice.FinishReason)
		d.stopRaw = *choice.FinishReason
		if d.haveUsage {
			d.emitMessageDelta()
		}
	}
}

// openTextBlock closes any open block and starts a text block.
func (d *streamDecoder) openTextBlock(choiceIndex int) {
	d.closeActiveBlock()
	d.activeRef = blockRef{kind: blockText, index: choiceIndex}
	d.activeIndex = d.blocksIssued
	d.blocksIssued++
	d.activeValid = true
	d.queue = append(d.queue, msg.BlockStart{Index: d.activeIndex, Block: msg.Text{}})
}

// openToolBlock closes any open block and starts a tool_use block. The id and
// name arrive once, on the first chunk for that tool call, and are emitted only
// here; later chunks carry the index and the next arguments fragment. The input
// starts as the empty object a parameterless call legitimately ends up with.
func (d *streamDecoder) openToolBlock(tc streamToolCall) {
	d.closeActiveBlock()
	d.activeRef = blockRef{kind: blockTool, index: tc.Index}
	d.activeIndex = d.blocksIssued
	d.blocksIssued++
	d.activeValid = true
	name := ""
	if tc.Function != nil {
		name = tc.Function.Name
	}
	d.queue = append(d.queue, msg.BlockStart{
		Index: d.activeIndex,
		Block: msg.ToolUse{ID: tc.ID, Name: name, Input: json.RawMessage("{}")},
	})
}

// closeActiveBlock stops the open block, if any. The stop carries no
// replacement block: the accumulator holds everything the deltas built.
func (d *streamDecoder) closeActiveBlock() {
	if !d.activeValid {
		return
	}
	d.queue = append(d.queue, msg.BlockStop{Index: d.activeIndex})
	d.activeValid = false
}

// emitMessageDelta queues the MessageDelta with the latest usage, at most once.
func (d *streamDecoder) emitMessageDelta() {
	if !d.finishSeen || d.deltaSent {
		return
	}
	d.deltaSent = true
	d.queue = append(d.queue, msg.MessageDelta{StopReason: d.stop, StopReasonRaw: d.stopRaw, Usage: d.usage})
}

// fail ends the stream. Events already queued are delivered first.
func (d *streamDecoder) fail(err error) {
	d.err = err
	d.done = true
}

// streamError builds the in-band failure value: status 0 marks it as a stream
// failure rather than an HTTP refusal, and body is the offending payload when
// there is one.
func (d *streamDecoder) streamError(kind msg.ErrorKind, body []byte, message string) *msg.APIError {
	return &msg.APIError{Status: 0, Body: body, Kind: kind, Dialect: msg.DialectOpenAIChat, Message: message}
}

// streamChunk is the wire shape of one chat.completion.chunk. Only the fields
// this translation consumes are declared.
type streamChunk struct {
	ID       string         `json:"id"`
	Model    string         `json:"model"`
	Provider string         `json:"provider"`
	Choices  []streamChoice `json:"choices"`
	Usage    *chatUsage     `json:"usage"`
	// Error is an in-band refusal object, which some servers stream instead of
	// an HTTP status.
	Error json.RawMessage `json:"error"`
}

type streamChoice struct {
	Index int         `json:"index"`
	Delta streamDelta `json:"delta"`
	// FinishReason is a pointer so null/absent (every non-terminal chunk) is
	// distinct from a present value; "" is treated like absent.
	FinishReason *string `json:"finish_reason"`
}

type streamDelta struct {
	Content   *string          `json:"content"`
	ToolCalls []streamToolCall `json:"tool_calls"`
}

// streamToolCall is the delta form of chatToolCall: id, type and name arrive
// once, on the first chunk for a tool call; later chunks carry only the index
// and the next arguments fragment.
type streamToolCall struct {
	Index    int           `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function *chatFunction `json:"function"`
}
