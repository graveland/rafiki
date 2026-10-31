// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// newStoredConversation builds a Conversation over convID in cl's conversation
// store. It is the seam a test needs when the conversation already exists in a
// backend the client did not create: Conversation() mints its own id.
func newStoredConversation(cl *Client, convID string) *Conversation {
	return &Conversation{
		client: cl,
		ID:     convID,
		cfg:    convConfig{model: "claude-haiku-4-5", maxTokens: 16384, trim: defaultTrimPolicy{}, authorKind: "agent"},
	}
}

// TestConversationHistoryHitsCacheBetweenIterations pins the second half of the
// loop's cost: the first iteration populates the store's cache, and the next
// one reads the working set entirely out of it. A backend call here would mean
// every tool iteration re-walked and re-fetched the conversation.
func TestConversationHistoryHitsCacheBetweenIterations(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondUsage(10, 5, 0, 0, "first"),
		respondUsage(10, 5, 0, 0, "second"),
	}}
	mem := convpkg.NewMemBackend()
	mem.Ensure("conv-cache")
	counting := convtest.NewCounting(mem)
	cl, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
		WithConvStore(convpkg.NewStore(counting, convpkg.Options{})),
	)
	c.NoError(err)
	conv := newStoredConversation(cl, "conv-cache")

	if _, err := conv.Send(ctx, UserText("one")); err != nil {
		t.Fatalf("first iteration: %v", err)
	}
	counting.Reset()
	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("second iteration: %v", err)
	}
	counts := counting.Counts()
	c.Eq(0, counts["Walk"], "the second iteration walked the backend")
	c.Eq(0, counts["Fetch"], "the second iteration fetched nodes")
}

// TestNewClientWithConvStoreBuildsNoSecondStore pins the daemon's one-store
// rule at the client: WithConvStore must leave exactly the caller's store
// behind. The capture store used to build one of its own even when the caller
// shared theirs, which cached the same nodes twice and left the shared cache
// blind to everything capture wrote.
func TestNewClientWithConvStoreBuildsNoSecondStore(t *testing.T) {
	c := assert.NewAborting(t)
	// Parsed, never connected: NewClient only stores the pool, and capture's
	// construction queries nothing.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:secret@127.0.0.1:1/conv_store_test")
	c.NoError(err, "pgxpool.New parses the DSN without dialling")
	t.Cleanup(pool.Close)

	shared := convpkg.NewStore(convpkg.NewMemBackend(), convpkg.Options{})
	cl, err := NewClient(
		WithStore(pool),
		WithConvStore(shared),
		WithLogger(testLogger(t)),
	)
	c.NoError(err, "NewClient")
	c.Eq(shared, cl.convs, "the client keeps the caller's store")
	c.NotNil(cl.capture, "a pool builds the capture store")
	c.Eq(shared, cl.capture.ConvStore(), "capture shares it — a private one would be a second cache")
}

// TestWithConvStoreRefusesNil pins the other half of the one-store rule: nil
// through the option is a programming error, not "use the default". Accepting
// it would install nothing, and the client's private fallback would quietly
// become a SECOND cache over the same rows — the split the shared store exists
// to prevent. A client built WITHOUT the option still gets its own store, which
// is the standalone `rafikid agent` path and must keep working.
func TestWithConvStoreRefusesNil(t *testing.T) {
	c := assert.NewAborting(t)

	recovered := c.Panics(func() { _, _ = NewClient(WithConvStore(nil)) },
		"NewClient(WithConvStore(nil)) is refused")
	c.Eq("llm: WithConvStore(nil)", fmt.Sprint(recovered), "the panic names the option")

	cl, err := NewClient(WithLogger(testLogger(t)), WithDefaultModel("claude-haiku-4-5"))
	c.NoError(err, "NewClient with no conv store option")
	c.NotNil(cl.convs, "a client built without the option still has its own store")

	conv, err := cl.Conversation(context.Background(), NewConversation("", "agent"))
	c.NoError(err, "Conversation")
	_, err = conv.AppendUser(context.Background(), UserText("hi"))
	c.NoError(err, "the private default store accepts a write")
}

// leaseLostBackend is a conversation backend whose writes always report the
// lease as lost: the state a writer is in once another daemon takes over.
type leaseLostBackend struct{ convpkg.Backend }

func (b leaseLostBackend) Commit(context.Context, convpkg.Commit) (time.Time, error) {
	return time.Time{}, convpkg.ErrLeaseLost
}

// TestConversationLeaseLostStopsWriting pins the fence's error contract: a lost
// lease is surfaced as store.ErrLeaseLost, never retried and never softened
// into a generic append failure.
func TestConversationLeaseLostStopsWriting(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	mem := convpkg.NewMemBackend()
	mem.Ensure("conv-lease")
	cl, err := NewClient(
		WithProviderSender("anthropic", &scriptedSender{}),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
		WithConvStore(convpkg.NewStore(leaseLostBackend{Backend: mem}, convpkg.Options{})),
	)
	c.NoError(err)
	conv := newStoredConversation(cl, "conv-lease")

	_, err = conv.Send(ctx, UserText("hi"))
	c.True(errors.Is(err, store.ErrLeaseLost), "Send must surface the lost lease, got %v", err)
}
