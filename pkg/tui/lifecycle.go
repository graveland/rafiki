// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
// rather than imported for the same reason rail.LiveStatuses spells the eight
// out: this package renders status strings it receives over the wire and does
// not otherwise depend on the daemon's types.
const statusShuttingDown = "shutting_down"

type spawnedMsg struct {
	childID string
	err     error
}

type killedMsg struct {
	childID string
	name    string
	forced  bool
	err     error
}

type closedMsg struct {
	childID string
	name    string
	err     error
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

		resp, err := c.client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd:              p.cwd,
			Name:             p.name,
			Kind:             p.kind,
			Model:            p.model,
			ExecutorSelector: c.executorSelector,
		}))
		if err != nil {
			return spawnedMsg{err: err}
		}
		return spawnedMsg{childID: resp.Msg.GetChildId()}
	}
}

// killCmd ends a child. force asks the daemon to escalate immediately instead
// of waiting out its shutdown grace.
func (c *Cockpit) killCmd(childID, name string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		req := &rafikiv1.KillRequest{ChildId: childID}
		if force {
			req.ShutdownTimeoutMs = forceShutdownMs
		}
		if _, err := c.client.Kill(ctx, connect.NewRequest(req)); err != nil {
			return killedMsg{childID: childID, name: name, forced: force, err: err}
		}
		return killedMsg{childID: childID, name: name, forced: force}
	}
}

// closeCmd finalizes an exited child: it leaves the daemon's store and can
// never be resumed again. The transcript survives -- nothing references
// conversations.child -- so this ends resumption, not history.
func (c *Cockpit) closeCmd(childID, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		_, err := c.client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{
			ChildId: childID,
		}))
		return closedMsg{childID: childID, name: name, err: err}
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

	switch {
	case node.Exited:
		c.setNotice("closing " + name + "…")
		return c.closeCmd(id, name)
	case node.Status == statusShuttingDown:
		c.setNotice("force killing " + name + "…")
		return c.killCmd(id, name, true)
	default:
		c.setNotice("stopping " + name + "…")
		return c.killCmd(id, name, false)
	}
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
	if m.forced {
		c.setNotice("force killed " + m.name)
		return
	}
	c.setNotice("stopped " + m.name)
}

// applyClosed reports the outcome of a close and drops the row.
//
// Unlike a kill there is no event to wait for: closing removes the child from
// the daemon's store, so nothing will ever publish about it again. The rail row
// has to go here or it stays until the next reseed.
func (c *Cockpit) applyClosed(m closedMsg) {
	if m.err != nil {
		c.setNotice("could not close " + m.name + ": " + trimRPCError(m.err))
		return
	}
	c.forgetChild(m.childID)
	c.setNotice("closed " + m.name)
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
func (c *Cockpit) forgetChild(childID string) {
	if c.selected == childID {
		c.selected = c.neighbour(+1)
		if c.selected == childID {
			c.selected = ""
		}
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
}
