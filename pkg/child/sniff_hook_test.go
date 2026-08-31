package child

import (
	"sync"
	"testing"
)

// The sniff hook must fire for a frame that carries metadata, so a caller can
// persist the session id without waiting for a bus frame the child may never
// emit. claude's system/init is exactly that case.
func TestOnMetaFiresWhenMetadataIsSniffed(t *testing.T) {
	var mu sync.Mutex
	var got []SnifferMetadata

	c := &Child{
		ID:       "c_test",
		provider: newClaudeProvider(),
		sm:       NewStateMachine(),
		idle:     make(chan struct{}),
		onMeta: func(md SnifferMetadata) {
			mu.Lock()
			got = append(got, md)
			mu.Unlock()
		},
	}

	c.handleFrame([]byte(`{"type":"system","subtype":"init","session_id":"sess-abc","model":"claude-opus-5"}`))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("onMeta fired %d times, want 1", len(got))
	}
	if got[0].SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q, want %q", got[0].SessionID, "sess-abc")
	}
}

// A frame with no metadata must not fire the hook: a caller that persists on
// every call would write the store once per stdout line.
func TestOnMetaSilentWithoutMetadata(t *testing.T) {
	fired := 0
	c := &Child{
		ID:       "c_test",
		provider: newClaudeProvider(),
		sm:       NewStateMachine(),
		idle:     make(chan struct{}),
		onMeta:   func(SnifferMetadata) { fired++ },
	}

	c.handleFrame([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`))

	if fired != 0 {
		t.Fatalf("onMeta fired %d times, want 0", fired)
	}
}

// A nil hook is the normal case for pi and fundi children and must not panic.
func TestOnMetaNilIsSafe(t *testing.T) {
	c := &Child{ID: "c_test", provider: newClaudeProvider(), sm: NewStateMachine(), idle: make(chan struct{})}
	c.handleFrame([]byte(`{"type":"system","subtype":"init","session_id":"sess-abc"}`))
}
