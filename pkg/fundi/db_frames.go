package fundi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// DBToPiFramesMessages converts a conversation transcript (conv.Store.Transcript)
// into the pi AgentSessionEvent frames the attach TUI and GetRecent consumers
// expect. This is the read path for fundi and claude children once the DB is
// the canonical store — the ring buffer is bypassed entirely.
//
// Every node produces frames that mirror what the live fundi Emitter would have
// produced: user prompts get message_start/message_end, assistant turns get
// message_start/message_update/message_end plus tool_execution_start for each
// tool_use, and tool results get tool_execution_end. An agent_start frame is
// prepended and an agent_end frame carrying the Pi-format accumulated messages
// is appended so the TUI's AgentSession can seed its message cache.
//
// The transcript is the head's walk, so a row the head no longer reaches — a
// retracted prompt, a superseded reply — never reaches this function, and the
// compaction_tail copies of rows that appear earlier are already gone. A
// compaction_summary node renders as a boundary divider rather than a plain
// user prompt, mirroring eventconv.EventsFromNodes — the full-history reader
// contract (docs/reference/fundi-compaction.md).
func DBToPiFramesMessages(nodes []*conv.Node) []json.RawMessage {
	frames, _ := DBToPiFramesWithOrigins(nodes)
	return frames
}

// FrameOrigin names the node a frame was rendered from. FromRow is false for
// the synthetic agent_start/agent_end frames, which come from no row; it is the
// explicit flag because Ordinal 0 is a real ordinal.
type FrameOrigin struct {
	FromRow    bool
	Ordinal    int
	StopReason string
}

// DBToPiFramesWithOrigins is DBToPiFramesMessages plus a parallel slice naming
// the node each frame came from. The frames are exactly DBToPiFramesMessages';
// origins[i] names the node frames[i] came from.
func DBToPiFramesWithOrigins(nodes []*conv.Node) ([]json.RawMessage, []FrameOrigin) {
	names, resolved := toolUseMap(nodes)

	out := make([]json.RawMessage, 0, len(nodes)*3+2)
	origins := make([]FrameOrigin, 0, len(nodes)*3+2)
	piMsgs := make([]json.RawMessage, 0, len(nodes))

	out, origins = appendFrameWithOrigin(out, origins, child.PiAgentStart(), FrameOrigin{})

	for _, n := range nodes {
		// A transcript never carries a tail copy, but this function is exported
		// and a working path does: rendering one would show a row twice.
		if n.Kind == conv.KindCompactionTail {
			continue
		}
		if n.Kind == conv.KindCompactionSummary {
			out, origins = appendFrameWithOrigin(out, origins,
				compactionBoundaryFrame(n.InputTokens, ""),
				FrameOrigin{FromRow: true, Ordinal: n.Ordinal})
			continue
		}
		if conv.IsClearBoundary(n) {
			out, origins = appendFrameWithOrigin(out, origins,
				compactionBoundaryFrame(nil, "clear"),
				FrameOrigin{FromRow: true, Ordinal: n.Ordinal})
			continue
		}
		switch n.Msg.Role {
		case msg.User:
			frames, piMsgsFor := dbUserFramesWithTools(n.Msg, names)
			out = append(out, frames...)
			origins = appendOrigins(origins, len(frames), n)
			piMsgs = append(piMsgs, piMsgsFor...)
		case msg.Assistant:
			frames, piMsg := dbAssistantFramesWithTools(n.Msg, resolved)
			out = append(out, frames...)
			origins = appendOrigins(origins, len(frames), n)
			piMsgs = appendPiMsg(piMsgs, piMsg)
		}
	}

	out, origins = appendFrameWithOrigin(out, origins, child.PiAgentEnd(piMsgs, nil), FrameOrigin{})
	return out, origins
}

// appendFrameWithOrigin appends v to out and origin to origins, keeping the two
// slices in step: the origin is appended only when the frame actually landed,
// since appendFrame silently drops a frame whose marshal fails.
func appendFrameWithOrigin(out []json.RawMessage, origins []FrameOrigin, v any, origin FrameOrigin) ([]json.RawMessage, []FrameOrigin) {
	before := len(out)
	out = appendFrame(out, v)
	if len(out) == before {
		return out, origins
	}
	return out, append(origins, origin)
}

// appendOrigins appends count copies of n's origin, one per frame n produced.
func appendOrigins(origins []FrameOrigin, count int, n *conv.Node) []FrameOrigin {
	origin := FrameOrigin{FromRow: true, Ordinal: n.Ordinal, StopReason: n.StopReason}
	for i := 0; i < count; i++ {
		origins = append(origins, origin)
	}
	return origins
}

// compactionBoundaryFrame is the pi-vocabulary divider DBToPiFramesMessages
// emits in place of a compaction_summary node. preTokens is the replaced-context
// size (InputTokens on the node); it is omitted when the node carries none.
// trigger is "clear" for a /clear boundary and empty for a compaction.
func compactionBoundaryFrame(preTokens *int64, trigger string) json.RawMessage {
	f := map[string]any{"type": "compaction_boundary"}
	if preTokens != nil {
		f["preTokens"] = *preTokens
	}
	if trigger != "" {
		f["trigger"] = trigger
	}
	return mustFrame(f)
}

// frameBlockKind is what a stored block contributes to the frame vocabulary.
// The zero value is "nothing renders here": an image, and any block type this
// vocabulary cannot name, contributes no frame block of its own.
type frameBlockKind uint8

const (
	frameBlockNone frameBlockKind = iota
	frameBlockText
	frameBlockThinking
	frameBlockToolUse
	frameBlockToolResult
)

// frameBlock is one block as the frame vocabulary reads it: its kind plus the
// fields that kind renders.
type frameBlock struct {
	kind       frameBlockKind
	text       string
	toolUse    msg.ToolUse
	toolResult msg.ToolResult
}

// frameBlocks reads a message's content blocks, dropping the ones that render
// nothing.
func frameBlocks(m msg.Message) []frameBlock {
	out := make([]frameBlock, 0, len(m.Content))
	for _, b := range m.Content {
		if fb := frameBlockOf(b); fb.kind != frameBlockNone {
			out = append(out, fb)
		}
	}
	return out
}

// frameBlockOf classifies one block. An Opaque block is read by the wire type
// it declares (msg.OpaqueType) and decoded into that type's fields: the codec
// keeps a block verbatim when its typed form cannot reproduce it, not because
// the block is unreadable, and the SDK decode this replaced rendered it as its
// typed form.
func frameBlockOf(b msg.Block) frameBlock {
	switch t := b.(type) {
	case msg.Text:
		return frameBlock{kind: frameBlockText, text: t.Text}
	case msg.Thinking:
		return frameBlock{kind: frameBlockThinking, text: t.Text}
	case msg.ToolUse:
		return frameBlock{kind: frameBlockToolUse, toolUse: t}
	case msg.ToolResult:
		return frameBlock{kind: frameBlockToolResult, toolResult: t}
	case msg.Opaque:
		return opaqueFrameBlock(t)
	}
	return frameBlock{}
}

// opaqueFrameBlock decodes a block kept verbatim into the frame vocabulary by
// its declared type, reporting none for a type the vocabulary cannot name.
func opaqueFrameBlock(o msg.Opaque) frameBlock {
	switch msg.OpaqueType(o) {
	case "text":
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(o.Raw, &v); err != nil {
			return frameBlock{}
		}
		return frameBlock{kind: frameBlockText, text: v.Text}
	case "thinking":
		var v struct {
			Thinking string `json:"thinking"`
		}
		if err := json.Unmarshal(o.Raw, &v); err != nil {
			return frameBlock{}
		}
		return frameBlock{kind: frameBlockThinking, text: v.Thinking}
	case "tool_use":
		var v struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(o.Raw, &v); err != nil {
			return frameBlock{}
		}
		return frameBlock{kind: frameBlockToolUse, toolUse: msg.ToolUse{ID: v.ID, Name: v.Name, Input: v.Input}}
	case "tool_result":
		var v struct {
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(o.Raw, &v); err != nil {
			return frameBlock{}
		}
		tr := msg.ToolResult{ToolUseID: v.ToolUseID, IsError: v.IsError}
		decoded, err := msganthropic.Decode(msg.User.String(), v.Content)
		if err != nil {
			return frameBlock{kind: frameBlockToolResult, toolResult: tr}
		}
		tr.Content = decoded.Content
		return frameBlock{kind: frameBlockToolResult, toolResult: tr}
	}
	return frameBlock{}
}

// toolUseMap scans the transcript's blocks, returning the name of every
// tool_use id (for tool_execution_end to name the tool) and the set of ids a
// tool_result answers — only those get tool_execution_start frames.
func toolUseMap(nodes []*conv.Node) (map[string]string, map[string]bool) {
	names := make(map[string]string)
	resolved := make(map[string]bool)
	for _, n := range nodes {
		for _, fb := range frameBlocks(n.Msg) {
			switch fb.kind {
			case frameBlockToolUse:
				if fb.toolUse.ID != "" {
					names[fb.toolUse.ID] = fb.toolUse.Name
				}
			case frameBlockToolResult:
				if fb.toolResult.ToolUseID != "" {
					resolved[fb.toolResult.ToolUseID] = true
				}
			}
		}
	}
	return names, resolved
}

// dbUserFramesWithTools produces frames and Pi-format messages for a user
// message. Tool_result blocks emit tool_execution_end (the TUI renders tool
// output from tool_execution_start/end, not from message content blocks).
// Pure tool_result messages do NOT get a user message_start/message_end —
// the live Emitter accumulates PiToolResultMessage but does not echo user
// message frames for tool results.
func dbUserFramesWithTools(m msg.Message, names map[string]string) ([]json.RawMessage, []json.RawMessage) {
	ts := time.Now().UnixMilli()
	id := fmt.Sprintf("user-%d", ts)

	var (
		frames []json.RawMessage
		piMsgs []json.RawMessage
		texts  []string
	)

	for _, fb := range frameBlocks(m) {
		switch fb.kind {
		case frameBlockText:
			if fb.text != "" {
				texts = append(texts, fb.text)
			}
		case frameBlockToolResult:
			tr := fb.toolResult
			resultText := toolResultText(tr.Content)
			name := names[tr.ToolUseID]
			frames = appendFrame(frames, child.PiToolExecutionEnd(tr.ToolUseID, name, resultText, tr.IsError, ""))
			piMsgs = appendPiMsg(piMsgs, child.PiToolResultMessage{
				Role:       "toolResult",
				ToolCallID: tr.ToolUseID,
				ToolName:   name,
				Content:    []child.PiContentBlock{child.PiTextBlock(resultText)},
				IsError:    tr.IsError,
				Timestamp:  ts,
			})
		}
	}

	if len(texts) > 0 {
		// This is a user prompt or steer — emit message_start/message_end.
		text := texts[0]
		piUser := child.PiUserMessage{Role: "user", ID: id, Content: text, Timestamp: ts}
		frames = appendFrame(frames, child.PiUserMessageStart(piUser))
		frames = appendFrame(frames, child.PiUserMessageEnd(piUser))
		piMsgs = appendPiMsg(piMsgs, piUser)
	}

	return frames, piMsgs
}

// dbAssistantFramesWithTools produces frames and a Pi-format message for an
// assistant message. In addition to message_start/message_update/message_end,
// it emits tool_execution_start for each tool_use block that has a matching
// tool_result later in the conversation.
func dbAssistantFramesWithTools(m msg.Message, resolved map[string]bool) ([]json.RawMessage, json.RawMessage) {
	fbs := frameBlocks(m)
	blocks := make([]child.PiContentBlock, 0, len(fbs))
	hasToolUse := false
	for _, fb := range fbs {
		switch fb.kind {
		case frameBlockText:
			if fb.text != "" {
				blocks = append(blocks, child.PiTextBlock(fb.text))
			}
		case frameBlockThinking:
			if fb.text != "" {
				blocks = append(blocks, child.PiThinkingBlock(fb.text))
			}
		case frameBlockToolUse:
			hasToolUse = true
			blocks = append(blocks, child.PiToolCallBlock(fb.toolUse.ID, fb.toolUse.Name, toolUseArgs(fb.toolUse.Input)))
		}
	}

	stopReason := "stop"
	if hasToolUse {
		stopReason = "toolUse"
	}
	piMessage := child.PiAssistantMessage{
		Role:       "assistant",
		Content:    blocks,
		API:        "anthropic-messages",
		Provider:   "", // unknown from stored messages alone; zero is acceptable
		Model:      "", // unknown from stored messages alone; zero is acceptable
		Usage:      child.PiUsage{},
		StopReason: stopReason,
		Timestamp:  time.Now().UnixMilli(),
	}

	var frames []json.RawMessage
	frames = appendFrame(frames, child.PiMessageStart(piMessage, ""))
	frames = appendFrame(frames, child.PiMessageUpdate(piMessage, ""))
	// Emit tool_execution_start for each tool_use that has a matching
	// tool_result, mirroring what the live Emitter does (ToolStart before
	// message_end). Without these, the TUI shows the tool_use block but not
	// the tool-execution lifecycle that renders the output.
	for _, fb := range fbs {
		if fb.kind == frameBlockToolUse && resolved[fb.toolUse.ID] {
			frames = appendFrame(frames, child.PiToolExecutionStart(fb.toolUse.ID, fb.toolUse.Name, toolUseArgs(fb.toolUse.Input), ""))
		}
	}
	frames = appendFrame(frames, child.PiMessageEnd(piMessage, ""))

	return frames, mustFrame(piMessage)
}

// toolResultText extracts a plain-text representation from a tool result's
// content blocks. The first text block wins, an Opaque one included. Content
// with no text at all (an image, say) falls back to its provider-shaped JSON
// rather than rendering as nothing.
func toolResultText(content []msg.Block) string {
	for _, b := range content {
		if fb := frameBlockOf(b); fb.kind == frameBlockText {
			return fb.text
		}
	}
	if len(content) == 0 {
		return ""
	}
	encoded, err := msganthropic.EncodeContent(msg.Message{Role: msg.User, Content: content})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// appendFrame marshals v and appends the result to out. Marshal failures are
// silently dropped (v is always a well-formed child.Pi* type).
func appendFrame(out []json.RawMessage, v any) []json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	return append(out, b)
}

// mustFrame marshals v and returns the result, or nil on error.
func mustFrame(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// appendPiMsg marshals v and appends the result to out. Nil entries are
// skipped, not appended.
//
// Callers sometimes pass an already-marshaled json.RawMessage (e.g. the
// return of mustFrame, which is nil when marshaling failed upstream). A nil
// or empty json.RawMessage must be special-cased BEFORE the marshal below:
// json.RawMessage.MarshalJSON returns the literal `null` for an empty
// receiver, never a nil slice or an error, so checking json.Marshal's result
// for b == nil never catches this case — it would instead append a literal
// JSON `null` into agent_end.messages.
func appendPiMsg(out []json.RawMessage, v any) []json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok && len(raw) == 0 {
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	return append(out, b)
}

// toolUseArgs converts a tool_use block's input into the map[string]any that
// PiToolCallBlock expects. Input that carries no object renders no arguments,
// which is the empty map; input that is not valid JSON stays visible under
// "_raw" rather than being dropped.
func toolUseArgs(input json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(input)) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return map[string]any{"_raw": string(input)}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}
