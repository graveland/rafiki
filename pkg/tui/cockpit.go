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
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/session"
	"go.graveland.dev/rafiki/pkg/tui/streams"
)

// railWidth is fixed rather than proportional: names are short, and a rail that
// resizes with the window makes the conversation reflow on every drag.
const railWidth = 22

// maxSessions bounds retained transcripts. An evicted session falls back to a
// full replay on next focus, which is exactly what `attach` did before the
// cockpit -- the eviction path is the already-shipped path, not a new one.
const maxSessions = 12

// ── Messages ────────────────────────────────────────────────────────────────

type eventMsg struct{ ev *rafikiv1.Event }
type tickMsg time.Time
type seedMsg struct {
	children []*rafikiv1.ChildSummary
	err      error
}
type sendFailedMsg struct{ err error }

// ── Options ─────────────────────────────────────────────────────────────────

// Options configures the cockpit.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	// ChildID is the initially focused child. Empty opens rail-first, with
	// nothing focused -- which is what a bare `rafiki attach` does.
	ChildID string
	// Subject is the rail subscription's scope. Nil means `all`.
	Subject *rafikiv1.EventSubject
}

// ── Model ───────────────────────────────────────────────────────────────────

// Cockpit is the bubbletea model.
type Cockpit struct {
	cfg     streams.Config
	client  rafikiv1connect.ControlClient
	subject *rafikiv1.EventSubject

	rail     *rail.Rail
	sessions map[string]*session.Session
	lru      []string // least-recently-focused first

	renderer *renderer
	ta       textarea.Model

	evCh      chan *rafikiv1.Event
	stopRail  func()
	stopFocus func()

	width, height int
	ready         bool
	quitting      bool
	status        string
	pending       string
	showHelp      bool
	railHidden    bool
	// reseeding guards the self-heal in applyEvent so a burst of events from a
	// child we do not know cannot queue one ListChildren per event.
	reseeding bool
}

// NewCockpit builds the cockpit. A non-empty opts.ChildID opens session-first on
// that child; an empty one opens rail-first with nothing focused.
func NewCockpit(opts Options) *Cockpit {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	cfg := streams.Config{HTTPClient: httpClient, BaseURL: opts.BaseURL}

	ta := textarea.New()
	ta.Placeholder = "Type a message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)

	subject := opts.Subject
	if subject == nil {
		subject = &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}}
	}

	c := &Cockpit{
		cfg:      cfg,
		client:   rafikiv1connect.NewControlClient(httpClient, opts.BaseURL),
		subject:  subject,
		rail:     rail.New(),
		sessions: make(map[string]*session.Session),
		renderer: newRenderer(),
		ta:       ta,
		evCh:     make(chan *rafikiv1.Event, 256),
		status:   "connecting…",
	}
	if opts.ChildID != "" {
		c.rail.SetFocus(opts.ChildID)
		c.sessions[opts.ChildID] = session.New(opts.ChildID)
		c.lru = []string{opts.ChildID}
	}
	return c
}

func (c *Cockpit) focused() string { return c.rail.Focus() }

// Init seeds the rail from ListChildren and starts the event pump.
func (c *Cockpit) Init() tea.Cmd {
	return tea.Batch(c.seedCmd(), waitForEvent(c.evCh), tick(), textarea.Blink)
}

func (c *Cockpit) seedCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.client.ListChildren(ctx,
			connect.NewRequest(&rafikiv1.ListChildrenRequest{Statuses: rail.LiveStatuses()}))
		if err != nil {
			return seedMsg{err: err}
		}
		return seedMsg{children: resp.Msg.GetChildren()}
	}
}

// ── Update ──────────────────────────────────────────────────────────────────

func (c *Cockpit) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.ta.SetWidth(msg.Width - 4)
		c.ready = true
		return c, nil

	case seedMsg:
		c.reseeding = false
		if msg.err != nil {
			c.status = "list children: " + msg.err.Error()
			return c, nil
		}
		first := c.stopRail == nil
		c.rail.Seed(msg.children)
		if first {
			c.status = "connected"
			c.stopRail = streams.StartRail(context.Background(), c.cfg, c.subject,
				c.rail.Cursor, c.evCh)
			if f := c.focused(); f != "" {
				return c, c.hop(f)
			}
		}
		return c, nil

	case eventMsg:
		c.applyEvent(msg.ev)
		var cmd tea.Cmd
		if c.reseeding {
			cmd = c.seedCmd()
		}
		return c, tea.Batch(waitForEvent(c.evCh), cmd)

	case sendFailedMsg:
		c.status = "send failed: " + msg.err.Error()
		c.pending = ""
		return c, nil

	case tickMsg:
		return c, tick()

	case tea.KeyPressMsg:
		return c.handleKey(msg)
	}

	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return c, cmd
}

// applyEvent routes one event to the rail and, when it is the focused child's,
// to that session. Both see it: the rail needs the ordinal for RailCursor even
// while the user is looking at the child.
func (c *Cockpit) applyEvent(ev *rafikiv1.Event) {
	id := ev.GetChildId()

	// Self-heal. child_spawned is the only event that introduces a rail row,
	// and a child spawned while this client was disconnected has its
	// child_spawned in the past -- the server replays only children named in
	// the cursor, so that row would never appear and the child would be
	// invisible for the rest of the session. Seeing traffic from a child we do
	// not know is the signal to re-seed from ListChildren, which is idempotent
	// and preserves reading history.
	if _, known := c.rail.Get(id); !known && id != "" && ev.GetChildSpawned() == nil {
		c.reseeding = true
	}

	c.rail.Apply(ev)

	s := c.sessions[id]
	if s == nil {
		return
	}
	before := len(s.Blocks)
	s.Apply(ev)
	if id != c.focused() {
		return
	}
	if s.HasCursor {
		// Delivery to the focused session IS reading.
		c.rail.MarkRead(id, s.Cursor)
	}
	if s.Status != "" {
		c.status = "agent: " + s.Status
	}
	// Our own message coming back is the acknowledgement. Send means DURABLY
	// QUEUED, not written to a pipe, so the RPC returning is not the agent
	// having taken it into a turn.
	if c.pending != "" && len(s.Blocks) > before &&
		s.Blocks[len(s.Blocks)-1].Kind == session.KindUser {
		c.pending = ""
	}
}

func (c *Cockpit) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		c.quitting = true
		c.shutdown()
		return c, tea.Quit
	case "tab":
		return c, c.hop(c.rail.NextAttention())
	case "ctrl+up":
		return c, c.hop(c.neighbour(-1))
	case "ctrl+down":
		return c, c.hop(c.neighbour(+1))
	case "ctrl+b":
		c.railHidden = !c.railHidden
		return c, nil
	case "ctrl+g":
		c.showHelp = !c.showHelp
		return c, nil
	}
	if mode := modeForKey(msg.String()); mode != rafikiv1.SendMode_SEND_MODE_UNSPECIFIED {
		text := strings.TrimSpace(c.ta.Value())
		if mode != rafikiv1.SendMode_SEND_MODE_ABORT {
			if text == "" {
				return c, nil
			}
			c.ta.Reset()
		}
		return c, c.send(mode, text)
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return c, cmd
}

// modeForKey maps a keypress to a send mode, or UNSPECIFIED for keys that do
// not send.
//
// ctrl+s is the steer fallback because many terminals and multiplexers consume
// alt+enter before the program sees it.
//
// Prompt and steer stay SEPARATE keys rather than being inferred from agent
// state. Inferring reads as elegant and removes a real choice: C1a-2 made a
// prompt to a busy agent durably QUEUE, so "queue a follow-up for when it
// finishes" and "interrupt what it is doing now" are both things a user wants,
// and only the user knows which.
func modeForKey(key string) rafikiv1.SendMode {
	switch key {
	case "enter":
		return rafikiv1.SendMode_SEND_MODE_PROMPT
	case "alt+enter", "ctrl+s":
		return rafikiv1.SendMode_SEND_MODE_STEER
	case "ctrl+x":
		return rafikiv1.SendMode_SEND_MODE_ABORT
	}
	return rafikiv1.SendMode_SEND_MODE_UNSPECIFIED
}

// send submits to the focused child.
func (c *Cockpit) send(mode rafikiv1.SendMode, text string) tea.Cmd {
	child := c.focused()
	if child == "" {
		return nil
	}
	req := &rafikiv1.SendRequest{ChildId: child, Mode: mode}
	if mode != rafikiv1.SendMode_SEND_MODE_ABORT {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		req.Blocks = []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
		}}
		c.pending = text
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.client.Send(ctx, connect.NewRequest(req)); err != nil {
			return sendFailedMsg{err: err}
		}
		return nil
	}
}

// hop changes focus. It closes the old focus stream, WATERMARKS the child being
// left, keeps its transcript, and opens the new child's stream from wherever
// that session's cursor left off -- so only the delta replays.
func (c *Cockpit) hop(childID string) tea.Cmd {
	if childID == "" || childID == c.focused() {
		return nil
	}
	if old := c.focused(); old != "" {
		if s := c.sessions[old]; s != nil && s.HasCursor {
			c.rail.MarkRead(old, s.Cursor)
		}
	}
	if c.stopFocus != nil {
		c.stopFocus()
		c.stopFocus = nil
	}

	c.rail.SetFocus(childID)
	s := c.sessions[childID]
	if s == nil {
		s = session.New(childID)
		c.sessions[childID] = s
	}
	c.touch(childID)

	// -1 rather than 0 for a fresh session: ordinal 0 is a real event and the
	// log's Read is exclusive on afterOrdinal, so 0 would skip the first event.
	after := int32(-1)
	if s.HasCursor {
		after = s.Cursor
	}
	c.stopFocus = streams.StartFocus(context.Background(), c.cfg, childID,
		&rafikiv1.EventCursor{Ordinals: map[string]int32{childID: after}}, c.evCh)
	return nil
}

// touch marks childID most-recently-used and evicts past maxSessions.
func (c *Cockpit) touch(childID string) {
	for i, id := range c.lru {
		if id == childID {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}
	c.lru = append(c.lru, childID)
	for len(c.lru) > maxSessions {
		victim := c.lru[0]
		c.lru = c.lru[1:]
		if victim == c.focused() {
			// Never evict what the user is looking at.
			c.lru = append([]string{victim}, c.lru...)
			break
		}
		delete(c.sessions, victim)
	}
}

// neighbour returns the child delta rows away in display order.
func (c *Cockpit) neighbour(delta int) string {
	nodes := c.rail.Nodes()
	if len(nodes) == 0 {
		return ""
	}
	idx := 0
	for i, n := range nodes {
		if n.ChildID == c.focused() {
			idx = i
			break
		}
	}
	return nodes[(idx+delta+len(nodes))%len(nodes)].ChildID
}

// shutdown stops both streams. Safe to call more than once.
func (c *Cockpit) shutdown() {
	if c.stopFocus != nil {
		c.stopFocus()
		c.stopFocus = nil
	}
	if c.stopRail != nil {
		c.stopRail()
		c.stopRail = nil
	}
}

// ── View ────────────────────────────────────────────────────────────────────

const helpText = "⏎ send · ⌥⏎ or ^S steer the running turn · ^X abort · " +
	"⇥ next agent needing you · ^↑/^↓ move · ^B rail · ^G help · ^C quit"

func (c *Cockpit) View() tea.View {
	if !c.ready {
		v := tea.NewView("Initializing…\n")
		v.AltScreen = true
		return v
	}
	if c.quitting {
		v := tea.NewView("Goodbye.\n")
		v.AltScreen = true
		return v
	}

	railText := ""
	if !c.railHidden {
		railText = renderRail(c.rail.Nodes(), c.focused(), railWidth)
	}

	bodyHeight := c.height - 6
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	convWidth := c.width
	if railText != "" {
		convWidth = c.width - railWidth - 1
	}
	if convWidth < 10 {
		convWidth = 10
	}

	var conv string
	if f := c.focused(); f == "" {
		conv = styleMeta.Render("Pick an agent — ^↑/^↓ to move, ⇥ for the next that needs you.")
	} else if s := c.sessions[f]; s != nil {
		conv = c.renderer.renderBlocks(s.Blocks, s.Finalized)
	} else {
		conv = styleMeta.Render("loading…")
	}
	if c.pending != "" {
		conv += "\n" + stylePending.Render("⏳ "+c.pending)
	}

	footer := helpText
	if !c.showHelp {
		footer = "⏎ send  ⌥⏎ steer  ^X abort  ⇥ next  ^B rail  ^G help"
	}

	out := joinColumns(railText, conv, railWidth, convWidth, bodyHeight) +
		"\n" + styleDivider + "\n" + c.ta.View() + "\n" +
		styleMeta.Render(c.status) + "\n" + styleMeta.Render(footer)

	v := tea.NewView(out)
	v.AltScreen = true
	return v
}

// joinColumns places the rail beside the conversation, both clipped to height.
func joinColumns(left, right string, leftWidth, rightWidth, height int) string {
	rightLines := lastN(strings.Split(right, "\n"), height)
	if left == "" {
		return strings.Join(rightLines, "\n")
	}
	leftLines := strings.Split(strings.TrimRight(left, "\n"), "\n")

	var sb strings.Builder
	for i := 0; i < height; i++ {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		sb.WriteString(padTo(l, leftWidth))
		sb.WriteString("│")
		sb.WriteString(clip(r, rightWidth))
		if i < height-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// lastN returns the final n lines, padded at the top so the transcript sits at
// the bottom of its pane.
func lastN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	out := make([]string, 0, n)
	for i := len(lines); i < n; i++ {
		out = append(out, "")
	}
	return append(out, lines...)
}

// ── Plumbing ────────────────────────────────────────────────────────────────

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
