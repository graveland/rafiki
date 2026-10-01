// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// pngOf builds a w×h PNG whose pixels are deterministic PCG noise, so its
// compressed size stays close to raw and a thumbnail of it exercises Kitty's
// chunked transmission. A non-zero tag recolours one pixel, minting an image
// with content distinct from every other same-size call.
func pngOf(t *testing.T, w, h int, tag ...uint64) []byte {
	t.Helper()
	seed := uint64(1)
	if len(tag) > 0 {
		seed = tag[0]
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{
				R: byte(rng.Uint32()), G: byte(rng.Uint32() >> 8),
				B: byte(rng.Uint32() >> 16), A: 255,
			})
		}
	}
	if len(tag) > 0 {
		img.SetNRGBA(0, 0, color.NRGBA{R: byte(tag[0]), G: byte(tag[0] >> 8), A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func newTestStore() *imageStore {
	return newImageStore(rand.New(rand.NewPCG(1, 2)))
}

// runThumb runs one queued image through thumbCmd at the default 8×16 cell.
func runThumb(t *testing.T, q queuedImage) thumbReadyMsg {
	t.Helper()
	msg, ok := thumbCmd(q, 8, 16)().(thumbReadyMsg)
	if !ok {
		t.Fatalf("thumbCmd returned %T, want thumbReadyMsg", msg)
	}
	return msg
}

// thumbThrough takes a pending image through the whole pipeline: queue it,
// thumbnail it, ready it. It fails the test if lookup already reports the
// thumb ready.
func thumbThrough(t *testing.T, s *imageStore, img *rafikiv1.ImageBlock, smallIDs bool) thumbReadyMsg {
	t.Helper()
	if s.lookup(img) != nil {
		t.Fatal("lookup returned a thumb before any thumbnail was ready")
	}
	q := s.takeQueued(smallIDs)
	if len(q) != 1 {
		t.Fatalf("takeQueued returned %d items, want 1", len(q))
	}
	msg := runThumb(t, q[0])
	s.ready(msg)
	return msg
}

// splitAPCs splits a transmit string into its APC sequences and returns each
// one's control data (between "\x1b_G" and ";") and payload (between ";" and
// "\x1b\\"). Payloads are base64, so no ';' can appear inside either part; a
// sequence with no payload (a delete) has no ';' at all.
func splitAPCs(t *testing.T, transmit string) (ctrls, payloads []string) {
	t.Helper()
	for _, part := range strings.Split(transmit, "\x1b\\") {
		if part == "" {
			continue
		}
		rest, ok := strings.CutPrefix(part, "\x1b_G")
		if !ok {
			t.Fatalf("APC chunk does not start with \\x1b_G: %q", part)
		}
		ctrl, payload, found := strings.Cut(rest, ";")
		if !found {
			payload = "" // payload-less control, e.g. a delete sequence
		}
		ctrls = append(ctrls, ctrl)
		payloads = append(payloads, payload)
	}
	return ctrls, payloads
}

func TestKittyPlaceholderRowsTruecolor(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := placeholderRows(&thumb{id: 0x123456, cols: 3, rows: 2}, false)
	c.Len(rows, 2)
	for i, row := range rows {
		c.True(strings.HasPrefix(row, "\x1b[38;2;18;52;86m"), "row %d lacks the truecolor foreground: %q", i, row)
		c.True(strings.HasSuffix(row, "\x1b[39m"), "row %d lacks the foreground reset: %q", i, row)
		c.Eq(3, ansi.StringWidth(row), "row %d renders as %d cells", i, ansi.StringWidth(row))
	}
	// Row 1's second cell: placeholder, row diacritic, column diacritic,
	// id-high-byte diacritic (0x123456 >> 24 == 0).
	cell := []rune(strings.TrimSuffix(strings.TrimPrefix(rows[1], "\x1b[38;2;18;52;86m"), "\x1b[39m"))[4:8]
	c.Eq(kitty.Placeholder, cell[0], "second cell of row 1: rune 0")
	c.Eq(kitty.Diacritic(1), cell[1], "second cell of row 1: row diacritic")
	c.Eq(kitty.Diacritic(1), cell[2], "second cell of row 1: column diacritic")
	c.Eq(kitty.Diacritic(0), cell[3], "second cell of row 1: id-byte diacritic")
}

func TestKittyPlaceholderRowsSmallIDs(t *testing.T) {
	rows := placeholderRows(&thumb{id: 7, cols: 1, rows: 1}, true)
	assert.NewCollecting(t).True(strings.HasPrefix(rows[0], "\x1b[38;5;7m"),
		"row lacks the 256-colour foreground: %q", rows[0])
}

func TestKittyEveryCellCarriesThreeDiacritics(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := placeholderRows(&thumb{id: 0x2a2b2c00, cols: 5, rows: 3}, false)
	for i, row := range rows {
		var placeholders, diacritics int
		for _, r := range ansi.Strip(row) {
			if r == kitty.Placeholder {
				placeholders++
			} else {
				diacritics++
			}
		}
		c.Eq(5, placeholders, "row %d: %d placeholders", i, placeholders)
		c.Eq(15, diacritics, "row %d: %d diacritics, want 5 cells × 3", i, diacritics)
	}
}

func TestKittyLookupQueuesOncePerContent(t *testing.T) {
	s := newTestStore()
	data := pngOf(t, 4, 4)
	a := &rafikiv1.ImageBlock{MediaType: "image/png", Data: data}
	b := &rafikiv1.ImageBlock{MediaType: "image/png", Data: data}

	// Lookup returns nil while the thumbnail is pending, and the same bytes
	// under two different pointers queue ONCE.
	c := assert.NewCollecting(t)
	c.Nil(s.lookup(a))
	c.Nil(s.lookup(b))
	c.Nil(s.lookup(a))
	c.Nil(s.lookup(b))
	c.Len(s.takeQueued(false), 1)

	// A later lookup of already-queued content queues nothing more.
	c.Nil(s.lookup(a))
	c.Nil(s.lookup(b))
	c.Len(s.takeQueued(false), 0)
}

func TestKittyIDsAreUniqueAndInRange(t *testing.T) {
	c := assert.NewCollecting(t)

	s := newTestStore()
	for i := range 1000 {
		s.lookup(&rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 4, 4, uint64(i)+1)})
	}
	ids := map[uint32]bool{}
	for _, q := range s.takeQueued(false) {
		c.True(q.id >= 256 && q.id < 1<<24, "id %d outside [256, 1<<24)", q.id)
		c.False(ids[q.id], "id %d handed out twice", q.id)
		ids[q.id] = true
	}
	c.Len(ids, 1000)

	small := newTestStore()
	for i := range 300 {
		small.lookup(&rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 4, 4, uint64(i)+1)})
	}
	smallIDs := map[uint32]bool{}
	for _, q := range small.takeQueued(true) {
		c.True(q.id >= 1 && q.id <= 255, "small id %d outside [1, 255]", q.id)
		smallIDs[q.id] = true
	}
	c.Len(smallIDs, 255, "255 distinct small ids, then reuse")
}

func TestKittyThumbCmdBuildsTransmitAndPlacement(t *testing.T) {
	c := assert.NewCollecting(t)
	q := queuedImage{hash: "h", id: 4242, data: pngOf(t, 400, 300)}
	msg := runThumb(t, q)
	c.Nil(msg.err)
	c.Eq(32, msg.cols, "400×300 into a 384×192 box at cell 8×16")
	c.Eq(12, msg.rows)

	ctrls, payloads := splitAPCs(t, msg.transmit)
	if len(ctrls) == 0 {
		t.Fatal("transmit carries no APC sequence")
	}
	want := []string{"a=T", "U=1", "f=100", "q=2", "i=4242", "c=32", "r=12"}
	first := strings.Split(ctrls[0], ",")
	for _, w := range want {
		c.Contains(first, w, "first APC control data %q", ctrls[0])
	}

	c.GreaterOrEqual(2, len(payloads), "a noisy 256×192 PNG should need several chunks")
	var b64 strings.Builder
	for i, p := range payloads {
		c.LessOrEqual(4096, len(p), "chunk %d payload is %d bytes", i, len(p))
		b64.WriteString(p)
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	c.NoError(err)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	c.NoError(err)
	c.Eq(256, cfg.Width, "decoded payload width")
	c.Eq(192, cfg.Height, "decoded payload height")
}

func TestKittyThumbCmdReportsUndecodableBytes(t *testing.T) {
	c := assert.NewCollecting(t)
	s := newTestStore()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte("garbage")}

	c.Nil(s.lookup(img))
	q := s.takeQueued(false)
	c.Len(q, 1)
	msg := runThumb(t, q[0])
	c.Error(msg.err)
	c.False(s.ready(msg), "a failed thumbnail is not usable")
	c.False(s.ready(thumbReadyMsg{hash: "unknown"}), "an unknown hash is not usable")

	// The failed image renders the placeholder forever: never re-queued.
	c.Nil(s.lookup(img))
	c.Len(s.takeQueued(false), 0)
}

func TestKittyReadyThenLookupReturnsTheThumb(t *testing.T) {
	s := newTestStore()
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 64, 48)}
	msg := thumbThrough(t, s, img, false)

	tw := s.lookup(img)
	if tw == nil {
		t.Fatal("lookup returned nil for a ready thumb")
	}
	c := assert.NewCollecting(t)
	c.Eq(msg.cols, tw.cols)
	c.Eq(msg.rows, tw.rows)
	c.Eq(thumbReady, tw.state)
	c.Eq(msg.transmit, s.transmits(), "transmits() is the ready thumb's sequence")
}

func TestKittyTransmitsAreDeterministic(t *testing.T) {
	img1 := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 64, 48)}
	img2 := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 48, 64)}

	// Both stores seed alike and queue in the same order, so every image gets
	// the same id in both; only the order the thumbs were READIED in differs.
	a := newTestStore()
	a.lookup(img1)
	a.lookup(img2)
	qa := a.takeQueued(false)
	if len(qa) != 2 {
		t.Fatalf("takeQueued returned %d items, want 2", len(qa))
	}
	ma1, ma2 := runThumb(t, qa[0]), runThumb(t, qa[1])
	a.ready(ma2) // reverse order
	a.ready(ma1)

	b := newTestStore()
	b.lookup(img1)
	b.lookup(img2)
	qb := b.takeQueued(false)
	if len(qb) != 2 {
		t.Fatalf("takeQueued returned %d items, want 2", len(qb))
	}
	b.ready(runThumb(t, qb[0])) // forward order
	b.ready(runThumb(t, qb[1]))

	assert.NewCollecting(t).Eq(a.transmits(), b.transmits(),
		"readying in either order must produce the same transmit string")
}

func TestKittyCleanupDeletesEveryTransmittedID(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("", newTestStore().cleanup())

	s := newTestStore()
	img1 := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 64, 48)}
	img2 := &rafikiv1.ImageBlock{MediaType: "image/png", Data: pngOf(t, 48, 64)}
	thumbThrough(t, s, img1, false)
	thumbThrough(t, s, img2, false)
	id1, id2 := s.lookup(img1).id, s.lookup(img2).id

	ctrls, _ := splitAPCs(t, s.cleanup())
	c.Len(ctrls, 2, "one delete sequence per ready thumb")
	deleted := map[uint32]bool{}
	for i, ctrl := range ctrls {
		opts := strings.Split(ctrl, ",")
		c.Contains(opts, "a=d", "delete sequence %d control data %q", i, ctrl)
		c.Contains(opts, "d=I", "delete sequence %d control data %q", i, ctrl)
		var id uint32
		for _, o := range opts {
			if v, ok := strings.CutPrefix(o, "i="); ok {
				_, _ = fmt.Sscanf(v, "%d", &id)
			}
		}
		c.NotZero(id, "delete sequence %d carries no image id: %q", i, ctrl)
		deleted[id] = true
	}
	c.True(deleted[id1], "cleanup misses id %d: %v", id1, deleted)
	c.True(deleted[id2], "cleanup misses id %d: %v", id2, deleted)
}
