// SPDX-License-Identifier: Apache-2.0

// Package tui is the rafiki cockpit: a tree rail beside one child's
// conversation, over the Connect control plane.
//
// State lives in three pure subpackages -- session (one conversation), rail
// (the tree, activity and attention) and streams (subscription lifetimes).
// This package is the bubbletea shell over them: layout, keys and rendering.
// See docs/plans/2026-08-30-rafiki-tui-c1b-design.md.
package tui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/tui/session"
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

	sess     *session.Session
	renderer *renderer
	dirty    bool

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
		sess:     session.New(opts.ChildID),
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
		before := len(m.sess.Blocks)
		m.sess.Apply(msg.ev)
		if m.sess.Status != "" {
			m.status = "agent: " + m.sess.Status
		}
		// Our own message coming back is the acknowledgement. Send means
		// DURABLY QUEUED, not written to a pipe, so the RPC returning is not
		// the agent having taken it.
		if m.pending != "" && len(m.sess.Blocks) > before &&
			m.sess.Blocks[len(m.sess.Blocks)-1].Kind == session.KindUser {
			m.pending = ""
		}
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
	transcript := m.renderer.renderBlocks(m.sess.Blocks, m.sess.Finalized)

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
