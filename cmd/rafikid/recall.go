// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/embed"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/recalldb"
	"go.graveland.dev/rafiki/pkg/users"
)

// llmCompleter issues the summarizer's completions on the daemon's own llm
// client. Every call is captured as a conversation with origin_entrypoint
// recall.SummaryEntrypoint, which recall.ExcludedEntrypoints filters out of
// the indexer's candidate queries — without that, the summarizer would
// summarize its own summaries forever.
type llmCompleter struct {
	client *llm.Client
	model  string
	// pricer prices the call exactly like pkg/analyze's detector does; nil (a
	// client with no catalog pricing) leaves CostUSD 0.
	pricer insights.Pricer
}

var _ recall.Completer = (*llmCompleter)(nil)

func (c *llmCompleter) Model() string { return c.model }

// ContextWindow resolves the model's context length from the shared catalog.
// ok=false (unknown model, cold cache) is the Summarizer's fallback signal,
// not an error.
func (c *llmCompleter) ContextWindow() (int, bool) {
	tokens, _, ok := c.client.Catalog().ContextWindow(c.model)
	return tokens, ok
}

func (c *llmCompleter) Complete(ctx context.Context, ownerUserID, system, user string, maxTokens int) (recall.Completion, error) {
	// ownerUserID is a conversations.users id; "" is the analyze path's
	// unattributed shape for the daemon's own LLM jobs.
	conv, err := c.client.Conversation(ctx,
		llm.NewConversation(ownerUserID, recall.SummaryEntrypoint),
		llm.Model(c.model), llm.SystemText(system))
	if err != nil {
		return recall.Completion{}, fmt.Errorf("recall summaries: %w", err)
	}
	resp, err := conv.Send(ctx, llm.UserText(user),
		llm.WithMaxTokens(int64(maxTokens)), llm.WithSource(recall.SummaryEntrypoint))
	if err != nil {
		return recall.Completion{}, fmt.Errorf("recall summaries: %w", err)
	}
	var text strings.Builder
	for _, blk := range resp.Content {
		text.WriteString(blk.Text)
	}
	model := string(resp.Model)
	return recall.Completion{
		Text:         text.String(),
		Model:        model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CostUSD:      completionCost(c.pricer, model, resp.Usage),
	}, nil
}

// completionCost prices one response's usage via pricer, returning 0 when the
// pricer is nil or has no entry for the model — the same arithmetic as
// pkg/analyze/detect.go's detectCost, which routes through
// routing.ModelPricing.Cost so every cost consumer shares one formula.
func completionCost(pricer insights.Pricer, model string, usage anthropic.Usage) float64 {
	if pricer == nil {
		return 0
	}
	price, ok := pricer(model)
	if !ok {
		return 0
	}
	return price.Cost(usage).Total
}

// recallBinding is recall + memories for one caller, bound at construction —
// the caller's conversation scope and own owner id are the constructor's
// inputs, never method arguments, so a tool argument can never widen them.
type recallBinding struct {
	st    recall.Store
	emb   recall.Embedder // nil = BM25-only search
	scope recall.Scope
	owner string
}

var _ tools.RecallBinding = (*recallBinding)(nil)

// newRecallBinding binds the recall tools to one caller. Scope follows
// newMCPConversationReader exactly (admin → everything, a named user → their
// own rows, an anonymous caller → the zero Scope that admits nothing); the
// MEMORY owner is always the caller's own user id, admin included — a saved
// memory is private to whoever saved it. nil when the daemon's recall
// subsystem is not wired: the caller must assign the result to the
// interface-typed option only when non-nil (a typed-nil in the interface
// would defeat the blueprints' nil-decline).
func newRecallBinding(c *Controller, owner users.Identity) tools.RecallBinding {
	if c.recall == nil {
		return nil
	}
	var scope recall.Scope
	switch {
	case owner.IsAdmin:
		scope = recall.Scope{All: true}
	case owner.UserID != "":
		scope = recall.Scope{OwnerUserID: owner.UserID}
	default:
		scope = recall.Scope{}
	}
	return &recallBinding{st: c.recall.st, emb: c.recall.emb, scope: scope, owner: owner.UserID}
}

func (b *recallBinding) Recall(ctx context.Context, q tools.RecallQuery) (string, error) {
	sq := recall.SearchQuery{
		Scope:       b.scope,
		MemoryOwner: b.owner,
		Text:        q.Query,
		Under:       q.Under,
		Repo:        q.Repo,
		Limit:       q.Limit,
	}
	// Sources arrive as NAMES and the tool layer does not validate them: an
	// unknown one is an error here, never a silently-dropped filter.
	for _, name := range q.Sources {
		src, err := recallSourceFromName(name)
		if err != nil {
			return "", err
		}
		sq.Sources = append(sq.Sources, src)
	}
	if t := unixSecPtr(q.SinceUnix); t != nil {
		sq.Since = t
	}
	if t := unixSecPtr(q.UntilUnix); t != nil {
		sq.Until = t
	}
	hits, err := recall.Search(ctx, b.st, b.emb, sq, q.Limit)
	if err != nil {
		return "", err
	}
	return recall.FormatHits(hits), nil
}

// recallSourceFromName maps a tool-facing source name onto its recall.Source.
func recallSourceFromName(name string) (recall.Source, error) {
	switch name {
	case string(recall.SourceMemory):
		return recall.SourceMemory, nil
	case string(recall.SourceSummary):
		return recall.SourceSummary, nil
	case string(recall.SourceWindow):
		return recall.SourceWindow, nil
	}
	return "", fmt.Errorf("recall: unknown source %q (want %q, %q or %q)",
		name, recall.SourceMemory, recall.SourceSummary, recall.SourceWindow)
}

func (b *recallBinding) Context(ctx context.Context, hitID string, before, after, maxChars int) (string, error) {
	src, id, err := recall.ParseHitID(hitID)
	if err != nil {
		return "", err
	}
	switch src {
	case recall.SourceWindow:
		w, err := b.st.Window(ctx, b.scope, id)
		if err != nil {
			return "", err
		}
		ms, err := b.st.Messages(ctx, b.scope, w.ConversationID, w.OrdinalFrom-before, w.OrdinalTo+after)
		if err != nil {
			return "", err
		}
		return truncateContext(recall.RenderContext(ms), maxChars), nil
	case recall.SourceSummary:
		s, err := b.st.Summary(ctx, b.scope, id)
		if err != nil {
			return "", err
		}
		conv, err := b.st.Conversation(ctx, b.scope, s.ConversationID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("conversation %s · %s · ordinals %d-%d\n\n%s\n\n%s",
			s.ConversationID, conv.Name, s.OrdinalFrom, s.OrdinalTo, s.Title, s.Summary), nil
	case recall.SourceMemory:
		m, err := b.st.MemoryByID(ctx, b.owner, id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/%s\n\n%s", m.Path, m.Name, m.Body), nil
	}
	return "", fmt.Errorf("recall: bad hit id %q", hitID)
}

// truncateContext caps rendered context at maxChars, telling the model how to
// get the rest rather than leaving it to guess.
func truncateContext(text string, maxChars int) string {
	runes := []rune(text)
	if maxChars <= 0 || len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + "\n… (truncated; narrow before/after or use conversation_export)"
}

func (b *recallBinding) MemoryPut(ctx context.Context, path, name, body string, meta json.RawMessage) (recall.Memory, error) {
	if len(meta) == 0 {
		meta = json.RawMessage("{}") // the store documents "{}" when absent
	}
	return b.st.PutMemory(ctx, b.owner, recall.Memory{
		Path: path, Name: name, Body: body,
		Meta: meta,
	})
}

func (b *recallBinding) MemoryGet(ctx context.Context, path, name string) (recall.Memory, error) {
	return b.st.GetMemory(ctx, b.owner, path, name)
}

func (b *recallBinding) MemoryTree(ctx context.Context, path string, depth int) (string, error) {
	ms, err := b.st.MemoryTree(ctx, b.owner, path, depth)
	if err != nil {
		return "", err
	}
	if len(ms) == 0 {
		return "no memories under " + path, nil
	}
	var full strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&full, "## %s/%s\n%s\n\n", m.Path, m.Name, m.Body)
	}
	if full.Len() <= recall.TreeMaxChars {
		return strings.TrimRight(full.String(), "\n"), nil
	}
	// Subtree too large for full bodies: one line per memory plus the ask to
	// narrow, so the model keeps orientation instead of receiving a wall.
	var outline strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&outline, "%s/%s: %s\n", m.Path, m.Name, firstLine(m.Body, 120))
	}
	outline.WriteString("(subtree too large for full bodies; call memory_tree on a narrower path)")
	return outline.String(), nil
}

func (b *recallBinding) MemoryDelete(ctx context.Context, path, name string) error {
	return b.st.DeleteMemory(ctx, b.owner, path, name)
}

// firstLine returns s's first line, truncated to max runes.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max])
	}
	return s
}

// recallRuntime is the daemon's wired recall subsystem. Nil on a DB-less
// daemon; the store is present whenever the runtime is, the embedder and
// summarizer follow their provider tables.
type recallRuntime struct {
	st      recall.Store
	emb     recall.Embedder    // nil = BM25-only
	sums    recall.SummaryPass // nil = no summaries
	indexer *recall.Indexer
	// summaryModel is the configured [summaries] model, "" when none.
	summaryModel string
}

// buildRecallRuntime assembles the runtime without starting anything: the
// store, then the embedder and summarizer strictly behind their config-nil
// branches, then the indexer over both.
//
// The two interface variables are declared as the INTERFACE type and assigned
// ONLY inside the != nil branches. Assigning a typed-nil (*embed.Client or
// *recall.Summarizer built from a nil dependency) would enter the interface as
// non-nil and the indexer would then dereference it — the zero-value/typed-nil
// trap, mirrored by every caller of newRecallBinding.
func buildRecallRuntime(pool *pgxpool.Pool, prov *providers.Set, client *llm.Client, logger *slog.Logger) *recallRuntime {
	st := recalldb.New(pool)
	var emb recall.Embedder
	var sums recall.SummaryPass
	var summaryModel string
	if prov != nil {
		if prov.Embeddings != nil {
			emb = embed.New(*prov.Embeddings, nil)
		}
		if prov.Summaries != nil {
			summaryModel = prov.Summaries.Model
			if client != nil {
				sums = recall.NewSummarizer(recall.SummarizerOptions{
					Store: st,
					Completer: &llmCompleter{
						client: client,
						model:  prov.Summaries.Model,
						pricer: client.Catalog().Pricing,
					},
					MaxSegmentTokens: prov.Summaries.MaxSegmentTokens,
					Logger:           logger,
				})
			}
		}
	}
	ix := recall.NewIndexer(recall.IndexerOptions{
		Store:     st,
		Embedder:  emb,
		Summaries: sums,
		Logger:    logger,
	})
	return &recallRuntime{st: st, emb: emb, sums: sums, indexer: ix, summaryModel: summaryModel}
}

// startRecall wires the recall subsystem on a daemon with a database: the
// store behind every recall/memory tool, the background indexer (windows,
// embeddings, rolling summaries) started on ctx and waited on by Stop().
//
// A nil pool disables recall entirely — the DB-less daemon's posture, logged
// once at Info like every other degradation, never an error.
func startRecall(ctx context.Context, c *Controller, pool *pgxpool.Pool, prov *providers.Set, client *llm.Client, logger *slog.Logger) {
	if pool == nil {
		logger.Info("recall disabled: no agent database")
		return
	}
	rt := buildRecallRuntime(pool, prov, client, logger)
	if prov != nil && prov.Summaries != nil && client == nil {
		logger.Warn("recall summaries configured but the daemon has no llm client; continuing without summaries")
	}
	if rt.emb != nil {
		// Once, at startup: the HNSW indexes the vector search needs. A failure
		// degrades to BM25-only on a table that may still be empty; refusing to
		// start the daemon for an index that the next boot would create is
		// worse than a degraded search.
		if err := rt.st.EnsureVectorIndexes(ctx, rt.emb.Model(), rt.emb.Dimensions()); err != nil {
			logger.Warn("recall: could not ensure vector indexes; vector search degraded to BM25", "error", err)
		}
	}
	c.recall = rt
	c.recallWg.Add(1)
	go func() {
		defer c.recallWg.Done()
		rt.indexer.Run(ctx)
	}()
	logger.Info("recall index started",
		"embeddings", rt.emb != nil,
		"summaries", rt.sums != nil,
		"summary_model", rt.summaryModel)
}
