package main

import (
	"errors"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/persist"
	"git.graveland.dev/brent/pi-controller/internal/ring"
	"git.graveland.dev/brent/pi-controller/internal/server"
	"git.graveland.dev/brent/pi-controller/internal/store"
	"git.graveland.dev/brent/pi-controller/protocol"
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
	var ce *server.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *server.ControllerError, got %T: %v", err, err)
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
	ctrl.st.Insert(&store.Session{
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
	ctrl.st.Insert(&store.Session{
		ChildID:          "c1",
		Kind:             "claude",
		Status:           protocol.StatusExited,
		ExitedRing:       []ring.Event{{Bytes: []byte(`{"type":"system"}`)}},
		ExitedRenderRing: []ring.Event{{Bytes: []byte(`{"type":"message_end"}`)}},
	})

	raw, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the raw frame", raw.Events)
	}

	rendered, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: true})
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
	ctrl.st.Insert(&store.Session{
		ChildID:    "c2",
		Kind:       "claude",
		Status:     protocol.StatusExited,
		ExitedRing: []ring.Event{{Bytes: []byte(`{"type":"system"}`)}},
		// ExitedRenderRing intentionally empty; no logsDir dump.
	})

	rendered, err := ctrl.GetRecent("c2", server.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 0 {
		t.Fatalf("rendered events = %v, want zero (no raw fallback for claude)", rendered.Events)
	}

	raw, err := ctrl.GetRecent("c2", server.RecentQuery{Rendered: false})
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

	ctrl.st.Insert(&store.Session{
		ChildID: "c1",
		Kind:    "claude",
		Status:  protocol.StatusExited,
		// ExitedRing / ExitedRenderRing intentionally empty (lost on restart).
	})

	raw, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the disk out frame", raw.Events)
	}

	rendered, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: true})
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

	ctrl.st.Insert(&store.Session{
		ChildID: "c1",
		Kind:    "claude",
		Status:  protocol.StatusExited,
	})

	res, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: true, Since: 1716000000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || string(res.Events[0]) != `{"type":"message_end"}` {
		t.Fatalf("events = %v, want the zero-TS disk frame kept despite Since", res.Events)
	}
}
