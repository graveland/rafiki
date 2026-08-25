// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// ── Messages ────────────────────────────────────────────────────────────────

type eventMsg struct{ ev *rafikiv1.Event }
type tickMsg time.Time

// ── Model ────────────────────────────────────────────────────────────────────

// Model is the bubbletea Model for the single-session TUI.
type Model struct {
	client  rafikiv1connect.ControlClient
	http    *http.Client
	baseURL string
	childID string

	blocks    []Block
	finalized int // index after which blocks may not be cached
	renderer  *renderer
	dirty     bool

	stream *eventStream
	evCh   chan *rafikiv1.Event

	ta       textarea.Model
	status   string
	width    int
	height   int
	quitting bool
	pending  string // sent-but-unacknowledged message text

	ready bool
}

// Options configures the TUI model.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	ChildID    string
}

// NewModel builds a TUI model for the given child.
func NewModel(opts Options) *Model {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := rafikiv1connect.NewControlClient(httpClient, opts.BaseURL)

	ta := textarea.New()
	ta.Placeholder = "Type a message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)

	m := &Model{
		client:   client,
		http:     httpClient,
		baseURL:  opts.BaseURL,
		childID:  opts.ChildID,
		renderer: newRenderer(),
		ta:       ta,
		status:   "connecting…",
	}
	return m
}

// Init starts the event stream.
func (m *Model) Init() tea.Cmd {
	m.evCh = make(chan *rafikiv1.Event, 128)
	m.stream, _ = startStream(m.http, m.baseURL, m.childID)
	if m.stream != nil {
		m.evCh = m.stream.ch
	}
	return tea.Batch(
		waitForEvent(m.evCh),
		tick(),
		textarea.Blink,
	)
}

// ── Update ──────────────────────────────────────────────────────────────────

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ta.SetWidth(msg.Width - 4)
		if !m.ready {
			m.ready = true
		}

	case eventMsg:
		m.handleEvent(msg.ev)
		m.dirty = true
		return m, waitForEvent(m.evCh)

	case tickMsg:
		if m.dirty {
			m.dirty = false
		}
		return m, tick()

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			m.pending = text
			m.sendPrompt(text)
			return m, nil
		default:
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			m.dirty = true
			return m, cmd
		}
	}

	// Route textarea blinks
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// ── View ────────────────────────────────────────────────────────────────────

func (m *Model) View() tea.View {
	if !m.ready {
		v := tea.NewView("Initializing…\n")
		v.AltScreen = true
		return v
	}
	if m.quitting {
		v := tea.NewView("Goodbye.\n")
		v.AltScreen = true
		return v
	}

	// Transcript area takes most of the screen.
	transcriptHeight := m.height - 5 // reserve for input + status
	if transcriptHeight < 1 {
		transcriptHeight = 1
	}

	// Render the transcript.
	transcript := m.renderer.renderBlocks(m.blocks, m.finalized)

	// If there's a pending message, show it.
	var pendingLine string
	if m.pending != "" {
		pendingLine = stylePending.Render("⏳ "+m.pending) + "\n"
	}

	// Assemble: transcript + pending + input + status.
	content := transcript
	if pendingLine != "" {
		content += "\n" + pendingLine
	}

	// Fill remaining space.
	lines := strings.Split(content, "\n")
	visible := lines
	if len(visible) > transcriptHeight {
		visible = visible[len(visible)-transcriptHeight:]
	}
	for len(visible) < transcriptHeight {
		visible = append([]string{""}, visible...)
	}

	input := m.ta.View()
	status := styleMeta.Render(m.status)

	result := strings.Join(visible, "\n")
	result += "\n" + styleDivider + "\n"
	result += input + "\n"
	result += status

	v := tea.NewView(result)
	v.AltScreen = true
	return v
}

// ── Event handling ──────────────────────────────────────────────────────────

func (m *Model) handleEvent(ev *rafikiv1.Event) {
	switch p := ev.Payload.(type) {
	case *rafikiv1.Event_UserMessage:
		m.handleUserMessage(p.UserMessage)
	case *rafikiv1.Event_AssistantMessage:
		m.handleAssistantMessage(p.AssistantMessage)
	case *rafikiv1.Event_TurnStart:
		m.handleTurnStart(p.TurnStart)
	case *rafikiv1.Event_TurnEnd:
		m.handleTurnEnd(p.TurnEnd)
	case *rafikiv1.Event_ContentBlockDelta:
		m.handleContentDelta(p.ContentBlockDelta)
	case *rafikiv1.Event_ToolExecutionStart:
		m.handleToolStart(p.ToolExecutionStart)
	case *rafikiv1.Event_ToolExecutionEnd:
		m.handleToolEnd(p.ToolExecutionEnd)
	case *rafikiv1.Event_AgentStatus:
		m.status = fmt.Sprintf("agent: %s", p.AgentStatus.GetState())
	case *rafikiv1.Event_Error:
		m.status = fmt.Sprintf("error: %s", p.Error.GetMessage())
		m.blocks = appendBlock(m.blocks, Block{
			Kind:  kindSystem,
			At:    time.Now(),
			Text:  fmt.Sprintf("Error: %s", p.Error.GetMessage()),
			Final: true,
		})
	}
}

func (m *Model) handleUserMessage(um *rafikiv1.UserMessage) {
	text := textFromContent(um.GetContent())
	m.blocks = appendBlock(m.blocks, Block{
		Kind:  kindUser,
		At:    time.Now(),
		Text:  text,
		Final: true,
	})
	// Clear pending indicator — our message was acknowledged.
	if m.pending != "" {
		m.pending = ""
	}
	m.finalized = len(m.blocks) // everything including this is finalized
}

func (m *Model) handleAssistantMessage(am *rafikiv1.AssistantMessage) {
	block := Block{
		Kind:       kindAssistant,
		At:         time.Now(),
		Content:    am.GetContent(),
		StopReason: am.GetRawStopReason(),
		Final:      true,
	}
	// Populate tool calls and text from content blocks.
	for _, cb := range am.GetContent() {
		switch b := cb.Block.(type) {
		case *rafikiv1.ContentBlock_Text:
			block.Text += b.Text.GetText()
		case *rafikiv1.ContentBlock_Thinking:
			block.ThinkText += b.Thinking.GetThinking()
		case *rafikiv1.ContentBlock_ToolUse:
			block.ToolCalls = append(block.ToolCalls, toolCallState{
				ID:   b.ToolUse.GetId(),
				Name: b.ToolUse.GetName(),
			})
		case *rafikiv1.ContentBlock_ToolResult:
			for i := range block.ToolCalls {
				if block.ToolCalls[i].ID == b.ToolResult.GetToolUseId() {
					block.ToolCalls[i].IsError = b.ToolResult.GetIsError()
					block.ToolCalls[i].Result = textFromContent(b.ToolResult.GetContent())
				}
			}
		}
	}
	m.blocks = appendBlock(m.blocks, block)
	m.finalized = len(m.blocks)
}

func (m *Model) handleTurnStart(_ *rafikiv1.TurnStart) {
	m.blocks = appendBlock(m.blocks, Block{
		Kind:  kindAssistant,
		At:    time.Now(),
		Final: false,
	})
}

func (m *Model) handleTurnEnd(te *rafikiv1.TurnEnd) {
	last := lastAssistant(m.blocks)
	if last != nil {
		last.Final = true
		if te.StopReason != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			last.StopReason = te.GetRawStopReason()
		}
	}
	m.finalized = len(m.blocks)
}

func (m *Model) handleContentDelta(delta *rafikiv1.ContentBlockDelta) {
	last := lastAssistant(m.blocks)
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

func (m *Model) handleToolStart(ts *rafikiv1.ToolExecutionStart) {
	last := lastAssistant(m.blocks)
	if last == nil {
		return
	}
	last.ToolCalls = append(last.ToolCalls, toolCallState{
		ID:      ts.GetToolUseId(),
		Name:    ts.GetName(),
		Running: true,
	})
}

func (m *Model) handleToolEnd(te *rafikiv1.ToolExecutionEnd) {
	last := lastAssistant(m.blocks)
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

// ── Sending ──────────────────────────────────────────────────────────────────

func (m *Model) sendPrompt(text string) {
	go func() {
		_, _ = m.client.Send(context.Background(),
			connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: m.childID,
				Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
				Blocks: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
				}},
			}))
	}()
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func textFromContent(content []*rafikiv1.ContentBlock) string {
	var sb strings.Builder
	for _, cb := range content {
		t := cb.GetText()
		if t != nil {
			sb.WriteString(t.GetText())
		}
	}
	return sb.String()
}

func waitForEvent(ch <-chan *rafikiv1.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return tea.Quit()
		}
		return eventMsg{ev}
	}
}

func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
