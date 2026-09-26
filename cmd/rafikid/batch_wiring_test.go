package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/users"
)

// openrouterSet returns a provider registry whose openrouter entry carries the
// given kind and key env var — newBatcher's provider-side cases in one builder.
func openrouterSet(kind providers.Kind, keyEnv string) *providers.Set {
	return &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic": {Name: "anthropic", Kind: providers.KindAnthropic, APIKeyEnv: "ANTHROPIC_API_KEY"},
			"openrouter": {
				Name:      "openrouter",
				Kind:      kind,
				APIKeyEnv: keyEnv,
			},
		},
	}
}

// testPool is a parsed-but-never-connected pool: pgxpool.New only parses the
// DSN (Ping is what proves reachability), which is all newBatcher needs — it
// hands the pool to batchdb.New without querying.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://batch:test@127.0.0.1:1/batch_wiring_test")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestNewBatcherNeedsOpenRouterKey(t *testing.T) {
	t.Run("nil pool refuses even with a perfect provider", func(t *testing.T) {
		t.Setenv("BATCH_TEST_OPENROUTER_KEY", "sk-batch-test")
		b, ok := newBatcher(openrouterSet(providers.KindAnthropicOpenRouter, "BATCH_TEST_OPENROUTER_KEY"), nil)
		if ok || b != nil {
			t.Errorf("newBatcher(nil pool) = (%v, %v), want (nil, false)", b, ok)
		}
	})

	t.Run("no openrouter provider", func(t *testing.T) {
		set := &providers.Set{
			DefaultProvider: "anthropic",
			Providers: map[string]providers.Provider{
				"anthropic": {Name: "anthropic", Kind: providers.KindAnthropic, APIKeyEnv: "ANTHROPIC_API_KEY"},
			},
		}
		b, ok := newBatcher(set, testPool(t))
		if ok || b != nil {
			t.Errorf("newBatcher(no openrouter) = (%v, %v), want (nil, false)", b, ok)
		}
	})

	t.Run("wrong kind", func(t *testing.T) {
		t.Setenv("BATCH_TEST_OPENROUTER_KEY", "sk-batch-test")
		b, ok := newBatcher(openrouterSet(providers.KindAnthropic, "BATCH_TEST_OPENROUTER_KEY"), testPool(t))
		if ok || b != nil {
			t.Errorf("newBatcher(wrong kind) = (%v, %v), want (nil, false)", b, ok)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		// Deliberately NOT set: the refusal case.
		set := openrouterSet(providers.KindAnthropicOpenRouter, "BATCH_TEST_OPENROUTER_KEY_UNSET")
		b, ok := newBatcher(set, testPool(t))
		if ok || b != nil {
			t.Errorf("newBatcher(empty key) = (%v, %v), want (nil, false)", b, ok)
		}
	})

	t.Run("fully-qualified provider builds a batcher", func(t *testing.T) {
		t.Setenv("BATCH_TEST_OPENROUTER_KEY", "sk-batch-test")
		b, ok := newBatcher(openrouterSet(providers.KindAnthropicOpenRouter, "BATCH_TEST_OPENROUTER_KEY"), testPool(t))
		if !ok || b == nil {
			t.Fatalf("newBatcher(qualified provider, pool) = (%v, %v), want non-nil, true", b, ok)
		}
	})
}

// TestSetBatcherNilIsRefused pins the typed-nil trap: SetBatcher must leave
// the controller's batcher field nil when handed a nil *batch.Batcher, so
// agentRuntimeOptions' ro.Batcher ends up a true nil llm.Batcher. Storing the
// typed nil would widen it into a NON-nil interface value and make pkg/llm
// nil-panic on the first :batch send.
func TestSetBatcherNilIsRefused(t *testing.T) {
	c := newTestController(t)

	c.SetBatcher(nil)
	if c.batcher != nil {
		t.Fatalf("SetBatcher(nil) stored a non-nil batcher: %#v", c.batcher)
	}

	ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}, "c_batch_nil", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Batcher != nil {
		t.Errorf("ro.Batcher = %#v, want a true nil llm.Batcher after SetBatcher(nil)", ro.Batcher)
	}
}

// TestSetBatcherReachesRuntimeOptions proves the happy wiring end to end at
// the unit level: SetBatcher(b) → agentRuntimeOptions → ro.Batcher != nil.
func TestSetBatcherReachesRuntimeOptions(t *testing.T) {
	c := newTestController(t)
	b := batch.New(nil, nil, batch.Options{}) // store-less batcher: nothing Parks in a unit test
	c.SetBatcher(b)

	ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}, "c_batch_ok", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Batcher == nil {
		t.Fatal("ro.Batcher = nil, want the SetBatcher'd batcher")
	}
}

// newBatchSpawnController is a Controller with temp dirs and no children —
// enough to reach Spawn's request-validation block, which is all these two
// tests exercise.
func newBatchSpawnController(t *testing.T) *Controller {
	t.Helper()
	dir := testSocketDir(t)
	return NewController(
		childstore.New(), filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, nil, nil, false, t.Context(),
		nil, nil, nil, nil,
	)
}

// TestSpawnRefusesBatchModelWithAPIKey pins the Spawn-side refusal: a :batch
// model is submitted with the daemon's OpenRouter key, so a spawn carrying its
// own per-spawn key must be refused before anything is minted.
func TestSpawnRefusesBatchModelWithAPIKey(t *testing.T) {
	ctrl := newBatchSpawnController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.ShutdownAllChildren(ctx, time.Second, time.Second)
	})

	_, err := ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Cwd:    t.TempDir(),
		Kind:   protocol.KindFundi,
		Model:  "openrouter/z-ai/glm-5.3-flash:batch",
		APIKey: "sk-caller",
	}, users.Identity{})
	if err == nil {
		t.Fatal("Spawn(:batch model with APIKey) succeeded, want refusal")
	}
	want := ":batch models are submitted with the daemon's OpenRouter key"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("error = %q, want it to contain %q", got, want)
	}
}

// TestSpawnAllowsBatchModelWithoutAPIKey proves the refusal is keyed on
// APIKey specifically: the same :batch model with no per-spawn key must not
// trip the API-key message. It may still fail later on this bare controller
// (no fake binary wired) — that failure is not this test's concern.
func TestSpawnAllowsBatchModelWithoutAPIKey(t *testing.T) {
	ctrl := newBatchSpawnController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.ShutdownAllChildren(ctx, time.Second, time.Second)
	})

	_, err := ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Cwd:   t.TempDir(),
		Kind:  protocol.KindFundi,
		Model: "openrouter/z-ai/glm-5.3-flash:batch",
	}, users.Identity{})
	if err != nil && strings.Contains(err.Error(), ":batch models are submitted") {
		t.Errorf("Spawn(:batch model without APIKey) refused with the API-key message: %v", err)
	}
}
