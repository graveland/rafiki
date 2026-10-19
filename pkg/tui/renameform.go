// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// renameForm is the modal shown by `R` on the agents pane: one field renaming
// a child and its conversations.
type renameForm struct {
	childID string
	input   textinput.Model
	err     string
	busy    bool
}

func newRenameForm(childID, currentName string) *renameForm {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	in.SetWidth(40)
	in.SetValue(currentName)
	in.CursorEnd()
	in.Focus()
	return &renameForm{childID: childID, input: in}
}

func (f *renameForm) view(width int) string {
	f.input.SetWidth(max(20, min(width-2, 60)))
	body := f.input.View()
	if f.err != "" {
		body += "\n" + styleMeta.Render(f.err)
	}
	body += "\n" + styleMeta.Render("⏎ save · esc cancel")
	return "Rename " + f.childID + "\n\n" + body
}

type renamedMsg struct {
	origin  *renameForm
	childID string
	name    string
	err     error
}

func (c *Cockpit) renameCmd(origin *renameForm, childID, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		resp, err := c.client.Rename(ctx, connect.NewRequest(&rafikiv1.RenameRequest{
			ChildId: childID,
			Name:    name,
		}))
		if err == nil {
			name = resp.Msg.GetName()
		}
		return renamedMsg{origin: origin, childID: childID, name: name, err: err}
	}
}

// applyRenamed reports the outcome and, on success, updates the rail's
// displayed name and dismisses the originating modal.
func (c *Cockpit) applyRenamed(m renamedMsg) {
	if m.err != nil {
		c.setNotice("could not rename " + m.childID + ": " + trimRPCError(m.err))
		if m.origin != nil {
			m.origin.busy = false
			m.origin.err = trimRPCError(m.err)
		}
		return
	}
	c.rail.SetName(m.childID, m.name)
	if c.renameForm == m.origin {
		c.renameForm = nil
	}
	c.setNotice(fmt.Sprintf("renamed %s to %s", m.childID, m.name))
}

// handleRenameFormKey routes a keystroke while the rename modal is up.
func (c *Cockpit) handleRenameFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := c.renameForm
	switch msg.String() {
	case "esc", "ctrl+c":
		c.renameForm = nil
		return c, nil
	case "enter":
		if f.busy {
			return c, nil
		}
		name := strings.TrimSpace(f.input.Value())
		if name == "" {
			f.err = "name cannot be empty"
			return c, nil
		}
		f.busy = true
		return c, c.renameCmd(f, f.childID, name)
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return c, cmd
}
