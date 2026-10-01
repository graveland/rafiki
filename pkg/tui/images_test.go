// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
)

// newCtrlY is the ^Y keypress.
func newCtrlY() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
}

// readyStore drives the real pipeline end to end for one 400×300 PNG — the
// same image TestKittyThumbCmdBuildsTransmitAndPlacement sizes at 32×12
// cells — and returns the store and the block still carrying it.
func readyStore(t *testing.T) (*imageStore, *rafikiv1.ImageBlock) {
	t.Helper()
	st := newTestStore()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}
	st.lookup(img)
	q := st.takeQueued(false)
	if len(q) != 1 {
		t.Fatalf("takeQueued returned %d items, want 1", len(q))
	}
	if !st.ready(runThumb(t, q[0])) {
		t.Fatal("thumbnail did not become ready")
	}
	return st, img
}

// readyCockpitThumb readies one thumbnail on a cockpit's own store and
// returns the thumb (its id is what cleanup must delete).
func readyCockpitThumb(t *testing.T, c *Cockpit) *thumb {
	t.Helper()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}
	c.images.lookup(img)
	q := c.images.takeQueued(false)
	if len(q) != 1 {
		t.Fatalf("takeQueued returned %d items, want 1", len(q))
	}
	if !c.images.ready(runThumb(t, q[0])) {
		t.Fatal("thumbnail did not become ready")
	}
	return c.images.lookup(img)
}

// pixelRows splits a rendered transcript into its pixel rows: every line whose
// stripped text carries a Kitty placeholder rune.
func pixelRows(out string) []string {
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if strings.ContainsRune(ansi.Strip(l), kitty.Placeholder) {
			rows = append(rows, l)
		}
	}
	return rows
}

// checkPixelRows pins one block's image rendering: the thumb's rows, each
// under the stripped gutter, each gutter+cols wide, each carrying one
// placeholder per column.
func checkPixelRows(t *testing.T, out, gutter string, cols, rows int) {
	t.Helper()
	c := assert.NewCollecting(t)
	got := pixelRows(out)
	width := ansi.StringWidth(gutter)
	c.Require().Eq(rows, len(got), "want %d pixel rows:\n%s", rows, out)
	for i, row := range got {
		stripped := ansi.Strip(row)
		c.True(strings.HasPrefix(stripped, gutter), "row %d must start with %q: %q", i, gutter, stripped)
		c.Eq(width+cols, ansi.StringWidth(stripped), "row %d width", i)
		c.Eq(cols, strings.Count(row, string(kitty.Placeholder)), "row %d placeholder count", i)
	}
}

// TestImageRowsDrawPixelsUnderTheUserGutter pins the user block: a ready
// thumbnail renders as Kitty placeholder rows under the same ▌ gutter the
// fallback would use, BEFORE the text (sendWith's image-first order).
func TestImageRowsDrawPixelsUnderTheUserGutter(t *testing.T) {
	st, img := readyStore(t)
	r := newRenderer()
	r.images, r.drawImages = st, true
	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindUser, Text: "look",
		Images: []*rafikiv1.ImageBlock{img},
	}}, 1, 100), "\n")

	checkPixelRows(t, out, "▌ ", 32, 12)
	// The rows are built from the thumbnail id, never from the image's own
	// data — in particular not the placeholder line's text.
	assert.NewCollecting(t).NotStrContains(ansi.Strip(out), "image/png",
		"the pixel rows must not carry the fallback's media-type line:\n")
}

// A tool result's image draws pixels under the SAME gutter as its text.
func TestImageRowsDrawPixelsUnderTheToolGutter(t *testing.T) {
	st, img := readyStore(t)
	r := newRenderer()
	r.images, r.drawImages = st, true
	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{
			Name: "bash", HasResult: true, Result: "exit 0",
			Images: []*rafikiv1.ImageBlock{img},
		}},
	}}, 1, 100), "\n")

	checkPixelRows(t, out, "    │ ", 32, 12)
	assert.NewCollecting(t).StrContains(ansi.Strip(out), "exit 0", "the result text must still render")
}

// An assistant block's image draws pixels under the same bar as its prose.
func TestImageRowsDrawPixelsUnderTheAssistantBar(t *testing.T) {
	st, img := readyStore(t)
	r := newRenderer()
	r.images, r.drawImages = st, true
	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindAssistant, Final: true,
		Images: []*rafikiv1.ImageBlock{img},
	}}, 1, 100), "\n")

	checkPixelRows(t, out, "▌ ", 32, 12)
}

// Before the thumbnail is ready (or when it failed), the image renders the
// one-line placeholder — never nothing, never half pixels.
func TestImageFallsBackBeforeTheThumbnailIsReady(t *testing.T) {
	st := newTestStore()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}
	st.lookup(img) // queued, not ready

	r := newRenderer()
	r.images, r.drawImages = st, true
	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindUser, Images: []*rafikiv1.ImageBlock{img},
	}}, 1, 100), "\n")

	c := assert.NewCollecting(t)
	c.Eq(1, strings.Count(out, "🖼"), "exactly one placeholder line:\n%s", out)
	c.False(strings.ContainsRune(out, kitty.Placeholder), "no pixel rune before the thumbnail is ready")
}

// Placeholder mode (images off) must not even QUEUE an image: drawing the
// one-line placeholder costs nothing and decodes nothing.
func TestImageFallsBackInPlaceholderModeWithoutQueueing(t *testing.T) {
	st := newTestStore()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}

	r := newRenderer()
	r.images = st // drawImages stays false
	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindUser, Images: []*rafikiv1.ImageBlock{img},
	}}, 1, 100), "\n")

	c := assert.NewCollecting(t)
	c.Eq(1, strings.Count(out, "🖼"), "the placeholder line:\n%s", out)
	c.False(strings.ContainsRune(out, kitty.Placeholder), "no pixel rune in placeholder mode")
	c.Len(st.takeQueued(false), 0, "placeholder mode must not queue the image for decoding")
}

// A pane too narrow for gutter+cols draws the placeholder instead of clipping
// pixel rows mid-image.
func TestImageFallsBackInANarrowPane(t *testing.T) {
	st, img := readyStore(t)
	r := newRenderer()
	r.images, r.drawImages = st, true

	c := assert.NewCollecting(t)
	c.Nil(r.imagePixels("▌ ", img), "gutter+32 cols is wider than 20")

	out := strings.Join(r.Lines([]session.Block{{
		Kind: session.KindUser, Images: []*rafikiv1.ImageBlock{img},
	}}, 1, 20), "\n")
	// The fallback line wraps at width 20, so the assertion is that the image
	// renders as THE PLACEHOLDER — named once, no pixel runes — rather than
	// as pixels.
	c.Eq(1, strings.Count(out, "🖼"), "the placeholder must still name the image:\n%s", out)
	c.False(strings.ContainsRune(out, kitty.Placeholder), "no pixel runes in a narrow pane")
}

// A model or tool that prints U+10EEEE must not draw a cell the terminal
// treats as an image placeholder; sanitizeControlChars maps it to U+FFFD.
func TestSanitizeMapsKittyPlaceholder(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{
			Name: "bash", HasResult: true,
			Result: "a\U0010EEEE\x1b[38;2;1;2;3mb",
		}},
	}}
	out := strings.Join(newRenderer().Lines(blocks, 1, 100), "\n")

	c := assert.NewCollecting(t)
	c.False(strings.ContainsRune(out, kitty.Placeholder), "a printed U+10EEEE must not become a pixel cell:\n%s", out)
	c.NotStrContains(out, "38;2;1;2;3", "the escape sequence must not reach the terminal")
	c.StrContains(out, "\uFFFD", "the placeholder maps to U+FFFD")
}

// ── cockpit plumbing ─────────────────────────────────────────────────────────

// ^Y toggles thumbnails and must invalidate every pane — a cached pane never
// re-renders otherwise, since a thumbnail's arrival changes no paneSig field.
func TestCockpitImagesToggleInvalidatesPanes(t *testing.T) {
	c := assert.NewCollecting(t)
	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	p := ck.pane("c_1")
	p.sigInit = true

	_, _ = ck.Update(newCtrlY())
	c.True(ck.imagesHidden, "^Y must hide the thumbnails")
	c.False(p.sigInit, "^Y must invalidate the pane")

	off := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "off"})
	off.pane("c_1")
	_, _ = off.Update(newCtrlY())
	c.False(off.imagesHidden, "a terminal without kitty support must not flip the toggle")
	c.StrContains(off.notice, "--images=kitty", "the notice must say how to force it")
}

// A thumbnail readied in kitty mode transmits immediately; in placeholder
// mode Update transmits nothing.
func TestCockpitImagesReadyTransmitsInKittyMode(t *testing.T) {
	c := assert.NewCollecting(t)
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}

	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	ck.images.lookup(img)
	q := ck.images.takeQueued(false)
	c.Require().Len(q, 1)
	msg := runThumb(t, q[0])
	_, cmd := ck.Update(msg)
	raw, ok := cmd().(tea.RawMsg)
	c.Require().True(ok, "kitty mode must transmit, got %T", cmd)
	c.Eq(msg.transmit, raw.Msg.(string))

	off := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "off"})
	off.images.lookup(img)
	q = off.images.takeQueued(false)
	c.Require().Len(q, 1)
	_, cmd = off.Update(runThumb(t, q[0]))
	c.Nil(cmd, "placeholder mode must not transmit")
}

// The tick drains the queue ONLY in kitty mode: placeholder mode never decodes.
func TestCockpitImagesTickDrainsOnlyInKittyMode(t *testing.T) {
	c := assert.NewCollecting(t)
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}

	off := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "off"})
	off.images.lookup(img)
	off.Update(tickMsg(time.Now()))
	c.Len(off.images.takeQueued(false), 1, "placeholder mode must not drain on a tick")

	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	ck.images.lookup(img)
	ck.Update(tickMsg(time.Now()))
	c.Len(ck.images.takeQueued(false), 0, "kitty mode drains the queue on a tick")
}

// A terminal whose kitty support resolves AFTER thumbnails were readied gets
// the backlog on the DA1 sentinel, and panes built meanwhile are invalidated.
func TestCockpitImagesLateCapabilitySendsTheBacklog(t *testing.T) {
	c := assert.NewCollecting(t)
	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "auto"})
	// A hand-built resolver: the test host's own environment may be tmux or
	// Apple Terminal, which auto mode refuses to query at all.
	ck.caps = newTermCaps("auto", func(string) string { return "" })

	// Ready one thumbnail while the capability is still unresolved.
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}
	ck.images.lookup(img)
	q := ck.images.takeQueued(false)
	c.Require().Len(q, 1)
	if !ck.images.ready(runThumb(t, q[0])) {
		t.Fatal("thumbnail did not become ready")
	}

	// A pane built before the capability resolved.
	p := ck.pane("c_1")
	p.sigInit = true

	_, _ = ck.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	_, _ = ck.Update(kittyOK())
	_, da1Cmd := ck.Update(tea.TerminalVersionMsg{Name: "iTerm2 3.7.3"})
	c.Nil(da1Cmd, "the version alone does not flip the mode")
	_, da1Cmd = ck.Update(uv.PrimaryDeviceAttributesEvent{62})
	raw, ok := da1Cmd().(tea.RawMsg)
	c.Require().True(ok, "the DA1 must send the backlog, got %T", da1Cmd)
	c.Eq(ck.images.transmits(), raw.Msg.(string))
	c.False(p.sigInit, "the capability change must invalidate panes built before it")
}

// retransmitCmd is both the backlog and ^L's recovery: full when there is
// something to re-send, nil when there is not or when the mode is placeholder.
func TestCockpitImagesRetransmitCmd(t *testing.T) {
	c := assert.NewCollecting(t)
	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	c.Nil(ck.retransmitCmd(), "no ready thumb, no retransmit")

	readyCockpitThumb(t, ck)
	raw, ok := ck.retransmitCmd()().(tea.RawMsg)
	c.Require().True(ok, "kitty mode with a ready thumb must retransmit")
	c.Eq(ck.images.transmits(), raw.Msg.(string))

	off := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "off"})
	readyCockpitThumb(t, off)
	c.Nil(off.retransmitCmd(), "placeholder mode never retransmits")
}

// ImageCleanup deletes every transmitted image by id; a cockpit that
// transmitted nothing cleans up nothing.
func TestCockpitImageCleanup(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("", NewCockpit(Options{BaseURL: "http://127.0.0.1:1"}).ImageCleanup(),
		"a fresh cockpit transmitted nothing")

	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	th := readyCockpitThumb(t, ck)
	out := ck.ImageCleanup()
	c.StrContains(out, "a=d", "cleanup:\n%s", out)
	c.StrContains(out, fmt.Sprintf("i=%d", th.id), "cleanup must name the thumb's id:\n%s", out)
}

// poisonText carries everything a transcript text path must not draw raw: a
// Kitty placeholder rune (would forge a pixel cell of a real thumbnail), an
// SGR escape, and a C1 control. After sanitizing the visible text is
// "pre\uFFFDpost"; every raw byte the terminal would act on is gone.
const poisonText = "pre\U0010EEEE\x1b[38;2;1;2;3mpost"

// assertNoForgedCells pins F1 on one rendered block: the text survives (the
// placeholder rune mapped to U+FFFD) but no pixel rune and no escape reaches
// the terminal.
func assertNoForgedCells(t *testing.T, out string) {
	t.Helper()
	c := assert.NewCollecting(t)
	c.StrContains(ansi.Strip(out), "pre\uFFFDpost", "the sanitized text must still render:\n%s", out)
	c.False(strings.ContainsRune(out, kitty.Placeholder), "a pixel rune must never be forged:\n%s", out)
	c.NotStrContains(out, "38;2;1;2;3", "an escape sequence must never reach the terminal:\n%s", out)
}

// TestSanitizeCoversEveryTranscriptTextPath pins F1: every renderer path that
// draws model- or tool-controlled TEXT — pending-user, user, system, a tool
// call's NAME (in each of its four states) and a stop reason — passes through
// sanitizeControlChars. imagePixels' rows are the ONE intentional bypass.
func TestSanitizeCoversEveryTranscriptTextPath(t *testing.T) {
	r := newRenderer()
	r.width = 100

	// User and pending-user and system text.
	assertNoForgedCells(t, r.renderBlock(session.Block{Kind: session.KindPendingUser, Text: poisonText}))
	assertNoForgedCells(t, r.renderBlock(session.Block{Kind: session.KindUser, Text: poisonText}))
	assertNoForgedCells(t, r.renderBlock(session.Block{Kind: session.KindSystem, Text: poisonText}))

	// A tool call's NAME, in each of its four states.
	states := []session.ToolCall{
		{Name: poisonText, Running: true},
		{Name: poisonText, IsError: true, HasResult: true, Result: "boom"},
		{Name: poisonText, HasResult: true, Result: "ok"},
		{Name: poisonText},
	}
	for _, tc := range states {
		out := r.renderBlock(session.Block{Kind: session.KindAssistant, Final: true,
			ToolCalls: []session.ToolCall{tc}})
		assertNoForgedCells(t, out)
	}

	// The stop reason.
	assertNoForgedCells(t, r.renderBlock(session.Block{
		Kind: session.KindAssistant, Final: true, StopReason: poisonText}))
}

// TestSanitizeDropsC1Controls pins F4: UTF-8-encoded C1 controls
// (U+0080–U+009F) are control sequences like any ESC byte and must not reach
// the terminal; U+00A0 and above are real text and must survive.
func TestSanitizeDropsC1Controls(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("ab", sanitizeControlChars("a\u009bb"), "U+009B (CSI) dropped")
	c.Eq("ab", sanitizeControlChars("a\u0085b"), "U+0085 (NEL) dropped")
	c.Eq("a\u00a0b", sanitizeControlChars("a\u00a0b"), "U+00A0 is text, not control")
}

// TestCockpitImagesReadinessInvalidatesOnTick pins the coalescing contract:
// a ready thumbnail only SETS imagesDirty (panes stay valid), and the next
// tickMsg is what drains the queue, clears the flag and invalidates.
func TestCockpitImagesReadinessInvalidatesOnTick(t *testing.T) {
	c := assert.NewCollecting(t)
	ck := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", Images: "kitty"})
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 400, 300)}
	ck.images.lookup(img)
	q := ck.images.takeQueued(false)
	c.Require().Len(q, 1)
	msg := runThumb(t, q[0])

	p := ck.pane("c_1")
	p.sigInit = true

	_, _ = ck.Update(msg)
	c.True(ck.imagesDirty, "a ready thumbnail marks the panes dirty")
	c.True(p.sigInit, "readiness is coalesced: the ready message alone must not invalidate")

	_, _ = ck.Update(tickMsg(time.Now()))
	c.False(ck.imagesDirty, "the tick consumed the dirty flag")
	c.False(p.sigInit, "the tick must invalidate every pane")
}
