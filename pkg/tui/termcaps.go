// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// imageMode is how the transcript draws images. The zero value is
// imageModePlaceholder ON PURPOSE: it is the safe default — the one-line
// placeholder, and no image is ever decoded.
type imageMode int

const (
	imageModePlaceholder imageMode = iota
	imageModeKitty
)

// kittyProbeID is the image id termcaps queries with, and the id a supporting
// terminal echoes back with payload "OK".
const kittyProbeID = 31

// termCaps resolves whether this terminal can draw Kitty graphics thumbnails.
//
// It is a pure state machine — nothing here touches a screen. The owner writes
// queries() once at startup, feeds every tea.Msg to observe until the sentinel
// arrives, and reads mode() whenever it must decide how an image renders.
type termCaps struct {
	flag   string // "auto", "kitty", or "off" ("" and anything else mean "auto")
	probe  bool   // auto mode may query the terminal at all (no tmux, not Apple)
	probes string // what queries() returns; decided once at construction

	kittyReply  bool // the id-31 probe answered OK, before the sentinel
	termVersion string
	da1         bool // sentinel: every query has been answered or never will be
	profile     colorprofile.Profile

	cellW, cellH int
	cellAnswered bool // a CellSizeEvent arrived; beats the pixel-derived estimate
	pixelW       int
	pixelH       int
	cols         int
	rows         int
}

// newTermCaps builds the resolver. flag is "auto", "kitty", "off", or "" (=
// "auto"); getenv is os.Getenv in production, and a map lookup in tests.
func newTermCaps(flag string, getenv func(string) string) *termCaps {
	t := &termCaps{
		profile: colorprofile.Unknown,
		cellW:   8,
		cellH:   16,
	}
	switch flag {
	case "kitty", "off":
		t.flag = flag
	default:
		t.flag = "auto"
		t.probe = getenv("TMUX") == "" && !strings.Contains(getenv("TERM_PROGRAM"), "Apple")
	}
	switch {
	case t.flag == "off":
		// send nothing
	case t.flag == "kitty":
		t.probes = ansi.WindowOp(16) + ansi.WindowOp(14)
	case t.probe:
		t.probes = ansi.KittyGraphics([]byte("AAAA"), "i=31", "s=1", "v=1", "a=q", "t=d", "f=24") +
			ansi.WindowOp(16) + ansi.WindowOp(14) +
			ansi.RequestNameVersion + ansi.RequestPrimaryDeviceAttributes
	}
	return t
}

// queries returns the escape sequences to write at startup, in order; ""
// means send nothing.
func (t *termCaps) queries() string {
	return t.probes
}

// mode reports how the transcript should draw images right now.
func (t *termCaps) mode() imageMode {
	switch t.flag {
	case "off":
		return imageModePlaceholder
	case "kitty":
		return imageModeKitty
	}
	if !t.probe || !t.da1 || !t.kittyReply || !versionAllowed(t.termVersion) {
		return imageModePlaceholder
	}
	if t.profile != colorprofile.ANSI256 && t.profile != colorprofile.TrueColor {
		return imageModePlaceholder
	}
	return imageModeKitty
}

// observe feeds the machine one message and reports whether mode() changed.
//
// Every capability answer counts only while it can still change the outcome:
// once the DA1 sentinel arrives, a late kitty reply or XTVERSION name is
// ignored, because the queries that produced them have already been judged
// unanswered.
func (t *termCaps) observe(msg tea.Msg) bool {
	before := t.mode()
	switch m := msg.(type) {
	case uv.KittyGraphicsEvent:
		if !t.da1 && m.Options.ID == kittyProbeID && string(m.Payload) == "OK" {
			t.kittyReply = true
		}
	case tea.TerminalVersionMsg:
		if !t.da1 {
			t.termVersion = m.Name
		}
	case uv.PrimaryDeviceAttributesEvent:
		t.da1 = true
	case uv.CellSizeEvent:
		if m.Width > 0 && m.Height > 0 {
			t.cellW, t.cellH = m.Width, m.Height
			t.cellAnswered = true
		}
	case uv.PixelSizeEvent:
		t.pixelW, t.pixelH = m.Width, m.Height
		t.deriveCellSize()
	case tea.WindowSizeMsg:
		t.cols, t.rows = m.Width, m.Height
		t.deriveCellSize()
	case tea.ColorProfileMsg:
		t.profile = m.Profile
	}
	return t.mode() != before
}

// deriveCellSize estimates the cell size by dividing the pixel size by the
// window size in cells, until a real CellSizeEvent arrives. A quotient that
// rounds to zero — a reported pixel size smaller than the window size in
// cells — is refused, and the 8×16 default stands.
func (t *termCaps) deriveCellSize() {
	if t.cellAnswered {
		return
	}
	if t.pixelW > 0 && t.pixelH > 0 && t.cols > 0 && t.rows > 0 {
		if w, h := t.pixelW/t.cols, t.pixelH/t.rows; w > 0 && h > 0 {
			t.cellW, t.cellH = w, h
		}
	}
}

// cellSize returns the cell size in pixels: measured, derived, or the 8×16
// default until anything has been answered.
func (t *termCaps) cellSize() (int, int) {
	return t.cellW, t.cellH
}

// smallIDs reports whether image ids must fit in one byte (1–255).
func (t *termCaps) smallIDs() bool {
	return t.profile != colorprofile.TrueColor && t.profile != colorprofile.Unknown
}

// settled reports whether the verdict is final: an override and an auto mode
// that may not probe have nothing to wait for, and a probing auto mode is
// done once the DA1 sentinel arrives.
func (t *termCaps) settled() bool {
	return t.flag != "auto" || !t.probe || t.da1
}

// logAttrs names every input mode() judged, so the startup log line of a
// terminal left on the placeholder says which check refused it.
func (t *termCaps) logAttrs() []any {
	mode := "placeholder"
	if t.mode() == imageModeKitty {
		mode = "kitty"
	}
	return []any{
		"mode", mode,
		"flag", t.flag,
		"probe", t.probe,
		"kitty_reply", t.kittyReply,
		"terminal", t.termVersion,
		"profile", t.profile.String(),
	}
}

// versionAllowed reports whether an XTVERSION name names a terminal whose
// graphics support is trusted. The match is case-insensitive; iTerm2 counts
// from 3.7.0, the oldest build seen drawing placeholder thumbnails.
func versionAllowed(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "kitty"), strings.HasPrefix(n, "ghostty"):
		return true
	case strings.HasPrefix(n, "iterm2 "):
		return versionGE(n[len("iterm2 "):], 3, 7, 0)
	}
	return false
}

// versionGE parses v as dotted integers up to the first character that is
// neither a digit nor a dot, and reports whether it is at least want.
// Missing components count as 0.
func versionGE(v string, want ...int) bool {
	end := 0
	for end < len(v) && (v[end] >= '0' && v[end] <= '9' || v[end] == '.') {
		end++
	}
	parts := strings.Split(v[:end], ".")
	for i, w := range want {
		n := 0
		if i < len(parts) {
			if p, err := strconv.Atoi(parts[i]); err == nil {
				n = p
			}
		}
		if n != w {
			return n > w
		}
	}
	return true
}
