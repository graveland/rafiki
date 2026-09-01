// SPDX-License-Identifier: Apache-2.0

// Package session holds one child's conversation as a pure state machine.
//
// It takes events and nothing else: no bubbletea, no network, no terminal.
// That purity is the point -- rendering and transport are the shell's problem,
// and a conversation you can drive from a slice of events is a conversation you
// can test.
package session

import (
	"bytes"
	"image"
	_ "image/gif"  // registered for DecodeConfig
	_ "image/jpeg" // registered for DecodeConfig
	_ "image/png"  // registered for DecodeConfig
	"strconv"
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Kind identifies what kind of conversational unit a Block is.
type Kind int

const (
	KindSystem Kind = iota
	KindUser
	KindAssistant
	KindPendingUser
)

// ToolCall is one tool invocation inside an assistant turn.
type ToolCall struct {
	ID     string
	Name   string
	Input  string
	Result string
	// HasResult records that a tool_result actually arrived, which is NOT the
	// same as Result being non-empty: a tool can legitimately return nothing.
	// Without it a call that never produced a result at all — interrupted, or
	// its turn cut short — is indistinguishable from one that succeeded
	// silently, and the transcript claims a ✓ for work that never finished.
	// There are real instances of this: a production database here holds 38
	// bash calls with no matching tool_result.
	HasResult  bool
	IsError    bool
	Running    bool
	DurationMs int64
}

// Block is one renderable unit in the transcript.
type Block struct {
	Kind       Kind
	At         time.Time
	Text       string // user/pending text or assistant plain-text fallback
	Content    []*rafikiv1.ContentBlock
	ThinkText  string // accumulated thinking
	ToolCalls  []ToolCall
	StopReason string
	Final      bool // true when the turn has ended
}

// Fingerprint returns a cheap content hash for cache-invalidation.
func (b Block) Fingerprint() string {
	var sb strings.Builder
	sb.WriteString(b.Text)
	sb.WriteString(b.ThinkText)
	for _, tc := range b.ToolCalls {
		sb.WriteString(tc.ID)
		sb.WriteString(tc.Name)
		sb.WriteString(tc.Result)
		if tc.Running {
			sb.WriteString("running")
		}
	}
	sb.WriteString(b.StopReason)
	if b.Final {
		sb.WriteString("final")
	}
	return sb.String()
}

// Settled reports whether b can never change again.
//
// This is the whole meaning of Session.Finalized, and getting it wrong is
// silent: renderer.Lines caches every block below the watermark exactly once
// and never looks at it again, so a block finalized too early is frozen on
// screen showing whatever was true at that instant. An assistant block's tool
// calls resolve AFTER the message announcing them arrives, which is why
// Final alone is not enough.
func (b Block) Settled() bool {
	if b.Kind != KindAssistant {
		return true
	}
	if !b.Final {
		return false
	}
	for _, tc := range b.ToolCalls {
		if !tc.HasResult {
			return false
		}
	}
	return true
}

// Session is one child's conversation.
type Session struct {
	ChildID string
	Blocks  []Block
	// Finalized is the index into Blocks after which blocks may still change.
	Finalized int
	// Cursor is the highest ordinal delivered to this session, and HasCursor
	// says whether any ordinal has been seen at all. They are separate because
	// 0 is a legal ordinal -- a child's first event is ordinal 0, so a zero
	// Cursor cannot mean "nothing yet".
	Cursor    int32
	HasCursor bool
	// Status is the last agent_status state seen.
	Status string
}

// New returns an empty session for one child.
func New(childID string) *Session {
	return &Session{ChildID: childID}
}

// recomputeFinalized advances the watermark to the first block that can still
// change. It only ever moves FORWARD: renderer.Lines treats a backwards move
// as "this is a different transcript" and throws its whole cache away.
func (s *Session) recomputeFinalized() {
	i := s.Finalized
	if i < 0 {
		i = 0
	}
	for i < len(s.Blocks) && s.Blocks[i].Settled() {
		i++
	}
	s.Finalized = i
}

// settleAll marks every block that exists now as immutable, whether or not
// its tool calls were ever answered.
//
// This is the anti-stall guard and it is not optional. A turn that ends with
// a call still outstanding — interrupted, or cut short — would otherwise park
// the watermark permanently, leaving every later block in the live region to
// be re-rendered on every tick. The live region must be bounded by the length
// of one turn; if it is ever longer, this is missing.
func (s *Session) settleAll() {
	s.Finalized = len(s.Blocks)
}

// Apply folds one event into the session.
//
// It ignores events for other children rather than trusting the caller to
// route: Phase A shipped its one Critical because every test double at every
// layer ignored the child id, and this is the layer where the check is free.
func (s *Session) Apply(ev *rafikiv1.Event) {
	if ev == nil {
		return
	}
	if id := ev.GetChildId(); id != "" && s.ChildID != "" && id != s.ChildID {
		return
	}
	// An ordinal we have already folded in is a duplicate, and duplicates are
	// routine rather than exceptional: the rail and focus subscriptions overlap
	// on the durable tier, so a focused child's turn_end and error events arrive
	// on BOTH. Without this an error would append its KindSystem block twice.
	// Ephemeral events carry no ordinal and are always applied.
	if ev.Ordinal != nil {
		ord := ev.GetOrdinal()
		if s.HasCursor && ord <= s.Cursor {
			return
		}
		s.Cursor = ord
		s.HasCursor = true
	}

	s.applyPayload(ev)
}

// ApplyHistory folds in one event from GetHistory WITHOUT touching the cursor.
//
// That exclusion is the whole reason this exists rather than being Apply. The
// two ordinal spaces are unrelated: GetHistory stamps
// conversation_message.ordinal (0..N over every message ever persisted), while
// the focus stream resumes from conversations.event_log.ordinal (0..M over one
// child's logged EVENTS). A 1217-message conversation whose log holds five
// events would set the cursor to 1216, and the next subscription would resume
// past the end of the log and receive nothing, forever, with no error.
func (s *Session) ApplyHistory(ev *rafikiv1.Event) {
	if ev == nil {
		return
	}
	if id := ev.GetChildId(); id != "" && s.ChildID != "" && id != s.ChildID {
		return
	}
	s.applyPayload(ev)
}

func (s *Session) applyPayload(ev *rafikiv1.Event) {
	switch p := ev.Payload.(type) {
	case *rafikiv1.Event_UserMessage:
		s.applyUserMessage(p.UserMessage)
	case *rafikiv1.Event_AssistantMessage:
		s.applyAssistantMessage(p.AssistantMessage)
	case *rafikiv1.Event_TurnStart:
		s.Blocks = append(s.Blocks, Block{Kind: KindAssistant, At: time.Now()})
	case *rafikiv1.Event_TurnEnd:
		s.applyTurnEnd(p.TurnEnd)
	case *rafikiv1.Event_ContentBlockDelta:
		s.applyDelta(p.ContentBlockDelta)
	case *rafikiv1.Event_ToolExecutionStart:
		s.applyToolStart(p.ToolExecutionStart)
	case *rafikiv1.Event_ToolExecutionEnd:
		s.applyToolEnd(p.ToolExecutionEnd)
	case *rafikiv1.Event_AgentStatus:
		s.Status = p.AgentStatus.GetState()
	case *rafikiv1.Event_Error:
		s.Blocks = append(s.Blocks, Block{
			Kind:  KindSystem,
			At:    time.Now(),
			Text:  "Error: " + p.Error.GetMessage(),
			Final: true,
		})
		s.settleAll()
	case *rafikiv1.Event_ChildExited:
		// A dead child answers no more tool calls.
		s.settleAll()
	}
}

// LastAssistant returns a pointer to the most recent assistant block, or nil.
func (s *Session) LastAssistant() *Block {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind == KindAssistant {
			return &s.Blocks[i]
		}
	}
	return nil
}

// applyUserMessage appends the user's turn -- and routes any tool_result
// blocks onto the tool calls they answer.
//
// Anthropic puts tool_result in the USER message that follows the assistant's
// tool_use, so a tool-calling conversation alternates assistant/user with the
// "user" half being nothing but results. TextFromContent reads text blocks
// only, so those messages used to render as EMPTY user bubbles and the results
// were dropped on the floor -- one blank bubble per tool call, and no output
// anywhere. A message that carries only results appends no block at all.
func (s *Session) applyUserMessage(um *rafikiv1.UserMessage) {
	var results int
	for _, cb := range um.GetContent() {
		tr, ok := cb.Block.(*rafikiv1.ContentBlock_ToolResult)
		if !ok {
			continue
		}
		results++
		s.attachToolResult(tr.ToolResult)
	}

	text := TextFromContent(um.GetContent())
	if text == "" && results > 0 {
		// A results-only message appends no block, but it just answered tool
		// calls -- which is exactly what unsticks the watermark.
		s.recomputeFinalized()
		return
	}
	s.Blocks = append(s.Blocks, Block{
		Kind:  KindUser,
		At:    time.Now(),
		Text:  text,
		Final: true,
	})
	s.recomputeFinalized()
}

// attachToolResult files one result against its call, searching backwards:
// a turn's results arrive in the message AFTER the assistant block that holds
// the calls, and with parallel tool use several may answer calls made in the
// same block.
func (s *Session) attachToolResult(tr *rafikiv1.ToolResultBlock) {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind != KindAssistant {
			continue
		}
		for j := range s.Blocks[i].ToolCalls {
			if s.Blocks[i].ToolCalls[j].ID != tr.GetToolUseId() {
				continue
			}
			s.Blocks[i].ToolCalls[j].Result = TextFromContent(tr.GetContent())
			s.Blocks[i].ToolCalls[j].HasResult = true
			// Never DOWNGRADE a failure. tool_execution_end may already have
			// said this call failed, and it is the more direct witness — it
			// carries the tool's own error. A stored tool_result block whose
			// is_error is absent or false would otherwise turn a ✗ back into a
			// ✓, which is the one direction that must never happen silently.
			if tr.GetIsError() {
				s.Blocks[i].ToolCalls[j].IsError = true
			}
			s.Blocks[i].ToolCalls[j].Running = false
			return
		}
	}
}

func (s *Session) applyAssistantMessage(am *rafikiv1.AssistantMessage) {
	block := Block{
		Kind:       KindAssistant,
		At:         time.Now(),
		Content:    am.GetContent(),
		StopReason: am.GetRawStopReason(),
		Final:      true,
	}
	for _, cb := range am.GetContent() {
		switch b := cb.Block.(type) {
		case *rafikiv1.ContentBlock_Text:
			block.Text += b.Text.GetText()
		case *rafikiv1.ContentBlock_Thinking:
			block.ThinkText += b.Thinking.GetThinking()
		case *rafikiv1.ContentBlock_ToolUse:
			block.ToolCalls = append(block.ToolCalls, ToolCall{
				ID:    b.ToolUse.GetId(),
				Name:  b.ToolUse.GetName(),
				Input: b.ToolUse.GetInputJson(),
			})
		case *rafikiv1.ContentBlock_Image:
			block.Text += ImagePlaceholder(b.Image)
		case *rafikiv1.ContentBlock_ToolResult:
			for i := range block.ToolCalls {
				if block.ToolCalls[i].ID == b.ToolResult.GetToolUseId() {
					block.ToolCalls[i].IsError = b.ToolResult.GetIsError()
					block.ToolCalls[i].Result = TextFromContent(b.ToolResult.GetContent())
					block.ToolCalls[i].HasResult = true
				}
			}
		}
	}
	s.Blocks = append(s.Blocks, block)
	s.recomputeFinalized()
}

func (s *Session) applyTurnEnd(te *rafikiv1.TurnEnd) {
	if last := s.LastAssistant(); last != nil {
		last.Final = true
		if te.StopReason != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			last.StopReason = te.GetRawStopReason()
		}
	}
	s.settleAll()
}

func (s *Session) applyDelta(delta *rafikiv1.ContentBlockDelta) {
	last := s.LastAssistant()
	if last == nil {
		return
	}
	switch d := delta.Delta.(type) {
	case *rafikiv1.ContentBlockDelta_Text:
		last.Text += d.Text
	case *rafikiv1.ContentBlockDelta_Thinking:
		last.ThinkText += d.Thinking
	case *rafikiv1.ContentBlockDelta_InputJson:
		if len(last.ToolCalls) > 0 {
			last.ToolCalls[len(last.ToolCalls)-1].Input += d.InputJson
		}
	}
}

// applyToolStart marks a call running, appending it only if it is not already
// known.
//
// It is normally NOT new: the assistant message carrying the tool_use block is
// published before the tool runs, so the call is already in the block and this
// event only says execution began. Appending unconditionally listed every tool
// call twice — invisible for as long as fundi published no assistant messages
// at all, and immediate once it did.
func (s *Session) applyToolStart(ts *rafikiv1.ToolExecutionStart) {
	last := s.LastAssistant()
	if last == nil {
		return
	}
	for i := range last.ToolCalls {
		if last.ToolCalls[i].ID == ts.GetToolUseId() {
			last.ToolCalls[i].Running = true
			return
		}
	}
	last.ToolCalls = append(last.ToolCalls, ToolCall{
		ID:      ts.GetToolUseId(),
		Name:    ts.GetName(),
		Running: true,
	})
}

func (s *Session) applyToolEnd(te *rafikiv1.ToolExecutionEnd) {
	last := s.LastAssistant()
	if last == nil {
		return
	}
	for i := range last.ToolCalls {
		if last.ToolCalls[i].ID == te.GetToolUseId() {
			last.ToolCalls[i].Running = false
			last.ToolCalls[i].DurationMs = te.GetDurationMs()
			last.ToolCalls[i].IsError = te.GetIsError()
		}
	}
}

// TextFromContent concatenates the text of every text block in content.
func TextFromContent(content []*rafikiv1.ContentBlock) string {
	var sb strings.Builder
	for _, cb := range content {
		switch b := cb.Block.(type) {
		case *rafikiv1.ContentBlock_Text:
			sb.WriteString(b.Text.GetText())
		case *rafikiv1.ContentBlock_Image:
			// Named rather than dropped. The cockpit cannot draw pixels (see
			// ImagePlaceholder), and an image block that renders as nothing at
			// all is indistinguishable from a tool that returned nothing —
			// which is the same false reassurance a swallowed error gives.
			sb.WriteString(ImagePlaceholder(b.Image))
		}
	}
	return sb.String()
}

// ImagePlaceholder names an image the transcript cannot draw.
//
// bubbletea v2 renders through ultraviolet's cell grid — one grapheme, a
// style, a hyperlink and a width per cell — so a graphics escape sequence
// written into a View is parsed into cells, has nowhere to live, and is
// dropped: measured over a pty, the text either side of an iTerm2 inline-image
// sequence survives and the sequence itself does not.
//
// That is not the last word on images, only on escape sequences. Kitty's
// UNICODE PLACEHOLDER protocol transmits the image out of band and then places
// it with ordinary printable runes (U+10EEEE plus diacritics encoding the row,
// column and image id), which pass through a cell renderer untouched because
// they are just text. charmbracelet/crush does exactly that on this same
// bubbletea version — see internal/ui/image/image.go — with an ANSI half-block
// fallback where the terminal cannot. Until that is built here, say what the
// image is.
func ImagePlaceholder(img *rafikiv1.ImageBlock) string {
	if img == nil {
		return ""
	}
	media := img.GetMediaType()
	if media == "" {
		media = "image"
	}
	out := "🖼 " + media
	// DecodeConfig reads only the header, so this costs a few hundred bytes of
	// parsing rather than decoding the pixels.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(img.GetData())); err == nil {
		out += " " + itoa(cfg.Width) + "×" + itoa(cfg.Height)
	}
	if n := len(img.GetData()); n > 0 {
		out += " (" + humanBytes(n) + ")"
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.Itoa(n) + " B"
	}
}
