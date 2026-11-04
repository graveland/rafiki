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

// tagForm is the modal shown by `t` on the agents pane: one field naming a
// tag on the child's last completed turn. Creating it is the grant that makes
// the point forkable (`rafiki create --from-tag`, or another child's spawn).
//
// Scope stays the daemon's lineage default and the cursor stays empty (last
// completed turn): both are the common case, and `rafiki tag add` carries the
// flags for the rest. Naming a fork point mid-history is a transcript-side
// picker this cockpit does not have yet.
type tagForm struct {
	childID string
	name    string
	input   textinput.Model
	err     string
	busy    bool
}

func newTagForm(childID, name string) *tagForm {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 64
	in.SetWidth(40)
	in.Placeholder = "point-in-history"
	in.Focus()
	return &tagForm{childID: childID, name: name, input: in}
}

func (f *tagForm) view(width int) string {
	f.input.SetWidth(max(20, min(width-2, 60)))
	body := f.input.View()
	if f.err != "" {
		body += "\n" + styleMeta.Render(f.err)
	}
	body += "\n" + styleMeta.Render("⏎ tag · esc cancel")
	return "Tag " + f.name + "\n\n" + body
}

type taggedMsg struct {
	origin  *tagForm
	childID string
	name    string
	row     *rafikiv1.TagRow
	err     error
}

func (c *Cockpit) tagCmd(origin *tagForm, childID, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		// Empty cursor: the child's last completed turn — the fork point a
		// transcript-level picker would offer, once one exists. Unspecified
		// scope: the daemon's lineage default.
		resp, err := c.client.Tag(ctx, connect.NewRequest(&rafikiv1.TagRequest{
			Name:    name,
			ChildId: childID,
		}))
		if err != nil {
			return taggedMsg{origin: origin, childID: childID, name: name, err: err}
		}
		return taggedMsg{origin: origin, childID: childID, name: name, err: nil, row: resp.Msg.GetTag()}
	}
}

// applyTagged reports the outcome and dismisses the originating modal.
func (c *Cockpit) applyTagged(m taggedMsg) {
	if m.err != nil {
		c.setNotice("could not tag " + m.childID + ": " + trimRPCError(m.err))
		if m.origin != nil {
			m.origin.busy = false
			m.origin.err = trimRPCError(m.err)
		}
		return
	}
	if c.tagForm == m.origin {
		c.tagForm = nil
	}
	at := ""
	if m.row != nil {
		at = fmt.Sprintf("m:%d", m.row.GetOrdinal())
	}
	c.setNotice(fmt.Sprintf("tagged %s at %s — forkable as --from-tag %s", m.childID, at, m.name))
}

// handleTagFormKey routes a keystroke while the tag modal is up.
func (c *Cockpit) handleTagFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := c.tagForm
	switch msg.String() {
	case "esc", "ctrl+c":
		c.tagForm = nil
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
		return c, c.tagCmd(f, f.childID, name)
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return c, cmd
}
