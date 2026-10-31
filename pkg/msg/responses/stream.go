package responses

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
)

// defaultMaxLine caps one SSE line. It is not a formality: the endpoint sends
// whole output items on a single `data:` line, both on response.output_item.done
// and on response.completed, and a reasoning item's encrypted_content is a
// multi-kilobyte (sometimes multi-megabyte) base64 blob. bufio.Scanner's 64 KiB
// default fails the moment a turn has reasoning in it, which is every turn that
// asked for it.
const defaultMaxLine = 64 << 20

// The kinds of content block a Responses stream can produce, tracked per open
// block so a delta knows which block it belongs to.
const (
	blockText     = "text"
	blockTool     = "tool"
	blockThinking = "thinking"
)

// NewStreamDecoder adapts a Responses text/event-stream body to native stream
// events.
//
// idle is how long the decoder waits for the next line before declaring the
// connection dead; zero or less disables the watchdog. maxLine caps a single
// line; zero or less means 64 MiB.
//
// A codec with an unknown Mode yields a decoder that reports the misconfiguration
// on its first Next rather than a stream that silently drops every reasoning item
// (see Codec.check).
func (c Codec) NewStreamDecoder(r io.ReadCloser, idle time.Duration, maxLine int) msg.StreamDecoder {
	if err := c.check(); err != nil {
		return &streamDecoder{rc: r, err: err, done: true, closeCh: make(chan struct{})}
	}
	if maxLine <= 0 {
		maxLine = defaultMaxLine
	}
	d := &streamDecoder{
		rc:           r,
		idle:         idle,
		maxLine:      maxLine,
		mode:         c.Mode,
		lines:        make(chan streamLine, 1),
		closeCh:      make(chan struct{}),
		closedBlocks: map[int]bool{},
	}
	go d.readLoop()
	return d
}

// streamDecoder translates each SSE frame into zero or more native stream
// events.
//
// Failures split in two, and the split is the part that matters:
//
//   - A condition a retry can plausibly fix — a dead connection, a silent
//     backend, a truncated body, an unparseable payload — becomes an
//     *msg.APIError whose Kind is ErrTransient.
//   - A condition the same request reproduces — an over-long line — is
//     ErrOther, which is not retryable.
//
// Two endpoint conditions carry their own types instead: a subscription usage
// limit is a *UsageLimitError (never an APIError), and a context-length overflow
// is an *msg.APIError whose Kind is ErrPromptTooLarge, which is what the trim
// path keys on.
//
// One content block is open at a time, because a delta always applies to the
// most recently started block.
type streamDecoder struct {
	rc      io.ReadCloser
	idle    time.Duration
	maxLine int
	// mode is the Responses dialect this stream came from, and so the mode its
	// reasoning signatures are minted under. The encrypted content is only
	// meaningful to the backend that produced it.
	mode Mode

	// lines is fed by the ONE reader goroutine, which is the only code that
	// touches the bufio.Reader. Two readers on one buffered reader is a data race
	// and a torn frame, so the timeout path stops the reader instead of racing it
	// (see readLine).
	lines    chan streamLine
	closeCh  chan struct{}
	stopOnce sync.Once
	stopErr  error // set by stopReading, read after it

	queue  []msg.StreamEvent
	err    error // the terminal error, delivered once
	closed bool
	done   bool

	// Translation state.
	started      bool // MessageStart emitted
	model        string
	blocksIssued int // content-block indexes handed out
	toolCalls    int // function_call blocks emitted, for the stop reason
	active       *openBlock
	closedBlocks map[int]bool // output indexes whose block is already closed
}

// streamLine is one line handed from the reader goroutine to the decoder.
type streamLine struct {
	line []byte
	err  error
}

// openBlock is the content block currently being streamed, keyed by the
// endpoint's output_index (its own per-response item counter).
type openBlock struct {
	outputIndex int
	index       int    // native block index
	kind        string // blockText / blockTool / blockThinking
	itemID      string // reasoning item id, for the signature
	sawDeltas   bool   // the block already streamed its own content
}

// readLoop is the ONLY goroutine that reads the body. It hands one line at a time
// to the decoder and exits on the first error (or when the decoder closes).
//
// The send selects on closeCh so a consumer that stops reading (a terminal event
// reached, Close called) can never block this goroutine forever.
func (d *streamDecoder) readLoop() {
	defer close(d.lines)
	br := bufio.NewReaderSize(d.rc, 64<<10)
	for {
		line, err := d.readLineCapped(br)
		select {
		case d.lines <- streamLine{line: line, err: err}:
		case <-d.closeCh:
			return
		}
		if err != nil {
			return
		}
	}
}

// Next returns the next event, the terminal error once, then io.EOF.
func (d *streamDecoder) Next() (msg.StreamEvent, error) {
	if d.closed {
		return nil, io.EOF
	}
	for len(d.queue) == 0 && d.err == nil && !d.done {
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
		d.done = true
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the stream.
func (d *streamDecoder) Close() error {
	d.closed = true
	d.stopReading(errClosed)
	return nil
}

// stopReading stops the reader goroutine for good: it closes closeCh (so a
// pending send cannot block) and closes the body (so a blocked read returns).
// Exactly one goroutine may own the reader, so once this has run there is no
// further read, whoever calls it.
func (d *streamDecoder) stopReading(cause error) {
	d.stopOnce.Do(func() {
		d.stopErr = cause
		close(d.closeCh)
		_ = d.rc.Close()
	})
}

// pull reads one SSE frame (its `event:` and `data:` lines up to the next blank
// line) and dispatches it. Comment lines are skipped; split data lines are joined
// with '\n' as the SSE spec requires.
//
// A frame is dispatched only when its terminating blank line arrived. A read
// failure mid-frame (a timeout, a truncated body, an over-long line) never
// dispatches what it collected: a half frame is not an event, and treating one as
// an event can turn a partial response.completed into a completed turn.
func (d *streamDecoder) pull() {
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
//     it is not retryable;
//   - any other read failure, and a stream that ends without a terminal event, is
//     transient: a dead connection or a silent backend is worth re-issuing.
//
// partial says a frame was cut short by the failure, which is always reported as
// truncated (it is never dispatched).
func (d *streamDecoder) failRead(err error, partial bool) {
	switch {
	case errors.Is(err, errLineTooLong):
		d.failWith(&msg.APIError{Kind: msg.ErrOther, Dialect: d.mode.Dialect(), Message: err.Error()})
	case err != nil && !errors.Is(err, io.EOF):
		d.failWith(&msg.APIError{
			Kind:    msg.ErrTransient,
			Dialect: d.mode.Dialect(),
			Message: fmt.Sprintf("stream read failed: %v", err),
		})
	case d.done:
		// A terminal event was already processed; the stream simply ended.
	case partial || err != nil:
		d.failWith(&msg.APIError{
			Kind:    msg.ErrTransient,
			Dialect: d.mode.Dialect(),
			Message: "stream ended without response.completed, response.incomplete or response.failed (response truncated)",
		})
	default:
		d.done = true
	}
}

// failWith records the terminal error the next Next reports.
func (d *streamDecoder) failWith(err error) {
	if d.err == nil {
		d.err = err
	}
	d.done = true
}

// readLine returns one line INCLUDING its terminator (which the caller may
// ignore), or the bytes read before EOF alongside that error.
//
// Reading is time-capped when an idle timeout is configured. The timeout does not
// race the reader: it stops it (closing the body) and reports the timeout, so
// after it fires no code reads the buffered reader ever again.
func (d *streamDecoder) readLine() ([]byte, error) {
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
			d.stopReading(errIdleTimeout)
			return nil, errIdleTimeout
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
func (d *streamDecoder) readEnded() error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return io.EOF
}

// errIdleTimeout marks the idle-timeout read failure. It never escapes as a
// value: pull turns it into a transient stream error, because a silent backend is
// a retryable condition, not a broken turn.
var errIdleTimeout = fmt.Errorf("idle timeout")

// errClosed marks a stream the caller closed before it finished. It is not an
// error to report: reading simply stops.
var errClosed = fmt.Errorf("stream closed")

// errLineTooLong is the sentinel every overflow matches, whatever the cap was.
var errLineTooLong = &lineTooLongError{}

// lineTooLongError marks a line that exceeded the cap. It is DETERMINISTIC (the
// same request produces the same oversized line), so it is classified as
// non-retryable; a malformed payload, which may be a one-off corruption, stays
// transient.
type lineTooLongError struct {
	limit int
}

func (e *lineTooLongError) Error() string {
	return fmt.Sprintf("responses: stream line exceeds %d bytes", e.limit)
}

func (e *lineTooLongError) Is(target error) bool {
	_, ok := target.(*lineTooLongError)
	return ok
}

// readLineCapped reads one line, allowing up to d.maxLine. It is the explicit
// replacement for bufio.Scanner's fixed 64 KiB ceiling (see defaultMaxLine). Only
// the reader goroutine calls it.
func (d *streamDecoder) readLineCapped(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > d.maxLine {
			return nil, &lineTooLongError{limit: d.maxLine}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// dispatch translates one SSE payload. eventName is the frame's `event:` field,
// used only when the payload itself carries no type.
func (d *streamDecoder) dispatch(data []byte, eventName string) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		d.failWith(&msg.APIError{
			Kind:    msg.ErrTransient,
			Dialect: d.mode.Dialect(),
			Message: fmt.Sprintf("stream event failed to parse: %v: %.500s", err, trimmed),
		})
		return
	}
	typ := ev.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "response.created":
		d.created(ev.Response)
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
		d.ensureBlock(outputIndex(ev.OutputIndex), blockText, "")
		if d.active != nil && d.active.kind == blockText {
			d.emitDelta(d.active.index, msg.DeltaText, ev.Delta)
		}
	case "response.function_call_arguments.delta":
		d.ensureStarted()
		// The arguments belong to the tool_use block the item's `added` event
		// opened. Without one there is no call_id to name the block, so the
		// fragment is dropped rather than attached to a block that would be
		// rejected for having no id.
		if d.active != nil && d.active.kind == blockTool &&
			d.active.outputIndex == outputIndex(ev.OutputIndex) && ev.Delta != "" {
			d.active.sawDeltas = true
			d.emitDelta(d.active.index, msg.DeltaInputJSON, ev.Delta)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		d.ensureStarted()
		idx := outputIndex(ev.OutputIndex)
		d.ensureBlock(idx, blockThinking, ev.ItemID)
		if d.active != nil && d.active.kind == blockThinking && d.active.outputIndex == idx {
			if ev.Delta != "" {
				d.active.sawDeltas = true
				d.emitDelta(d.active.index, msg.DeltaThinking, ev.Delta)
			}
		}
	case "response.completed":
		d.complete(ev.Response, false)
	case "response.incomplete":
		d.complete(ev.Response, true)
	case "response.failed", "error":
		d.fail(trimmed)
	default:
		// response.in_progress, response.metadata (moderation data),
		// response.content_part.*, response.output_text.done,
		// response.function_call_arguments.done, response.reasoning_summary_part.*,
		// response.custom_tool_call_input.* and anything newer than this decoder:
		// nothing native-shaped to say.
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

// ensureStarted emits MessageStart the first time anything needs a message.
// response.created normally does it, but a decoder that sees only item events must
// still produce a well-formed stream.
func (d *streamDecoder) ensureStarted() {
	if d.started {
		return
	}
	d.started = true
	d.emitMessageStart("", d.model)
}

// created handles response.created, which carries the response's identity.
func (d *streamDecoder) created(resp *responseObject) {
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

// openItem starts the content block for one output item. Items with no native
// counterpart (custom_tool_call, custom_tool_call_output, anything newer) are
// ignored.
func (d *streamDecoder) openItem(item outputItem, idx int) {
	switch item.Type {
	case "message":
		d.openBlock(idx, blockText)
		d.emitBlockStart(d.active.index, msg.Text{})
	case "function_call":
		d.openBlock(idx, blockTool)
		d.toolCalls++
		d.emitBlockStart(d.active.index, msg.ToolUse{
			ID:    item.CallID,
			Name:  item.Name,
			Input: json.RawMessage("{}"),
		})
	case "reasoning":
		d.openBlock(idx, blockThinking)
		d.active.itemID = item.ID
		d.emitBlockStart(d.active.index, msg.Thinking{Sig: msg.Signature{Dialect: d.mode.Dialect()}})
	}
}

// finishItem closes the block for one output item. A reasoning item gets its
// signature first (mode, id and encrypted content), and that signature is what
// makes the reasoning resendable on the next turn by the backend that minted it.
//
// A reasoning item with NO encrypted content is still signed, with an empty blob.
// The signature is not only a resend token: it is how the block announces which
// provider owns it. An unsigned thinking block is indistinguishable from an
// Anthropic one, so a failover to an Anthropic provider would keep it and then
// reject a signature it cannot verify. With the empty-blob signature the block is
// recognisably ours (Anthropic strips it) while this codec still drops it when
// resending (the parsed blob is empty, so there is nothing to resume).
//
// A done for an index whose block is already closed (a late event under
// interleaving) is ignored: reopening it would duplicate a tool_use block.
func (d *streamDecoder) finishItem(item outputItem, idx int) {
	if d.closedBlocks[idx] {
		return
	}
	if d.active == nil || d.active.outputIndex != idx {
		// The item had no `added` event, so nothing is open for it. Open it now so
		// a done-only item still reaches the caller, then close it below.
		d.openItem(item, idx)
		if d.active == nil {
			return
		}
	}
	switch d.active.kind {
	case blockThinking:
		if !d.active.sawDeltas {
			// Some models deliver the summary only in the completed item.
			if text := summaryText(item.Summary); text != "" {
				d.emitDelta(d.active.index, msg.DeltaThinking, text)
			}
		}
		id := item.ID
		if id == "" {
			id = d.active.itemID
		}
		d.emitDelta(d.active.index, msg.DeltaSignature, Signature(d.mode, id, item.EncryptedContent))
	case blockTool:
		if item.Arguments != "" && !d.active.sawDeltas {
			// No delta carried the arguments, so the completed item's own string is
			// the whole input.
			d.emitDelta(d.active.index, msg.DeltaInputJSON, item.Arguments)
		}
	}
	d.closeActiveBlock()
}

// complete handles response.completed and response.incomplete.
func (d *streamDecoder) complete(resp *responseObject, incomplete bool) {
	d.ensureStarted()
	d.closeActiveBlock()
	reason := ""
	if resp != nil && resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	if incomplete && reason == reasonContentFilter {
		d.failWith(&msg.APIError{Kind: msg.ErrOther, Dialect: d.mode.Dialect(), Message: contentFilterMessage})
		return
	}
	stop := msg.StopEndTurn
	switch {
	case incomplete && reason == reasonMaxOutputTokens:
		stop = msg.StopMaxTokens
	case d.toolCalls > 0:
		stop = msg.StopToolUse
	case resp != nil && resp.Status == "incomplete" && reason == reasonMaxOutputTokens:
		stop = msg.StopMaxTokens
	}
	var usage *usageWire
	if resp != nil {
		usage = resp.Usage
	}
	d.emitMessageDelta(stop, reason, usageOf(usage))
	d.done = true
}

// fail handles response.failed and the top-level error event, whose payload names
// the endpoint error wherever its own shape put it.
//
// No MessageStart is emitted: a stream that failed produced no message, and the
// caller is about to discard whatever it accumulated.
func (d *streamDecoder) fail(data []byte) {
	d.closeActiveBlock()
	env := decodeErrorEnvelope(data)
	if u := usageLimitFromEnvelope(env); u != nil {
		d.failWith(u)
		return
	}
	_, _, message, _ := env.info()
	d.failWith(&msg.APIError{
		Status:  0,
		Body:    append([]byte(nil), data...),
		Kind:    Classify(0, nil, data),
		Dialect: d.mode.Dialect(),
		Message: message,
	})
}

// ensureBlock makes idx's block of the given kind the open one, starting it if it
// is not already. Starts of a different kind or a different output index close
// whatever was open first.
func (d *streamDecoder) ensureBlock(idx int, kind, itemID string) {
	if d.active != nil && d.active.outputIndex == idx && d.active.kind == kind {
		return
	}
	d.openBlock(idx, kind)
	switch kind {
	case blockText:
		d.emitBlockStart(d.active.index, msg.Text{})
	case blockThinking:
		d.active.itemID = itemID
		d.emitBlockStart(d.active.index, msg.Thinking{Sig: msg.Signature{Dialect: d.mode.Dialect()}})
	}
}

// openBlock closes the current block and issues the next native index. Indexes are
// sequential because the accumulator appends a block per start.
func (d *streamDecoder) openBlock(idx int, kind string) {
	d.closeActiveBlock()
	d.active = &openBlock{outputIndex: idx, index: d.blocksIssued, kind: kind}
	d.blocksIssued++
}

// closeActiveBlock emits BlockStop for the open block, if any.
func (d *streamDecoder) closeActiveBlock() {
	if d.active == nil {
		return
	}
	d.emitBlockStop(d.active.index)
	// Remember the index: a later done for a block that is no longer open must not
	// reopen it (see finishItem).
	d.closedBlocks[d.active.outputIndex] = true
	d.active = nil
}

func (d *streamDecoder) emit(ev msg.StreamEvent) { d.queue = append(d.queue, ev) }

func (d *streamDecoder) emitMessageStart(id, model string) {
	d.emit(msg.MessageStart{ID: id, Model: model})
}

func (d *streamDecoder) emitBlockStart(index int, block msg.Block) {
	d.emit(msg.BlockStart{Index: index, Block: block})
}

func (d *streamDecoder) emitDelta(index int, kind msg.DeltaKind, text string) {
	d.emit(msg.BlockDelta{Index: index, Kind: kind, Text: text})
}

func (d *streamDecoder) emitBlockStop(index int) {
	d.emit(msg.BlockStop{Index: index})
}

func (d *streamDecoder) emitMessageDelta(stop msg.StopReason, stopRaw string, usage msg.Usage) {
	d.emit(msg.MessageDelta{StopReason: stop, StopReasonRaw: stopRaw, Usage: usage})
}

// streamEvent is one SSE payload. Only the fields this translation consumes are
// declared; the endpoint sends many more.
type streamEvent struct {
	Type        string          `json:"type"`
	Response    *responseObject `json:"response"`
	Item        *outputItem     `json:"item"`
	OutputIndex *int            `json:"output_index"`
	ItemID      string          `json:"item_id"`
	Delta       string          `json:"delta"`
}
