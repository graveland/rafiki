// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
)

// budgetForm is the modal shown by `b` on the agents pane: a single field
// setting one child's max-cost cap with operator authority.
//
// Unlike spawnForm's max-cost field, 0 is an ACCEPTED, intentional value
// here -- it means "clear the cap", not a typo -- so this does not reuse
// spawnForm's params() rejection of a typed "0".
type budgetForm struct {
	childID   string
	childName string
	input     textinput.Model
	err       string
	busy      bool
}

// newBudgetForm opens the modal for one child, prefilling the current cap
// (blank when currently unlimited, matching spawnForm's own "blank means no
// cap" convention).
func newBudgetForm(childID, childName string, currentMaxCost float64, cur *clientstate.Currency) *budgetForm {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	in.Placeholder = "(unlimited)"
	in.SetWidth(40)
	if currentMaxCost > 0 {
		in.SetValue(strconv.FormatFloat(costfmt.ToDisplay(currentMaxCost, cur), 'f', -1, 64))
	}
	in.Focus()
	return &budgetForm{childID: childID, childName: childName, input: in}
}

// params validates the field and returns the USD amount to send, or an
// error message. A blank field means 0 (unlimited) -- there is no rejection
// of an explicit "0" the way spawnForm's create-time field has, because
// clearing a cap is exactly what this modal exists to let an operator do.
func (f *budgetForm) params(cur *clientstate.Currency) (maxCost float64, problem string) {
	raw := strings.TrimSpace(f.input.Value())
	if raw == "" {
		return 0, ""
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0, fmt.Sprintf("budget: %q is not a non-negative number", raw)
	}
	return costfmt.ToUSD(v, cur), ""
}

func (f *budgetForm) view(width, height int, cur *clientstate.Currency) string {
	f.input.SetWidth(max(20, min(width-2, 60)))
	title := fmt.Sprintf("Set budget for %s", f.childName)
	body := f.input.View()
	if f.err != "" {
		body += "\n" + styleMeta.Render(f.err)
	}
	body += "\n" + styleMeta.Render("⏎ save · esc cancel · blank = unlimited")
	return title + "\n\n" + body
}

// handleBudgetFormKey routes a keystroke while the budget modal is up.
func (c *Cockpit) handleBudgetFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := c.budgetForm
	switch msg.String() {
	case "esc", "ctrl+c":
		c.budgetForm = nil
		return c, nil
	case "enter":
		if f.busy {
			return c, nil
		}
		maxCost, problem := f.params(c.currency)
		if problem != "" {
			f.err = problem
			return c, nil
		}
		f.busy = true
		return c, c.setBudgetCmd(f, f.childID, f.childName, maxCost)
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return c, cmd
}
