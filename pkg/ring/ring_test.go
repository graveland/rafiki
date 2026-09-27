package ring_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/ring"

	"github.com/multigres/testkit/assert"
)

func TestRing_AppendAndRecent_InOrder(t *testing.T) {
	c := assert.NewAborting(t)
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := 0; i < 5; i++ {
		r.Append([]byte("event-"+string(rune('a'+i))), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	c.Len(got, 5, "got %d, want 5", len(got))
	for i, ev := range got {
		want := "event-" + string(rune('a'+i))
		c.Eq(want, string(ev.Bytes), "event %d: got %q, want", i, ev.Bytes)
		c.Eq(int64(i), ev.Timestamp, "event %d ts: got %d, want %d", i, ev.Timestamp, i)
	}
}

func TestRing_EvictsByEventCount(t *testing.T) {
	c := assert.NewAborting(t)
	r := ring.New(ring.Options{MaxEvents: 3, MaxBytes: 1 << 20})
	for i := 0; i < 5; i++ {
		r.Append([]byte{byte('a' + i)}, int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	c.Len(got, 3, "got %d, want 3", len(got))
	wantFirst := byte('c') // a, b evicted
	c.Eq(wantFirst, got[0].Bytes[0], "first: got %q, want", got[0].Bytes)
}

func TestRing_EvictsByByteSize(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 1000, MaxBytes: 30})
	for i := 0; i < 5; i++ {
		// each event is 10 bytes
		r.Append([]byte(strings.Repeat(string(rune('a'+i)), 10)), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	assert.NewAborting(t).Len(got, 3, "got %d, want 3 (kept under 30 bytes)", len(got))
}

func TestRing_RecentSince(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := int64(0); i < 10; i++ {
		r.Append([]byte("x"), i*100)
	}
	got := r.Recent(ring.Query{Since: 500})
	assert.NewAborting(t).Len(got, 5, "got %d events since 500, want 5", len(got))
}

func TestRing_RecentLimit(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := 0; i < 20; i++ {
		r.Append([]byte("x"), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 5})
	assert.NewAborting(t).Len(got, 5, "got %d, want 5", len(got))
	// Returns the *most recent* 5.
	if got[0].Timestamp != 15 || got[4].Timestamp != 19 {
		t.Fatalf("expected [15..19], got [%d..%d]", got[0].Timestamp, got[4].Timestamp)
	}
}
