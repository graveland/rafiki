package child

// ProtocolProvider abstracts a child's wire protocol: how to kick it off, how
// to turn its stdout lines into normalized state-machine signals + metadata,
// and how to encode outbound frames for its stdin.
//
// Concurrency: Parse is called only from the readStdout goroutine and
// EncodeOutbound + BootstrapFrame only from the supervise goroutine, so a
// provider that keeps per-child parsing state needs no internal locks. The
// built-in providers (PiProvider, ClaudeProvider) are stateless value types.
type ProtocolProvider interface {
	// BootstrapFrame returns a frame to write to the child's stdin immediately
	// after the write loop starts, or nil if the child needs no kickoff.
	BootstrapFrame() []byte

	// Parse decodes one stdout line into normalized signals. An unparseable or
	// irrelevant line returns the zero ParseResult (a no-op).
	Parse(line []byte) ParseResult

	// EncodeOutbound translates a normalized outbound frame (as sent by clients
	// via ctrl_send: {"type":"prompt"|"steer"|"abort"|...}) into the child's
	// native stdin envelope. Returning nil drops the frame (unsupported for this
	// protocol). Providers whose native protocol already matches the normalized
	// vocabulary return frame unchanged.
	EncodeOutbound(frame []byte) []byte
}

// ParseResult is the normalized outcome of parsing one stdout line.
type ParseResult struct {
	// FirstResponse is true on the single line that signals the child finished
	// starting and is ready for input (pi: response.get_state; claude: the
	// system/init line). It drives the spawning→idle transition and closes the
	// Idle() channel exactly once.
	FirstResponse bool

	// Meta carries session/model fields found on this line; only honored when
	// HasMeta is true.
	Meta    SnifferMetadata
	HasMeta bool

	// Events are normalized state-machine events applied in order. Type strings
	// match the StateMachine vocabulary: agent_start, agent_end,
	// tool_execution_start, tool_execution_end, compaction_start,
	// compaction_end, extension_ui_request, auto_retry_start, extension_error.
	Events []ParsedEvent
}

// ParsedEvent is one normalized state-machine event.
type ParsedEvent struct {
	Type       string
	UI         *PiUIRequestMeta // non-nil only for extension_ui_request
	RetryError string           // set only for auto_retry_start
}
