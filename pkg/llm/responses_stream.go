// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// responsesMaxLineBytes caps one SSE line. It is not a formality: the endpoint
// sends whole output items on a single `data:` line, both on
// response.output_item.done and on response.completed, and a reasoning item's
// encrypted_content is a multi-kilobyte (sometimes multi-megabyte) base64 blob.
// bufio.Scanner's 64 KiB default fails the moment a turn has reasoning in it,
// which is every turn that asked for it.
const responsesMaxLineBytes = 64 << 20

// defaultResponsesStreamIdleTimeout is how long the decoder waits for the next
// event before declaring the stream dead. codex uses 300 s; a reasoning model
// can sit silent for a long time before the first token, so a short timeout
// would fail healthy turns. Held in the sender (not hardcoded here) so tests
// can shorten it.
const defaultResponsesStreamIdleTimeout = 300 * time.Second

// responsesContentFilterMessage is what a content-filter stop and a
// content_filter incomplete reason both report. Same words everywhere so a
// child's transcript does not depend on which path caught it.
const responsesContentFilterMessage = "response blocked by the provider's content filter"

// responsesSSEDecoder adapts a Responses text/event-stream body to
// ssestream.Decoder, translating each event into zero or more Anthropic stream
// events. It is modelled on openAISSEDecoder (same queue-between-pulls shape,
// same "the emitted Type is the ANTHROPIC type" rule) but the source vocabulary
// is entirely different, so nothing is shared beyond the ssestream contract.
//
// Failures split in two, and the split is the part that matters:
//
//   - RETRYABLE failures are delivered as an in-band {"type":"error"} event
//     using pkg/llm/streamerr.go's vocabulary. The SDK wraps that payload in a
//     plain string error, and IsTransientStreamError / IsRateLimitStreamError
//     recover the classification from the text. Emitting the event (rather than
//     returning a Go error) is what keeps the existing retry machinery working
//     unchanged.
//   - TERMINAL failures that carry their own type (a subscription usage limit, a
//     context-length overflow) are set as the decoder's error, which
//     ssestream.Stream.Err surfaces as-is. A string error cannot carry a type,
//     and both of those consumers need one: UsageLimitError must not look like
//     an *anthropic.Error 429, and the trim path matches on *anthropic.Error.
//
// One content block is open at a time, for the same reason openAISSEDecoder
// keeps that rule: Message.Accumulate appends a block per content_block_start
// and applies every content_block_delta to the LAST block, so a delta must never
// arrive for a block other than the most recently started one.
type responsesSSEDecoder struct {
	rc   io.ReadCloser
	idle time.Duration
	// maxLineBytes is responsesMaxLineBytes in production (see
	// newResponsesSSEDecoder), settable so tests can exercise the overflow
	// without materialising 64 MiB.
	maxLineBytes int

	// lines is fed by the ONE reader goroutine, which is the only code that
	// touches the bufio.Reader. Two readers on one buffered reader is a data
	// race and a torn frame, so the timeout path stops the reader instead of
	// racing it (see readLine).
	lines    chan responsesLine
	closeCh  chan struct{}
	stopOnce sync.Once
	stopErr  error // set by stopReading, read after it

	queue  []ssestream.Event
	cur    ssestream.Event
	err    error
	closed bool
	done   bool

	// Translation state.
	started      bool // message_start emitted
	model        string
	blocksIssued int64 // Anthropic content-block indexes handed out
	toolCalls    int   // function_call blocks emitted, for the stop reason
	active       *responsesOpenBlock
	closedBlocks map[int]bool // output indexes whose block is already closed
}

// responsesLine is one line handed from the reader goroutine to the decoder.
type responsesLine struct {
	line []byte
	err  error
}

// responsesOpenBlock is the content block currently being streamed, keyed by the
// endpoint's output_index (its own per-response item counter).
type responsesOpenBlock struct {
	outputIndex int
	index       int64  // Anthropic index
	kind        string // responsesBlockText / Tool / Thinking
	itemID      string // reasoning item id, for the signature
	sawDeltas   bool   // a reasoning block already streamed summary text
}

const (
	responsesBlockText     = "text"
	responsesBlockTool     = "tool"
	responsesBlockThinking = "thinking"
)

// newResponsesSSEDecoder builds the decoder and starts its one reader goroutine.
// maxLine caps a single line; <= 0 means responsesMaxLineBytes. It is a parameter
// (rather than a field written after construction) because the reader goroutine
// starts here: mutating it afterwards would race the reader.
func newResponsesSSEDecoder(rc io.ReadCloser, idle time.Duration, maxLine int) *responsesSSEDecoder {
	if maxLine <= 0 {
		maxLine = responsesMaxLineBytes
	}
	d := &responsesSSEDecoder{
		rc:           rc,
		idle:         idle,
		maxLineBytes: maxLine,
		lines:        make(chan responsesLine, 1),
		closeCh:      make(chan struct{}),
		closedBlocks: map[int]bool{},
	}
	go d.readLoop()
	return d
}

// readLoop is the ONLY goroutine that reads the body. It hands one line at a
// time to readLine and exits on the first error (or when the decoder closes).
//
// The send is unbuffered-or-buffered with a select on closeCh so a consumer that
// stops reading (a terminal event reached, Close called) can never block this
// goroutine forever.
func (d *responsesSSEDecoder) readLoop() {
	defer close(d.lines)
	br := bufio.NewReaderSize(d.rc, 64<<10)
	for {
		line, err := d.readLineCapped(br)
		select {
		case d.lines <- responsesLine{line: line, err: err}:
		case <-d.closeCh:
			return
		}
		if err != nil {
			return
		}
	}
}

func (d *responsesSSEDecoder) Event() ssestream.Event { return d.cur }

func (d *responsesSSEDecoder) Err() error { return d.err }

func (d *responsesSSEDecoder) Close() error {
	d.closed = true
	d.stopReading(errResponsesClosed)
	return nil
}

// stopReading stops the reader goroutine for good: it closes closeCh (so a
// pending send cannot block) and closes the body (so a blocked read returns).
// Exactly one goroutine may own the reader, so once this has run there is no
// further read, whoever calls it.
func (d *responsesSSEDecoder) stopReading(cause error) {
	d.stopOnce.Do(func() {
		d.stopErr = cause
		close(d.closeCh)
		_ = d.rc.Close()
	})
}

func (d *responsesSSEDecoder) Next() bool {
	if d.closed {
		return false
	}
	for len(d.queue) == 0 && !d.done && d.err == nil {
		d.pull()
	}
	if len(d.queue) == 0 {
		return false
	}
	d.cur = d.queue[0]
	d.queue = d.queue[1:]
	return true
}

// pull reads one SSE frame (its `event:` and `data:` lines up to the next blank
// line) and dispatches it. Comment lines are skipped; split data lines are
// joined with '\n' as the SSE spec requires.
//
// A frame is dispatched only when its terminating blank line arrived. A read
// failure mid-frame (a timeout, a truncated body, an over-long line) never
// dispatches what it collected: a half frame is not an event, and treating one
// as an event can turn a partial `response.completed` into a completed turn.
func (d *responsesSSEDecoder) pull() {
	var data []byte
	var eventName string
	var readErr error
	for {
		var line []byte
		line, readErr = d.readLine()
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 && len(line) > 0 {
			// A blank line ends the frame.
			if len(data) > 0 {
				d.dispatch(data, eventName)
				return
			}
			if readErr == nil {
				continue
			}
			break
		}
		if len(trimmed) > 0 && trimmed[0] != ':' {
			name, value, _ := bytes.Cut(trimmed, []byte(":"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch string(name) {
			case "event":
				eventName = string(value)
			case "data":
				data = append(data, value...)
				data = append(data, '\n')
			}
		}
		if readErr != nil {
			break
		}
	}
	d.failRead(readErr, len(data) > 0)
}

// failRead reports the read failure that ended the stream, split by whether a
// retry can plausibly help:
//
//   - an over-long line is DETERMINISTIC: the same request produces it again, so
//     it is non-retryable (see errResponsesLineTooLong);
//   - any other read failure, and a stream that ends without a terminal event,
//     is transient: a dead connection or a silent backend is worth re-issuing.
//
// partial says a frame was cut short by the failure, which is always reported as
// truncated (it is never dispatched).
func (d *responsesSSEDecoder) failRead(err error, partial bool) {
	switch {
	case errors.Is(err, errResponsesLineTooLong):
		d.emitStreamError("invalid_request_error", err.Error())
	case err != nil && !errors.Is(err, io.EOF):
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai-responses: stream read failed: %v", err))
	case d.done:
		// A terminal event was already processed; the stream simply ended.
	case partial || err != nil:
		d.emitStreamError("api_error", "llm: openai-responses: stream ended without response.completed, response.incomplete or response.failed (response truncated)")
	default:
		d.done = true
	}
}

// readLine returns one line INCLUDING its terminator (which the caller may
// ignore), or the bytes read before EOF alongside that error.
//
// Reading is time-capped when an idle timeout is configured. The timeout does
// not race the reader: it stops it (closing the body) and reports the timeout,
// so after it fires no code reads the buffered reader ever again.
func (d *responsesSSEDecoder) readLine() ([]byte, error) {
	if d.idle > 0 {
		timer := time.NewTimer(d.idle)
		defer timer.Stop()
		select {
		case r, ok := <-d.lines:
			if !ok {
				return nil, d.readEnded()
			}
			return r.line, r.err
		case <-timer.C:
			d.stopReading(errResponsesIdleTimeout)
			return nil, errResponsesIdleTimeout
		}
	}
	r, ok := <-d.lines
	if !ok {
		return nil, d.readEnded()
	}
	return r.line, r.err
}

// readEnded is what a closed lines channel means: whichever cause stopped the
// reader, or a clean EOF when nothing did.
func (d *responsesSSEDecoder) readEnded() error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return io.EOF
}

// errResponsesIdleTimeout marks the idle-timeout read failure. It never escapes
// as a value: pull turns it into a transient in-band stream error, because a
// silent backend is a retryable condition, not a broken turn.
var errResponsesIdleTimeout = fmt.Errorf("idle timeout")

// errResponsesClosed marks a stream the caller closed before it finished. It is
// not an error to report: reading simply stops.
var errResponsesClosed = fmt.Errorf("stream closed")

// errResponsesLineTooLong marks a line that exceeded the cap. It is DETERMINISTIC
// (the same request produces the same oversized line), so pull classifies it as
// non-retryable; a malformed payload, which may be a one-off corruption, stays
// transient.
type responsesLineTooLongError struct {
	limit int
}

func (e *responsesLineTooLongError) Error() string {
	return fmt.Sprintf("llm: openai-responses: stream line exceeds %d bytes", e.limit)
}

func (e *responsesLineTooLongError) Is(target error) bool {
	_, ok := target.(*responsesLineTooLongError)
	return ok
}

// errResponsesLineTooLong is the sentinel every overflow matches, whatever the
// cap was.
var errResponsesLineTooLong = &responsesLineTooLongError{}

// readLineCapped reads one line, allowing up to d.maxLineBytes. It is the
// explicit replacement for bufio.Scanner's fixed 64 KiB ceiling (see
// responsesMaxLineBytes). Only the reader goroutine calls it.
func (d *responsesSSEDecoder) readLineCapped(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > d.maxLineBytes {
			return nil, &responsesLineTooLongError{limit: d.maxLineBytes}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// dispatch translates one SSE payload. eventName is the frame's `event:` field,
// used only when the payload itself carries no type.
func (d *responsesSSEDecoder) dispatch(data []byte, eventName string) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return
	}
	var ev responsesStreamEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai-responses: stream event failed to parse: %v: %.500s", err, trimmed))
		return
	}
	typ := ev.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "response.created":
		d.emitCreated(ev.Response)
	case "response.output_item.added":
		d.ensureStarted()
		if ev.Item != nil {
			d.openItem(*ev.Item, outputIndex(ev.OutputIndex))
		}
	case "response.output_item.done":
		d.ensureStarted()
		if ev.Item != nil {
			d.finishItem(*ev.Item, outputIndex(ev.OutputIndex))
		}
	case "response.output_text.delta":
		d.ensureStarted()
		d.ensureBlock(outputIndex(ev.OutputIndex), responsesBlockText, "")
		if d.active != nil && d.active.kind == responsesBlockText {
			d.emitTextDelta(d.active.index, ev.Delta)
		}
	case "response.function_call_arguments.delta":
		d.ensureStarted()
		// The arguments belong to the tool_use block the item's `added` event
		// opened. Without one there is no call_id to name the block, so the
		// fragment is dropped rather than attached to a block that would be
		// rejected for having no id.
		if d.active != nil && d.active.kind == responsesBlockTool && d.active.outputIndex == outputIndex(ev.OutputIndex) && ev.Delta != "" {
			d.active.sawDeltas = true
			d.emitInputJSONDelta(d.active.index, ev.Delta)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		d.ensureStarted()
		idx := outputIndex(ev.OutputIndex)
		d.ensureBlock(idx, responsesBlockThinking, ev.ItemID)
		if d.active != nil && d.active.kind == responsesBlockThinking && d.active.outputIndex == idx {
			d.active.sawDeltas = d.active.sawDeltas || ev.Delta != ""
			if ev.Delta != "" {
				d.emitThinkingDelta(d.active.index, ev.Delta)
			}
		}
	case "response.completed":
		d.complete(ev.Response, false)
	case "response.incomplete":
		d.complete(ev.Response, true)
	case "response.failed":
		d.fail(ev.Response)
	case "error":
		d.failEvent(ev)
	default:
		// response.in_progress, response.metadata (moderation data),
		// response.content_part.*, response.output_text.done,
		// response.function_call_arguments.done, response.reasoning_summary_part.*
		// and anything newer than this decoder: nothing Anthropic-shaped to say.
	}
}

// outputIndex normalises the optional output_index field: absent counts as 0,
// which is also the only index a single-item response ever uses.
func outputIndex(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ensureStarted emits message_start the first time anything needs a message.
// response.created normally does it, but a decoder that sees only item events
// must still produce a well-formed stream.
func (d *responsesSSEDecoder) ensureStarted() {
	if d.started {
		return
	}
	d.started = true
	d.emitMessageStart("", d.model)
}

func (d *responsesSSEDecoder) emitCreated(resp *responsesResponseObject) {
	if d.started {
		return
	}
	d.started = true
	if resp != nil {
		d.model = resp.Model
		d.emitMessageStart(resp.ID, resp.Model)
		return
	}
	d.emitMessageStart("", d.model)
}

// openItem starts the content block for one output item. Items with no
// Anthropic counterpart (custom_tool_call, custom_tool_call_output, anything
// newer) are ignored.
func (d *responsesSSEDecoder) openItem(item responsesOutputItem, idx int) {
	switch item.Type {
	case "message":
		d.openBlock(idx, responsesBlockText)
		d.emitBlockStart(d.active.index, responsesEventTextBlock{Type: "text"})
	case "function_call":
		d.openBlock(idx, responsesBlockTool)
		d.toolCalls++
		d.emitBlockStart(d.active.index, responsesEventToolBlock{
			Type:  "tool_use",
			ID:    item.CallID,
			Name:  item.Name,
			Input: struct{}{},
		})
	case "reasoning":
		d.openBlock(idx, responsesBlockThinking)
		d.active.itemID = item.ID
		d.emitBlockStart(d.active.index, responsesEventThinkingBlock{Type: "thinking", Thinking: "", Signature: ""})
	}
}

// finishItem closes the block for one output item. A reasoning item gets its
// signature first (id plus encrypted content, in the format
// responsesSignature writes), and is what makes the reasoning resendable on the
// next turn.
//
// A reasoning item with NO encrypted content is still signed, with an empty blob
// ("openai-responses:<id>:"). The signature is not only a resend token: it is how
// the block announces which provider owns it. An unsigned thinking block is
// indistinguishable from an Anthropic one, so StripForeignThinking keeps it on a
// failover to an Anthropic API-key provider, which then rejects a signature it
// cannot verify. With the empty-blob signature the block is recognisably ours
// (Anthropic strips it) while this sender still drops it when resending (the
// parsed blob is empty, so there is nothing to resume).
//
// A done for an index whose block is already closed (a late event under
// interleaving) is ignored: reopening it would duplicate a tool_use block.
func (d *responsesSSEDecoder) finishItem(item responsesOutputItem, idx int) {
	if d.closedBlocks[idx] {
		return
	}
	if d.active == nil || d.active.outputIndex != idx {
		// The item had no `added` event, so nothing is open for it. Open it now
		// so a done-only item still reaches the caller, then close it below.
		d.openItem(item, idx)
		if d.active == nil {
			return
		}
	}
	switch d.active.kind {
	case responsesBlockThinking:
		if !d.active.sawDeltas {
			// Some models deliver the summary only in the completed item.
			if text := responsesSummaryText(item.Summary); text != "" {
				d.emitThinkingDelta(d.active.index, text)
			}
		}
		id := item.ID
		if id == "" {
			id = d.active.itemID
		}
		d.emitSignatureDelta(d.active.index, responsesSignature(id, item.EncryptedContent))
	case responsesBlockTool:
		if item.CallID != "" {
			d.active.itemID = item.CallID
		}
		if item.Arguments != "" && !d.active.sawDeltas {
			// No delta carried the arguments, so the completed item's own
			// string is the whole input. Message.Accumulate replaces the
			// block's "{}" seed with a partial_json that starts it.
			d.emitInputJSONDelta(d.active.index, item.Arguments)
		}
	}
	d.closeActiveBlock()
}

// complete handles response.completed and response.incomplete.
func (d *responsesSSEDecoder) complete(resp *responsesResponseObject, incomplete bool) {
	d.ensureStarted()
	d.closeActiveBlock()
	reason := ""
	if resp != nil && resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	if incomplete && reason == "content_filter" {
		d.emitStreamError("invalid_request_error", responsesContentFilterMessage)
		return
	}
	stop := anthropic.StopReasonEndTurn
	switch {
	case incomplete && reason == "max_output_tokens":
		stop = anthropic.StopReasonMaxTokens
	case d.toolCalls > 0:
		stop = anthropic.StopReasonToolUse
	case resp != nil && resp.Status == "incomplete" && reason == "max_output_tokens":
		stop = anthropic.StopReasonMaxTokens
	}
	var usage *responsesUsage
	if resp != nil {
		usage = resp.Usage
	}
	d.emitMessageDelta(stop, responsesUsageAnthropic(usage))
	d.emitMessageStop()
	d.done = true
}

// fail handles response.failed, which carries the terminal error inside the
// response object.
func (d *responsesSSEDecoder) fail(resp *responsesResponseObject) {
	d.ensureStarted()
	d.closeActiveBlock()
	env := responsesAPIErrorEnvelope{}
	if resp != nil && resp.Error != nil {
		env.Error = resp.Error
	}
	d.failWith(env)
}

// failEvent handles the top-level `error` event.
func (d *responsesSSEDecoder) failEvent(ev responsesStreamEvent) {
	d.ensureStarted()
	d.closeActiveBlock()
	env := responsesAPIErrorEnvelope{Type: "error", Code: ev.Code, Message: ev.Message, Error: ev.Error}
	d.failWith(env)
}

// failWith classifies one endpoint error and reports it: terminal errors that
// need a Go type are set as the decoder's error, everything else becomes an
// in-band error event the stream classifiers can read.
func (d *responsesSSEDecoder) failWith(env responsesAPIErrorEnvelope) {
	_, _, message, _ := env.info()
	if u := responsesUsageLimitFromEvent(env); u != nil {
		d.err = u
		d.done = true
		return
	}
	code := responsesErrorCode(env)
	switch code {
	case "context_length_exceeded":
		d.err = newResponsesPromptTooLargeError(message)
		d.done = true
	case "invalid_prompt", "cyber_policy":
		d.emitStreamError("invalid_request_error", responsesErrorText(code, message))
	case "rate_limit_exceeded":
		d.emitStreamError("rate_limit_error", responsesErrorText(code, message))
	case "slow_down", "server_is_overloaded":
		d.emitStreamError("overloaded_error", responsesErrorText(code, message))
	default:
		// Unknown codes stay retryable: a code this decoder has not been taught
		// yet must not fail an otherwise-recoverable turn.
		d.emitStreamError("api_error", responsesErrorText(code, message))
	}
}

// responsesErrorCode picks the name to classify an endpoint error on: the first
// one this decoder recognises, else the first the payload names ("" when it names
// none). It reads every name the payload carries because the endpoint puts the
// meaningful code in either field: codex keys on `type`, and a payload whose
// `code` is something else must not fall through to "unknown, therefore
// transient".
func responsesErrorCode(env responsesAPIErrorEnvelope) string {
	codes := env.codes()
	for _, c := range codes {
		switch c {
		case "context_length_exceeded", "invalid_prompt", "cyber_policy",
			"rate_limit_exceeded", "slow_down", "server_is_overloaded":
			return c
		}
	}
	if len(codes) > 0 {
		return codes[0]
	}
	return ""
}

// responsesErrorText renders one endpoint error for a stream event message,
// keeping the code so a log names what the backend actually said.
func responsesErrorText(code, message string) string {
	switch {
	case code != "" && message != "":
		return "llm: openai-responses: " + code + ": " + message
	case code != "":
		return "llm: openai-responses: " + code
	case message != "":
		return "llm: openai-responses: " + message
	default:
		return "llm: openai-responses: the endpoint reported an error"
	}
}

// ensureBlock makes idx's block of the given kind the open one, starting it if
// it is not already. Starts of a different kind or a different output index
// close whatever was open first.
func (d *responsesSSEDecoder) ensureBlock(idx int, kind, itemID string) {
	if d.active != nil && d.active.outputIndex == idx && d.active.kind == kind {
		return
	}
	d.openBlock(idx, kind)
	switch kind {
	case responsesBlockText:
		d.emitBlockStart(d.active.index, responsesEventTextBlock{Type: "text"})
	case responsesBlockThinking:
		d.active.itemID = itemID
		d.emitBlockStart(d.active.index, responsesEventThinkingBlock{Type: "thinking", Thinking: "", Signature: ""})
	}
}

// openBlock closes the current block and issues the next Anthropic index.
// Indexes are sequential because Message.Accumulate appends a block per
// content_block_start and ignores the carried index.
func (d *responsesSSEDecoder) openBlock(idx int, kind string) {
	d.closeActiveBlock()
	d.active = &responsesOpenBlock{outputIndex: idx, index: d.blocksIssued, kind: kind}
	d.blocksIssued++
}

// closeActiveBlock emits content_block_stop for the open block, if any.
func (d *responsesSSEDecoder) closeActiveBlock() {
	if d.active == nil {
		return
	}
	d.emit("content_block_stop", responsesEventBlockStop{Type: "content_block_stop", Index: d.active.index})
	// Remember the index: a later done for a block that is no longer open must
	// not reopen it (see finishItem).
	if d.closedBlocks != nil {
		d.closedBlocks[d.active.outputIndex] = true
	}
	d.active = nil
}

func (d *responsesSSEDecoder) emitBlockStart(index int64, block any) {
	d.emit("content_block_start", responsesEventBlockStart{Type: "content_block_start", Index: index, ContentBlock: block})
}

func (d *responsesSSEDecoder) emitTextDelta(index int64, text string) {
	d.emit("content_block_delta", responsesEventBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: responsesEventTextDelta{Type: "text_delta", Text: text},
	})
}

func (d *responsesSSEDecoder) emitThinkingDelta(index int64, text string) {
	d.emit("content_block_delta", responsesEventBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: responsesEventThinkingDelta{Type: "thinking_delta", Thinking: text},
	})
}

func (d *responsesSSEDecoder) emitSignatureDelta(index int64, signature string) {
	d.emit("content_block_delta", responsesEventBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: responsesEventSignatureDelta{Type: "signature_delta", Signature: signature},
	})
}

func (d *responsesSSEDecoder) emitInputJSONDelta(index int64, fragment string) {
	d.emit("content_block_delta", responsesEventBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: responsesEventInputJSONDelta{Type: "input_json_delta", PartialJSON: fragment},
	})
}

func (d *responsesSSEDecoder) emitMessageStart(id, model string) {
	d.emit("message_start", responsesEventMessageStart{
		Type: "message_start",
		Message: responsesEventMessage{
			ID:      id,
			Model:   model,
			Role:    "assistant", // constant.Assistant
			Type:    "message",   // constant.Message
			Content: []any{},
			Usage:   responsesEventUsage{}, // real numbers are not known yet
		},
	})
}

func (d *responsesSSEDecoder) emitMessageDelta(stop anthropic.StopReason, usage responsesEventUsage) {
	d.emit("message_delta", responsesEventMessageDelta{
		Type:  "message_delta",
		Delta: responsesEventMessageDeltaDelta{StopReason: stop},
		Usage: usage,
	})
}

func (d *responsesSSEDecoder) emitMessageStop() {
	d.emit("message_stop", responsesEventMessageStop{Type: "message_stop"})
}

// emit marshals one translated event and queues it. These payload shapes are
// fixed (strings and integers only), so a marshal failure cannot happen in
// practice; if one ever did, it must not wedge the stream.
func (d *responsesSSEDecoder) emit(eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai-responses: marshal %s event: %v", eventType, err))
		return
	}
	d.queue = append(d.queue, ssestream.Event{Type: eventType, Data: data})
}

// emitStreamError queues the in-band Anthropic error envelope. errType comes
// from pkg/llm/streamerr.go's vocabulary only (api_error, overloaded_error,
// rate_limit_error, invalid_request_error, ...), because that is what the two
// stream classifiers key on. The stream is terminal after an error event.
func (d *responsesSSEDecoder) emitStreamError(errType, message string) {
	d.emit("error", responsesEventStreamError{
		Type:  "error",
		Error: responsesEventErrorBody{Type: errType, Message: message},
	})
	d.done = true
}

// ---- wire shapes of the Responses stream ----

// responsesStreamEvent is one SSE payload. Only the fields this translation
// consumes are declared; the endpoint sends many more.
type responsesStreamEvent struct {
	Type        string                   `json:"type"`
	Response    *responsesResponseObject `json:"response"`
	Item        *responsesOutputItem     `json:"item"`
	OutputIndex *int                     `json:"output_index"`
	ItemID      string                   `json:"item_id"`
	Delta       string                   `json:"delta"`
	// The flat form of the top-level error event.
	Code    json.RawMessage    `json:"code"`
	Message string             `json:"message"`
	Error   *responsesAPIError `json:"error"`
}

// responsesResponseObject is the response carried by response.created,
// response.completed, response.incomplete and response.failed, and the body of a
// non-streamed call.
type responsesResponseObject struct {
	ID                string                     `json:"id"`
	Model             string                     `json:"model"`
	Status            string                     `json:"status"`
	Output            []responsesOutputItem      `json:"output"`
	Usage             *responsesUsage            `json:"usage"`
	IncompleteDetails *responsesIncompleteDetail `json:"incomplete_details"`
	Error             *responsesAPIError         `json:"error"`
}

type responsesIncompleteDetail struct {
	Reason string `json:"reason"`
}

// responsesOutputItem is one entry of a response's output array, and the item a
// stream event carries.
type responsesOutputItem struct {
	Type             string                 `json:"type"`
	ID               string                 `json:"id"`
	CallID           string                 `json:"call_id"`
	Name             string                 `json:"name"`
	Arguments        string                 `json:"arguments"`
	EncryptedContent string                 `json:"encrypted_content"`
	Summary          []responsesSummaryPart `json:"summary"`
	Content          []responsesContentPart `json:"content"`
	Role             string                 `json:"role"`
	Status           string                 `json:"status"`
}

type responsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesUsage struct {
	InputTokens        int64                    `json:"input_tokens"`
	InputTokensDetails *responsesInputTokenInfo `json:"input_tokens_details"`
	OutputTokens       int64                    `json:"output_tokens"`
}

type responsesInputTokenInfo struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// responsesUsageAnthropic maps the endpoint's usage onto Anthropic's counters.
// input_tokens excludes the cached prefix (Anthropic reports the two
// separately, and a caller that added them would double-count the cache);
// output_tokens already includes reasoning tokens, which is also how Anthropic
// counts them.
func responsesUsageAnthropic(u *responsesUsage) responsesEventUsage {
	if u == nil {
		return responsesEventUsage{}
	}
	var cached, written int64
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
		written = u.InputTokensDetails.CacheWriteTokens
	}
	input := u.InputTokens - cached
	if input < 0 {
		input = 0
	}
	return responsesEventUsage{
		InputTokens:              input,
		CacheReadInputTokens:     cached,
		CacheCreationInputTokens: written,
		OutputTokens:             u.OutputTokens,
	}
}

// responsesSummaryText joins a reasoning item's summary parts into one string.
func responsesSummaryText(parts []responsesSummaryPart) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// ---- payloads of the Anthropic stream events this decoder emits ----

// Minimal on purpose, exactly like openAISender's: ssestream.Stream.Next
// unmarshals Event.Data into the flat MessageStreamEventUnion, so a subset of
// each variant's fields is sufficient, and emitting only what the endpoint
// actually said keeps the translated wire honest.
type responsesEventUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens"`
}

type responsesEventMessage struct {
	ID      string              `json:"id"`
	Model   string              `json:"model"`
	Role    string              `json:"role"`
	Type    string              `json:"type"`
	Content []any               `json:"content"`
	Usage   responsesEventUsage `json:"usage"`
}

type responsesEventMessageStart struct {
	Type    string                `json:"type"`
	Message responsesEventMessage `json:"message"`
}

type responsesEventTextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type responsesEventToolBlock struct {
	Type  string `json:"type"` // "tool_use"
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type responsesEventThinkingBlock struct {
	Type      string `json:"type"` // "thinking"
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type responsesEventBlockStart struct {
	Type         string `json:"type"`
	Index        int64  `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type responsesEventBlockDelta struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
	Delta any    `json:"delta"`
}

type responsesEventBlockStop struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
}

type responsesEventTextDelta struct {
	Type string `json:"type"` // "text_delta"
	Text string `json:"text"`
}

type responsesEventThinkingDelta struct {
	Type     string `json:"type"` // "thinking_delta"
	Thinking string `json:"thinking"`
}

type responsesEventSignatureDelta struct {
	Type      string `json:"type"` // "signature_delta"
	Signature string `json:"signature"`
}

type responsesEventInputJSONDelta struct {
	Type        string `json:"type"` // "input_json_delta"
	PartialJSON string `json:"partial_json"`
}

type responsesEventMessageDeltaDelta struct {
	StopReason anthropic.StopReason `json:"stop_reason"`
}

type responsesEventMessageDelta struct {
	Type  string                          `json:"type"`
	Delta responsesEventMessageDeltaDelta `json:"delta"`
	Usage responsesEventUsage             `json:"usage"`
}

type responsesEventMessageStop struct {
	Type string `json:"type"`
}

type responsesEventStreamError struct {
	Type  string                  `json:"type"`
	Error responsesEventErrorBody `json:"error"`
}

type responsesEventErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---- non-streamed responses ----

// parseResponsesResponse translates a complete (stream:false) Responses body into
// an *anthropic.Message, the shape every rafiki caller already consumes. It is
// the non-streaming twin of responsesSSEDecoder and shares its item handling
// rules: function calls become tool_use blocks keyed by call_id, and reasoning
// items become thinking blocks carrying the Responses signature (empty blob when
// the item carried no encrypted content: see finishItem for why it is still
// signed).
func parseResponsesResponse(resp *responsesResponseObject) (*anthropic.Message, error) {
	if resp == nil {
		return nil, fmt.Errorf("llm: openai-responses: response body carries no response object")
	}
	if resp.Error != nil {
		return nil, responsesTerminalError(*resp.Error)
	}
	msg := &anthropic.Message{
		ID:    resp.ID,
		Model: resp.Model,
		Role:  "assistant", // constant.Assistant
		Type:  "message",   // constant.Message
	}
	sawTool := false
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			var sb strings.Builder
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "text" {
					sb.WriteString(c.Text)
				}
			}
			if sb.Len() > 0 {
				msg.Content = append(msg.Content, anthropic.ContentBlockUnion{Type: "text", Text: sb.String()})
			}
		case "function_call":
			input, err := openAIToolCallInput(item.Arguments)
			if err != nil {
				return nil, fmt.Errorf("llm: openai-responses: function call %q: %w", item.CallID, err)
			}
			msg.Content = append(msg.Content, anthropic.ContentBlockUnion{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: input,
			})
			sawTool = true
		case "reasoning":
			// Always signed, even with no encrypted content: the signature is
			// what marks the block as Responses-owned so an Anthropic failover
			// strips it instead of rejecting it (see finishItem).
			msg.Content = append(msg.Content, anthropic.ContentBlockUnion{
				Type:      "thinking",
				Thinking:  responsesSummaryText(item.Summary),
				Signature: responsesSignature(item.ID, item.EncryptedContent),
			})
		}
	}
	reason := ""
	if resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	if reason == "content_filter" {
		return nil, fmt.Errorf("llm: openai-responses: %s", responsesContentFilterMessage)
	}
	switch {
	case reason == "max_output_tokens":
		msg.StopReason = anthropic.StopReasonMaxTokens
	case sawTool:
		msg.StopReason = anthropic.StopReasonToolUse
	default:
		msg.StopReason = anthropic.StopReasonEndTurn
	}
	u := responsesUsageAnthropic(resp.Usage)
	msg.Usage = anthropic.Usage{
		InputTokens:              u.InputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		OutputTokens:             u.OutputTokens,
	}
	return msg, nil
}

// responsesTerminalError turns a non-streamed error object into the error this
// package returns. A usage limit gets its own type; everything else is a plain
// error carrying the endpoint's code and message, which is non-retryable by
// pkg/agentloop's classifiers.
func responsesTerminalError(e responsesAPIError) error {
	env := responsesAPIErrorEnvelope{Error: &e}
	if u := responsesUsageLimitFromEvent(env); u != nil {
		return u
	}
	_, _, message, _ := env.info()
	code := responsesErrorCode(env)
	if code == "context_length_exceeded" {
		return newResponsesPromptTooLargeError(message)
	}
	return fmt.Errorf("llm: openai-responses: %s", responsesErrorText(code, message))
}
