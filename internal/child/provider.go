package child

// ProtocolProvider abstracts a child's wire protocol: how to kick it off, how
// to turn its stdout lines into normalized state-machine signals + metadata,
// how to translate its stdout into pi AgentSessionEvent frames for the bus, and
// how to encode outbound frames for its stdin.
//
// Concurrency: Parse and BusFrames are called only from the readStdout
// goroutine (in that order, per line), and EncodeOutbound + BootstrapFrame +
// OutboundEcho only from the supervise goroutine. These two goroutines run
// concurrently, so any per-child state touched by BOTH sides (e.g. the claude
// translator's message list, appended by BusFrames and by OutboundEcho) must be
// guarded; state touched by only one side needs no lock. Each Child obtains its
// own instance via Fresh() in Spawn, so stateful providers (ClaudeProvider)
// never share state across children.
type ProtocolProvider interface {
	// Fresh returns a per-child instance of this provider. Stateless providers
	// (PiProvider) may return an equivalent value; stateful translators
	// (ClaudeProvider) MUST return a distinct instance so per-child accumulation
	// (messages, pending tool calls, turn flag) is isolated.
	Fresh() ProtocolProvider

	// BootstrapFrame returns a frame to write to the child's stdin immediately
	// after the write loop starts, or nil if the child needs no kickoff.
	BootstrapFrame() []byte

	// ReadyOnSpawn reports whether the child is ready for input the instant its
	// process launches, rather than after a stdout readiness signal. pi returns
	// false: it must be probed (BootstrapFrame) and waits for response.get_state.
	// claude returns true: it is silent on stdout until prompted, so there is no
	// signal to wait for; the Child fires spawning→idle on launch. When true the
	// provider's Parse FirstResponse is not relied upon for the initial readiness.
	ReadyOnSpawn() bool

	// Parse decodes one stdout line into normalized signals. An unparseable or
	// irrelevant line returns the zero ParseResult (a no-op). Parse drives the
	// state machine + sniffed metadata and is independent of BusFrames.
	Parse(line []byte) ParseResult

	// BusFrames translates one raw stdout line into zero or more pi
	// AgentSessionEvent frames to publish on the bus, in order. ts is the frame
	// arrival time in unix milliseconds (used for synthesized message
	// timestamps). Identity providers (PiProvider) return the raw line; stateful
	// translators (ClaudeProvider) accumulate state and emit the pi event
	// sequence. The RAW line is what the ring stores; BusFrames output is what
	// the bus carries.
	BusFrames(line []byte, ts int64) [][]byte

	// EncodeOutbound translates a normalized outbound frame (as sent by clients
	// via ctrl_send: {"type":"prompt"|"steer"|"abort"|...}) into the child's
	// native stdin envelope. Returning nil drops the frame (unsupported for this
	// protocol). Providers whose native protocol already matches the normalized
	// vocabulary return frame unchanged.
	EncodeOutbound(frame []byte) []byte

	// OutboundEcho returns pi AgentSessionEvent frames to publish on the bus when
	// an outbound frame is sent to the child, or nil. It exists for providers
	// whose child does not echo the user's own prompt on its stdout (claude emits
	// user frames only for tool results): without it the user's typed message
	// never reaches the bus, so the TUI — which renders a user bubble solely from
	// a message_start(role:user) event — shows nothing. Identity providers whose
	// child echoes user input natively (PiProvider) return nil to avoid a double
	// render. ts is the send time in unix milliseconds. Called from the supervise
	// goroutine; see the concurrency note above re: state shared with BusFrames.
	OutboundEcho(frame []byte, ts int64) [][]byte

	// Normalizes reports whether the provider translates the child's raw stdout
	// into a different pi-vocabulary bus stream (claude), versus the raw stdout
	// already being the bus stream (pi, identity). When true the Child maintains
	// a render-ring capturing the bus output, since the raw ring alone is not
	// renderable.
	Normalizes() bool
}

// ParseResult is the normalized outcome of parsing one stdout line.
type ParseResult struct {
	// FirstResponse is true on the line that signals the child finished starting
	// and is ready for input (pi: response.get_state). It drives the
	// spawning→idle transition and closes the Idle() channel exactly once.
	// Providers that are ReadyOnSpawn (claude) instead have the Child fire this
	// transition on launch; their Parse need not report FirstResponse for initial
	// readiness (claude is silent on stdout until prompted).
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
