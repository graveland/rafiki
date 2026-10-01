// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// lifecycleTimeout bounds a spawn/kill/close RPC.
//
// Generous compared with the cockpit's other calls because a kill is not a
// query: Controller.Kill waits for the child to actually die, and the daemon's
// own shutdown timeout can be minutes. A deadline shorter than the daemon's
// would report a failure for a kill that then succeeds, which is the one
// outcome worse than a slow one.
const lifecycleTimeout = 3 * time.Minute

// forceShutdownMs is the shutdown grace a FORCED kill allows before the daemon
// escalates to SIGKILL. One millisecond rather than zero: zero means "use the
// daemon's default" on this wire (protocol.KillRequest omits the field when
// unset), so it would ask for the polite kill the user just pressed a key to
// escape.
const forceShutdownMs = 1

// statusShuttingDown is protocol.StatusShuttingDown's wire value. Spelled out
// rather than imported for the same reason rail.LiveStatuses spells its
// statuses out: this package renders status strings it receives over the wire
// and does not otherwise depend on the daemon's types.
const statusShuttingDown = "shutting_down"

type spawnedMsg struct {
	childID string
	err     error
}

type killedMsg struct {
	childID     string
	name        string
	forced      bool
	descendants int
	err         error
}

type closedMsg struct {
	childID string
	name    string
	// descendants are the rows a cascading close took with childID, deepest
	// first.
	descendants []string
	err         error
}

// endAsk is the question `x` poses when the agent it is about to end has
// subagents: take them along or end only this one. It is modal to the rail: the
// next key answers it, and any key but the two answers cancels.
type endAsk struct {
	id   string
	name string
	// verb is what the unanswered action would have been: stop, force kill or
	// close.
	verb string
	kids int
}

type budgetSetMsg struct {
	origin  *budgetForm
	childID string
	name    string
	maxCost float64
	err     error
}

// setBudgetCmd changes a child's budget with operator authority via the
// Connect SetBudget RPC.
func (c *Cockpit) setBudgetCmd(origin *budgetForm, childID, name string, maxCost float64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		_, err := c.client.SetBudget(ctx, connect.NewRequest(&rafikiv1.SetBudgetRequest{
			ChildId: childID,
			MaxCost: maxCost,
		}))
		return budgetSetMsg{origin: origin, childID: childID, name: name, maxCost: maxCost, err: err}
	}
}

// applyBudgetSet reports the outcome and, on success, updates the rail's
// displayed cap and dismisses the originating modal.
func (c *Cockpit) applyBudgetSet(m budgetSetMsg) {
	if m.err != nil {
		c.setNotice("could not set budget for " + m.name + ": " + trimRPCError(m.err))
		if m.origin != nil {
			m.origin.busy = false
			m.origin.err = trimRPCError(m.err)
		}
		return
	}
	c.rail.SetMaxCost(m.childID, m.maxCost)
	if c.budgetForm == m.origin {
		c.budgetForm = nil
	}
	if m.maxCost == 0 {
		c.setNotice("budget cleared for " + m.name)
	} else {
		c.setNotice("budget set for " + m.name)
	}
}

// buildSpawnRequest turns the form's params into the wire request, applying
// the SAME kind-aware executor precedence the CLI's runCreate does (see
// docs/plans/2026-09-06-executor-selection-design.md §5): an explicit
// executor field always wins; otherwise the selector applies -- but WHICH
// selector applies depends on what c.executorSelector is. The session
// executor's selector (executorSelectorFromFlag false) is fundi-only by
// construction: it never advertises LaunchKinds, so pinning a
// launch-required kind to this machine would force a spawn the daemon must
// refuse. A declared --executor-selector (executorSelectorFromFlag true) is
// a policy that applies to every kind, exactly as the CLI's flag branch
// honors it. With neither, a launch-required kind gets NEITHER ExecutorRef
// nor ExecutorSelector -- letting the daemon's chooseLaunchExecutor
// auto-resolve is strictly better than forcing a wrong machine. fundi keeps
// its historical default: the session executor stood up for this cockpit,
// when there was one.
func (c *Cockpit) buildSpawnRequest(p spawnParams) *rafikiv1.SpawnRequest {
	req := &rafikiv1.SpawnRequest{
		Cwd:     p.cwd,
		Name:    p.name,
		Kind:    p.kind,
		Model:   p.model,
		MaxCost: p.maxCost,
		Preset:  p.preset,
	}
	switch {
	case p.executor != "":
		req.ExecutorRef = p.executor
	case p.kind == protocol.KindFundi || c.executorSelectorFromFlag:
		req.ExecutorSelector = c.executorSelector
	}
	return req
}

// spawnCmd creates a child and reports the id the daemon assigned.
//
// cwd is REQUIRED by the server (connectapi.Spawn answers InvalidArgument
// without it) and is not defaulted here: the form prefills it, so an empty one
// reaching this point is a bug worth surfacing rather than papering over.
func (c *Cockpit) spawnCmd(p spawnParams) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		resp, err := c.client.Spawn(ctx, connect.NewRequest(c.buildSpawnRequest(p)))
		if err != nil {
			return spawnedMsg{err: err}
		}
		return spawnedMsg{childID: resp.Msg.GetChildId()}
	}
}

// killCmd ends a child. force asks the daemon to escalate immediately instead
// of waiting out its shutdown grace.
func (c *Cockpit) killCmd(childID, name string, force, include bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		req := &rafikiv1.KillRequest{ChildId: childID, IncludeDescendants: include}
		if force {
			req.ShutdownTimeoutMs = forceShutdownMs
		}
		resp, err := c.client.Kill(ctx, connect.NewRequest(req))
		if err != nil {
			return killedMsg{childID: childID, name: name, forced: force, err: err}
		}
		return killedMsg{childID: childID, name: name, forced: force, descendants: len(resp.Msg.GetDescendantIds())}
	}
}

// closeCmd finalizes an exited child: it leaves the daemon's store and can
// never be resumed again. The transcript survives -- nothing references
// conversations.child -- so this ends resumption, not history.
//
// With include, still-running subagents are stopped first (a close refuses a
// live child), and the target is stopped too if it is itself still up; an
// already-exited target is the common case and not an error.
func (c *Cockpit) closeCmd(childID, name string, include bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		if include {
			_, err := c.client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{
				ChildId: childID, IncludeDescendants: true,
			}))
			if err != nil && rpcreason.Reason(err) != protocol.ErrChildExited {
				return closedMsg{childID: childID, name: name, err: err}
			}
		}
		resp, err := c.client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{
			ChildId: childID, IncludeDescendants: include,
		}))
		if err != nil {
			return closedMsg{childID: childID, name: name, err: err}
		}
		return closedMsg{childID: childID, name: name, descendants: resp.Msg.GetDescendantIds()}
	}
}

// endSelected implements `x` on the agents pane: one key, three outcomes,
// each confirmed by a repeat.
//
// The outcome is chosen from the row's own state rather than from a mode the
// user has to hold in their head:
//
//   - exited            -> close (finalize; the transcript survives)
//   - shutting_down     -> kill, forced (it was already asked politely)
//   - anything else     -> kill, graceful (the daemon escalates on its own)
//
// The arm is per-CHILD. Arming on one row and moving the cursor before the
// repeat re-arms on the new row instead of ending it, so the agent that gets
// ended is always the one the confirmation named.
func (c *Cockpit) endSelected() tea.Cmd {
	id := c.selected
	if id == "" {
		return nil
	}
	node, ok := c.rail.Get(id)
	if !ok {
		return nil
	}
	name := node.Name
	if name == "" {
		name = id
	}

	armed := c.endArmedID == id &&
		!c.endArmed.IsZero() &&
		time.Since(c.endArmed) < quitConfirmWindow

	verb := "stop"
	switch {
	case node.Exited:
		verb = "close"
	case node.Status == statusShuttingDown:
		verb = "force kill"
	}

	if !armed {
		c.endArmed = time.Now()
		c.endArmedID = id
		c.setNotice("press " + c.keys.EndAgent.Help().Key + " again to " + verb + " " + name)
		return nil
	}
	c.endArmed = time.Time{}
	c.endArmedID = ""

	if kids := len(c.subagentIDs(id, verb != "close")); kids > 0 {
		c.endAsk = &endAsk{id: id, name: name, verb: verb, kids: kids}
		c.setNotice(name + " has " + pluralSubagents(kids) + ": " +
			endAskInclude + " = " + verb + " them too, " + endAskOnly + " = only " + name + ", any other key cancels")
		return nil
	}
	return c.dispatchEnd(id, name, verb, false)
}

const (
	endAskInclude = "a"
	endAskOnly    = "o"
)

func pluralSubagents(n int) string {
	if n == 1 {
		return "1 subagent"
	}
	return strconv.Itoa(n) + " subagents"
}

// answerEndAsk consumes the key that answers the pending endAsk.
func (c *Cockpit) answerEndAsk(key string) tea.Cmd {
	ask := c.endAsk
	c.endAsk = nil
	switch key {
	case endAskInclude:
		return c.dispatchEnd(ask.id, ask.name, ask.verb, true)
	case endAskOnly:
		return c.dispatchEnd(ask.id, ask.name, ask.verb, false)
	}
	c.setNotice("cancelled")
	return nil
}

// dispatchEnd runs the confirmed outcome for id.
func (c *Cockpit) dispatchEnd(id, name, verb string, include bool) tea.Cmd {
	switch verb {
	case "close":
		c.setNotice("closing " + name + "…")
		return c.closeCmd(id, name, include)
	case "force kill":
		c.setNotice("force killing " + name + "…")
		return c.killCmd(id, name, true, include)
	default:
		c.setNotice("stopping " + name + "…")
		return c.killCmd(id, name, false, include)
	}
}

// subagentIDs lists the rows beneath id in the rail, deepest first. Task
// subagents the proxy synthesized are left out: they end and close with their
// parent without being asked, so they are not a decision. onlyLive drops the
// exited ones, which is what a stop cares about; a close cares about every
// row, since closing a parent strands the rows beneath it.
func (c *Cockpit) subagentIDs(id string, onlyLive bool) []string {
	nodes := c.rail.Nodes()
	byID := make(map[string]rail.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ChildID] = n
	}
	var ids []string
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		if n.Native || (onlyLive && n.Exited) {
			continue
		}
		for cur, hops := n, 0; hops < len(nodes); hops++ {
			if cur.ParentID == id {
				ids = append(ids, n.ChildID)
				break
			}
			p, ok := byID[cur.ParentID]
			if !ok {
				break
			}
			cur = p
		}
	}
	return ids
}

// applyKilled reports the outcome of a kill.
//
// It does not remove the row: the child's own child_exited event does that,
// and reporting the exit from two places would race. What this owns is the
// notice, because a kill that FAILED is otherwise indistinguishable from one
// still in progress.
func (c *Cockpit) applyKilled(m killedMsg) {
	if m.err != nil {
		c.setNotice("could not stop " + m.name + ": " + trimRPCError(m.err))
		return
	}
	verb := "stopped "
	if m.forced {
		verb = "force killed "
	}
	if m.descendants > 0 {
		c.setNotice(verb + m.name + " and " + pluralSubagents(m.descendants))
		return
	}
	c.setNotice(verb + m.name)
}

// applyClosed reports the outcome of a close and drops the row.
//
// Unlike a kill there is no event to wait for: closing removes the child from
// the daemon's store, so nothing will ever publish about it again. The rail row
// has to go here or it stays until the next reseed.
func (c *Cockpit) applyClosed(m closedMsg) tea.Cmd {
	if m.err != nil {
		c.setNotice("could not close " + m.name + ": " + trimRPCError(m.err))
		return nil
	}
	var cmds []tea.Cmd
	for _, id := range m.descendants {
		cmds = append(cmds, c.forgetChild(id))
	}
	cmds = append(cmds, c.forgetChild(m.childID))
	if len(m.descendants) > 0 {
		c.setNotice("closed " + m.name + " and " + pluralSubagents(len(m.descendants)))
	} else {
		c.setNotice("closed " + m.name)
	}
	return tea.Batch(cmds...)
}

// trimRPCError strips connect-go's transport prefix so the daemon's own
// sentence is what reaches a one-line notice. "internal: child is still
// running" reads as an error about the child; the untrimmed form reads as an
// error about the RPC.
func trimRPCError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}

// forgetChild drops every trace of a closed child from the cockpit.
//
// All four are needed and each for its own reason: the rail row is what the
// user sees, the session and pane are memory that would otherwise be held
// until eviction, and the lru entry would keep a dead id in the rotation. The
// selection is moved off it because a cursor parked on a row that no longer
// exists makes the next `x` a no-op with no explanation.
func (c *Cockpit) forgetChild(childID string) tea.Cmd {
	if c.selected == childID {
		c.selected = c.successor(childID)
	}
	wasFocused := c.focused() == childID

	c.rail.Remove(childID)
	delete(c.sessions, childID)
	c.evictPane(childID)
	for i, id := range c.lru {
		if id == childID {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}

	// Closing the agent you were reading leaves the body pane pointed at
	// nothing. Stop its stream and land on a neighbour rather than rendering
	// a transcript for a child the daemon has forgotten.
	if wasFocused {
		if c.stopFocus != nil {
			c.stopFocus()
			c.stopFocus = nil
		}
		c.rail.SetFocus("")
	}

	// Closing the last row out of the rail leaves nothing to view, the same
	// state a bare attach with no children lands in, so it gets the same
	// answer: the create form. Focus still moves off the rail first (the form
	// is modal, and esc out of it must not land on a list nothing draws), and
	// the peek goes with the row: a peek that outlived it would resurrect the
	// rail for the NEXT single agent, one nobody asked to see.
	if c.rail.Len() == 0 {
		c.railPeek = false
		if c.focus == focusRail {
			_ = c.setFocus(focusInput)
		}
		if c.form == nil {
			return c.openSpawnForm()
		}
	}
	return nil
}
