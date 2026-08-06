package main

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/ring"
)

// TestController_GetStreams_StoreMiss verifies that GetStreams returns a
// ControllerError with ErrChildNotFound when the child is absent from the store.
func TestController_GetStreams_StoreMiss(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	_, err := ctrl.GetStreams("does-not-exist", "all")
	if err == nil {
		t.Fatalf("expected error for missing child, got nil")
	}
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *control.ControllerError, got %T: %v", err, err)
	}
	if ce.Code != protocol.ErrChildNotFound {
		t.Fatalf("expected code %s, got %s", protocol.ErrChildNotFound, ce.Code)
	}
}

// TestController_GetStreams_StoreOnlyChild verifies the Alive:false path: a
// child present in the store but not registered in the child manager (e.g. one
// that has exited) returns Alive:false with no error and no stream data.
func TestController_GetStreams_StoreOnlyChild(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	ctrl.st.Insert(&childstore.Session{
		ChildID: "exited-child",
		Status:  protocol.StatusExited,
	})

	res, err := ctrl.GetStreams("exited-child", "all")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Alive {
		t.Fatalf("expected alive=false for store-only child, got %+v", res)
	}
	if res.In != nil || res.Err != nil {
		t.Fatalf("expected no stream data, got %+v", res)
	}
}

// TestGetRecentRenderedExited verifies the raw-vs-rendered selector on an
// exited claude child: raw reads ExitedRing, rendered reads ExitedRenderRing.
func TestGetRecentRenderedExited(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	ctrl.st.Insert(&childstore.Session{
		ChildID:          "c1",
		Kind:             "claude",
		Status:           protocol.StatusExited,
		ExitedRing:       []ring.Event{{Bytes: []byte(`{"type":"system"}`)}},
		ExitedRenderRing: []ring.Event{{Bytes: []byte(`{"type":"message_end"}`)}},
	})

	raw, err := ctrl.GetRecent("c1", control.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the raw frame", raw.Events)
	}

	rendered, err := ctrl.GetRecent("c1", control.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 1 || string(rendered.Events[0]) != `{"type":"message_end"}` {
		t.Fatalf("rendered events = %v, want the render frame", rendered.Events)
	}
}

// TestGetRecentRenderedExitedNoRenderData verifies a rendered request for an
// exited claude child with no render frames stays EMPTY rather than dumping
// raw stream-json into the rendered view (claude raw stdout is not renderable).
func TestGetRecentRenderedExitedNoRenderData(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	ctrl.st.Insert(&childstore.Session{
		ChildID:    "c2",
		Kind:       "claude",
		Status:     protocol.StatusExited,
		ExitedRing: []ring.Event{{Bytes: []byte(`{"type":"system"}`)}},
		// ExitedRenderRing intentionally empty; no logsDir dump.
	})

	rendered, err := ctrl.GetRecent("c2", control.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 0 {
		t.Fatalf("rendered events = %v, want zero (no raw fallback for claude)", rendered.Events)
	}

	raw, err := ctrl.GetRecent("c2", control.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the raw frame", raw.Events)
	}
}

// TestGetRecentDiskFallback exercises the orphan-after-restart path: the
// in-memory exit snapshots are empty, so GetRecent must backfill from the
// on-disk dump (out.jsonl.gz / render.jsonl.gz).
func TestGetRecentDiskFallback(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	dumper := persist.NewLogDumper(ctrl.logsDir, persist.ModeOnExit)
	out := [][]byte{[]byte(`{"type":"system"}`)}
	render := [][]byte{[]byte(`{"type":"message_end"}`)}
	if err := dumper.Dump("c1", nil, out, render, nil,
		persist.Meta{ChildID: "c1"}, persist.ExitInfo{}); err != nil {
		t.Fatalf("dump: %v", err)
	}

	ctrl.st.Insert(&childstore.Session{
		ChildID: "c1",
		Kind:    "claude",
		Status:  protocol.StatusExited,
		// ExitedRing / ExitedRenderRing intentionally empty (lost on restart).
	})

	raw, err := ctrl.GetRecent("c1", control.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the disk out frame", raw.Events)
	}

	rendered, err := ctrl.GetRecent("c1", control.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 1 || string(rendered.Events[0]) != `{"type":"message_end"}` {
		t.Fatalf("rendered events = %v, want the disk render frame", rendered.Events)
	}
}

// TestGetRecentDiskZeroTimestampSinceGuard verifies that disk-sourced frames
// (which carry no timestamp) survive a nonzero Since filter rather than being
// dropped wholesale.
func TestGetRecentDiskZeroTimestampSinceGuard(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	dumper := persist.NewLogDumper(ctrl.logsDir, persist.ModeOnExit)
	render := [][]byte{[]byte(`{"type":"message_end"}`)}
	if err := dumper.Dump("c1", nil, nil, render, nil,
		persist.Meta{ChildID: "c1"}, persist.ExitInfo{}); err != nil {
		t.Fatalf("dump: %v", err)
	}

	ctrl.st.Insert(&childstore.Session{
		ChildID: "c1",
		Kind:    "claude",
		Status:  protocol.StatusExited,
	})

	res, err := ctrl.GetRecent("c1", control.RecentQuery{Rendered: true, Since: 1716000000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || string(res.Events[0]) != `{"type":"message_end"}` {
		t.Fatalf("events = %v, want the zero-TS disk frame kept despite Since", res.Events)
	}
}

// TestGetRecentByteBudget verifies GetRecent trims oldest events so the
// response payload never exceeds the per-frame byte budget: every client
// reads responses through a protocol.MaxFrameBytes-capped frame reader, so
// an unbounded history dump would kill the connection (frame too large).
func TestGetRecentByteBudget(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	// 5 events of ~3 MiB each (~15 MiB total) — past the 8 MiB budget.
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = 'a'
	}
	events := make([]ring.Event, 5)
	for i := range events {
		events[i] = ring.Event{
			Bytes:     []byte(`{"type":"system","seq":` + string(rune('0'+i)) + `,"pad":"` + string(big) + `"}`),
			Timestamp: int64(i + 1),
		}
	}
	ctrl.st.Insert(&childstore.Session{
		ChildID:    "c1",
		Kind:       "pi",
		Status:     protocol.StatusExited,
		ExitedRing: events,
	})

	res, err := ctrl.GetRecent("c1", control.RecentQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TruncatedBySize {
		t.Fatalf("TruncatedBySize = false, want true (payload exceeded budget)")
	}
	if len(res.Events) == 0 || len(res.Events) >= 5 {
		t.Fatalf("len(Events) = %d, want a newest-tail subset (0 < n < 5)", len(res.Events))
	}
	total := 0
	for _, ev := range res.Events {
		total += len(ev) + 1
	}
	if total > protocol.MaxFrameBytes/2 {
		t.Fatalf("payload = %d bytes, exceeds budget %d", total, protocol.MaxFrameBytes/2)
	}
	// Newest events must be the ones kept.
	last := res.Events[len(res.Events)-1]
	if string(last) != string(events[4].Bytes) {
		t.Fatalf("newest event not retained")
	}
	if res.TotalInBuffer != 5 {
		t.Fatalf("TotalInBuffer = %d, want 5", res.TotalInBuffer)
	}

	// A small history passes through untrimmed.
	ctrl.st.Insert(&childstore.Session{
		ChildID:    "c2",
		Kind:       "pi",
		Status:     protocol.StatusExited,
		ExitedRing: []ring.Event{{Bytes: []byte(`{"type":"system"}`), Timestamp: 1}},
	})
	small, err := ctrl.GetRecent("c2", control.RecentQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if small.TruncatedBySize || len(small.Events) != 1 {
		t.Fatalf("small history: TruncatedBySize=%v len=%d, want false/1", small.TruncatedBySize, len(small.Events))
	}
}

// TestGetRecentFundiDiskFallback verifies that a fundi child (PiProvider — no
// render ring) falls back to out.jsonl.gz when both in-memory snapshots are
// empty.  This is the post-restart recovery path: loadOrphans has no
// ExitedRing, ExitedRenderRing doesn't exist for fundi, and render.jsonl.gz
// is never written — only out.jsonl.gz survives.
func TestGetRecentFundiDiskFallback(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	dumper := persist.NewLogDumper(ctrl.logsDir, persist.ModeOnExit)
	out := [][]byte{[]byte(`{"type":"agent_end","messages":[{"role":"assistant"}]}`)}
	// render.jsonl.gz intentionally absent — PiProvider children never produce one.
	if err := dumper.Dump("f1", nil, out, nil, nil,
		persist.Meta{ChildID: "f1"}, persist.ExitInfo{}); err != nil {
		t.Fatalf("dump: %v", err)
	}

	ctrl.st.Insert(&childstore.Session{
		ChildID: "f1",
		Kind:    protocol.KindFundi,
		Status:  protocol.StatusExited,
		// ExitedRing / ExitedRenderRing intentionally empty (lost on restart).
	})

	// Rendered request: ExitedRenderRing nil → render.jsonl.gz absent →
	// fundi is not claude so falls through to ExitedRing → out.jsonl.gz.
	rendered, err := ctrl.GetRecent("f1", control.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 1 {
		t.Fatalf("rendered events = %v, want 1 event from out.jsonl.gz disk fallback", rendered.Events)
	}

	// Raw request: same chain, just without the render.jsonl.gz detour.
	raw, err := ctrl.GetRecent("f1", control.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != string(out[0]) {
		t.Fatalf("raw events = %v, want the disk out frame", raw.Events)
	}
}
