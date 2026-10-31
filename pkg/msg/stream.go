package msg

// StreamEvent is one event from a provider's streamed completion. The interface
// is sealed by design: only this package's event types satisfy it.
type StreamEvent interface{ streamEvent() }

// MessageStart opens a streamed response with its identity and initial usage.
type MessageStart struct {
	ID       string
	Model    string
	Provider string
	Usage    Usage
}

// BlockStart opens content block Index. A Text or Thinking block may carry
// initial text; a ToolUse carries its id, name and initial input; an Opaque
// block arrives complete, because only its codec knows its shape.
type BlockStart struct {
	Index int
	Block Block
}

// DeltaKind is what a BlockDelta appends to.
type DeltaKind uint8

const (
	// DeltaText appends to a Text block.
	DeltaText DeltaKind = iota + 1
	// DeltaThinking appends to a Thinking block's text.
	DeltaThinking
	// DeltaSignature appends to a Thinking block's signature.
	DeltaSignature
	// DeltaInputJSON appends to a ToolUse block's input JSON.
	DeltaInputJSON
)

// BlockDelta appends Text to the block at Index according to Kind.
type BlockDelta struct {
	Index int
	Kind  DeltaKind
	Text  string
}

// BlockStop closes the block at Index. A non-nil Block is the finalized block,
// which replaces the accumulated one; nil keeps what was accumulated.
type BlockStop struct {
	Index int
	Block Block
}

// MessageDelta closes a streamed response with its stop reason and final usage.
type MessageDelta struct {
	StopReason StopReason
	// StopReasonRaw is the stop reason exactly as the wire spelled it, carried
	// for the same reason Response.StopReasonRaw is: a value the enum does not
	// model must still reach the record.
	StopReasonRaw string
	Usage         Usage
}

// StreamDecoder yields a response's events in order.
type StreamDecoder interface {
	// Next returns the next event, or io.EOF after the last event of a clean
	// stream.
	Next() (StreamEvent, error)
	// Close releases the stream.
	Close() error
}

func (MessageStart) streamEvent() {}
func (BlockStart) streamEvent()   {}
func (BlockDelta) streamEvent()   {}
func (BlockStop) streamEvent()    {}
func (MessageDelta) streamEvent() {}

// DeliversContent reports whether ev carries content a caller must forward as
// it arrives: a block starting, growing or finishing.
func DeliversContent(ev StreamEvent) bool {
	switch ev.(type) {
	case BlockStart, *BlockStart, BlockDelta, *BlockDelta, BlockStop, *BlockStop:
		return true
	}
	return false
}
