package child

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// claudeProvider is the per-child stateful translator returned by
// ClaudeProvider.Fresh(). It embeds the stateless ClaudeProvider so it inherits
// BootstrapFrame / Parse / EncodeOutbound / Fresh unchanged, and overrides
// BusFrames to translate claude's raw stream-json into the pi AgentSessionEvent
// sequence. State fields accumulate across the child's lifetime; access is
// single-goroutine (readStdout), so no locking is needed.
type claudeProvider struct {
	ClaudeProvider
	st claudeState
}

// claudeState holds the per-child translation accumulators:
//   - messages: the ordered user/assistant/toolResult message list, pre-
//     marshalled, replayed in full on every agent_end. Appended by BusFrames
//     (readStdout goroutine) AND by OutboundEcho (supervise goroutine), so it is
//     guarded by mu; the other fields are touched only by BusFrames, except
//     nativeTurnActive, which the prompt echo raises (also under mu).
//   - model/provider/api: captured from system/init, stamped on every
//     synthesized assistant message.
//   - turnActive: whether a turn (agent_start) is currently open, so the first
//     assistant/user frame of a turn opens it exactly once.
type claudeState struct {
	mu         sync.Mutex
	messages   []json.RawMessage
	model      string
	provider   string
	api        string
	turnActive bool
	// nativeTurnActive tracks the same fact for the NATIVE path, and cannot
	// share turnActive: readStdout runs BusFrames before BusFramesNative on
	// every line, so openTurn has already raised turnActive by the time the
	// native translator sees the same frame. It gates the TurnStart, the one
	// event the translator owns besides tool events: message events come from
	// persisted rows (cmd/rafikid/capture_sink.go), not from this translator.
	// It is raised by the prompt echo (OutboundEchoNative, on the supervise
	// goroutine) so the turn is open before claude's first request reaches the
	// proxy, and cleared by nativeResult. Because two goroutines touch it,
	// every access is under mu — unlike the other state fields, which only
	// BusFrames/ResetState touch.
	nativeTurnActive bool
	// lastTotalCost is the session total_cost_usd the previous priced result
	// frame reported. claude's stream-json result restates the RUNNING total for
	// the whole process on every turn, while TurnEnd.cost_usd is one turn's cost
	// and every consumer sums it, so turnCost reports the difference. Under mu
	// like nativeTurnActive. ResetState deliberately leaves it: a reset does not
	// say whether claude zeroed its counter, and turnCost already rebaselines
	// when the total goes down.
	lastTotalCost float64
}

// newClaudeProvider constructs a fresh per-child claude translator.
func newClaudeProvider() *claudeProvider { return &claudeProvider{} }

// appendMessage records a pre-marshalled message under the lock. Called from
// both the readStdout goroutine (BusFrames) and the supervise goroutine
// (OutboundEcho), hence the guard.
func (p *claudeProvider) appendMessage(raw json.RawMessage) {
	p.st.mu.Lock()
	p.st.messages = append(p.st.messages, raw)
	p.st.mu.Unlock()
}

// ResetState clears every accumulator back to a fresh child's state. Called
// by readStdout when the underlying claude process has been silently
// replaced (a daraja Restart — deliberate, via an interrupt, or daraja's own
// crash-respawn) so the translator does not carry a stale turnActive/model/
// message history into frames from the NEW process. Takes the same lock
// appendMessage does: readStdout is the only caller, but messages is also
// touched by the supervise goroutine (OutboundEcho), so clearing it needs the
// guard even though model/provider/api/turnActive do not.
func (p *claudeProvider) ResetState() {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	p.st.messages = nil
	p.st.model = ""
	p.st.provider = ""
	p.st.api = ""
	p.st.turnActive = false
	p.st.nativeTurnActive = false
}

// snapshotMessages returns a copy of the accumulated messages under the lock,
// for replay in agent_end.
func (p *claudeProvider) snapshotMessages() []json.RawMessage {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	msgs := make([]json.RawMessage, len(p.st.messages))
	copy(msgs, p.st.messages)
	return msgs
}

// claudeUserEcho builds the synthetic user-message echo for an outbound
// prompt/steer frame: the PiUserMessage plus its message_start/message_end bus
// frames. ok is false for frames carrying no user-authored text (abort,
// set_session_name, empty message, ...). Pure (no state), so the stateless
// ClaudeProvider template and the per-child *claudeProvider share it.
//
// claude's stdout never carries the user's own prompt text (its user frames hold
// only tool_result blocks — see emitUser), so without publishing this the TUI
// never receives a message_start(role:user) and the user's typed message is
// invisible even though it reached the model.
func claudeUserEcho(frame []byte, ts int64) (PiUserMessage, [][]byte, bool) {
	var in struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(frame, &in); err != nil {
		return PiUserMessage{}, nil, false
	}
	if in.Type != "prompt" && in.Type != "steer" {
		return PiUserMessage{}, nil, false
	}
	if in.Message == "" {
		return PiUserMessage{}, nil, false
	}
	msg := PiUserMessage{
		Role:      "user",
		ID:        fmt.Sprintf("user-%d", ts),
		Content:   in.Message,
		Timestamp: ts,
	}
	return msg, [][]byte{mustFrame(PiUserMessageStart(msg)), mustFrame(PiUserMessageEnd(msg))}, true
}

// OutboundEcho (per-child, stateful) records the user message in state so the
// turn's agent_end replays it, then returns the echo bus frames. Runs on the
// supervise goroutine concurrently with BusFrames; the append is mutex-guarded.
func (p *claudeProvider) OutboundEcho(frame []byte, ts int64) [][]byte {
	msg, frames, ok := claudeUserEcho(frame, ts)
	if !ok {
		return nil
	}
	if raw, err := json.Marshal(msg); err == nil {
		p.appendMessage(raw)
	}
	return frames
}

// OutboundEchoNative is the rafiki-native sibling of OutboundEcho, reduced to
// opening the turn.
//
// It reuses claudeUserEcho's parse rather than re-deciding which frames carry
// user text: that judgment (reject abort, set_session_name, empty messages)
// exists once and a second copy would drift from it.
//
// It emits no UserMessage: the user's prompt reaches the native stream from its
// persisted conversation_message row, published by the daemon's capture sink
// (cmd/rafikid/capture_sink.go) with the row's created_at. What this owns is the
// TurnStart, opened HERE at prompt delivery rather than on claude's first
// assistant frame, because claude's first request reaches the proxy before that
// frame arrives; an assistant-frame open left the turn's first `chat` span an
// orphan root (and a one-call turn with no nested chat span at all).
// openNativeTurn's guard makes the first assistant frame a no-op, and a second
// prompt/steer inside the same turn publishes no second TurnStart. The model is
// left empty because no frame names one yet; the observer falls back to the
// child's metadata model.
//
// It does NOT append to p.st.messages — OutboundEcho already did that for the
// pi path, and both run on the supervise goroutine for the same frame.
func (p *claudeProvider) OutboundEchoNative(frame []byte, ts int64) []*rafikiv1.Event {
	if _, _, ok := claudeUserEcho(frame, ts); !ok {
		return nil
	}
	return p.openNativeTurn("", ts)
}

// claudeStreamFrame is the full claude stream-json envelope BusFrames needs to
// translate. It is richer than the minimal claudeFrame used by Parse: it decodes
// content blocks (text/thinking/tool_use/tool_result) and the result text.
type claudeStreamFrame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Model   string `json:"model,omitempty"`
	Result  string `json:"result,omitempty"`
	// ParentToolUseID is set on assistant/user frames produced by a sub-agent
	// (the Task tool); it names the spawning tool call so the activity can be
	// attributed and indented downstream.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	// Result-frame token/cost totals for the turn (present only on type=result).
	TotalCostUSD float64      `json:"total_cost_usd,omitempty"`
	Usage        *claudeUsage `json:"usage,omitempty"`
	Message      *struct {
		ID      string               `json:"id"`
		Role    string               `json:"role"`
		Model   string               `json:"model,omitempty"`
		Content []claudeContentBlock `json:"content"`
	} `json:"message,omitempty"`
	// CompactMetadata is present on system/compact_boundary frames (Claude
	// Code's own compaction having just run). Absent otherwise.
	CompactMetadata *struct {
		Trigger    string `json:"trigger,omitempty"`
		PreTokens  int    `json:"pre_tokens,omitempty"`
		PostTokens int    `json:"post_tokens,omitempty"`
	} `json:"compact_metadata,omitempty"`
}

// claudeUsage is the subset of claude's result-frame usage we surface: turn
// token counts. The per-category cost breakdown isn't provided (only a single
// total_cost_usd), so only the total cost is carried.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// SubagentObservation pairs a Claude Code assistant message with the Task tool
// call that spawned the subagent producing it. The message id is the join key
// to conversation_turn.response_message_id, which is how the daemon finds the
// thread and therefore the synthetic child.
type SubagentObservation struct {
	MessageID       string
	ParentToolUseID string
}

// SubagentFrame reports whether a stdout line came from a Task subagent, and if
// so which tool call spawned it. ok is false for a main-thread frame
// (parent_tool_use_id is null there), for malformed input, and for a frame
// carrying no message id, which cannot be joined to anything.
func (p *claudeProvider) SubagentFrame(line []byte) (SubagentObservation, bool) {
	var f claudeStreamFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return SubagentObservation{}, false
	}
	if f.ParentToolUseID == "" || f.Message == nil || f.Message.ID == "" {
		return SubagentObservation{}, false
	}
	return SubagentObservation{MessageID: f.Message.ID, ParentToolUseID: f.ParentToolUseID}, true
}

// GatedMessageID names the response a main-thread assistant frame belongs
// to, so the child's gate holds the frame's tool events (and everything
// after them) until the daemon has published that response's row.
func (p *claudeProvider) GatedMessageID(line []byte) string {
	var f claudeStreamFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return ""
	}
	if f.Type != "assistant" || f.ParentToolUseID != "" || f.Message == nil {
		return ""
	}
	return f.Message.ID
}

// claudeContentBlock is one assistant/user content block in claude's wire shape.
// tool_result.content may be a string or an array of text/image blocks, so
// it is captured as json.RawMessage and decoded by decodeToolResult.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// BusFrames translates one raw claude stdout line into the ordered pi
// AgentSessionEvent frames to publish on the bus, accumulating per-child state.
// The mapping follows Plan 4's translation contract:
//
//	system/init               → (none) — model captured for later messages
//	assistant{content}        → [agent_start?] message_start, message_update,
//	                            tool_execution_start*, message_end; append msg
//	user{tool_result*}        → [agent_start?] tool_execution_end*; append
//	                            toolResult msgs
//	result                    → agent_end(full messages[]); close turn
//	rate_limit_event/hook/... → (none)
func (p *claudeProvider) BusFrames(line []byte, ts int64) [][]byte {
	var f claudeStreamFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil
	}

	switch f.Type {
	case "system":
		if f.Subtype == "init" && f.Model != "" {
			p.st.model = f.Model
			p.st.provider, p.st.api = claudeProviderAPI(f.Model)
		}
		return nil

	case "assistant":
		if f.Message == nil || len(f.Message.Content) == 0 {
			return nil
		}
		return p.emitAssistant(f.Message.Content, f.Message.Model, f.ParentToolUseID, ts)

	case "user":
		if f.Message == nil {
			return nil
		}
		return p.emitUser(f.Message.Content, f.ParentToolUseID, ts)

	case "result":
		return p.emitResult(f)

	default:
		// rate_limit_event, unknown — no bus output.
		return nil
	}
}

// emitAssistant maps a claude assistant frame to message_start / message_update /
// tool_execution_start* / message_end, appends the assistant message to state,
// and opens the turn if needed.
func (p *claudeProvider) emitAssistant(blocks []claudeContentBlock, frameModel, parentToolUseID string, ts int64) [][]byte {
	model := frameModel
	if model == "" {
		model = p.st.model
	}

	var toolCalls []claudeContentBlock
	content := make([]PiContentBlock, 0, len(blocks))
	hasToolUse := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			// Skip empty text. PiContentBlock.Text is `omitempty`, so an empty
			// string serializes to {type:"text"} with no `text` field, and pi's
			// TUI does c.text.trim() → crashes on the undefined field.
			if b.Text != "" {
				content = append(content, PiTextBlock(b.Text))
			}
		case "thinking":
			// Same hazard, hit in practice: Fable 5 with display:omitted streams
			// thinking blocks with EMPTY text, which would serialize to
			// {type:"thinking"} (no `thinking` field) and crash pi's TUI at
			// c.thinking.trim(). Skip empty thinking blocks entirely.
			if b.Thinking != "" {
				content = append(content, PiThinkingBlock(b.Thinking))
			}
		case "tool_use":
			content = append(content, PiToolCallBlock(b.ID, b.Name, b.Input))
			toolCalls = append(toolCalls, b)

			hasToolUse = true
		}
	}

	stopReason := "stop"
	if hasToolUse {
		stopReason = "toolUse"
	}
	msg := PiAssistantMessage{
		Role:       "assistant",
		Content:    content,
		API:        p.apiOrDefault(),
		Provider:   p.providerOrDefault(),
		Model:      model,
		StopReason: stopReason,
		Timestamp:  ts,
	}

	var out [][]byte
	out = append(out, p.openTurn()...)
	out = appendFrame(out, PiMessageStart(msg, parentToolUseID))
	out = appendFrame(out, PiMessageUpdate(msg, parentToolUseID))
	for _, tc := range toolCalls {
		out = appendFrame(out, PiToolExecutionStart(tc.ID, tc.Name, anyArgs(tc.Input), parentToolUseID))
	}
	out = appendFrame(out, PiMessageEnd(msg, parentToolUseID))

	if raw, err := json.Marshal(msg); err == nil {
		p.appendMessage(raw)
	}
	return out
}

// emitUser maps a claude user frame's tool_result blocks to tool_execution_end
// frames and appends a toolResult message per block. A user frame carrying only
// tool results is the agent loop's tool-result turn, not a fresh user prompt; it
// still opens the turn if one isn't active (so a tool result arriving first is
// rendered).
func (p *claudeProvider) emitUser(blocks []claudeContentBlock, parentToolUseID string, ts int64) [][]byte {
	var out [][]byte
	opened := false
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		if !opened {
			out = append(out, p.openTurn()...)
			opened = true
		}
		text := toolResultText(b.Content)
		out = appendFrame(out, PiToolExecutionEnd(b.ToolUseID, p.toolNameFor(b.ToolUseID), text, b.IsError, parentToolUseID))

		tr := PiToolResultMessage{
			Role:       "toolResult",
			ToolCallID: b.ToolUseID,
			ToolName:   p.toolNameFor(b.ToolUseID),
			Content:    []PiContentBlock{PiTextBlock(text)},
			IsError:    b.IsError,
			Timestamp:  ts,
		}
		if raw, err := json.Marshal(tr); err == nil {
			p.appendMessage(raw)
		}
	}
	return out
}

// emitResult closes the turn with an agent_end carrying the full accumulated
// messages[] and the turn's token/cost totals, followed by agent_settled
// (claude has no retry continuation, so the turn is truly idle here). The
// messages are retained (not cleared) so a later GetChild or a subsequent
// turn's agent_end still reflects the whole conversation.
func (p *claudeProvider) emitResult(f claudeStreamFrame) [][]byte {
	p.st.turnActive = false
	return [][]byte{
		mustFrame(PiAgentEnd(p.snapshotMessages(), claudeResultUsage(f))),
		mustFrame(PiAgentSettled()),
	}
}

// claudeResultUsage maps a claude result frame's token counts and total cost to
// a PiUsage. Returns nil when the frame carries no usage data so agent_end omits
// the field entirely. Only the total cost is known (claude gives a single
// total_cost_usd, no per-category breakdown). That figure is the CUMULATIVE
// session total, passed through untouched: this legacy agent_end has no summing
// consumer, unlike TurnEnd, which turnCost reduces to one turn's share.
func claudeResultUsage(f claudeStreamFrame) *PiUsage {
	if f.Usage == nil && f.TotalCostUSD == 0 {
		return nil
	}
	u := &PiUsage{Cost: PiCost{Total: f.TotalCostUSD}}
	if f.Usage != nil {
		u.Input = f.Usage.InputTokens
		u.Output = f.Usage.OutputTokens
		u.CacheRead = f.Usage.CacheReadInputTokens
		u.CacheWrite = f.Usage.CacheCreationInputTokens
		u.TotalTokens = u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	return u
}

// openTurn returns an agent_start frame the first time a turn opens, or nil if a
// turn is already active.
func (p *claudeProvider) openTurn() [][]byte {
	if p.st.turnActive {
		return nil
	}
	p.st.turnActive = true
	return [][]byte{mustFrame(PiAgentStart())}
}

// openNativeTurn is openTurn's native-plane counterpart: it returns the durable
// TurnStart for a turn that is opening now, or nil when this turn's TurnStart
// has already been published.
//
// It guards on nativeTurnActive rather than turnActive for the reason that
// field documents: BusFrames has already opened the pi-path turn for this same
// line, so sharing the flag would mean the native event never fires.
//
// The guard is raised by BOTH callers but is a single gate: OutboundEchoNative
// opens at prompt delivery, and nativeAssistant calls this again on the first
// assistant frame only to find it already open. The lock is taken because those
// two callers run on different goroutines (supervise vs readStdout).
//
// frameModel is the model the opening frame names, and is left as given — a
// prompt echo names none and publishes an empty model rather than the last
// model seen, mirroring fundi (whose turn opens before routing picks a model).
// A consumer falls back to the child's model metadata.
func (p *claudeProvider) openNativeTurn(frameModel string, ts int64) []*rafikiv1.Event {
	p.st.mu.Lock()
	if p.st.nativeTurnActive {
		p.st.mu.Unlock()
		return nil
	}
	p.st.nativeTurnActive = true
	p.st.mu.Unlock()
	return []*rafikiv1.Event{{
		Ts: timestamppb.New(time.UnixMilli(ts)),
		Payload: &rafikiv1.Event_TurnStart{
			TurnStart: &rafikiv1.TurnStart{Model: frameModel},
		},
	}}
}

// toolNameFor recovers the tool name for a tool_use id from the accumulated
// assistant messages so a tool_result frame (which lacks the name) can be paired.
// Returns "" if no matching tool call has been seen.
func (p *claudeProvider) toolNameFor(toolUseID string) string {
	for _, raw := range p.st.messages {
		var m struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "assistant" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "toolCall" && b.ID == toolUseID {
				return b.Name
			}
		}
	}
	return ""
}

func (p *claudeProvider) apiOrDefault() string {
	if p.st.api != "" {
		return p.st.api
	}
	return "anthropic-messages"
}

func (p *claudeProvider) providerOrDefault() string {
	if p.st.provider != "" {
		return p.st.provider
	}
	return "anthropic"
}

// claudeProviderAPI derives a pi provider/api pair from a claude model id. claude
// always speaks the Anthropic Messages API, so the api is fixed; the provider is
// "anthropic". The model string itself is carried verbatim on the message.
func claudeProviderAPI(_ string) (provider, api string) {
	return "anthropic", "anthropic-messages"
}

// toolResultText flattens a claude tool_result content value (string or array of
// content blocks) into a plain string for the pi tool result, whose vocabulary
// is text-only.
func toolResultText(raw json.RawMessage) string {
	text, _ := decodeToolResult(raw)
	return text
}

// decodeToolResult splits a claude tool_result content value into its
// concatenated text and its base64 image blocks. An unknown shape comes back as
// the raw JSON text so nothing is silently dropped.
func decodeToolResult(raw json.RawMessage) (string, []*rafikiv1.ImageBlock) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw), nil
	}
	var sb strings.Builder
	var images []*rafikiv1.ImageBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			img := &rafikiv1.ImageBlock{MediaType: b.Source.MediaType}
			if b.Source.Type == "base64" {
				if data, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
					img.Data = data
				}
			}
			images = append(images, img)
		}
	}
	return sb.String(), images
}

// anyArgs coerces a nil input map to an empty map so tool_execution_start.args is
// always a non-null object.
func anyArgs(input map[string]any) any {
	if input == nil {
		return map[string]any{}
	}
	return input
}

// appendFrame marshals a pi event value and appends its bytes; a marshal failure
// drops that frame rather than corrupting the stream.
func appendFrame(out [][]byte, v any) [][]byte {
	if b, err := json.Marshal(v); err == nil {
		return append(out, b)
	}
	return out
}

// mustFrame marshals a pi event value whose shape is statically known to be
// serializable (agent_start / agent_end). A failure yields an empty frame rather
// than a panic, but in practice cannot occur for these fixed shapes.
func mustFrame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// BusFramesNative translates one claude stdout line into rafiki-native events.
// It mirrors BusFrames's translation logic but emits protobuf events for the
// Connect control plane's StreamEvents, so a claude child renders in the TUI
// without going through the pi vocabulary.
func (p *claudeProvider) BusFramesNative(line []byte, ts int64) []*rafikiv1.Event {
	var f claudeStreamFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil
	}

	switch f.Type {
	case "system":
		if f.Subtype == "init" && f.Model != "" {
			p.st.model = f.Model
			p.st.provider, p.st.api = claudeProviderAPI(f.Model)
			return nil
		}
		if f.Subtype == "compact_boundary" && f.CompactMetadata != nil {
			pre := int32(f.CompactMetadata.PreTokens)
			post := int32(f.CompactMetadata.PostTokens)
			return []*rafikiv1.Event{{
				Ts: timestamppb.New(time.UnixMilli(ts)),
				Payload: &rafikiv1.Event_CompactionBoundary{
					CompactionBoundary: &rafikiv1.CompactionBoundary{
						Trigger:    f.CompactMetadata.Trigger,
						PreTokens:  &pre,
						PostTokens: &post,
					},
				},
			}}
		}
		return nil

	case "assistant":
		if f.Message == nil || len(f.Message.Content) == 0 {
			return nil
		}
		if f.ParentToolUseID != "" {
			return nil
		}
		return p.nativeAssistant(f.Message.Content, f.Message.Model, ts)

	case "user":
		if f.Message == nil {
			return nil
		}
		if f.ParentToolUseID != "" {
			return nil
		}
		return p.nativeUser(f.Message.Content, ts)

	case "result":
		return p.nativeResult(f, ts)

	default:
		return nil
	}
}

// nativeAssistant maps one claude assistant frame to rafiki-native turn and
// tool events.
//
// It emits no AssistantMessage: the response's message event comes from its
// persisted conversation_message row, published by the daemon's capture sink
// (cmd/rafikid/capture_sink.go). The child's message gate (GatedMessageID) holds
// the ToolExecutionStart events emitted here until that row is published, which
// keeps the tool events after the message that declares them — the cockpit's
// applyToolStart looks the call up by id on the last assistant block and only
// marks it running.
//
// A turn is already open by the time this runs: OutboundEchoNative opened it at
// prompt delivery, before claude's first request reached the proxy (see
// openNativeTurn). This call is the fallback for a frame arriving with no
// preceding echo, and its guard makes the ordinary case a no-op; nativeResult
// closes the turn so the next prompt opens a fresh one.
//
// ContentBlockDelta is never emitted: claude's stream-json carries COMPLETE
// messages, so a synthesized delta would duplicate the row-sourced message.
func (p *claudeProvider) nativeAssistant(blocks []claudeContentBlock, frameModel string, ts int64) []*rafikiv1.Event {
	out := p.openNativeTurn(frameModel, ts)

	for _, b := range blocks {
		if b.Type == "tool_use" {
			out = append(out, &rafikiv1.Event{
				Ts: timestamppb.New(time.UnixMilli(ts)),
				Payload: &rafikiv1.Event_ToolExecutionStart{
					ToolExecutionStart: &rafikiv1.ToolExecutionStart{
						ToolUseId: b.ID,
						Name:      b.Name,
					},
				},
			})
		}
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			out = append(out, toolExecutionEnd(b, ts))
		}
	}

	return out
}

// nativeUser maps a claude user frame to one ToolExecutionEnd per tool_result
// block. The result's output reaches the native stream as a UserMessage from the
// persisted row (cmd/rafikid/capture_sink.go); ToolExecutionEnd carries no text,
// and the child's gate orders it after the response whose tool_use it completes.
func (p *claudeProvider) nativeUser(blocks []claudeContentBlock, ts int64) []*rafikiv1.Event {
	var out []*rafikiv1.Event
	for _, b := range blocks {
		if b.Type == "tool_result" {
			out = append(out, toolExecutionEnd(b, ts))
		}
	}
	return out
}

func toolExecutionEnd(b claudeContentBlock, ts int64) *rafikiv1.Event {
	return &rafikiv1.Event{
		Ts: timestamppb.New(time.UnixMilli(ts)),
		Payload: &rafikiv1.Event_ToolExecutionEnd{
			ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
				ToolUseId: b.ToolUseID,
				IsError:   b.IsError,
			},
		},
	}
}

func (p *claudeProvider) nativeResult(f claudeStreamFrame, ts int64) []*rafikiv1.Event {
	p.st.turnActive = false
	// Close the native turn too, or the next turn's prompt echo (or assistant
	// frame) would see nativeTurnActive still set and publish no TurnStart.
	// Under mu: the echo path reads and raises this from the supervise goroutine.
	p.st.mu.Lock()
	p.st.nativeTurnActive = false
	p.st.mu.Unlock()

	var usage *rafikiv1.Usage
	if f.Usage != nil {
		usage = &rafikiv1.Usage{
			InputTokens:      int64Ptr(int64(f.Usage.InputTokens)),
			OutputTokens:     int64Ptr(int64(f.Usage.OutputTokens)),
			CacheReadTokens:  int64Ptr(int64(f.Usage.CacheReadInputTokens)),
			CacheWriteTokens: int64Ptr(int64(f.Usage.CacheCreationInputTokens)),
		}
	}
	costUSD := p.turnCost(f.TotalCostUSD)
	return []*rafikiv1.Event{{
		Ts: timestamppb.New(time.UnixMilli(ts)),
		Payload: &rafikiv1.Event_TurnEnd{
			TurnEnd: &rafikiv1.TurnEnd{
				Usage:   usage,
				CostUsd: costUSD,
			},
		},
	}}
}

// turnCost converts claude's cumulative session total into this turn's share,
// nil when the frame carries no total. A total below the last one means claude's
// counter restarted (a fresh process), so the new figure is the baseline and the
// whole of that turn's cost.
func (p *claudeProvider) turnCost(total float64) *float64 {
	if total <= 0 {
		return nil
	}
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	delta := total - p.st.lastTotalCost
	if delta < 0 {
		delta = total
	}
	p.st.lastTotalCost = total
	return &delta
}

func int64Ptr(v int64) *int64 { return &v }
