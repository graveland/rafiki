// SPDX-License-Identifier: Apache-2.0

package main

// Unit tests for the daemon's recall wiring: the caller binding (scope and
// memory-owner rules), the runtime assembly (the typed-nil trap), the context
// and tree renderers, and the summarizer completer's entrypoint contract.
// DB-backed coverage — the completer's capture entrypoint — uses scratchPool
// and skips without RAFIKI_TEST_DSN, like every other DB-gated test here.

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// fakeSender records the last request and answers with a fixed reply — the
// llm.Sender a test client routes through instead of a real provider.
type fakeSender struct {
	got   *anthropic.MessageNewParams
	reply *anthropic.Message
}

func (s *fakeSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	p := params
	s.got = &p
	if s.reply != nil {
		return s.reply, nil
	}
	return &anthropic.Message{}, nil
}

// fakeRecallStore answers only the calls a test wires; anything else panics
// through the embedded nil interface, which keeps the fake small.
type fakeRecallStore struct {
	recall.Store

	searchScope recall.Scope
	searchOwner string
	searchHits  []recall.Hit

	window   recall.Window
	messages []recall.Message
	msgFrom  int
	msgTo    int

	memories []recall.Memory
	putOwner string
}

func (f *fakeRecallStore) SearchBM25(ctx context.Context, q recall.SearchQuery, src recall.Source) ([]recall.Hit, error) {
	f.searchScope = q.Scope
	f.searchOwner = q.MemoryOwner
	// The same fail-closed contract recalldb implements: an invalid scope
	// admits nothing, before any other work.
	if !q.Scope.Valid() {
		return nil, recall.ErrInvalidScope
	}
	return f.searchHits, nil
}

func (f *fakeRecallStore) PutMemory(ctx context.Context, ownerUserID string, m recall.Memory) (recall.Memory, error) {
	f.putOwner = ownerUserID
	m.ID = "m-id"
	return m, nil
}

func (f *fakeRecallStore) Window(ctx context.Context, scope recall.Scope, id string) (recall.Window, error) {
	return f.window, nil
}

func (f *fakeRecallStore) Messages(ctx context.Context, scope recall.Scope, conversationID string, fromOrdinal, toOrdinal int) ([]recall.Message, error) {
	f.msgFrom = fromOrdinal
	f.msgTo = toOrdinal
	return f.messages, nil
}

func (f *fakeRecallStore) MemoryTree(ctx context.Context, ownerUserID, path string, depth int) ([]recall.Memory, error) {
	return f.memories, nil
}

// bindingWith wires a controller whose recall runtime is backed by st — the
// shape startRecall leaves behind on a daemon with a database.
func bindingWith(st recall.Store, owner users.Identity) *recallBinding {
	c := &Controller{recall: &recallRuntime{st: st}}
	return newRecallBinding(c, owner, false).(*recallBinding)
}

func TestRecallBindingScopeFromIdentity(t *testing.T) {
	c := assert.NewAborting(t)
	admin := bindingWith(&fakeRecallStore{}, users.Identity{UserID: "u-admin", IsAdmin: true})
	c.Eq((recall.Scope{All: true}), admin.scope, "admin scope")
	user := bindingWith(&fakeRecallStore{}, users.Identity{UserID: "u-bob"})
	c.Eq((recall.Scope{OwnerUserID: "u-bob"}), user.scope, "user scope")
	anon := bindingWith(&fakeRecallStore{}, users.Identity{})
	c.Eq((recall.Scope{}), anon.scope, "anonymous scope")

	// The zero scope must reach the store and be refused there — the binding
	// degrades exactly like newMCPConversationReader's deny-all Scope, and
	// recall.Search surfaces the store's ErrInvalidScope.
	fs := &fakeRecallStore{}
	_, err := bindingWith(fs, users.Identity{}).Recall(t.Context(), toolsRecallQuery("ghosts"))
	c.ErrorIs(err, recall.ErrInvalidScope, "zero-scope Recall error")
	c.Eq((recall.Scope{}), fs.searchScope, "store saw scope")
}

// toolsRecallQuery builds the tool-facing query shape the recall tool sends.
func toolsRecallQuery(text string) tools.RecallQuery {
	return tools.RecallQuery{Query: text}
}

func TestRecallBindingMemoryOwnerAlwaysCaller(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeRecallStore{}
	b := bindingWith(fs, users.Identity{UserID: "u-admin", IsAdmin: true})
	if _, err := b.Recall(t.Context(), toolsRecallQuery("x")); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	c.Eq("u-admin", fs.searchOwner, "search MemoryOwner")
	_, err := b.MemoryPut(t.Context(), "proj.r", "note", "body", nil)
	c.NoError(err, "MemoryPut")
	c.Eq("u-admin", fs.putOwner, "PutMemory owner")
}

func TestRecallBindingNilWhenDisabled(t *testing.T) {
	c := &Controller{} // a DB-less daemon: startRecall never ran
	rb := newRecallBinding(c, users.Identity{UserID: "u-alice"}, false)
	// The interface itself must be nil — a typed-nil *recallBinding here would
	// defeat every recall blueprint's decline.
	assert.NewAborting(t).Nil(rb, "newRecallBinding on a nil recall runtime")
}

// TestRecallWiring runs the wiring tests whose pinned names do not contain
// the verify pattern ('TestRecall|TestMCPFace' — an UNANCHORED substring
// match): TestStartRecall* and TestLLMCompleter* would otherwise silently
// skip inside the gate. Same shim rule as the other pinned-name suites; the
// subtests call the bodies directly.
func TestRecallWiring(t *testing.T) {
	t.Run("start-recall-no-config-leaves-interfaces-nil", TestStartRecallNoConfigLeavesInterfacesNil)
	t.Run("start-recall-no-pool-disables", TestStartRecallNoPoolDisables)
	t.Run("llm-completer-uses-summary-entrypoint", TestLLMCompleterUsesSummaryEntrypoint)
	t.Run("llm-completer-resolves-catalog-model", TestLLMCompleterResolvesCatalogModel)
	t.Run("llm-completer-completion-cost-fallback", TestLLMCompleterCompletionCostFallback)
}

func TestStartRecallNoConfigLeavesInterfacesNil(t *testing.T) {
	c := assert.NewAborting(t)
	rt := buildRecallRuntime(nil, &providers.Set{}, nil, discardLogger())
	c.NotNil(rt.st, "store must always be built when a pool exists")
	c.NotNil(rt.indexer, "indexer must always be built; it runs BM25-only without an embedder")
	c.Nil(rt.emb, "embedder")
	c.Nil(rt.sums, "summarizer")
	c.Eq("", rt.summaryModel, "summaryModel")

	// The positive branch: both configs present and a client to complete on
	// materialize both — proving the nil above is a branch decision, not an
	// accident of the constructor.
	sender := &fakeSender{}
	client, err := llm.NewClient(llm.WithProviderSender("anthropic", sender), llm.WithDefaultModel("claude-haiku-4-5"))
	c.NoError(err, "llm.NewClient")
	prov := &providers.Set{
		Embeddings: &providers.EmbeddingsConfig{URL: "http://127.0.0.1:1/v1/embeddings", Model: "emb-1", Dimensions: 8},
		Summaries:  &providers.SummariesConfig{Model: "claude-haiku-4-5", MaxSegmentTokens: 9000},
	}
	rt = buildRecallRuntime(nil, prov, client, discardLogger())
	c.NotNil(rt.emb, "[embeddings] config must build the embedder")
	c.NotNil(rt.sums, "[summaries] config with a client must build the summarizer")
	c.Eq("claude-haiku-4-5", rt.summaryModel, "summaryModel")

	// Summaries configured but no client: the completer is what the
	// summarizer drives, so without one the pass stays off — windowing and
	// memories are client-free and still run.
	rt = buildRecallRuntime(nil, prov, nil, discardLogger())
	c.Nil(rt.sums, "summarizer built without an llm client")
}

func TestStartRecallNoPoolDisables(t *testing.T) {
	c := &Controller{}
	startRecall(t.Context(), c, nil, &providers.Set{}, nil, discardLogger())
	assert.NewAborting(t).Nil(c.recall, "startRecall without a pool wired")
}

func TestRecallContextWindowRendersNeighbours(t *testing.T) {
	c := assert.NewCollecting(t)
	msg := func(ord int, role, text string) recall.Message {
		return recall.Message{
			ConversationID: "c1", Ordinal: ord, Role: role,
			Content: []byte(`[{"type":"text","text":"` + text + `"}]`),
		}
	}
	fs := &fakeRecallStore{
		window: recall.Window{ID: "w1", ConversationID: "c1", OrdinalFrom: 10, OrdinalTo: 12},
		messages: []recall.Message{
			msg(9, "user", "before question"),
			msg(10, "assistant", "span answer"),
			msg(11, "user", "follow-up"),
			msg(12, "assistant", "closing"),
		},
	}
	b := bindingWith(fs, users.Identity{UserID: "u-bob"})
	out, err := b.Context(t.Context(), "w:w1", 1, 2, 8000)
	c.Require().NoError(err, "Context")
	// before=1, after=2 around ordinals 10..12: the window asks for 9..14.
	if fs.msgFrom != 9 || fs.msgTo != 14 {
		t.Fatalf("Messages range = %d..%d, want 9..14", fs.msgFrom, fs.msgTo)
	}
	for _, want := range []string{"#9 user: before question", "#10 assistant: span answer", "#12 assistant: closing"} {
		c.StrContains(out, want, "context output missing")
	}

	// The summary shape: id, conversation name, ordinals, title, body.
	summaryStore := &fakeRecallStoreWithConversation{summary: recall.Summary{
		ID: "s1", ConversationID: "c1", OrdinalFrom: 2, OrdinalTo: 9,
		Title: "A title", Summary: "A body",
	}}
	sb := bindingWith(summaryStore, users.Identity{UserID: "u-bob"})
	out, err = sb.Context(t.Context(), "s:s1", 0, 0, 8000)
	c.Require().NoError(err, "summary Context")
	for _, want := range []string{"conversation c1 · A conversation", "ordinals 2-9", "A title", "A body"} {
		c.StrContains(out, want, "summary context missing")
	}
}

// fakeRecallStoreWithConversation adds the Conversation lookup the summary
// context path needs (the summary row carries no name of its own).
type fakeRecallStoreWithConversation struct {
	fakeRecallStore
	summary recall.Summary
}

func (f *fakeRecallStoreWithConversation) Summary(ctx context.Context, scope recall.Scope, id string) (recall.Summary, error) {
	return f.summary, nil
}

func (f *fakeRecallStoreWithConversation) Conversation(ctx context.Context, scope recall.Scope, conversationID string) (recall.ConversationMeta, error) {
	return recall.ConversationMeta{ID: conversationID, Name: "A conversation"}, nil
}

func TestRecallMemoryTreeFallsBackWhenLarge(t *testing.T) {
	c := assert.NewCollecting(t)
	big := strings.Repeat("x", recall.TreeMaxChars/2) // two of these bust the budget
	fs := &fakeRecallStore{memories: []recall.Memory{
		{ID: "1", Path: "proj", Name: "one", Body: big},
		{ID: "2", Path: "proj", Name: "two", Body: big},
	}}
	b := bindingWith(fs, users.Identity{UserID: "u-bob"})

	out, err := b.MemoryTree(t.Context(), "proj", 0)
	c.Require().NoError(err, "MemoryTree")
	c.False(!strings.Contains(out, "proj/one: ") || !strings.Contains(out, "proj/two: "), "degraded tree missing the path lines:\n%.200s", out)
	c.StrContains(out, "(subtree too large for full bodies; call memory_tree on a narrower path)", "degraded tree missing the narrowing ask:\n")
	c.NotStrContains(out, big, "degraded tree leaked a full body")
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "proj/") {
			continue
		}
		_, first, ok := strings.Cut(line, ": ")
		c.Require().True(ok, "degraded line %q has no first-line separator", line)
		c.LessOrEqual(120, len([]rune(first)), "first line longer than 120 chars: %q", first)
	}

	// Under budget: full bodies.
	fs = &fakeRecallStore{memories: []recall.Memory{{ID: "1", Path: "proj", Name: "one", Body: "small body"}}}
	out, err = bindingWith(fs, users.Identity{UserID: "u-bob"}).MemoryTree(t.Context(), "proj", 0)
	c.Require().NoError(err, "MemoryTree small")
	c.Require().Eq("## proj/one\nsmall body", out, "small tree")

	// Empty: the explicit nothing answer, never an empty string.
	fs = &fakeRecallStore{}
	out, err = bindingWith(fs, users.Identity{UserID: "u-bob"}).MemoryTree(t.Context(), "proj", 0)
	c.Require().NoError(err, "MemoryTree empty")
	c.Require().Eq("no memories under proj", out, "empty tree =")
}

// TestRecallUnknownSourceNameErrors pins the addendum's strictness: the tool
// layer passes source names through unvalidated, so the binding must refuse
// an unknown one rather than silently dropping the filter.
func TestRecallUnknownSourceNameErrors(t *testing.T) {
	b := bindingWith(&fakeRecallStore{searchHits: []recall.Hit{{ID: "m:1", Source: recall.SourceMemory}}}, users.Identity{UserID: "u-bob"})
	q := toolsRecallQuery("x")
	q.Sources = []string{"memory", "summarry"}
	if _, err := b.Recall(t.Context(), q); err == nil || !strings.Contains(err.Error(), "summarry") {
		t.Fatalf("unknown source error = %v, want it to name the offending source", err)
	}
}

// TestLLMCompleterUsesSummaryEntrypoint is DB-backed because the entrypoint
// is capture metadata — invisible to a sender, written to the conversation
// row. The indexer's candidate queries filter on exactly this value, so this
// is the self-capture-exclusion contract pinned end to end.
func TestLLMCompleterUsesSummaryEntrypoint(t *testing.T) {
	c := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := t.Context()
	sender := &fakeSender{reply: &anthropic.Message{
		Content: []anthropic.ContentBlockUnion{{Type: "text", Text: "the summary text"}},
		Model:   "claude-haiku-4-5",
		Usage:   anthropic.Usage{InputTokens: 100, OutputTokens: 40},
	}}
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-haiku-4-5"),
		llm.WithLogger(discardLogger()),
	)
	c.NoError(err, "llm.NewClient")
	pricer := insights.Pricer(func(model string) (routing.ModelPricing, bool) {
		return routing.ModelPricing{PromptUSD: 1, CompletionUSD: 2}, true
	})
	comp := &llmCompleter{client: client, model: "claude-haiku-4-5", pricer: pricer}
	got, err := comp.Complete(ctx, "", "summarize this", "the transcript", 1234)
	c.NoError(err, "Complete")
	c.False(got.Text != "the summary text" || got.Model != "claude-haiku-4-5", "completion = %+v", got)
	c.False(got.InputTokens != 100 || got.OutputTokens != 40, "usage = %d/%d, want 100/40 from the response", got.InputTokens, got.OutputTokens)
	// detectCost's arithmetic: input*1 + output*2.
	c.Eq(180, got.CostUSD, "CostUSD")
	c.NotNil(sender.got, "sender saw no request")
	c.Eq(1234, sender.got.MaxTokens, "MaxTokens")
	if sys := sender.got.System; len(sys) == 0 || sys[0].Text != "summarize this" {
		t.Fatalf("system = %+v, want the summarizer prompt", sys)
	}

	// The load-bearing pin: the captured conversation's origin entrypoint.
	var entrypoint string
	c.NoError(pool.QueryRow(ctx,
		`SELECT origin_entrypoint FROM conversations.conversation WHERE origin_entrypoint = $1`,
		recall.SummaryEntrypoint).Scan(&entrypoint), "no conversation captured under %q", recall.SummaryEntrypoint)
	c.Eq(recall.SummaryEntrypoint, entrypoint, "entrypoint =")
}

// TestLLMCompleterResolvesCatalogModel pins the catalog-model resolution the
// completer performs once at construction: the shared catalog indexes
// OpenRouter-NATIVE ids (z-ai/glm-5.3-flash), so a provider-qualified
// [summaries] model (the documented providers.toml shape) must be split
// before any ContextWindow/pricing query — the raw string never matches and
// would silently degrade the segment budget to the unknown-context fallback.
// Rides the TestRecallWiring shim (the pinned name lacks the -run pattern).
func TestLLMCompleterResolvesCatalogModel(t *testing.T) {
	prov := func(models map[string]providers.ModelAlias) *providers.Set {
		return &providers.Set{
			DefaultProvider: "anthropic",
			Providers: map[string]providers.Provider{
				"anthropic":  {Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
				"openrouter": {Kind: "anthropic-openrouter", APIKeyEnv: "OPENROUTER_API_KEY", Models: models},
			},
		}
	}

	t.Run("provider-qualified id splits to the catalog-native id", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newLLMCompleter(nil, prov(nil), "openrouter/z-ai/glm-5.3-flash", nil)
		ck.Eq("openrouter/z-ai/glm-5.3-flash", c.model, "model")
		ck.Eq("z-ai/glm-5.3-flash", c.catalogModel, "catalogModel")
	})
	t.Run("alias resolves to the real id", func(t *testing.T) {
		p := prov(map[string]providers.ModelAlias{"glm": {ID: "z-ai/glm-5.3-flash", ContextWindow: 1310720}})
		c := newLLMCompleter(nil, p, "openrouter/glm", nil)
		assert.NewAborting(t).Eq("z-ai/glm-5.3-flash", c.catalogModel, "catalogModel")
	})
	t.Run("bare id resolves against the default provider unchanged", func(t *testing.T) {
		c := newLLMCompleter(nil, prov(nil), "claude-haiku-4-5", nil)
		assert.NewAborting(t).Eq("claude-haiku-4-5", c.catalogModel, "catalogModel")
	})
	t.Run("unknown provider falls back to the raw string", func(t *testing.T) {
		c := newLLMCompleter(nil, prov(nil), "weird/x", nil)
		assert.NewAborting(t).Eq("weird/x", c.catalogModel, "catalogModel")
	})
	t.Run("nil provider set falls back to the raw string", func(t *testing.T) {
		c := newLLMCompleter(nil, nil, "openrouter/z-ai/glm-5.3-flash", nil)
		assert.NewAborting(t).Eq("openrouter/z-ai/glm-5.3-flash", c.catalogModel, "catalogModel")
	})
}

// TestLLMCompleterCompletionCostFallback pins completionCost's fallback: the
// response echoes the serving provider's id, but when that spelling misses the
// catalog the configured model's provider-local id is tried before giving up —
// a summary's cost is never silently zero because the responder spelled the id
// differently than the catalog does. Rides the TestRecallWiring shim.
func TestLLMCompleterCompletionCostFallback(t *testing.T) {
	c := assert.NewAborting(t)
	usage := anthropic.Usage{InputTokens: 1000, OutputTokens: 100}
	known := func(ids ...string) insights.Pricer {
		set := map[string]struct{}{}
		for _, id := range ids {
			set[id] = struct{}{}
		}
		return func(model string) (routing.ModelPricing, bool) {
			if _, ok := set[model]; !ok {
				return routing.ModelPricing{}, false
			}
			return routing.ModelPricing{}, true // zero prices; ok is what the test pins
		}
	}
	// Zero prices make Cost 0 — indistinguishable from a miss. Assert "found"
	// instead: track which id the pricer saw.
	var saw []string
	recorder := func(model string) (routing.ModelPricing, bool) {
		saw = append(saw, model)
		return routing.ModelPricing{}, false
	}
	completionCost(recorder, "echoed-id", "z-ai/glm-5.3-flash", usage)
	c.False(len(saw) != 2 || saw[0] != "echoed-id" || saw[1] != "z-ai/glm-5.3-flash", "pricer saw %v, want [echoed-id z-ai/glm-5.3-flash]", saw)
	saw = nil
	completionCost(recorder, "echoed-id", "echoed-id", usage)
	c.Len(saw, 1, "pricer saw")
	saw = nil
	c.Eq(0, completionCost(known("echoed-id"), "echoed-id", "z-ai/glm-5.3-flash", usage), "cost")
}
