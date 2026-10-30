// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
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
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
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
