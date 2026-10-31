package msg

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
)

// Accumulator folds a stream of StreamEvents into one Response.
//
// Events must arrive in the order a decoder produces them: a MessageStart, then
// for each block a BlockStart, its deltas and a BlockStop with indices dense
// from 0, then an optional MessageDelta. Add reports a violation rather than
// repairing one, because a stream that breaks that order is a decoder bug, not
// a shape worth guessing at.
type Accumulator struct {
	// resp holds everything but Content, which the blocks carry.
	resp   Response
	blocks []*accBlock
}

// accBlock is one block's accumulation state. block is the block as it stands,
// mutated by deltas and replaced by a finalized BlockStop. input is a ToolUse's
// input buffer, seeded from the start input unless that is a placeholder, and
// discarded when a replacement arrives.
type accBlock struct {
	block Block
	input []byte
}

// errNilEvent is Add's answer to a typed-nil event, which no decoder produces.
var errNilEvent = errors.New("msg: accumulate: nil event")

// NewAccumulator returns an empty accumulator.
func NewAccumulator() *Accumulator { return &Accumulator{} }

// Add folds ev into the response being accumulated. It returns an error for a
// delta or a stop on an index that was never started, a start that repeats an
// index or skips one, a delta whose kind does not match its block, and a
// replacement block of a different kind than the one it replaces.
func (a *Accumulator) Add(ev StreamEvent) error {
	switch e := ev.(type) {
	case MessageStart:
		a.messageStart(e)
	case *MessageStart:
		if e == nil {
			return errNilEvent
		}
		a.messageStart(*e)
	case BlockStart:
		return a.blockStart(e)
	case *BlockStart:
		if e == nil {
			return errNilEvent
		}
		return a.blockStart(*e)
	case BlockDelta:
		return a.blockDelta(e)
	case *BlockDelta:
		if e == nil {
			return errNilEvent
		}
		return a.blockDelta(*e)
	case BlockStop:
		return a.blockStop(e)
	case *BlockStop:
		if e == nil {
			return errNilEvent
		}
		return a.blockStop(*e)
	case MessageDelta:
		a.messageDelta(e)
	case *MessageDelta:
		if e == nil {
			return errNilEvent
		}
		a.messageDelta(*e)
	default:
		return fmt.Errorf("msg: accumulate: unmodelled event %T", ev)
	}
	return nil
}

// HasContent reports whether any block was started.
func (a *Accumulator) HasContent() bool { return len(a.blocks) > 0 }

// Response returns a snapshot of the response accumulated so far. The snapshot
// is safe to retain: its content, every block inside it, and its usage pointers
// are copies, so mutating it cannot affect this accumulator or a later
// snapshot.
func (a *Accumulator) Response() *Response {
	r := &Response{
		ID:            a.resp.ID,
		Model:         a.resp.Model,
		StopReason:    a.resp.StopReason,
		StopReasonRaw: a.resp.StopReasonRaw,
		Usage:         cloneUsage(a.resp.Usage),
		Provider:      a.resp.Provider,
	}
	for _, b := range a.blocks {
		r.Content = append(r.Content, b.snapshot())
	}
	return r
}

func (a *Accumulator) messageStart(e MessageStart) {
	a.resp.ID = e.ID
	a.resp.Model = e.Model
	a.resp.Provider = e.Provider
	a.resp.Usage = cloneUsage(e.Usage)
}

func (a *Accumulator) messageDelta(e MessageDelta) {
	if e.StopReason != StopUnknown {
		a.resp.StopReason = e.StopReason
	}
	if e.StopReasonRaw != "" {
		a.resp.StopReasonRaw = e.StopReasonRaw
	}
	a.resp.Usage = mergeUsage(a.resp.Usage, e.Usage)
}

func (a *Accumulator) blockStart(e BlockStart) error {
	switch {
	case e.Block == nil:
		return fmt.Errorf("msg: accumulate: block start at %d carries no block", e.Index)
	case e.Index < 0:
		return fmt.Errorf("msg: accumulate: block start at negative index %d", e.Index)
	case e.Index < len(a.blocks):
		return fmt.Errorf("msg: accumulate: second start for block %d", e.Index)
	case e.Index > len(a.blocks):
		return fmt.Errorf("msg: accumulate: block start at %d is out of order, next is %d", e.Index, len(a.blocks))
	}
	b := &accBlock{block: cloneBlock(e.Block)}
	if t, ok := b.block.(ToolUse); ok && !placeholderInput(t.Input) {
		b.input = cloneRaw(t.Input)
	}
	a.blocks = append(a.blocks, b)
	return nil
}

func (a *Accumulator) blockDelta(e BlockDelta) error {
	if e.Index < 0 || e.Index >= len(a.blocks) {
		return fmt.Errorf("msg: accumulate: delta for block %d that was never started", e.Index)
	}
	b := a.blocks[e.Index]
	switch blk := b.block.(type) {
	case Text:
		if e.Kind != DeltaText {
			return wrongDelta(e, blk)
		}
		blk.Text += e.Text
		b.block = blk
	case Thinking:
		switch e.Kind {
		case DeltaThinking:
			blk.Text += e.Text
		case DeltaSignature:
			blk.Sig.Value += e.Text
		default:
			return wrongDelta(e, blk)
		}
		b.block = blk
	case ToolUse:
		if e.Kind != DeltaInputJSON {
			return wrongDelta(e, blk)
		}
		b.input = append(b.input, e.Text...)
	default:
		return fmt.Errorf("msg: accumulate: the %T block at %d accepts no deltas", b.block, e.Index)
	}
	return nil
}

func (a *Accumulator) blockStop(e BlockStop) error {
	if e.Index < 0 || e.Index >= len(a.blocks) {
		return fmt.Errorf("msg: accumulate: stop for block %d that was never started", e.Index)
	}
	b := a.blocks[e.Index]
	if e.Block != nil {
		if reflect.TypeOf(e.Block) != reflect.TypeOf(b.block) {
			return fmt.Errorf("msg: accumulate: replacement for block %d is a %T, started as a %T", e.Index, e.Block, b.block)
		}
		b.block = cloneBlock(e.Block)
		b.input = nil
	}
	if t, ok := b.block.(ToolUse); ok {
		in := t.Input
		if e.Block == nil {
			switch {
			case len(b.input) > 0:
				in = b.input
			case len(t.Input) > 0 && !placeholderInput(t.Input):
				in = t.Input
			default:
				in = json.RawMessage(`{}`)
			}
		}
		if !json.Valid(in) {
			slog.Warn("msg: dropping invalid accumulated tool input", "index", e.Index, "input", string(in))
			in = json.RawMessage(`{}`)
		}
		t.Input = in
		b.block = t
	}
	b.input = nil
	return nil
}

// snapshot returns b's block as a deep copy, with an open ToolUse's input read
// from its buffer.
func (b *accBlock) snapshot() Block {
	if t, ok := b.block.(ToolUse); ok {
		switch {
		case len(b.input) > 0:
			t.Input = cloneRaw(b.input)
		case len(t.Input) > 0 && !placeholderInput(t.Input):
			t.Input = cloneRaw(t.Input)
		default:
			t.Input = json.RawMessage(`{}`)
		}
		return t
	}
	return cloneBlock(b.block)
}

// placeholderInput reports whether a ToolUse start input is a stand-in rather
// than arguments: absent, an empty JSON string, or an empty object. The first
// real delta fragment replaces one.
func placeholderInput(in json.RawMessage) bool {
	switch strings.TrimSpace(string(in)) {
	case "", `""`, "{}":
		return true
	}
	return false
}

func wrongDelta(e BlockDelta, blk Block) error {
	return fmt.Errorf("msg: accumulate: %s delta is not valid for the %T block at %d", deltaKindName(e.Kind), blk, e.Index)
}

func deltaKindName(k DeltaKind) string {
	switch k {
	case DeltaText:
		return "text"
	case DeltaThinking:
		return "thinking"
	case DeltaSignature:
		return "signature"
	case DeltaInputJSON:
		return "input_json"
	}
	return fmt.Sprintf("DeltaKind(%d)", uint8(k))
}

// cloneBlock returns a deep copy of b: no slice or byte array in the result
// aliases the original.
func cloneBlock(b Block) Block {
	switch t := b.(type) {
	case Text:
		return t
	case Thinking:
		return t
	case ToolUse:
		t.Input = cloneRaw(t.Input)
		return t
	case ToolResult:
		if t.Content != nil {
			content := make([]Block, len(t.Content))
			for i, c := range t.Content {
				content[i] = cloneBlock(c)
			}
			t.Content = content
		}
		return t
	case Image:
		t.Data = cloneBytes(t.Data)
		return t
	case Opaque:
		t.Raw = cloneRaw(t.Raw)
		return t
	}
	return b
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func cloneUsage(u Usage) Usage {
	return Usage{
		Input:      cloneCount(u.Input),
		Output:     cloneCount(u.Output),
		CacheRead:  cloneCount(u.CacheRead),
		CacheWrite: cloneCount(u.CacheWrite),
		Reasoning:  cloneCount(u.Reasoning),
	}
}

func cloneCount(p *int64) *int64 {
	if p == nil {
		return nil
	}
	return Ptr(*p)
}

// mergeUsage folds a delta's usage into the running totals. Input, CacheRead
// and CacheWrite only move into a field the provider had not reported yet or
// upward, because a delta that repeats a count as zero is not evidence the
// count was zero. Output and Reasoning are final totals, so a reported value
// always wins.
func mergeUsage(cur, d Usage) Usage {
	cur.Input = mergeCounted(cur.Input, d.Input)
	cur.Output = mergeReported(cur.Output, d.Output)
	cur.CacheRead = mergeCounted(cur.CacheRead, d.CacheRead)
	cur.CacheWrite = mergeCounted(cur.CacheWrite, d.CacheWrite)
	cur.Reasoning = mergeReported(cur.Reasoning, d.Reasoning)
	return cur
}

func mergeCounted(cur, d *int64) *int64 {
	if d == nil {
		return cur
	}
	if *d > 0 || cur == nil {
		return Ptr(*d)
	}
	return cur
}

func mergeReported(cur, d *int64) *int64 {
	if d == nil {
		return cur
	}
	return Ptr(*d)
}
