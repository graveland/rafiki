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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/session"
	"go.graveland.dev/rafiki/pkg/tui/streams"
)

// quitConfirmWindow is how long a first ^C/^D stays armed. Long enough to be a
// deliberate double-tap, short enough that a ^C now and a ^C a minute later are
// two separate intentions rather than a quit.
const quitConfirmWindow = 2 * time.Second

// A paste is folded into a token once it passes EITHER bound.
//
// Lines alone was not enough: a minified file, a long URL or one wide log line
// is a SINGLE line of many thousands of characters, and it went straight into
// the box — pinning the input at its ten-row cap where it stops showing the
// end of what you typed, which is what folding exists to prevent. The
// character bound sits near what the box can show at that cap, so the rule is
// roughly "more than fits on screen".
const (
	pasteLineThreshold = 6
	pasteCharThreshold = 800
)

// imageExts are the extensions recognised as an attachable image. A closed
// list, not a sniff: this decides whether an ordinary pasted string is treated
// as a file reference at all, and a false positive turns a path-shaped message
// into a failed file read.
var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// maxAttachmentBytes bounds what a paste can stage. Anthropic rejects images
// over roughly 5 MB, and a bound with a message beats an opaque upstream 400.
const maxAttachmentBytes = 5 << 20

// stagedAttachment is an image staged for the next prompt.
type stagedAttachment struct {
	token     string
	notice    string
	mediaType string
	data      []byte
}

// attachIfImagePath reads content as a path to an image and stages it.
//
// It reports false for anything that is not a single line naming a readable
// image file, so ordinary text is untouched. Every rejection after that point
// is reported rather than silent: someone who drags a file and sees their path
// turn into plain text has no idea why.
func (c *Cockpit) attachIfImagePath(content string) (stagedAttachment, bool) {
	line := strings.TrimSpace(content)
	// Terminals quote a dragged path when it contains spaces.
	line = strings.Trim(line, "'\"")
	if line == "" || strings.Contains(line, "\n") {
		return stagedAttachment{}, false
	}
	media, ok := imageExts[strings.ToLower(filepath.Ext(line))]
	if !ok {
		return stagedAttachment{}, false
	}
	if strings.HasPrefix(line, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			line = filepath.Join(home, line[2:])
		}
	}
	info, err := os.Stat(line)
	if err != nil || info.IsDir() {
		return stagedAttachment{}, false
	}
	if info.Size() > maxAttachmentBytes {
		c.setNotice("image is " + humanBytes(info.Size()) + "; the limit is " +
			humanBytes(maxAttachmentBytes))
		return stagedAttachment{}, true
	}
	data, err := os.ReadFile(line)
	if err != nil {
		c.setNotice("could not read " + filepath.Base(line) + ": " + err.Error())
		return stagedAttachment{}, true
	}
	name := filepath.Base(line)
	return stagedAttachment{
		token:     "[" + name + "]",
		notice:    "attached " + name + " (" + humanBytes(int64(len(data))) + ")",
		mediaType: media,
		data:      data,
	}, true
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return itoa(n) + " B"
	}
}

// pastedText is one folded paste, held until the prompt is sent.
type pastedText struct {
	token string
	text  string
}

// The input box grows with its content between these bounds.
const (
	minInputHeight = 1
	maxInputHeight = 10
)

// The rail sizes to its content between these bounds -- see railWidthFor for
// why content and not the window. railMin is the old fixed width, which is
// still the floor; railMaxPct stops one long name eating the transcript.
const (
	railMin    = 22
	railMaxPct = 40
)

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

type tasksLoadedMsg struct {
	childID string
	rows    []*rafikiv1.TaskRow
}

// historyMsg carries one child's persisted conversation, plus the event-log
// ordinal that was current when the fetch was ISSUED -- the focus stream
// resumes from that, so anything logged during the fetch still arrives.
type historyMsg struct {
	childID string
	events  []*rafikiv1.Event
	after   int32
	err     error
}

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
	// OpenCreate opens straight into the create form, for `rafiki create` with
	// nothing to go on. CreateDefaults prefills it -- with exactly what a bare
	// create would have spawned, so the default case costs one ⏎ and shows
	// what it is about to do rather than replacing it with a questionnaire.
	OpenCreate     bool
	CreateDefaults SpawnDefaults
}

// SpawnDefaults prefills the create form. Empty fields keep the form's own
// defaults; cwd falls back to the client's working directory.
type SpawnDefaults struct {
	Name  string
	Kind  string
	Model string
	Cwd   string
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
	expandArgs    bool
	// railPeek records that ⇥ revealed a hidden rail, so committing or
	// cancelling puts it back. Picking an agent is a round trip, not a mode
	// change: you wanted a different conversation, not a permanently wider
	// piece of furniture.
	railPeek bool
	// tasks holds the focused child's ledger, keyed by child id so a hop back
	// shows the last known list rather than an empty box while the refetch is
	// in flight.
	tasks map[string][]*rafikiv1.TaskRow
	// taskRefresh says the event just processed completed a ledger mutation
	// for the focused child and the next Update should issue one fetch.
	// applyEvent is a void fold, so the request rides out to the caller
	// instead of a tea.Cmd being minted mid-fold.
	taskRefresh bool
	// pastes holds folded pastes in insertion order, keyed by the token
	// standing in for each. Cleared on send: the tokens leave with the text.
	pastes []pastedText
	// attachments are non-text payloads staged for the next prompt. Same
	// lifetime as pastes and cleared with them: a token you deleted took its
	// payload with it.
	attachments []stagedAttachment
	// quitArmed is when the first ^C/^D landed. A single stray one must not
	// throw away an attached session, so the key arms and the repeat quits.
	quitArmed time.Time
	// notice is a transient line shown instead of status, for things the user
	// needs to see once (the quit confirmation) rather than a standing state.
	notice      string
	noticeUntil time.Time
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

	// form is the open create modal, nil when none. A modal owns every key
	// while it is up, so this is checked before the global bindings.
	form *spawnForm
	// picker is the full model browser opened from the form's model row, nil
	// when none. It stacks ON TOP of the form -- esc returns to the form
	// rather than dismissing both, because the other fields are still half
	// filled in.
	picker *modelPicker
	// models caches the daemon's answer per KIND, so the form's typeahead
	// filters locally on every keystroke instead of asking per character, and
	// so the full picker opens instantly rather than re-fetching what the
	// typeahead already has. Kind is the key because the two kinds have
	// genuinely different model universes.
	models     map[string][]*rafikiv1.ModelRow
	modelsErr  map[string]string
	modelsBusy map[string]bool
	// query is the open filter+sort band, nil when none. It sits OVER the
	// picker or the form rather than replacing them, so every keystroke
	// re-sorts the rows still visible above it.
	query *queryDialog
	// modelView is the shared sort and vision filter. On the cockpit rather
	// than either view: the typeahead and the full browser are two windows
	// onto one question, and sorting in one must hold in the other.
	modelView modelView
	// endArmed is when `x` was first pressed, and endArmedID is WHICH row it
	// was pressed on. The id is not optional bookkeeping: arming on one agent
	// and then moving the cursor before the repeat would otherwise end a
	// different agent than the one the confirmation named.
	endArmed   time.Time
	endArmedID string

	// currency is the display conversion loaded once at construction, the
	// same way modelView is -- a `rafiki config set` takes effect on the
	// next cockpit start, not live.
	currency *clientstate.Currency
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
	// Grow with the prompt, within bounds. A fixed three rows wasted two of
	// them on the common one-line prompt and hid everything past the third on
	// a long one. MaxHeight is a real cap rather than a formality: the input
	// and the transcript share the window, and a prompt that can swallow the
	// conversation it is about is worse than one that scrolls.
	ta.DynamicHeight = true
	ta.MinHeight = minInputHeight
	ta.MaxHeight = maxInputHeight
	ta.SetHeight(minInputHeight)
	// A textarea is constructed BLURRED, and bubbles' Update returns
	// immediately while it is -- so an unfocused one silently swallows every
	// printable key. The cockpit shipped without this call and could not be
	// typed into at all. The returned blink command is dropped because Init
	// already issues textarea.Blink; what matters here is the focus flag.
	_ = ta.Focus()

	subject := opts.Subject
	if subject == nil {
		subject = &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}}
	}

	c := &Cockpit{
		cfg:       cfg,
		client:    rafikiv1connect.NewControlClient(httpClient, opts.BaseURL),
		subject:   subject,
		rail:      rail.New(),
		sessions:  make(map[string]*session.Session),
		panes:     map[string]*paneState{},
		ta:        ta,
		keys:      defaultKeyMap(),
		evCh:      make(chan *rafikiv1.Event, 256),
		status:    "connecting…",
		modelView: loadModelView(),
		currency:  clientstate.Load().Currency,
	}
	if opts.OpenCreate {
		c.form = newSpawnForm()
		c.form.prefill(opts.CreateDefaults)
		c.form.refreshSuggestions(nil, c.modelView)
	}
	if opts.ChildID != "" {
		c.rail.SetFocus(opts.ChildID)
		c.sessions[opts.ChildID] = session.New(opts.ChildID)
		c.lru = []string{opts.ChildID}
	} else {
		// A bare `rafiki attach` has nothing focused, so the input box can do
		// nothing at all — Send returns early on an empty child. Opening on it
		// puts the cursor in a box that cannot accept work and hides the one
		// thing there is to do. Start on the rail; the seed narrows it further
		// once the children are known.
		c.focus = focusRail
		c.ta.Blur()
	}
	return c
}

func (c *Cockpit) focused() string { return c.rail.Focus() }

// setNotice shows one transient line in place of the status. It expires on the
// existing 250ms tick rather than on a timer of its own.
func (c *Cockpit) setNotice(s string) {
	c.notice = s
	c.noticeUntil = time.Now().Add(quitConfirmWindow)
}

// Init seeds the rail from ListChildren and starts the event pump.
func (c *Cockpit) Init() tea.Cmd {
	cmds := []tea.Cmd{c.seedCmd(), waitForEvent(c.evCh), tick(), textarea.Blink}
	if c.form != nil {
		// A form opened at CONSTRUCTION never saw the `n` keypress that
		// normally starts the catalog fetch, so its typeahead would sit empty
		// until something else asked.
		cmds = append(cmds, c.fetchModelsCmd(c.form.kind()))
	}
	return tea.Batch(cmds...)
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

// landRailFirst decides where a bare `rafiki attach` should start, once the
// children are known.
//
// One child is not a choice: open it, because making someone pick from a list
// of one is a keystroke that carries no information. No children is not a
// choice either, and the rail cannot be entered — fall back to the input so
// the empty state reads normally rather than trapping focus on a pane with
// nothing in it. Only a real choice lands on the rail, with the first row
// already under the cursor so ↑/↓ and ⏎ work without a priming keystroke.
func (c *Cockpit) landRailFirst() tea.Cmd {
	nodes := c.rail.Nodes()
	switch len(nodes) {
	case 0:
		return c.setFocus(focusInput)
	case 1:
		c.rail.SetFocus(nodes[0].ChildID)
		c.selected = nodes[0].ChildID
		cmd := c.openFocus(nodes[0].ChildID)
		return tea.Batch(cmd, c.setFocus(focusInput))
	default:
		if c.selected == "" {
			c.selected = nodes[0].ChildID
		}
		return c.setFocus(focusRail)
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

	case tasksLoadedMsg:
		if c.tasks == nil {
			c.tasks = map[string][]*rafikiv1.TaskRow{}
		}
		c.tasks[msg.childID] = msg.rows
		return c, nil

	case seedMsg:
		c.reseedInFlight = false
		if msg.err != nil {
			c.status = "list children: " + msg.err.Error()
			return c, nil
		}
		first := c.stopRail == nil
		c.rail.Seed(c.inSubject(msg.children))
		// Seed each child's spend before any turn_end arrives: the rail resumes
		// from the log head, so turns that predate this client are otherwise
		// invisible to the cost readout. CostUsd nil (no rollup) seeds nothing;
		// SetCost assigns rather than adds, so a re-seed cannot double the number.
		for _, s := range c.inSubject(msg.children) {
			if s.CostUsd != nil {
				c.rail.SetCost(s.GetChildId(), s.GetCostUsd())
			}
		}
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
				// fetchTasks as well as openFocus, matching hop: without it
				// `attach <id>` on an agent that already has a ledger showed no
				// box until a task_* call happened to complete, which is the
				// case the box exists for.
				return c, tea.Batch(c.openFocus(f), c.fetchTasks(f))
			}
			return c, c.landRailFirst()
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
		if c.taskRefresh {
			c.taskRefresh = false
			cmd = tea.Batch(cmd, c.fetchTasks(c.focused()))
		}
		return c, tea.Batch(waitForEvent(c.evCh), cmd)

	case historyMsg:
		s := c.sessions[msg.childID]
		if s == nil || msg.childID != c.focused() {
			// Hopped away while it was in flight. Dropping it is right: the
			// next focus on that child starts the fetch again, and applying it
			// now would seed a session whose stream nobody is opening.
			return c, nil
		}
		if msg.err != nil {
			// A conversation the daemon cannot resolve is normal, not fatal --
			// a child that has never taken a turn has no conversation row --
			// so fall back to replaying the whole log, which is exactly what
			// the cockpit did before it asked for history at all.
			c.status = "history unavailable: " + connect.CodeOf(msg.err).String()
			c.startFocus(msg.childID, -1)
			return c, nil
		}
		for _, ev := range msg.events {
			s.ApplyHistory(ev)
		}
		after := msg.after
		if len(msg.events) == 0 {
			// Nothing persisted: the log is all there is, so replay it whole
			// rather than resuming from its head and showing an empty pane.
			after = -1
		}
		c.startFocus(msg.childID, after)
		return c, nil

	case sendFailedMsg:
		c.status = "send failed: " + msg.err.Error()
		c.pending = ""
		return c, nil

	case modelsLoadedMsg:
		c.applyModelsLoaded(msg)
		return c, nil

	case spawnedMsg:
		return c, c.applySpawned(msg)

	case killedMsg:
		c.applyKilled(msg)
		return c, nil

	case closedMsg:
		c.applyClosed(msg)
		return c, nil

	case tickMsg:
		if c.notice != "" && time.Now().After(c.noticeUntil) {
			c.notice = ""
		}
		return c, tick()

	case tea.PasteMsg:
		return c, c.handlePaste(msg.Content)

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
// isTaskTool names the ledger mutations. A completed one is the refresh
// trigger, which is why there is no polling and no new event type: the ledger
// stays authoritative and the event plane is not asked to carry a lossy copy
// of it.
func isTaskTool(name string) bool {
	switch name {
	case "task_add", "task_update", "task_drop":
		return true
	}
	return false
}

// toolNameFor resolves a tool_use id to its name. ToolExecutionEnd carries a
// duration and an error flag and no name, so the name has to come from the
// assistant block that announced the call.
func toolNameFor(s *session.Session, toolUseID string) string {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		for _, tc := range s.Blocks[i].ToolCalls {
			if tc.ID == toolUseID {
				return tc.Name
			}
		}
	}
	return ""
}

// fetchTasks loads the focused child's ledger. A failure yields no message at
// all: the box is a readout, and an unreachable ledger should hide it rather
// than raise an error over the conversation.
func (c *Cockpit) fetchTasks(childID string) tea.Cmd {
	n, ok := c.rail.Get(childID)
	if !ok {
		return nil
	}
	convID := n.SessionID
	if convID == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.client.ListTasks(ctx,
			connect.NewRequest(&rafikiv1.ListTasksRequest{ConversationId: convID}))
		if err != nil {
			return nil
		}
		return tasksLoadedMsg{childID: childID, rows: resp.Msg.GetTasks()}
	}
}

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
	// A completed ledger mutation is the refresh trigger; no polling.
	if te := ev.GetToolExecutionEnd(); te != nil && isTaskTool(toolNameFor(s, te.GetToolUseId())) {
		c.taskRefresh = true
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

	// A modal owns EVERY key, checked before the globals rather than after.
	// The globals list holds ⇥, esc, ^A and the arrow keys, all of which the
	// form needs for itself; letting them match first would make the form's
	// own tab order unreachable.
	if c.query != nil {
		return c.handleQueryKey(msg)
	}
	if c.picker != nil {
		return c.handlePickerKey(msg, max(1, c.bodyHeight()-pickerChrome))
	}
	if c.form != nil {
		return c.handleFormKey(msg, c.form.suggestWindow(c.bodyHeight(), c.query))
	}

	// Any keystroke that is not the repeat disarms the end confirmation, for
	// the same reason the quit confirmation disarms: a live destructive
	// trigger must not survive an unrelated key.
	if !key.Matches(msg, k.EndAgent) {
		c.endArmed = time.Time{}
		c.endArmedID = ""
	}

	// ── globals: live from every pane ────────────────────────────────────
	// Any keystroke that is not the repeat disarms, so ^C followed by typing
	// does not leave a live trigger behind for minutes.
	armed := !c.quitArmed.IsZero() && time.Since(c.quitArmed) < quitConfirmWindow
	if !key.Matches(msg, k.Quit) {
		c.quitArmed = time.Time{}
	}

	switch {
	case key.Matches(msg, k.Quit):
		if !armed {
			c.quitArmed = time.Now()
			c.setNotice("press " + k.Quit.Help().Key + " again to quit — children keep running")
			return c, nil
		}
		c.quitting = true
		c.shutdown()
		return c, tea.Quit
	case key.Matches(msg, k.NextPane):
		return c, c.cyclePane(+1)
	case key.Matches(msg, k.PrevPane):
		return c, c.cyclePane(-1)
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
		// An explicit toggle is a decision: it outranks a peek, so ⏎ afterwards
		// must not undo what the user just asked for.
		c.railPeek = false
		// Never leave focus on a pane that just became invisible.
		return c, c.cyclePane(0)
	case key.Matches(msg, k.Help):
		c.showHelp = !c.showHelp
		return c, nil
	case key.Matches(msg, k.ExpandArgs):
		c.expandArgs = !c.expandArgs
		// paneSig alone is NOT enough. It makes linesFor call Lines again, but
		// Lines reuses r.cached for every block below Finalized -- which is the
		// whole visible transcript -- so the toggle reached only the live tail.
		for _, p := range c.panes {
			p.invalidate()
		}
		return c, nil
	case key.Matches(msg, k.Redraw):
		for _, p := range c.panes {
			p.invalidate()
		}
		return c, tea.ClearScreen
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
			return c, tea.Batch(cmd, c.leaveRail())
		case key.Matches(msg, k.NewAgent):
			c.form = newSpawnForm()
			// Fetch the catalog NOW rather than when the model row is
			// reached: the typeahead has to be instant when it gets there,
			// and a round trip started on first keystroke is not.
			c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
			return c, tea.Batch(c.fetchModelsCmd(c.form.kind()), textinput.Blink)
		case key.Matches(msg, k.EndAgent):
			return c, c.endSelected()
		case key.Matches(msg, k.Escape):
			return c, c.leaveRail()
		}
		// The rail swallows everything else: a stray letter here must not
		// reach the textarea, or you would type into an input you cannot see
		// the cursor of.
		return c, nil
	}

	// ── input ────────────────────────────────────────────────────────────
	// Newline is checked BEFORE send, and inserted by hand rather than passed
	// through: the textarea binds InsertNewline to enter/^M, both of which
	// Send has taken, so ⇧⏎ reaches its keymap and matches nothing at all.
	if key.Matches(msg, k.Newline) {
		c.ta.InsertString("\n")
		return c, nil
	}
	if key.Matches(msg, k.ClearInput) {
		// The folded pastes go with it: their tokens are what referred to
		// them, and leaving the text behind would attach it to the next
		// prompt that happened to contain a matching token.
		c.ta.Reset()
		c.pastes = nil
		c.attachments = nil
		return c, nil
	}
	switch {
	case key.Matches(msg, k.ScrollTop):
		// home/end were transcript-pane-only and so appeared not to work at
		// all: with PgUp/PgDn reading from here, nobody tabbed away, and a key
		// that only works in a pane you never visit is a key that does not
		// work. ^A/^E keep line-start/line-end for editing.
		c.jumpScroll(true)
		return c, nil
	case key.Matches(msg, k.ScrollBottom):
		c.jumpScroll(false)
		return c, nil
	case key.Matches(msg, k.ScrollPageUp), key.Matches(msg, k.ScrollPageDown):
		// Outright: a three-line box has no pages, so paging it is meaningless
		// and the transcript is what was meant.
		return c, c.scrollFocused(msg)
	case key.Matches(msg, k.ScrollLineUp), key.Matches(msg, k.ScrollLineDown):
		// Shared. The textarea gets first refusal and keeps the key whenever
		// the cursor actually moves; only an ↑ with no line above it — the
		// common single-line prompt — reaches the transcript. Comparing the
		// row before and after is the only honest test of "the textarea could
		// not use this", and it costs a multi-line prompt nothing.
		row := c.ta.Line()
		var cmd tea.Cmd
		c.ta, cmd = c.ta.Update(msg)
		if c.ta.Line() != row {
			return c, cmd
		}
		return c, c.scrollFocused(msg)
	}
	if mode := c.modeForKey(msg); mode != rafikiv1.SendMode_SEND_MODE_UNSPECIFIED {
		text := strings.TrimSpace(c.expandPastes(c.ta.Value()))
		if mode != rafikiv1.SendMode_SEND_MODE_ABORT {
			// An attachment alone is a message: a screenshot with nothing to
			// say still has something to say.
			if text == "" && len(c.attachments) == 0 {
				return c, nil
			}
			images := c.attachments
			c.ta.Reset()
			c.pastes = nil
			c.attachments = nil
			// You just spoke; you want the reply. Pin to the bottom even if you
			// had scrolled up to re-read something before sending.
			if f := c.focused(); f != "" {
				p := c.pane(f)
				p.vp.GotoBottom()
				p.atBottom = true
			}
			return c, c.sendWith(mode, text, images)
		}
		return c, c.send(mode, text)
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return c, cmd
}

// handlePaste folds a large paste into a token rather than unrolling it.
//
// Pasting the SAME content again inserts it in full — the second paste is how
// you say you actually wanted to see it, and it costs no key of its own.
func (c *Cockpit) handlePaste(content string) tea.Cmd {
	if c.focus != focusInput || content == "" {
		return nil
	}
	content = normalizeNewlines(content)
	// A dragged image is a PATH, not bytes: a terminal never sends image data
	// through a bracketed paste. Dropping a file from Finder pastes its path,
	// which is the only way an image reaches this box today.
	if att, handled := c.attachIfImagePath(content); handled {
		// handled covers the rejections too — too large, unreadable — which
		// have already set their own notice. Only a real read stages anything.
		if len(att.data) > 0 {
			c.attachments = append(c.attachments, att)
			c.ta.InsertString(att.token)
			c.setNotice(att.notice)
		}
		return nil
	}
	lines := strings.Count(content, "\n") + 1
	if lines <= pasteLineThreshold && len(content) <= pasteCharThreshold {
		c.ta.InsertString(content)
		return nil
	}
	for _, p := range c.pastes {
		if p.text == content {
			c.ta.InsertString(content)
			return nil
		}
	}
	// Describe it in the unit that made it too big: "1 lines" tells a reader
	// nothing about a 40KB paste that happens to contain no line breaks.
	size := itoa(int64(lines)) + " lines"
	if lines == 1 {
		size = itoa(int64(len(content))) + " chars"
	}
	token := "[pasted #" + itoa(int64(len(c.pastes)+1)) + ": " + size + "]"
	c.pastes = append(c.pastes, pastedText{token: token, text: content})
	c.ta.InsertString(token)
	c.setNotice("paste folded — paste again to insert it in full")
	return nil
}

// normalizeNewlines folds CRLF and bare CR to LF.
//
// A terminal sends CARRIAGE RETURNS for the line breaks inside a bracketed
// paste, and ultraviolet's paste buffer keeps them verbatim: a bare \r decodes
// as a KeyEnter press whose Text is empty, so the raw byte is appended. So the
// pasted content genuinely contains no \n at all, counting lines by \n
// returned 1 for a forty-line paste, and every real ⌘V of multi-line text
// sailed under the threshold and unrolled into the box. Verified over a pty:
// CRLF and LF folded, CR-only did not.
//
// Normalizing here also fixes what gets SENT — an agent should not receive a
// prompt whose line breaks are carriage returns.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// expandPastes puts the folded text back before the prompt leaves. A token the
// user deleted takes its paste with it, which is the whole point of a token
// you can select and remove.
func (c *Cockpit) expandPastes(text string) string {
	for _, p := range c.pastes {
		text = strings.ReplaceAll(text, p.token, p.text)
	}
	return text
}

// scrollFocused forwards a key to the focused child's viewport.
//
// The viewport keeps its DEFAULT keymap here — ↑/↓, j/k, space, PgUp/PgDn —
// which is safe precisely because typing is inactive while this pane holds
// focus. That is the payoff of the focus ring; with a permanently-focused
// textarea none of those keys were available.
// jumpScroll sends the focused pane to the top or the bottom. The viewport's
// default keymap binds neither home nor end.
func (c *Cockpit) jumpScroll(top bool) {
	f := c.focused()
	if f == "" {
		return
	}
	p := c.pane(f)
	if top {
		p.vp.GotoTop()
	} else {
		p.vp.GotoBottom()
	}
	p.atBottom = p.vp.AtBottom()
}

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

// workingStatus reports whether a child is mid-turn.
//
// The set of statuses is CLOSED (protocol.Status's eight) and there is no
// "running" -- a predicate written from intuition as status == "running"
// matches nothing and silently does nothing, which this repo has shipped once
// already in the recovery path.
func workingStatus(s string) bool {
	switch s {
	case "streaming", "tool_running", "compacting":
		return true
	}
	return false
}

// sendModeFor upgrades a PROMPT to a STEER when the target is mid-turn.
//
// ModePrompt is debounced and busy-GATED, so a prompt typed at a working agent
// waits in agent_inbox until the turn settles -- which is the ⏳ that never
// clears. A steer is delivered at the next opportunity instead.
//
// Over-applying steer is safe: Engine.HandleSteerID buffers for mid-turn
// injection when a turn is running and otherwise treats the text as a plain
// prompt, so the client's status view being a few milliseconds stale can only
// be wrong in the harmless direction.
//
// Only the cockpit's ⏎ is rewritten. SEND_MODE_PROMPT stays intact on the wire
// for callers that genuinely want queueing -- a coordinator's agent_send,
// where the debounce and the per-(child, source) coalescing are the point.
func sendModeFor(mode rafikiv1.SendMode, status string) rafikiv1.SendMode {
	if mode == rafikiv1.SendMode_SEND_MODE_PROMPT && workingStatus(status) {
		return rafikiv1.SendMode_SEND_MODE_STEER
	}
	return mode
}

// modeForKey maps an input-pane keystroke to a send mode.
//
// It reads the BINDINGS rather than switching on key strings. It used to carry
// its own copy of them, which is the drift footerHints was already rewritten to
// avoid: a keymap change had to be made in two places and only one of them
// failed a test.
func (c *Cockpit) modeForKey(msg tea.KeyPressMsg) rafikiv1.SendMode {
	switch {
	case key.Matches(msg, c.keys.Send):
		return rafikiv1.SendMode_SEND_MODE_PROMPT
	case key.Matches(msg, c.keys.Steer):
		return rafikiv1.SendMode_SEND_MODE_STEER
	case key.Matches(msg, c.keys.Abort):
		return rafikiv1.SendMode_SEND_MODE_ABORT
	}
	return rafikiv1.SendMode_SEND_MODE_UNSPECIFIED
}

// send submits to the focused child.
func (c *Cockpit) send(mode rafikiv1.SendMode, text string) tea.Cmd {
	return c.sendWith(mode, text, nil)
}

// sendWith submits to the focused child, carrying any staged attachments.
//
// Images go FIRST in the block list, matching llm.UserContent: the common shape
// is a screenshot followed by a question about it, and a model attends better
// to an image that precedes the text asking about it.
func (c *Cockpit) sendWith(mode rafikiv1.SendMode, text string, images []stagedAttachment) tea.Cmd {
	child := c.focused()
	if child == "" {
		return nil
	}
	if n, ok := c.rail.Get(child); ok {
		mode = sendModeFor(mode, n.Status)
	}
	req := &rafikiv1.SendRequest{ChildId: child, Mode: mode}
	if mode != rafikiv1.SendMode_SEND_MODE_ABORT {
		// An attachment alone is a message. Requiring text would mean a
		// screenshot with nothing to say could not be sent at all.
		if strings.TrimSpace(text) == "" && len(images) == 0 {
			return nil
		}
		for _, img := range images {
			req.Blocks = append(req.Blocks, &rafikiv1.ContentBlock{
				Index: int32(len(req.Blocks)),
				Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
					MediaType: img.mediaType, Data: img.data,
				}},
			})
		}
		if text != "" {
			req.Blocks = append(req.Blocks, &rafikiv1.ContentBlock{
				Index: int32(len(req.Blocks)),
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			})
		}
		c.pending = text
		if text == "" {
			c.pending = "(" + itoa(int64(len(images))) + " attached)"
		}
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
	return tea.Batch(c.openFocus(childID), c.fetchTasks(childID))
}

// openFocus starts the focus subscription for childID, creating its session if
// needed. It has no identity guard, which is the whole reason it is separate
// from hop: the seed path focuses a child and then needs its stream opened.
//
// A session with nothing in it fetches the conversation from GetHistory FIRST
// and opens its stream in the reply. The event log begins whenever the event
// plane was deployed, so it is not, and was never meant to be, the transcript:
// conversations.conversation_message holds every message and GetHistory
// already serves it in this exact vocabulary. A cockpit that read only the log
// showed nothing at all for a conversation older than the log -- which is
// every conversation that existed when the cockpit was written.
func (c *Cockpit) openFocus(childID string) tea.Cmd {
	s := c.sessions[childID]
	if s == nil {
		s = session.New(childID)
		c.sessions[childID] = s
	}
	c.touch(childID)

	if len(s.Blocks) == 0 && !s.HasCursor {
		return c.historyCmd(childID)
	}
	// Already read once: resume the log from where this session left off. Its
	// history is in hand and re-fetching it would re-render the transcript on
	// every hop.
	c.startFocus(childID, s.Cursor)
	return nil
}

// historyCmd loads the persisted conversation.
//
// The log head is captured BEFORE the fetch and carried through so the stream
// resumes from it. Taking it afterwards instead would drop any turn that
// landed while the fetch was in flight; taking it before can at worst show one
// turn twice, and a duplicate is visible while a gap is silent.
func (c *Cockpit) historyCmd(childID string) tea.Cmd {
	after := int32(-1)
	if n, ok := c.rail.Get(childID); ok {
		after = n.Latest
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := c.client.GetHistory(ctx,
			connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
		if err != nil {
			return historyMsg{childID: childID, after: after, err: err}
		}
		return historyMsg{childID: childID, events: resp.Msg.GetEvents(), after: after}
	}
}

// startFocus opens the subscription for childID resuming after ordinal.
//
// -1 rather than 0 for a fresh session: ordinal 0 is a real event and the
// log's Read is exclusive on afterOrdinal, so 0 would skip the first event.
func (c *Cockpit) startFocus(childID string, after int32) {
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
// setFocus moves the ring and carries the textarea's focus state with it.
//
// EVERY focus change goes through here, which is the point: the textarea must
// be focused to accept a key at all, and must be blurred whenever another pane
// owns the keys or it blinks a cursor in an input that is ignoring you. Two
// separate things have to agree on every transition, so there is exactly one
// place that can set them.
func (c *Cockpit) setFocus(p focusPane) tea.Cmd {
	c.focus = p
	if p == focusRail && c.selected == "" {
		c.selected = c.focused()
	}
	if p != focusInput {
		c.ta.Blur()
		return nil
	}
	return c.ta.Focus()
}

// cyclePane advances focus by delta, skipping the rail when it is hidden.
//
// A delta of 0 re-validates the current focus without moving, which is what
// hiding the rail needs: ctrl+b is global and can fire while the rail holds
// focus, and focus left on an invisible pane is how a modal UI traps someone.
func (c *Cockpit) cyclePane(delta int) tea.Cmd {
	// ⇥ REVEALS a hidden rail rather than doing nothing. Hiding it is about
	// screen space, not about giving up the ability to switch agents, and a
	// dead ⇥ reads as a broken key.
	if c.railHidden && delta != 0 && c.rail.Len() > 1 {
		c.railHidden = false
		c.railPeek = true
		return c.setFocus(focusRail)
	}
	order := []focusPane{focusInput, focusRail}
	if c.railHidden {
		order = []focusPane{focusInput}
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
		if delta == 0 {
			return c.setFocus(focusInput)
		}
		idx = 0
	}
	idx = (idx + delta + len(order)) % len(order)
	return c.setFocus(order[idx])
}

// leaveRail returns focus to the input, re-hiding the rail if ⇥ was what
// revealed it. Both exits go through here: picking an agent and changing your
// mind should leave the screen in the same shape you found it.
func (c *Cockpit) leaveRail() tea.Cmd {
	if c.railPeek {
		c.railHidden = true
		c.railPeek = false
	}
	return c.setFocus(focusInput)
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
	// ^R and ^V/^T change the query without ever opening the panel, so exit is
	// the other commit point.
	saveModelView(c.modelView)
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
// bodyHeight is what is left after the chrome, and the input box is part of
// the chrome that MOVES: it grows with the prompt, so a fixed subtraction
// would overlap the transcript by exactly the rows the box gained.
//
// The three fixed rows are the divider, the status line and the footer.
// taskBoxLines is the focused agent's box, or nil. Computed in one place so
// bodyHeight and View cannot disagree about how tall it is -- a fixed
// subtraction overlaps the transcript by exactly the rows the box gains, and
// stays invisible until someone hits the case.
func (c *Cockpit) taskBoxLines() []string {
	f := c.focused()
	if f == "" {
		return nil
	}
	return renderTaskBox(c.tasks[f], c.convWidth())
}

// taskBoxRows is how many SCREEN ROWS the box costs, which is not len(box):
// View draws a divider above it. bodyHeight subtracting only the box's own
// lines made the view one row too tall whenever the box was visible, pushing
// the footer off the bottom of the alt screen.
func taskBoxRows(box []string) int {
	if len(box) == 0 {
		return 0
	}
	return len(box) + 1
}

func (c *Cockpit) bodyHeight() int {
	h := c.height - c.ta.Height() - 3 - taskBoxRows(c.taskBoxLines())
	if h < 1 {
		return 1
	}
	return h
}

// railCols is the rail's current width, or 0 when it is not drawn.
//
// A modal takes the WHOLE panel: the create form and the model browser are
// full-attention tasks, and a rail behind them is a list you cannot act on
// costing width from a table that needs it.
func (c *Cockpit) railCols() int {
	if c.form != nil || c.picker != nil || c.railHidden || c.rail.Len() < 2 {
		return 0
	}
	return railWidthFor(c.rail.Nodes(), c.width, c.currency)
}

func (c *Cockpit) convWidth() int {
	w := c.width
	if rw := c.railCols(); rw > 0 {
		w = c.width - rw - 1
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

	h := c.bodyHeight()
	p.vp.SetWidth(c.convWidth())
	p.vp.SetHeight(h)
	// Bottom-anchored: a transcript shorter than the pane is padded at the TOP
	// so the newest line sits on the same row it will occupy once the
	// conversation is long. A viewport renders from the top by default, which
	// made a new or short conversation start at the ceiling and crawl down —
	// the reader's eye has to move, and then move back, for the first screenful
	// only. joinColumns used to do this padding; the viewport took over the
	// pane and it was lost with it.
	p.contentLines = len(lines)
	if pad := h - len(lines); pad > 0 {
		lines = append(make([]string, pad), lines...)
	}
	p.vp.SetContentLines(lines)

	if wasAtBottom {
		p.vp.GotoBottom()
	} else {
		p.vp.SetYOffset(prev)
	}
	p.atBottom = p.vp.AtBottom()
}

// scrollPosition is the bottom-right readout: how far down the transcript the
// last visible line is, and how long the transcript is.
//
// It reports the CONTENT's length, never the viewport's, because a short
// transcript is padded to bottom-anchor it and the viewport counts that padding
// as real. Lines rather than blocks: the reader is looking at lines, and a
// percentage of blocks jumps unevenly when one block is a 500-line tool result.
// costReadout is the footer's spend for the focused agent: self, then what
// this agent's subagents spent on its behalf. Two numbers only when there is a
// second one to show, and nothing at all while every total is zero -- a wall
// of $0.00 beside idle agents is noise, not information.
func (c *Cockpit) costReadout() string {
	f := c.focused()
	if f == "" {
		return ""
	}
	n, ok := c.rail.Get(f)
	if !ok {
		return ""
	}
	self := fmtCost(n.Cost, c.currency)
	sub := fmtCost(c.rail.SubtreeCost(f)-n.Cost, c.currency)
	switch {
	case self != "" && sub != "":
		return self + " +" + sub
	case self != "":
		return self
	}
	return ""
}

func (c *Cockpit) scrollPosition() string {
	f := c.focused()
	if f == "" {
		return ""
	}
	p := c.panes[f]
	if p == nil || p.contentLines == 0 {
		return ""
	}
	total := p.contentLines
	last := total
	if !p.atBottom {
		// Only meaningful while scrolled: at the bottom the last visible line
		// IS the last line, and deriving it from the offset would disagree by
		// the padding on a short transcript.
		if seen := p.vp.YOffset() + p.vp.Height(); seen < total {
			last = seen
		}
	}
	pct := 100
	if total > 0 {
		pct = last * 100 / total
	}
	arrow := " "
	if !p.atBottom {
		arrow = "↓"
	}
	return arrow + " " + itoa(int64(last)) + "/" + itoa(int64(total)) +
		" " + itoa(int64(pct)) + "%"
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
		bs = []key.Binding{k.SelectUp, k.SelectDown, k.Commit, k.NewAgent, k.EndAgent, k.Escape}
	default:
		bs = []key.Binding{k.Send, k.Newline, k.ClearInput, k.Steer, k.Abort}
	}
	parts := make([]string, 0, len(bs)+3)
	for _, b := range bs {
		h := b.Help()
		parts = append(parts, h.Key+" "+h.Desc)
	}
	parts = append(parts,
		c.keys.NextPane.Help().Key+" pane",
		c.keys.ToggleRail.Help().Key+" rail",
		c.keys.Help.Help().Key+" help",
		c.keys.ExpandArgs.Help().Key+" "+c.keys.ExpandArgs.Help().Desc,
		c.keys.Redraw.Help().Key+" "+c.keys.Redraw.Help().Desc)
	return strings.Join(parts, "  ")
}

// helpLines is the ^G overlay, laid out in two columns.
//
// Grouped by pane, because a key's meaning DEPENDS on which pane holds focus
// and a flat list would be wrong for two thirds of the cockpit. Derived from
// the keymap for the same reason footerHints is: the footer and the help must
// not be able to disagree with the bindings, or with each other.
//
// Two columns rather than one because the body pane clips from the TOP
// (lastN), so a single-column sheet silently loses the global bindings -- the
// half a lost user most needs -- on any terminal shorter than about 30 rows.
//
// ^G was a write-only toggle before this: it flipped showHelp and nothing ever
// read it, so the key advertised in the footer and in `attach --help` did
// nothing at all.
func (c *Cockpit) helpLines(width int) []string {
	group := func(title string, bs ...key.Binding) []string {
		out := []string{styleRailFocused.Render(title)}
		for _, b := range bs {
			h := b.Help()
			out = append(out, "  "+padTo(h.Key, 10)+h.Desc)
		}
		return append(out, "")
	}
	k := c.keys
	left := group("anywhere",
		k.NextPane, k.PrevPane, k.NextAttention, k.PrevAttention,
		k.HopPrev, k.HopNext, k.ToggleRail, k.Help, k.ExpandArgs, k.Redraw, k.Quit)
	right := group("input", k.Send, k.Newline, k.ClearInput, k.Steer, k.Abort)
	right = append(right, group("agents",
		k.SelectUp, k.SelectDown, k.Commit, k.NewAgent, k.EndAgent, k.Escape)...)
	right = append(right, group("reading",
		k.ScrollPageUp, k.ScrollTop, k.ScrollBottom,
		key.NewBinding(key.WithKeys("up", "down"),
			key.WithHelp("↑/↓", "scroll (when the cursor cannot move)")))...)

	colWidth := width / 2
	if colWidth < 24 {
		// Too narrow to sit side by side; stack and accept the clipping.
		return append(append([]string{}, left...), right...)
	}
	out := make([]string, 0, max(len(left), len(right))+1)
	for i := 0; i < len(left) || i < len(right); i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out = append(out, padTo(l, colWidth)+r)
	}
	return append(out, styleMeta.Render("^G closes this."))
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

	railCols := c.railCols()
	railText := ""
	if railCols > 0 {
		railText = renderRail(c.rail.Nodes(), c.focused(), c.selected, railCols,
			c.focus == focusRail, c.currency)
	}

	bodyHeight := c.bodyHeight()
	convWidth := c.convWidth()

	var conv string
	switch f := c.focused(); {
	case c.picker != nil:
		conv = c.picker.view(convWidth, bodyHeight, c.modelView, c.query)
	case c.form != nil:
		conv = c.form.view(convWidth, bodyHeight, c.modelView, c.query)
	case c.showHelp:
		conv = strings.Join(c.helpLines(convWidth), "\n")
	case f == "" && c.rail.Len() == 0:
		conv = styleMeta.Render("No agents running. Start one with `rafiki create`.")
	case f == "":
		conv = styleMeta.Render("Pick an agent — ↑/↓ to move, ⏎ to open.")
	case c.sessions[f] != nil:
		s := c.sessions[f]
		p := c.pane(f)
		lines := p.linesFor(s, convWidth, bodyHeight, c.expandArgs)
		// An empty transcript is a real, STEADY state -- a child whose event
		// log holds nothing but its lifecycle events, which is every freshly
		// created agent until it is asked something. The renderer used to
		// answer it with "Connecting…", so the pane claimed to be connecting
		// forever two rows above a status line reading "connected".
		if lines != nil && len(lines) == 0 {
			conv = styleMeta.Render("No messages yet — type below and press " +
				c.keys.Send.Help().Key + " to start.")
			break
		}
		if lines != nil {
			c.syncViewport(p, lines)
		}
		conv = p.vp.View()
	default:
		conv = styleMeta.Render("loading…")
	}
	if c.pending != "" {
		conv += "\n" + stylePending.Render("⏳ "+c.pending)
	}

	statusLine := c.status
	if c.notice != "" {
		statusLine = c.notice
	}

	// The focused pane is NAMED and MARKED, always. Naming it in the footer was
	// not enough on its own: a word in a grey status line is not where the eye
	// is, so finding the focused pane meant cycling ⇥ and watching for a
	// response. The badge is reversed out so it reads as a state rather than as
	// more footer text, and each pane carries an accent edge (below) so the
	// answer is also where you are looking.
	footer := styleFocusBadge.Render(" "+c.focus.String()+" ") + "  " + c.footerHints()
	// Position goes bottom-RIGHT, on its own end of the line: a scroll readout
	// that shares the left edge with the key hints moves every time the hints
	// change, and a number that moves is a number you have to hunt for. The
	// old marker said only "↓ more below", which answers whether you are at the
	// bottom and not where you are.
	readout := c.costReadout()
	if pos := c.scrollPosition(); pos != "" {
		if readout != "" {
			readout += "  "
		}
		readout += pos
	}
	if readout != "" {
		gap := c.width - ansi.StringWidth(ansi.Strip(footer)) - ansi.StringWidth(readout) - 1
		if gap < 1 {
			gap = 1
		}
		footer += strings.Repeat(" ", gap) + styleMeta.Render(readout)
	}

	out := joinColumns(railText, conv, railCols, convWidth, bodyHeight,
		c.focus == focusRail)
	// The task box sits between the transcript and the input, and bodyHeight
	// already subtracted its height -- the two must move together, which is
	// why both go through taskBoxLines.
	if box := c.taskBoxLines(); len(box) > 0 {
		out += "\n" + styleDivider + "\n" + strings.Join(box, "\n")
	}
	out += "\n" + styleDivider + "\n" + c.ta.View() + "\n" +
		styleMeta.Render(statusLine) + "\n" + styleMeta.Render(footer)

	v := tea.NewView(out)
	v.AltScreen = true
	return v
}

// joinColumns places the rail beside the conversation, both clipped to height.
func joinColumns(left, right string, leftWidth, rightWidth, height int, leftFocused bool) string {
	// The divider is the rail's accent edge: heavy and coloured while the rail
	// holds focus. The badge in the footer says which pane has it; this says it
	// where the eye already is.
	bar := styleMeta.Render("│")
	if leftFocused {
		bar = styleFocusEdge.Render("┃")
	}
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
		sb.WriteString(bar)
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
