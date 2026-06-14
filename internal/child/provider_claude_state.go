package child

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
//     guarded by mu; the other fields are touched only by BusFrames.
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
		Role    string               `json:"role"`
		Model   string               `json:"model,omitempty"`
		Content []claudeContentBlock `json:"content"`
	} `json:"message,omitempty"`
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

// claudeContentBlock is one assistant/user content block in claude's wire shape.
// tool_result.content may be a string or an array of {type:text,text} blocks, so
// it is captured as json.RawMessage and decoded by toolResultText.
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

	content := make([]PiContentBlock, 0, len(blocks))
	var toolCalls []claudeContentBlock
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
// messages[] and the turn's token/cost totals. The messages are retained (not
// cleared) so a later ctrl_get or a subsequent turn's agent_end still reflects
// the whole conversation.
func (p *claudeProvider) emitResult(f claudeStreamFrame) [][]byte {
	p.st.turnActive = false
	return [][]byte{mustFrame(PiAgentEnd(p.snapshotMessages(), claudeResultUsage(f)))}
}

// claudeResultUsage maps a claude result frame's token counts and total cost to
// a PiUsage. Returns nil when the frame carries no usage data so agent_end omits
// the field entirely. Only the total cost is known (claude gives a single
// total_cost_usd, no per-category breakdown).
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
// {type:text,text} blocks) into a plain string for the pi tool result.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	// Unknown shape — return the raw JSON so nothing is silently dropped.
	return string(raw)
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
