// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
	"weak"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/imagefit"
)

// A thumbnail is at most this many terminal cells, sized by the cell's pixel
// box; larger images downscale into it.
const (
	thumbMaxCols = 48
	thumbMaxRows = 12
)

type thumbState int

const (
	thumbPending thumbState = iota + 1 // queued or decoding; zero is "unknown"
	thumbReady
	thumbFailed
)

type thumb struct {
	state      thumbState
	id         uint32
	cols, rows int
}

// queuedImage is one image waiting for a thumbnail cmd. data is the image's
// bytes, shared with the proto message, never mutated.
type queuedImage struct {
	hash string
	id   uint32
	data []byte
}

// thumbReadyMsg carries a finished (or failed) thumbnail back to Update.
// transmit is the complete Kitty sequence: image data plus its one virtual
// placement.
type thumbReadyMsg struct {
	hash       string
	cols, rows int
	transmit   string
	err        error
}

// imageStore is shared by every pane's renderer. It is touched ONLY on the
// bubbletea goroutine (Update and View); thumbnail cmds receive a copy of
// what they need and report back through thumbReadyMsg.
type imageStore struct {
	// hashes memoizes content hashes by image pointer; a weak key lets an
	// evicted session's image bytes be collected.
	hashes    map[weak.Pointer[rafikiv1.ImageBlock]]string
	thumbs    map[string]*thumb
	sequences map[string]string // hash → transmit
	queue     []queuedImage
	used      map[uint32]bool
	nextSmall uint32 // next candidate id when running with small ids; 0 = not started
	rng       *rand.Rand
}

func newImageStore(rng *rand.Rand) *imageStore {
	if rng == nil {
		now := time.Now()
		rng = rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(now.Nanosecond())))
	}
	return &imageStore{
		hashes:    map[weak.Pointer[rafikiv1.ImageBlock]]string{},
		thumbs:    map[string]*thumb{},
		sequences: map[string]string{},
		used:      map[uint32]bool{},
		rng:       rng,
	}
}

// lookup returns the thumb for img once it is ready, else nil. The first
// sighting of a given content hash queues the image for thumbnailing; the
// same bytes under a different *ImageBlock pointer find the existing thumb
// and queue nothing.
func (s *imageStore) lookup(img *rafikiv1.ImageBlock) *thumb {
	if img == nil {
		return nil
	}
	key := weak.Make(img)
	hash, ok := s.hashes[key]
	if !ok {
		sum := sha256.Sum256(img.GetData())
		hash = hex.EncodeToString(sum[:])
		s.hashes[key] = hash
	}
	t, ok := s.thumbs[hash]
	if !ok {
		t = &thumb{state: thumbPending}
		s.thumbs[hash] = t
		s.queue = append(s.queue, queuedImage{hash: hash, data: img.GetData()})
	}
	if t.state != thumbReady {
		return nil
	}
	return t
}

// takeQueued hands out every queued image for thumbnailing, allocating each
// one a Kitty image id. Large ids are random in [256, 1<<24); small ids
// cycle through 1–255 and are only for terminals that repaint placeholders
// with a 256-colour foreground.
func (s *imageStore) takeQueued(smallIDs bool) []queuedImage {
	if len(s.queue) == 0 {
		return nil
	}
	items := s.queue
	s.queue = nil
	for i := range items {
		if smallIDs {
			items[i].id = s.nextSmallID()
		} else {
			items[i].id = s.nextLargeID()
		}
		s.used[items[i].id] = true
		if t, ok := s.thumbs[items[i].hash]; ok {
			t.id = items[i].id
		}
	}
	return items
}

func (s *imageStore) nextLargeID() uint32 {
	for {
		id := 256 + s.rng.Uint32N(1<<24-256)
		if !s.used[id] {
			return id
		}
	}
}

func (s *imageStore) nextSmallID() uint32 {
	if s.nextSmall == 0 {
		s.nextSmall = 1 + s.rng.Uint32N(255)
	}
	if !s.smallAllUsed() {
		for range 255 {
			id := s.nextSmall
			s.nextSmall = id%255 + 1
			if !s.used[id] {
				return id
			}
		}
	}
	// All 255 small ids are handed out: keep cycling through them — advance
	// the rotating next id and wrap 1→255 — rather than stalling on one. A
	// wrong image on screen is cosmetic.
	id := s.nextSmall
	s.nextSmall = id%255 + 1
	return id
}

// smallAllUsed reports whether every id in 1–255 has been handed out.
func (s *imageStore) smallAllUsed() bool {
	for id := uint32(1); id <= 255; id++ {
		if !s.used[id] {
			return false
		}
	}
	return true
}

// thumbCmd thumbnails one queued image off the UI goroutine and reports the
// complete Kitty transmit sequence back through thumbReadyMsg.
func thumbCmd(q queuedImage, cellW, cellH int) tea.Cmd {
	return func() tea.Msg {
		img, err := imagefit.Thumbnail(q.data, imagefit.Box{
			MaxWidth:  thumbMaxCols * cellW,
			MaxHeight: thumbMaxRows * cellH,
		})
		if err != nil {
			return thumbReadyMsg{hash: q.hash, err: err}
		}
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		cols := min(thumbMaxCols, max(1, (w+cellW-1)/cellW))
		rows := min(thumbMaxRows, max(1, (h+cellH-1)/cellH))
		var buf bytes.Buffer
		err = kitty.EncodeGraphics(&buf, img, &kitty.Options{
			ID:               int(q.id),
			Action:           kitty.TransmitAndPut,
			Transmission:     kitty.Direct,
			Format:           kitty.PNG,
			ImageWidth:       w,
			ImageHeight:      h,
			Columns:          cols,
			Rows:             rows,
			VirtualPlacement: true,
			Quiet:            2, // suppress every terminal response
			Chunk:            true,
		})
		if err != nil {
			return thumbReadyMsg{hash: q.hash, err: err}
		}
		return thumbReadyMsg{hash: q.hash, cols: cols, rows: rows, transmit: buf.String()}
	}
}

// ready folds a thumbnail result into the store. It reports whether the
// thumbnail is usable: a failed image stays failed — the placeholder renders
// forever and the image is never re-queued.
func (s *imageStore) ready(msg thumbReadyMsg) bool {
	t, ok := s.thumbs[msg.hash]
	if !ok {
		return false
	}
	if msg.err != nil {
		t.state = thumbFailed
		return false
	}
	t.state = thumbReady
	t.cols, t.rows = msg.cols, msg.rows
	s.sequences[msg.hash] = msg.transmit
	return true
}

// transmits concatenates every ready thumb's transmit sequence, ordered by
// hash. It is the backlog for a terminal whose kitty support arrived late
// and for redraws after ^L.
func (s *imageStore) transmits() string {
	if len(s.sequences) == 0 {
		return ""
	}
	hashes := make([]string, 0, len(s.sequences))
	for h := range s.sequences {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	var b strings.Builder
	for _, h := range hashes {
		b.WriteString(s.sequences[h])
	}
	return b.String()
}

// cleanup frees every transmitted image in the terminal. Uppercase d=I also
// frees the terminal's stored copy of the data, not just the placements.
func (s *imageStore) cleanup() string {
	ids := make([]int, 0, len(s.thumbs))
	for _, t := range s.thumbs {
		if t.state == thumbReady {
			ids = append(ids, int(t.id))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Ints(ids)
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(ansi.KittyGraphics(nil, "a=d", "d=I", fmt.Sprintf("i=%d", id), "q=2"))
	}
	return b.String()
}

// placeholderRows renders a ready thumb as t.rows rows of t.cols cells. Each
// cell is a Kitty placeholder whose diacritics carry the row, the column and
// the high byte of the image id; the foreground colour carries the rest of
// the id. ALL THREE diacritics go on EVERY cell, because ultraviolet repaints
// only changed cells (a repaint starting mid-row must not rely on the
// terminal inferring row/column from a left neighbour) and iTerm2 before
// 3.7.3 drew nothing for a three-byte id whose high-byte diacritic was
// omitted. No gutter: the renderer prefixes it.
func placeholderRows(t *thumb, smallIDs bool) []string {
	var fg string
	if smallIDs {
		fg = fmt.Sprintf("\x1b[38;5;%dm", t.id)
	} else {
		fg = fmt.Sprintf("\x1b[38;2;%d;%d;%dm", t.id>>16&0xff, t.id>>8&0xff, t.id&0xff)
	}
	idByte := int(t.id >> 24 & 0xff)
	rows := make([]string, t.rows)
	for r := range t.rows {
		var b strings.Builder
		b.Grow(t.cols*4 + 4 + len(fg))
		b.WriteString(fg)
		for c := range t.cols {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kitty.Diacritic(r))
			b.WriteRune(kitty.Diacritic(c))
			b.WriteRune(kitty.Diacritic(idByte))
		}
		b.WriteString("\x1b[39m")
		rows[r] = b.String()
	}
	return rows
}
