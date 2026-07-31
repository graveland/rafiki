package child

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/bus"
	"go.graveland.dev/rafiki/pkg/ring"
)

func newTestBus() *bus.Bus[[]byte] { return bus.New[[]byte](bus.Options{}) }

// writeSinkScript writes a runnable script that reads stdin until EOF and emits
// nothing. Enough to satisfy Spawn (which only needs a startable binary to
// allocate the render-ring); avoids depending on a provider-specific stdout.
func writeSinkScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "sink.sh")
	body := "#!/bin/bash\nwhile IFS= read -r line; do :; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write sink script: %v", err)
	}
	return script
}

func TestPublishBusCapturesToRenderRing(t *testing.T) {
	c := &Child{
		bus:        newTestBus(),
		renderRing: ring.New(ring.Options{}),
	}
	c.publishBus([]byte(`{"type":"message_start"}`), 1)
	c.publishBus([]byte(`{"type":"message_end"}`), 2)

	got := c.RenderRingSnapshot()
	if len(got) != 2 || string(got[0]) != `{"type":"message_start"}` || string(got[1]) != `{"type":"message_end"}` {
		t.Fatalf("render-ring = %v, want the two published frames in order", got)
	}
}

func TestRenderRingSnapshotNilWhenAbsent(t *testing.T) {
	c := &Child{bus: newTestBus()}

	sub, cancel := c.bus.Subscribe()
	defer cancel()

	c.publishBus([]byte(`{"type":"x"}`), 1)

	if c.RenderRingSnapshot() != nil {
		t.Fatal("RenderRingSnapshot() should be nil when renderRing is unset")
	}

	// The nil-render path must still forward to the bus.
	select {
	case got := <-sub:
		if string(got) != `{"type":"x"}` {
			t.Fatalf("bus frame = %q, want %q", got, `{"type":"x"}`)
		}
	case <-time.After(time.Second):
		t.Fatal("publishBus did not forward the frame to the bus on the nil-render path")
	}
}

// TestSpawnGatesRenderRingOnNormalizes exercises the real Spawn gate: a
// normalizing provider (claude) gets a render-ring allocated; a non-normalizing
// provider (pi, the default) does not.
func TestSpawnGatesRenderRingOnNormalizes(t *testing.T) {
	bin := writeSinkScript(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("claude allocates render-ring", func(t *testing.T) {
		c, err := Spawn(context.Background(), SpawnSpec{
			ChildID:  "c_claude",
			Cwd:      cwd,
			PiBinary: bin,
			Provider: ClaudeProvider{},
		})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

		if !c.Normalizes() {
			t.Fatal("claude child should normalize")
		}
		if c.RenderRingSnapshot() == nil {
			t.Fatal("claude child should have a render-ring (RenderRingSnapshot() != nil)")
		}
	})

	t.Run("pi has no render-ring", func(t *testing.T) {
		c, err := Spawn(context.Background(), SpawnSpec{
			ChildID:  "c_pi",
			Cwd:      cwd,
			PiBinary: bin,
		})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

		if c.Normalizes() {
			t.Fatal("pi child should not normalize")
		}
		if c.RenderRingSnapshot() != nil {
			t.Fatal("pi child should have no render-ring (RenderRingSnapshot() == nil)")
		}
	})
}
