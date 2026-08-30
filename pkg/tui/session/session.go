// SPDX-License-Identifier: Apache-2.0

// Package session holds one child's conversation as a pure state machine.
//
// It takes events and nothing else: no bubbletea, no network, no terminal.
// That purity is the point -- rendering and transport are the shell's problem,
// and a conversation you can drive from a slice of events is a conversation you
// can test.
package session

import (
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
	ID         string
	Name       string
	Input      string
	Result     string
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
		s.Finalized = len(s.Blocks)
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

func (s *Session) applyUserMessage(um *rafikiv1.UserMessage) {
	s.Blocks = append(s.Blocks, Block{
		Kind:  KindUser,
		At:    time.Now(),
		Text:  TextFromContent(um.GetContent()),
		Final: true,
	})
	s.Finalized = len(s.Blocks)
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
				ID:   b.ToolUse.GetId(),
				Name: b.ToolUse.GetName(),
			})
		case *rafikiv1.ContentBlock_ToolResult:
			for i := range block.ToolCalls {
				if block.ToolCalls[i].ID == b.ToolResult.GetToolUseId() {
					block.ToolCalls[i].IsError = b.ToolResult.GetIsError()
					block.ToolCalls[i].Result = TextFromContent(b.ToolResult.GetContent())
				}
			}
		}
	}
	s.Blocks = append(s.Blocks, block)
	s.Finalized = len(s.Blocks)
}

func (s *Session) applyTurnEnd(te *rafikiv1.TurnEnd) {
	if last := s.LastAssistant(); last != nil {
		last.Final = true
		if te.StopReason != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			last.StopReason = te.GetRawStopReason()
		}
	}
	s.Finalized = len(s.Blocks)
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

func (s *Session) applyToolStart(ts *rafikiv1.ToolExecutionStart) {
	last := s.LastAssistant()
	if last == nil {
		return
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
		if t := cb.GetText(); t != nil {
			sb.WriteString(t.GetText())
		}
	}
	return sb.String()
}
