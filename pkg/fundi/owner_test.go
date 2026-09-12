package fundi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/providers"
)

// TestBuildEngineAttributesTheConversationToItsOwner pins the pipe this plan
// exists to unplug: fundi.Config.OwnerUserID must reach the conversation row.
//
// The wiring is invisible at the call site — llm.Entrypoint alone sets the
// entrypoint and leaves the owner empty, and llm.NewConversation is the only
// way cfg.ownerUserID is ever populated — so the only honest check is the row
// itself. BuildEngine resolves the conversation (and INSERTs it) before any
// turn runs, so the row is observable straight after it returns.
//
// The users row is a real row, not an id the test invented:
// conversations.conversation.owner_user_id carries an FK to
// conversations.users, so a synthetic id would fail the INSERT and read as a
// wiring failure when it is a fixture problem.
func TestBuildEngineAttributesTheConversationToItsOwner(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()

	userID := seedUser(t, pool, "owner-attr-it")

	cfg := Config{
		Model:       "anthropic/claude-x",
		Cwd:         t.TempDir(),
		Pool:        pool,
		FakeTurns:   writeFakeTurns(t, sampleEndTurn),
		Tools:       fakeToolSet{},
		Providers:   providers.Default(),
		OwnerUserID: userID,
	}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	eng, shutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		t.Fatalf("BuildEngine: %v", err)
	}
	defer shutdown()
	defer eng.Close()

	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_user_id::text FROM conversations.conversation WHERE id = $1`,
		eng.conv.ID).Scan(&owner); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if owner == nil || *owner != userID {
		t.Fatalf("conversation owner_user_id = %v, want %s — the owner never reached the row", owner, userID)
	}

	// The NewConversation swap must not have cost the entrypoint: it is
	// required (Conversation errors without one) and the fundi child is the
	// "agent" origin.
	var entrypoint string
	if err := pool.QueryRow(ctx,
		`SELECT origin_entrypoint FROM conversations.conversation WHERE id = $1`,
		eng.conv.ID).Scan(&entrypoint); err != nil {
		t.Fatalf("read origin_entrypoint: %v", err)
	}
	if entrypoint != "agent" {
		t.Fatalf("origin_entrypoint = %q, want %q — NewConversation replaced Entrypoint and lost it", entrypoint, "agent")
	}
}

// Empty stays valid and means unattributed, exactly as before: an anonymous
// spawn is a legitimate shape (the standalone `rafikid fundi` process, an
// unauthenticated MCP notify). This is the negative half of the contract — a
// regression that makes an empty owner an error or a guessed owner would show
// up here.
func TestBuildEngineLeavesAnEmptyOwnerUnattributed(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()

	cfg := Config{
		Model:     "anthropic/claude-x",
		Cwd:       t.TempDir(),
		Pool:      pool,
		FakeTurns: writeFakeTurns(t, sampleEndTurn),
		Tools:     fakeToolSet{},
		Providers: providers.Default(),
	}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	eng, shutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		t.Fatalf("BuildEngine: %v", err)
	}
	defer shutdown()
	defer eng.Close()

	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_user_id::text FROM conversations.conversation WHERE id = $1`,
		eng.conv.ID).Scan(&owner); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if owner != nil {
		t.Fatalf("owner_user_id = %q for an empty OwnerUserID, want NULL", *owner)
	}
}

// seedUser inserts one users row and returns its id. token_sha256 is UNIQUE,
// so the digest is minted per call.
func seedUser(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	digest := hex.EncodeToString(raw[:])
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256) VALUES ($1,$2) RETURNING id::text`,
		username, digest).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	return id
}
