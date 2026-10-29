package main

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// ContextWindow resolves model against the provider registry's declared models
// first, then the daemon's shared OpenRouter catalog. The registry takes
// priority because the catalog can never know a local/custom provider's model
// — it only ever hears about OpenRouter ids. ok is false when neither source
// knows model.
func (c *Controller) ContextWindow(model string) (contextLen, maxCompletion int, ok bool) {
	if ctxLen, maxComp, _, declOK := c.declContextWindow(model); declOK {
		return ctxLen, maxComp, true
	}
	if c.catalog == nil {
		return 0, 0, false
	}
	return c.catalog.ContextWindow(model)
}

// declContextWindow looks up model against the provider registry's declared
// models.<id> table. ok is true only when the provider declares a model
// matching model's local id AND that declaration carries a context window — a
// declaration made purely for its shorthand (no context_window) is not "known"
// context-wise, same as no declaration at all, and falls through to the
// catalog.
func (c *Controller) declContextWindow(model string) (contextLen, maxCompletion int, resolvedID string, ok bool) {
	set := providersOrDefault(c.providers)
	if set == nil {
		return 0, 0, "", false
	}
	name, localID := providers.SplitRaw(model)
	if name == "" {
		name = set.DefaultProvider
	}
	p, found := set.Get(name)
	if !found {
		return 0, 0, "", false
	}
	decl, found := p.Models[localID]
	if !found || decl.ContextWindow <= 0 {
		return 0, 0, "", false
	}
	return decl.ContextWindow, decl.MaxCompletionTokens, name + "/" + decl.ID, true
}

// ModelInfo answers the ModelInfo RPC from the provider registry plus the
// daemon's already-warm catalog. Never returns an error: an unknown model and
// an unconfigured catalog are both Known=false, which is what every caller
// degrades on.
func (c *Controller) ModelInfo(model string) protocol.ModelInfoResponseData {
	out := protocol.ModelInfoResponseData{Model: model}
	if ctxLen, maxComp, resolvedID, ok := c.declContextWindow(model); ok {
		out.Known = true
		out.ContextWindow = ctxLen
		out.MaxCompletionTokens = maxComp
		out.AutoCompactWindow = routing.AutoCompactWindow(ctxLen, maxComp)
		out.ResolvedID = resolvedID
		return out
	}
	if c.catalog == nil {
		return out
	}
	ctxLen, maxComp, ok := c.catalog.ContextWindow(model)
	if !ok {
		return out
	}
	out.Known = true
	out.ContextWindow = ctxLen
	out.MaxCompletionTokens = maxComp
	out.AutoCompactWindow = routing.AutoCompactWindow(ctxLen, maxComp)
	if id, ok := c.catalog.ResolveID(model); ok {
		out.ResolvedID = id
	}
	return out
}

// ListModels enumerates LLM models from all configured sources.
// When provider is non-empty, only models whose Provider field matches are
// returned.  Best-effort: missing or unreachable sources produce no entries.
func (c *Controller) ListModels(ctx context.Context, provider string) ([]protocol.ModelInfo, error) {
	list := models.List(ctx, providersOrDefault(c.providers))
	out := make([]protocol.ModelInfo, 0, len(list))
	for _, m := range list {
		if provider != "" && m.Provider != provider {
			continue
		}
		out = append(out, protocol.ModelInfo{
			ID:       m.ID,
			Provider: m.Provider,
			Model:    m.Model,
			Name:     m.Name,
			Source:   string(m.Source),
		})
	}
	return out, nil
}

// catalogFacts is what the catalog contributes about one model id. Separate
// from connectapi.ModelRow so decorateRows is testable without building a
// ModelCatalog and without a network fixture.
//
// Every field is a POINTER because absent and zero differ all the way down:
// the pointers are copied straight from CatalogRow, whose presence comes from
// the raw OpenRouter entry. No > 0 guards here — a reported zero must survive
// as present-zero, and an absent field as nil.
type catalogFacts struct {
	created         int64
	supportedParams []string
	expiresAt       string
	knowledgeCutoff string
	agenticIndex    *float64
	intelIndex      *float64
	codingIndex     *float64
	name            string
	contextLength   *int
	maxCompletion   *int
	promptUSD       *float64
	completionUSD   *float64
	cacheReadUSD    *float64
	cacheWriteUSD   *float64
	inputModalities []string
}

// sourcesForKind returns the model sources a child of the given kind can
// actually resolve.
//
// Moved here from cmd/rafiki: the two kinds have genuinely different model
// universes, and offering one kind's ids to another produces a child that
// spawns, attaches, and then never answers — no error, just a TUI that never
// becomes idle. That knowledge belongs where the spawn happens, not in a copy
// the client keeps.
//
//   - "claude" shells out to Claude Code, which resolves only Anthropic ids.
//     A source set of {builtin} is still not narrow enough on its own — the
//     builtin curation is cross-provider — so the claude kind is narrowed
//     again at the row layer by filterRowsForKind.
//   - "fundi" (the DEFAULT, and the empty/not-yet-typed case) is rafiki's
//     native runtime, so it takes what rafiki resolves: the curated ids and
//     family aliases, the operator's declared models, plus any OpenRouter
//     slash id.
func sourcesForKind(kind string) map[models.Source]bool {
	switch kind {
	case protocol.KindClaude:
		return map[models.Source]bool{models.SourceBuiltin: true}
	default:
		return map[models.Source]bool{
			models.SourceUserConfig: true,
			models.SourceBuiltin:    true,
			models.SourceDeclared:   true,
			models.SourceOpenRouter: true,
			models.SourceLocal:      true,
		}
	}
}

// decorateRows joins the spine against the catalog by id.
//
// The SPINE decides which rows exist and what Source each carries; the catalog
// only ever contributes optional fields, copied through as-is — presence is
// decided by the catalog entry, never re-derived here (a > 0 guard here would
// turn a reported zero into absent and break the contract the catalog keeps).
// The catalog never invents a row, because the spine is where kind-scoping is
// applied and a row that bypassed it would be offered for a child that cannot
// run it.
func decorateRows(spine []models.Model, cat map[string]catalogFacts) []connectapi.ModelRow {
	out := make([]connectapi.ModelRow, 0, len(spine))
	for _, m := range spine {
		row := connectapi.ModelRow{
			ID:       m.ID,
			Provider: m.Provider,
			Model:    m.Model,
			Name:     m.Name,
			Source:   string(m.Source),
		}
		if f, ok := cat[m.ID]; ok {
			if f.name != "" && row.Name == "" {
				row.Name = f.name
			}
			row.ContextWindow = f.contextLength
			row.MaxCompletionTokens = f.maxCompletion
			row.PromptUSD = f.promptUSD
			row.CompletionUSD = f.completionUSD
			row.CacheReadUSD = f.cacheReadUSD
			row.CacheWriteUSD = f.cacheWriteUSD
			row.InputModalities = f.inputModalities
			row.SupportedParameters = f.supportedParams
			row.ExpiresAt = parseCatalogDate(f.expiresAt)
			row.KnowledgeCutoff = f.knowledgeCutoff
			row.AgenticIndex = f.agenticIndex
			row.IntelligenceIndex = f.intelIndex
			row.CodingIndex = f.codingIndex
			if f.created > 0 {
				t := time.Unix(f.created, 0).UTC()
				row.Created = &t
			}
		}
		out = append(out, row)
	}
	return out
}

// parseCatalogDate converts OpenRouter's YYYY-MM-DD expiration_date into a
// time; an empty or unparseable value is absent.
func parseCatalogDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &t
}

// filterByProvider drops rows whose provider does not match. An empty provider
// keeps everything.
func filterByProvider(rows []connectapi.ModelRow, provider string) []connectapi.ModelRow {
	if provider == "" {
		return rows
	}
	out := make([]connectapi.ModelRow, 0, len(rows))
	for _, r := range rows {
		if r.Provider == provider {
			out = append(out, r)
		}
	}
	return out
}

// filterRowsForKind narrows rows to the ids a child of kind can actually run,
// at the ROW layer. sourcesForKind decides which SOURCES may answer for a
// kind, and that is not enough on its own: the builtin source's curation is
// cross-provider — anthropic ids and openrouter ids share one list — so a
// claude child whose source set is exactly {builtin} would still be offered
// openrouter/openai/gpt-4o and friends, which Claude Code can never resolve.
// That is the exact spawns-attaches-never-answers failure sourcesForKind's
// comment warns about, invisible to a test that checks the source set rather
// than the rows. The kind's row-level contract is what the picker serves:
// claude keeps only provider "anthropic"; every other kind is row-unfiltered.
func filterRowsForKind(rows []connectapi.ModelRow, kind string) []connectapi.ModelRow {
	if kind != protocol.KindClaude {
		return rows
	}
	out := make([]connectapi.ModelRow, 0, len(rows))
	for _, r := range rows {
		if r.Provider == "anthropic" {
			out = append(out, r)
		}
	}
	return out
}

// catalogFactsByID flattens the catalog's rows into a join table. It takes
// rows from one Rows() call rather than doing per-id lookups: each of those
// resolves and locks, so a ~300-model catalog would cost 300 of each.
func (c *Controller) catalogFactsByID(rows []routing.CatalogRow) map[string]catalogFacts {
	out := make(map[string]catalogFacts, len(rows))
	for _, r := range rows {
		f := catalogFacts{
			name:            r.Name,
			contextLength:   r.ContextLength,
			maxCompletion:   r.MaxCompletionTokens,
			inputModalities: r.InputModalities,
			created:         r.Created,
			supportedParams: r.SupportedParameters,
			expiresAt:       r.ExpiresAt,
			knowledgeCutoff: r.KnowledgeCutoff,
			agenticIndex:    r.AgenticIndex,
			intelIndex:      r.IntelligenceIndex,
			codingIndex:     r.CodingIndex,
			promptUSD:       r.PromptUSD,
			completionUSD:   r.CompletionUSD,
			cacheReadUSD:    r.CacheReadUSD,
			cacheWriteUSD:   r.CacheWriteUSD,
		}
		out["openrouter/"+r.ID] = f
		out[r.ID] = f // the spine spells OpenRouter ids both ways
		if r.AnthropicID != "" {
			out["anthropic/"+r.AnthropicID] = f // native spelling: dashes, not dots
		}
	}
	// A <family>-latest alias carries the facts of the entry it resolves to.
	for _, alias := range routing.LatestFamilies() {
		fam, _ := routing.LatestAlias(alias)
		if _, orID, ok := c.catalog.ResolveLatest(fam); ok {
			if f, found := out[orID]; found {
				out["anthropic/"+alias] = f
			}
		}
	}
	return out
}

// modelSourceLogin marks the rows a user's own ChatGPT login contributes. It is
// spelled as a source of its own (not one of pkg/models' Sources) so a client
// can tell a subscription model from anything the catalog or the operator's
// config declared.
const modelSourceLogin = "login"

// ownerModelRows appends the caller's OWN ChatGPT-subscription models, one row
// per listable entry of the owner's cached /models directory.
//
// Three rules, and each is a way this listing can leak or lie:
//
//   - The directory is the CALLER's (spawnOwner, i.e. the authenticated
//     connection), read through the same per-owner cache the spawn path uses.
//     Another user's login is never consulted, because the store is keyed by
//     owner and this function only ever names one.
//   - Only visibility == "list" entries appear. The field also takes "hide" and
//     "none"; those models cannot be addressed by the account, so offering one
//     hands the picker a model whose every turn fails. supported_in_api is
//     deliberately NOT consulted: it describes API-key access and is false for
//     subscription models this account can run.
//   - No owner, no credential, no cached directory and a failed read all answer
//     NO ROWS for that provider, never an error. A model listing is a picker;
//     failing it because one provider has no directory would take the picker
//     down over the least important source on it.
//
// declared is the ids the registry's own sources already produced. An id the
// operator declared in providers.toml is not added twice: the declared row wins,
// because a duplicate id in a picker is a key collision in every client that
// keys rows by it.
//
// The rows are Source "login" so a client can tell them from the catalog's, and
// they carry the entry's own context_window and input_modalities rather than a
// catalog guess: for a subscription model the account's answer is the only one
// that is right.
func (c *Controller) ownerModelRows(ctx context.Context, facts map[string]catalogFacts, declared map[string]bool) []connectapi.ModelRow {
	if c == nil || c.providerCreds == nil {
		return nil
	}
	owner := spawnOwner(ctx).UserID
	if owner == "" {
		return nil
	}
	set := providersOrDefault(c.providers)
	if set == nil {
		return nil
	}
	if ok, _ := c.providerCreds.Available(); !ok {
		return nil
	}
	var out []connectapi.ModelRow
	for _, name := range set.Names() {
		p, ok := set.Get(name)
		if !ok || p.Kind != providers.KindOpenAIResponsesChatGPT {
			continue
		}
		has, err := c.providerCreds.HasCredential(ctx, owner, name)
		if err != nil {
			// The provider simply contributes nothing; the rest of the listing
			// still answers.
			slog.Warn("model listing: check the ChatGPT login failed",
				"owner", owner, "provider", name, "error", err)
			continue
		}
		if !has {
			continue
		}
		// Kick the refresh and answer from whatever is cached: the listing never
		// waits for the upstream, and a failed fetch is logged there, not here.
		c.providerCreds.refreshModelsInBackground(owner, name)
		dir, err := c.providerCreds.ownerModels(ctx, owner, name)
		if err != nil {
			slog.Warn("model listing: read the owner's model directory failed",
				"owner", owner, "provider", name, "error", err)
			continue
		}
		for _, m := range dir.listable() {
			id := name + "/" + m.Slug
			if declared[id] {
				continue
			}
			row := connectapi.ModelRow{
				ID:              id,
				Provider:        name,
				Model:           m.Slug,
				Name:            m.DisplayName,
				Source:          modelSourceLogin,
				ContextWindow:   optionalInt(m.ContextWindow),
				InputModalities: m.InputModalities,
			}
			applyOwnerModelPrice(&row, m.Slug, facts)
			out = append(out, row)
		}
	}
	return out
}

// applyOwnerModelPrice copies the catalog's price fields onto a login-sourced
// row.
//
// The lookup id is the CATALOG id for the slug (openai/<slug>), not the row's
// own <provider>/<slug>: the row is spelled after the operator's provider name,
// while the catalog only ever hears OpenRouter's id, so joining on the row id
// would find nothing and silently leave every subscription model unpriced. A
// miss leaves the fields nil (unknown), never zero.
func applyOwnerModelPrice(row *connectapi.ModelRow, slug string, facts map[string]catalogFacts) {
	f, ok := facts["openai/"+slug]
	if !ok {
		return
	}
	row.PromptUSD = f.promptUSD
	row.CompletionUSD = f.completionUSD
	row.CacheReadUSD = f.cacheReadUSD
	row.CacheWriteUSD = f.cacheWriteUSD
}

// optionalInt reports n as present when it is set, and as absent when it is 0.
// The directory's context_window is an int with no presence bit, and a model
// whose window the server did not report must not claim a zero-token window.
func optionalInt(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

// ListModelRows answers the Connect ListModels RPC.
//
// The daemon's OpenRouter rows come from routing.ModelCatalog, NOT from
// models.loadOpenRouter: the daemon warms the catalog for routing already, and
// consulting models' own OpenRouter source would run a second HTTP fetch with a
// second disk cache and a different TTL in the same process.
func (c *Controller) ListModelRows(ctx context.Context, provider, kind string) ([]connectapi.ModelRow, error) {
	want := sourcesForKind(kind)
	spineWant := make(map[models.Source]bool, len(want))
	for s, on := range want {
		if s == models.SourceOpenRouter {
			continue // served from the catalog below
		}
		spineWant[s] = on
	}
	spine := models.ListSources(ctx, providersOrDefault(c.providers), spineWant)

	var facts map[string]catalogFacts
	if c.catalog != nil {
		catRows := c.catalog.Rows()
		facts = c.catalogFactsByID(catRows)
		seen := make(map[string]bool, len(spine))
		for _, m := range spine {
			seen[m.ID] = true
		}
		add := func(m models.Model) {
			if !seen[m.ID] {
				seen[m.ID] = true
				spine = append(spine, m)
			}
		}
		for _, r := range catRows {
			// The curated list can't keep up with Anthropic releases; the
			// catalog's native-runnable entries extend it under builtin, the
			// source that serves the native Anthropic path.
			if want[models.SourceBuiltin] && r.AnthropicID != "" {
				add(models.Model{
					ID: "anthropic/" + r.AnthropicID, Provider: "anthropic", Model: r.AnthropicID,
					Name: r.Name, Source: models.SourceBuiltin,
				})
			}
			if want[models.SourceOpenRouter] {
				add(models.Model{
					ID: "openrouter/" + r.ID, Provider: "openrouter", Model: r.ID,
					Name: r.Name, Source: models.SourceOpenRouter,
				})
			}
		}
	}

	rows := decorateRows(spine, facts)
	declared := make(map[string]bool, len(rows))
	for _, r := range rows {
		declared[r.ID] = true
	}
	rows = append(rows, c.ownerModelRows(ctx, facts, declared)...)
	rows = filterRowsForKind(filterByProvider(rows, provider), kind)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}
