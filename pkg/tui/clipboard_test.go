// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/multigres/testkit/assert"
)

// swapClipboard installs a fake clipboard reader for one test.
func swapClipboard(t *testing.T, fake func() (string, []byte, string, error)) {
	t.Helper()
	orig := readClipboard
	readClipboard = fake
	t.Cleanup(func() { readClipboard = orig })
}

// clipboardCockpit gives an input-focused cockpit sized to render.
func clipboardCockpit(t *testing.T) *Cockpit {
	t.Helper()
	c := newTestCockpit("c_1")
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return c
}

// The whole ^V path drives the REAL handleKey, per the rule that a new key
// case wants a test through the handler, not the handler's internals.
func TestCtrlVStagesAClipboardImage(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	swapClipboard(t, func() (string, []byte, string, error) {
		return "image/png", []byte("\x89PNGfake"), "", nil
	})

	_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	ck.Require().NotNil(cmd, "^V produced no command")
	msg := cmd()
	cp, ok := msg.(clipboardPasteMsg)
	ck.Require().True(ok, "cmd returned %T, want clipboardPasteMsg", msg)
	c.Update(cp)

	ck.Eq(1, len(c.attachments), "staged attachment count")
	ck.Eq("image/png", c.attachments[0].mediaType, "media type")
	ck.Eq("[Image #1]", c.attachments[0].token, "token")
	ck.StrContains(c.ta.Value(), "[Image #1]", "token missing from the input")
}

// Two pastes before one send must be distinguishable to the model: the tokens
// number off ([Image #1], [Image #2]), never repeat.
func TestCtrlVNumbersRepeatedClipboardImages(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	swapClipboard(t, func() (string, []byte, string, error) {
		return "image/png", []byte("png"), "", nil
	})

	for _, want := range []string{"[Image #1]", "[Image #2]"} {
		_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
		ck.Require().NotNil(cmd, "no command")
		c.Update(cmd())
		ck.StrContains(c.ta.Value(), want, "token missing after paste")
	}
	ck.Eq(2, len(c.attachments), "both images staged")
}

// A clipboard holding text must still paste: ^V falls through to the ordinary
// paste path, folding and normalization included.
func TestCtrlVPastesTextWhenClipboardHoldsText(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	long := strings.Repeat("x", pasteCharThreshold+1)
	calls := 0
	swapClipboard(t, func() (string, []byte, string, error) {
		calls++
		if calls == 1 {
			return "", nil, "just some prose", nil
		}
		return "", nil, long, nil
	})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	c.Update(cmd())
	ck.Empty(c.attachments, "text staged as an attachment")
	ck.Eq("just some prose", c.ta.Value(), "text did not land in the box")

	c.ta.Reset()
	_, cmd = c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	c.Update(cmd())
	ck.StrContains(c.ta.Value(), "[pasted #1:", "long clipboard text was not folded")
	ck.Eq(1, len(c.pastes), "folded paste not held")
}

// A failed read is a notice, never an attachment and never an error dialog.
func TestCtrlVReportsAnUnreadableClipboard(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	swapClipboard(t, func() (string, []byte, string, error) {
		return "", nil, "", errors.New("clipboard holds no image or text")
	})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	ck.Require().NotNil(cmd, "no command")
	c.Update(cmd())
	ck.Empty(c.attachments, "a failed read staged an attachment")
	ck.StrContains(c.notice, "clipboard", "notice did not explain the failure")
}

// Oversized clipboard bytes are refused with the same message a path paste
// gets, for the same reason: Anthropic rejects them upstream.
func TestCtrlVRefusesAnOversizedImage(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	swapClipboard(t, func() (string, []byte, string, error) {
		return "image/png", make([]byte, maxAttachmentBytes+1), "", nil
	})
	_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	c.Update(cmd())
	ck.Empty(c.attachments, "oversized image staged")
	ck.StrContains(c.notice, "limit", "notice did not name the limit")
}

// The binding is input-pane-scoped: with the rail focused, ^V reaches the
// rail's swallow-everything, not the clipboard.
func TestCtrlVIsDeadWhileTheRailHoldsFocus(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := clipboardCockpit(t)
	swapClipboard(t, func() (string, []byte, string, error) {
		t.Error("the clipboard was read while the rail held focus")
		return "", nil, "", nil
	})
	c.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}) // ToggleRail: focus moves to the rail
	_, cmd := c.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	ck.Nil(cmd, "rail-focused ^V produced a command")
	ck.Empty(c.attachments, "rail-focused ^V staged an attachment")
}
