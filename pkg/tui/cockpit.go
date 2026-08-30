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

	"charm.land/bubbles/v2/key"
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

	panes map[string]*paneState
	ta    textarea.Model
	keys  keyMap

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
	// focus is the pane taking un-global keys. Zero value focusInput is
	// correct: a session starts ready to type.
	focus focusPane
	// selected is the rail CURSOR, distinct from the focused child. Browsing
	// must be free: hop opens a focus subscription per call, so a cursor that
	// hopped on every arrow churned one Connect stream per keystroke.
	selected string
	// reseeding is REQUESTED by applyEvent when it sees traffic from a child it
	// does not know; reseedInFlight says one ListChildren is already running.
	// Both are needed: without the second, every event arriving during the RPC
	// queues another one, and the cockpit self-amplifies against a daemon that
	// is already slow -- which is the exact condition the self-heal exists for.
	reseeding      bool
	reseedInFlight bool
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
		panes:    map[string]*paneState{},
		ta:       ta,
		keys:     defaultKeyMap(),
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

// inSubject narrows ListChildren to what the rail subscription actually covers.
//
// ListChildrenRequest has no subject filter -- only statuses -- so an unfiltered
// seed installs a row for every live child on the daemon, including whole trees
// the subscription excludes. Those rows are frozen by construction: their events
// never match the subject, so the glyph stays at its seed-time status forever,
// they never earn an attention badge, and ^↑/^↓ will still park focus on them.
// Rail.Seed's refresh path deliberately does not overwrite Status, so a re-seed
// cannot repair them either. They also bloat Rail.Cursor, making every reconnect
// replay rows the server then discards.
//
// A subject the client cannot evaluate (an unrecognised scope) narrows to
// nothing rather than everything: a rail that is missing rows is visibly wrong,
// while a rail full of frozen ones looks fine and lies.
func (c *Cockpit) inSubject(all []*rafikiv1.ChildSummary) []*rafikiv1.ChildSummary {
	switch scope := c.subject.GetScope().(type) {
	case *rafikiv1.EventSubject_All:
		return all
	case *rafikiv1.EventSubject_Child:
		for _, s := range all {
			if s.GetChildId() == scope.Child {
				return []*rafikiv1.ChildSummary{s}
			}
		}
		return nil
	case *rafikiv1.EventSubject_Subtree:
		parent := make(map[string]string, len(all))
		for _, s := range all {
			parent[s.GetChildId()] = s.GetLabels()[rail.ParentLabel]
		}
		descends := func(id string) bool {
			// Bounded by the node count so a malformed parent label cannot
			// spin here; the daemon's lineage rules make a cycle impossible
			// but nothing in a label enforces that.
			for i := 0; i < len(parent); i++ {
				p, ok := parent[id]
				if !ok || p == "" {
					return false
				}
				if p == scope.Subtree {
					return true
				}
				id = p
			}
			return false
		}
		out := make([]*rafikiv1.ChildSummary, 0, len(all))
		for _, s := range all {
			if s.GetChildId() == scope.Subtree {
				if c.subject.GetIncludeSelf() {
					out = append(out, s)
				}
				continue
			}
			if descends(s.GetChildId()) {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
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
		c.reseedInFlight = false
		if msg.err != nil {
			c.status = "list children: " + msg.err.Error()
			return c, nil
		}
		first := c.stopRail == nil
		c.rail.Seed(c.inSubject(msg.children))
		if first {
			c.status = "connected"
			c.stopRail = streams.StartRail(context.Background(), c.cfg, c.subject,
				c.rail.Cursor, c.evCh)
			// openFocus, NOT hop: hop refuses a no-op move, and the initial
			// child is already focused (NewCockpit set it so the first frame
			// renders the right pane). Routing this through hop made the call
			// unconditionally return nil, so `attach <id>` and `create` opened
			// a cockpit whose only event source was the rail -- six small types,
			// no messages, no deltas, no history: a permanently empty pane.
			if f := c.focused(); f != "" {
				c.openFocus(f)
			}
		}
		return c, nil

	case eventMsg:
		c.applyEvent(msg.ev)
		var cmd tea.Cmd
		if c.reseeding && !c.reseedInFlight {
			c.reseeding = false
			c.reseedInFlight = true
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

	// ONLY the focused child's session may be advanced. The rail subscription
	// covers every child in the subject but carries none of their content, so
	// applying it to a retained, non-focused session pushes that session's
	// cursor far past what it has actually rendered -- and hop resumes from
	// exactly that cursor. The effect was silent and total: hop away from a
	// working agent, hop back, and every message it produced meanwhile is gone
	// with no error and no gap marker.
	if id != c.focused() {
		return
	}
	s := c.sessions[id]
	if s == nil {
		return
	}
	before := len(s.Blocks)
	s.Apply(ev)
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

// handleKey routes a keystroke: globals first, then the focused pane.
//
// The ordering is the design. Anything matched GLOBALLY is a key the textarea
// can never receive, because the input pane's fallthrough is where unmatched
// keys end up. bubbles/v2/textarea's DefaultKeyMap is emacs-heavy and already
// claims pgup, pgdown, shift+arrows, ctrl+n/p/f/u/k/a/e and more — which is
// exactly why the cockpit has a focus ring instead of modified scroll keys.
// TestNoGlobalBindingStealsATextareaKey fails the build if this list grows
// into one of them.
func (c *Cockpit) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := c.keys

	// ── globals: live from every pane ────────────────────────────────────
	switch {
	case key.Matches(msg, k.Quit):
		c.quitting = true
		c.shutdown()
		return c, tea.Quit
	case key.Matches(msg, k.NextPane):
		c.cyclePane(+1)
		return c, nil
	case key.Matches(msg, k.PrevPane):
		c.cyclePane(-1)
		return c, nil
	case key.Matches(msg, k.NextAttention):
		return c, c.hop(c.rail.NextAttention())
	case key.Matches(msg, k.PrevAttention):
		return c, c.hop(c.rail.PrevAttention())
	case key.Matches(msg, k.HopPrev):
		return c, c.hop(c.neighbour(-1))
	case key.Matches(msg, k.HopNext):
		return c, c.hop(c.neighbour(+1))
	case key.Matches(msg, k.ToggleRail):
		c.railHidden = !c.railHidden
		// Never leave focus on a pane that just became invisible.
		c.cyclePane(0)
		return c, nil
	case key.Matches(msg, k.Help):
		c.showHelp = !c.showHelp
		return c, nil
	}

	// ── pane-local ───────────────────────────────────────────────────────
	switch c.focus {
	case focusRail:
		switch {
		case key.Matches(msg, k.SelectUp):
			c.moveSelection(-1)
		case key.Matches(msg, k.SelectDown):
			c.moveSelection(+1)
		case key.Matches(msg, k.Commit):
			cmd := c.hop(c.selected)
			c.focus = focusInput
			return c, cmd
		case key.Matches(msg, k.Escape):
			c.focus = focusInput
		}
		// The rail swallows everything else: a stray letter here must not
		// reach the textarea, or you would type into an input you cannot see
		// the cursor of.
		return c, nil

	case focusTranscript:
		if key.Matches(msg, k.Escape) {
			c.focus = focusInput
			return c, nil
		}
		return c, c.scrollFocused(msg)
	}

	// ── input ────────────────────────────────────────────────────────────
	if mode := modeForKey(msg.String()); mode != rafikiv1.SendMode_SEND_MODE_UNSPECIFIED {
		text := strings.TrimSpace(c.ta.Value())
		if mode != rafikiv1.SendMode_SEND_MODE_ABORT {
			if text == "" {
				return c, nil
			}
			c.ta.Reset()
			// You just spoke; you want the reply. Pin to the bottom even if you
			// had scrolled up to re-read something before sending.
			if f := c.focused(); f != "" {
				p := c.pane(f)
				p.vp.GotoBottom()
				p.atBottom = true
			}
		}
		return c, c.send(mode, text)
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return c, cmd
}

// scrollFocused forwards a key to the focused child's viewport.
//
// The viewport keeps its DEFAULT keymap here — ↑/↓, j/k, space, PgUp/PgDn —
// which is safe precisely because typing is inactive while this pane holds
// focus. That is the payoff of the focus ring; with a permanently-focused
// textarea none of those keys were available.
func (c *Cockpit) scrollFocused(msg tea.KeyPressMsg) tea.Cmd {
	f := c.focused()
	if f == "" {
		return nil
	}
	p := c.pane(f)
	var cmd tea.Cmd
	p.vp, cmd = p.vp.Update(msg)
	p.atBottom = p.vp.AtBottom()
	return cmd
}

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
	// No renderer reset: each child owns its renderer (see pane.go), so there
	// is no shared cache to invalidate and the target's cached work survives.
	c.openFocus(childID)
	return nil
}

// openFocus starts the focus subscription for childID, creating its session if
// needed. It has no identity guard, which is the whole reason it is separate
// from hop: the seed path focuses a child and then needs its stream opened.
func (c *Cockpit) openFocus(childID string) {
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
		c.evictPane(victim)
	}
}

// neighbour returns the child delta rows away in display order.
// cyclePane advances focus by delta, skipping the rail when it is hidden.
//
// A delta of 0 re-validates the current focus without moving, which is what
// hiding the rail needs: ctrl+b is global and can fire while the rail holds
// focus, and focus left on an invisible pane is how a modal UI traps someone.
func (c *Cockpit) cyclePane(delta int) {
	order := []focusPane{focusInput, focusRail, focusTranscript}
	if c.railHidden {
		order = []focusPane{focusInput, focusTranscript}
	}
	idx := 0
	found := false
	for i, p := range order {
		if p == c.focus {
			idx, found = i, true
			break
		}
	}
	if !found {
		// Current focus is not in the ring at all (the rail, just hidden).
		// Fall to input rather than guessing a neighbour.
		c.focus = focusInput
		if delta == 0 {
			return
		}
		idx = 0
	}
	idx = (idx + delta + len(order)) % len(order)
	c.focus = order[idx]
	if c.focus == focusRail && c.selected == "" {
		c.selected = c.focused()
	}
}

// moveSelection moves the rail cursor by delta without hopping.
//
// Clamped, where neighbour() wraps. Wrapping is what the attention jump does,
// and two bindings that both wrap are indistinguishable in use. An empty
// selection starts from the focused child, so tabbing into the rail begins
// where you are looking rather than at the top.
func (c *Cockpit) moveSelection(delta int) {
	nodes := c.rail.Nodes()
	if len(nodes) == 0 {
		return
	}
	if c.selected == "" {
		c.selected = c.focused()
	}
	idx := 0
	for i, n := range nodes {
		if n.ChildID == c.selected {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(nodes) {
		idx = len(nodes) - 1
	}
	c.selected = nodes[idx].ChildID
}

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

// bodyHeight and convWidth are the conversation pane's dimensions. Extracted
// so the viewport and the layout cannot disagree about the space available —
// they were inline in View and the viewport needs the same numbers.
func (c *Cockpit) bodyHeight() int {
	h := c.height - 6
	if h < 1 {
		return 1
	}
	return h
}

func (c *Cockpit) convWidth() int {
	w := c.width
	if !c.railHidden && c.rail.Len() > 1 {
		w = c.width - railWidth - 1
	}
	if w < 10 {
		return 10
	}
	return w
}

// syncViewport feeds rendered lines to a pane's viewport, preserving the
// reader's position.
//
// Follow mode: a pane at the bottom stays pinned as output arrives, which is
// the common case of watching a live agent. A pane scrolled up keeps its
// offset — being yanked to the bottom mid-read is worse than missing the
// newest line, and the footer's "↓ more below" marker says there is more.
//
// SetContentLines rather than SetContent: the renderer already works in lines,
// it avoids a join/split every frame, and when log-backed paging lands the
// YOffset shift after a prepend is exactly len(prepended).
func (c *Cockpit) syncViewport(p *paneState, lines []string) {
	wasAtBottom := p.vp.AtBottom()
	prev := p.vp.YOffset()

	p.vp.SetWidth(c.convWidth())
	p.vp.SetHeight(c.bodyHeight())
	p.vp.SetContentLines(lines)

	if wasAtBottom {
		p.vp.GotoBottom()
	} else {
		p.vp.SetYOffset(prev)
	}
	p.atBottom = p.vp.AtBottom()
}

// footerHints renders the bindings that apply in the focused pane.
//
// Derived from the keymap rather than hand-written, because the cockpit used to
// carry the key list in two separate literals with nothing keeping them in
// agreement. A footer that lies about the keys is worse than no footer,
// especially now that a key's meaning depends on which pane holds focus.
func (c *Cockpit) footerHints() string {
	k := c.keys
	var bs []key.Binding
	switch c.focus {
	case focusRail:
		bs = []key.Binding{k.SelectUp, k.SelectDown, k.Commit, k.Escape}
	case focusTranscript:
		bs = []key.Binding{k.Escape}
	default:
		bs = []key.Binding{k.Send, k.Steer, k.Abort, k.NextAttention}
	}
	parts := make([]string, 0, len(bs)+3)
	for _, b := range bs {
		h := b.Help()
		parts = append(parts, h.Key+" "+h.Desc)
	}
	parts = append(parts,
		c.keys.NextPane.Help().Key+" pane",
		c.keys.ToggleRail.Help().Key+" rail",
		c.keys.Help.Help().Key+" help")
	return strings.Join(parts, "  ")
}

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
		railText = renderRail(c.rail.Nodes(), c.focused(), c.selected, railWidth)
	}

	bodyHeight := c.bodyHeight()
	convWidth := c.convWidth()

	var conv string
	if f := c.focused(); f == "" {
		conv = styleMeta.Render("Pick an agent — ⇥ to the rail, ↑/↓ to move, ⏎ to open.")
	} else if s := c.sessions[f]; s != nil {
		p := c.pane(f)
		c.syncViewport(p, p.renderer.Lines(s.Blocks, s.Finalized))
		conv = p.vp.View()
	} else {
		conv = styleMeta.Render("loading…")
	}
	if c.pending != "" {
		conv += "\n" + stylePending.Render("⏳ "+c.pending)
	}

	// The focused pane is NAMED, always. A modal UI whose mode is invisible is
	// how someone gets stuck: keys stop doing what they expect and nothing on
	// screen says why.
	footer := "[" + c.focus.String() + "]  " + c.footerHints()
	if f := c.focused(); f != "" {
		if p := c.panes[f]; p != nil && !p.atBottom {
			footer = "↓ more below  " + footer
		}
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
