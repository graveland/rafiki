// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/multigres/testkit/assert"
)

// env builds the getenv stub newTermCaps takes, from a lookup table.
func env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// kittyOK is the reply a supporting terminal sends to the id-31 probe.
func kittyOK() uv.KittyGraphicsEvent {
	return uv.KittyGraphicsEvent{Options: kitty.Options{ID: 31}, Payload: []byte("OK")}
}

// observeAuto feeds caps everything auto mode asked for except the kitty
// reply: the XTVERSION name, the colour profile, then the DA1 sentinel last.
func observeAuto(c *termCaps, name string, profile colorprofile.Profile) {
	c.observe(tea.TerminalVersionMsg{Name: name})
	c.observe(tea.ColorProfileMsg{Profile: profile})
	c.observe(uv.PrimaryDeviceAttributesEvent{1})
}

// autoCaps is a fresh auto resolver that has received a complete, agreeing
// reply sequence: kitty probe OK, XTVERSION name, truecolour, DA1.
func autoCaps(name string) *termCaps {
	c := newTermCaps("auto", env(nil))
	c.observe(kittyOK())
	observeAuto(c, name, colorprofile.TrueColor)
	return c
}

// TestTermCapsOffSendsNothing pins the --images=off rule: no queries at all,
// and placeholder no matter what the terminal volunteers.
func TestTermCapsOffSendsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("off", env(nil))
	ck.Eq("", c.queries(), "off must not send capability queries")
	c.observe(kittyOK())
	observeAuto(c, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, c.mode(), "off must stay placeholder through a full kitty reply sequence")
}

// TestTermCapsForcedKittyIsKitty pins --images=kitty: kitty immediately, with
// no messages at all, asking only for the cell size.
func TestTermCapsForcedKittyIsKitty(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("kitty", env(nil))
	ck.Eq(imageModeKitty, c.mode(), "forced kitty needs no terminal answers")
	ck.Eq(ansi.WindowOp(16)+ansi.WindowOp(14), c.queries(),
		"forced kitty sends only the cell-size and pixel-size queries")
}

// TestTermCapsTmuxSendsNothing pins the tmux rule: passthrough mangles the
// replies, so auto mode inside tmux never queries and never draws.
func TestTermCapsTmuxSendsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("auto", env(map[string]string{"TMUX": "/tmp/x"}))
	ck.Eq("", c.queries(), "auto inside tmux must not query")
	c.observe(kittyOK())
	observeAuto(c, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, c.mode(), "auto inside tmux stays placeholder")
}

// TestTermCapsAppleTerminalSendsNothing pins the Apple_Terminal rule: it
// answers capability queries with lies, so auto mode never queries it.
func TestTermCapsAppleTerminalSendsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("auto", env(map[string]string{"TERM_PROGRAM": "Apple_Terminal"}))
	ck.Eq("", c.queries(), "auto under Apple_Terminal must not query")
	c.observe(kittyOK())
	observeAuto(c, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, c.mode(), "auto under Apple_Terminal stays placeholder")
}

// TestTermCapsQueriesEndWithDA1 pins the query order: the kitty probe and
// XTVERSION first, DA1 last, because every terminal answers DA1 and answers
// in order, so its reply is the sentinel that the rest have arrived — or
// never will.
func TestTermCapsQueriesEndWithDA1(t *testing.T) {
	ck := assert.NewCollecting(t)
	q := newTermCaps("auto", env(nil)).queries()
	ck.True(strings.HasSuffix(q, ansi.RequestPrimaryDeviceAttributes),
		"DA1 must come last: it is the sentinel")
	ck.True(strings.Contains(q, "i=31"), "auto must send the id-31 kitty probe")
	ck.True(strings.Contains(q, ansi.RequestNameVersion), "auto must ask XTVERSION")
}

// TestTermCapsNoSentinelIsPlaceholder pins the sentinel rule: a terminal that
// answered everything else but never DA1 is not trusted with images.
func TestTermCapsNoSentinelIsPlaceholder(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("auto", env(nil))
	c.observe(kittyOK())
	c.observe(tea.TerminalVersionMsg{Name: "iTerm2 3.7.3"})
	c.observe(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	ck.Eq(imageModePlaceholder, c.mode(), "no DA1, no kitty — even with every other answer")
}

// TestTermCapsDA1WithoutKittyIsPlaceholder pins the converse: the sentinel
// alone proves only that queries were delivered, not that graphics work.
func TestTermCapsDA1WithoutKittyIsPlaceholder(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("auto", env(nil))
	observeAuto(c, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, c.mode(), "DA1 without a kitty reply stays placeholder")
}

// TestTermCapsKittyReplyAfterSentinelIgnored pins the too-late rule: once DA1
// has arrived, a late kitty reply is ignored, and so is a late XTVERSION
// name — the verdict was already reached without them.
func TestTermCapsKittyReplyAfterSentinelIgnored(t *testing.T) {
	ck := assert.NewCollecting(t)

	c := newTermCaps("auto", env(nil))
	c.observe(tea.TerminalVersionMsg{Name: "iTerm2 3.7.3"})
	c.observe(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	c.observe(uv.PrimaryDeviceAttributesEvent{1})
	c.observe(kittyOK())
	ck.Eq(imageModePlaceholder, c.mode(), "a kitty reply after the sentinel does not count")

	d := newTermCaps("auto", env(nil))
	d.observe(kittyOK())
	d.observe(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	d.observe(uv.PrimaryDeviceAttributesEvent{1})
	d.observe(tea.TerminalVersionMsg{Name: "iTerm2 3.7.3"})
	ck.Eq(imageModePlaceholder, d.mode(), "a version arriving after the sentinel does not count")
}

// TestTermCapsITerm2Versions pins the allowlist: kitty and ghostty by prefix,
// iTerm2 only from 3.7.3, everything else (and no name at all) refused.
func TestTermCapsITerm2Versions(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		want imageMode
	}{
		{"iTerm2 3.7.3", imageModeKitty},
		{"iTerm2 3.7.10", imageModeKitty},
		{"iTerm2 3.8", imageModeKitty},
		{"iTerm2 3.7.2", imageModePlaceholder},
		{"iTerm2 3.6.9", imageModePlaceholder},
		{"kitty(0.39.1)", imageModeKitty},
		{"ghostty 1.2.0", imageModeKitty},
		{"WezTerm 20240203", imageModePlaceholder},
		{"", imageModePlaceholder},
	} {
		ck.Eq(tc.want, autoCaps(tc.name).mode(), "XTVERSION "+tc.name)
	}
}

// TestTermCapsNeedsAtLeast256Colours pins the colour floor: 16 colours is not
// enough, 256 is, and the id width follows the profile.
func TestTermCapsNeedsAtLeast256Colours(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, tc := range []struct {
		profile  colorprofile.Profile
		want     imageMode
		smallIDs bool
	}{
		{colorprofile.ANSI, imageModePlaceholder, true},
		{colorprofile.ANSI256, imageModeKitty, true},
		{colorprofile.TrueColor, imageModeKitty, false},
	} {
		c := newTermCaps("auto", env(nil))
		c.observe(kittyOK())
		observeAuto(c, "iTerm2 3.7.3", tc.profile)
		ck.Eq(tc.want, c.mode(), tc.profile.String()+" mode")
		ck.Eq(tc.smallIDs, c.smallIDs(), tc.profile.String()+" smallIDs")
	}

	fresh := newTermCaps("auto", env(nil))
	ck.False(fresh.smallIDs(), "a profile the terminal never reported must not widen ids")
}

// TestTermCapsObserveReportsModeChange pins the observe contract: true
// exactly when the verdict flips, so the caller knows when to invalidate.
func TestTermCapsObserveReportsModeChange(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTermCaps("auto", env(nil))
	c.observe(kittyOK())
	c.observe(tea.TerminalVersionMsg{Name: "iTerm2 3.7.3"})
	c.observe(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	ck.Eq("iTerm2 3.7.3", c.version(), "version() returns the XTVERSION name, for the log line")
	ck.True(c.observe(uv.PrimaryDeviceAttributesEvent{1}),
		"the DA1 that completes the sequence must report the mode change")
	ck.False(c.observe(uv.PrimaryDeviceAttributesEvent{1}), "a second DA1 changes nothing")
	ck.False(c.observe(tea.KeyPressMsg{}), "an unrelated message changes nothing")
}

// TestTermCapsCellSize pins the cell size ladder: measured beats derived
// beats the 8×16 default, whichever order the answers arrive in.
func TestTermCapsCellSize(t *testing.T) {
	ck := assert.NewCollecting(t)

	c := newTermCaps("auto", env(nil))
	w, h := c.cellSize()
	ck.Eq(8, w, "default width before anything is answered")
	ck.Eq(16, h, "default height before anything is answered")

	c.observe(uv.CellSizeEvent{Width: 9, Height: 18})
	w, h = c.cellSize()
	ck.Eq(9, w, "a CellSizeEvent is authoritative")
	ck.Eq(18, h, "a CellSizeEvent is authoritative")

	derived := newTermCaps("auto", env(nil))
	derived.observe(uv.PixelSizeEvent{Width: 1600, Height: 900})
	derived.observe(tea.WindowSizeMsg{Width: 200, Height: 50})
	w, h = derived.cellSize()
	ck.Eq(8, w, "pixel width / columns")
	ck.Eq(18, h, "pixel height / rows")

	// A pixel size smaller than the window size in cells divides to zero on
	// both axes; the default must stand, or the thumbnail maths divides by it.
	degenerate := newTermCaps("auto", env(nil))
	degenerate.observe(uv.PixelSizeEvent{Width: 40, Height: 20})
	degenerate.observe(tea.WindowSizeMsg{Width: 200, Height: 50})
	w, h = degenerate.cellSize()
	ck.Eq(8, w, "a zero quotient keeps the default width")
	ck.Eq(16, h, "a zero quotient keeps the default height")

	// One zero quotient is enough to refuse the estimate.
	halfDegenerate := newTermCaps("auto", env(nil))
	halfDegenerate.observe(uv.PixelSizeEvent{Width: 0, Height: 900})
	halfDegenerate.observe(tea.WindowSizeMsg{Width: 200, Height: 50})
	w, h = halfDegenerate.cellSize()
	ck.Eq(8, w, "one zero quotient keeps the default width")
	ck.Eq(16, h, "one zero quotient keeps the default height")

	afterPixels := newTermCaps("auto", env(nil))
	afterPixels.observe(uv.PixelSizeEvent{Width: 1600, Height: 900})
	afterPixels.observe(tea.WindowSizeMsg{Width: 200, Height: 50})
	afterPixels.observe(uv.CellSizeEvent{Width: 9, Height: 18})
	w, h = afterPixels.cellSize()
	ck.Eq(9, w, "a late CellSizeEvent beats the derived value")
	ck.Eq(18, h, "a late CellSizeEvent beats the derived value")

	beforePixels := newTermCaps("auto", env(nil))
	beforePixels.observe(uv.CellSizeEvent{Width: 9, Height: 18})
	beforePixels.observe(uv.PixelSizeEvent{Width: 1600, Height: 900})
	beforePixels.observe(tea.WindowSizeMsg{Width: 200, Height: 50})
	w, h = beforePixels.cellSize()
	ck.Eq(9, w, "pixels arriving after a CellSizeEvent must not overwrite it")
	ck.Eq(18, h, "pixels arriving after a CellSizeEvent must not overwrite it")
}

// TestTermCapsWrongKittyReplyIgnored pins the reply check: only the id-31
// probe answered with OK counts; any other payload or id is noise.
func TestTermCapsWrongKittyReplyIgnored(t *testing.T) {
	ck := assert.NewCollecting(t)

	badPayload := newTermCaps("auto", env(nil))
	badPayload.observe(uv.KittyGraphicsEvent{Options: kitty.Options{ID: 31},
		Payload: []byte("ENOTSUPPORTED")})
	observeAuto(badPayload, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, badPayload.mode(), "id 31 with a non-OK payload is not support")

	wrongID := newTermCaps("auto", env(nil))
	wrongID.observe(uv.KittyGraphicsEvent{Options: kitty.Options{ID: 7}, Payload: []byte("OK")})
	observeAuto(wrongID, "iTerm2 3.7.3", colorprofile.TrueColor)
	ck.Eq(imageModePlaceholder, wrongID.mode(), "an OK from another image id is not support")
}
