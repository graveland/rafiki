package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// ContextWindow resolves model against the provider registry's declared model
// aliases first, then the daemon's shared OpenRouter catalog. The registry
// takes priority because the catalog can never know a local/custom provider's
// model — it only ever hears about OpenRouter ids. ok is false when neither
// source knows model.
func (c *Controller) ContextWindow(model string) (contextLen, maxCompletion int, ok bool) {
	if ctxLen, maxComp, _, aliasOK := c.aliasContextWindow(model); aliasOK {
		return ctxLen, maxComp, true
	}
	if c.catalog == nil {
		return 0, 0, false
	}
	return c.catalog.ContextWindow(model)
}

// aliasContextWindow looks up model against the provider registry's declared
// models.<alias> table. ok is true only when the provider declares an alias
// matching model's local id AND that alias declares a context window — an
// alias declared purely for its shorthand (no context_window) is not "known"
// context-wise, same as no alias at all, and falls through to the catalog.
func (c *Controller) aliasContextWindow(model string) (contextLen, maxCompletion int, resolvedID string, ok bool) {
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
	alias, found := p.Models[localID]
	if !found || alias.ContextWindow <= 0 {
		return 0, 0, "", false
	}
	return alias.ContextWindow, alias.MaxCompletionTokens, name + "/" + alias.ID, true
}

// ModelInfo answers ctrl_model_info from the provider registry plus the
// daemon's already-warm catalog. Never returns an error: an unknown model and
// an unconfigured catalog are both Known=false, which is what every caller
// degrades on.
func (c *Controller) ModelInfo(model string) protocol.ModelInfoResponseData {
	out := protocol.ModelInfoResponseData{Model: model}
	if ctxLen, maxComp, resolvedID, ok := c.aliasContextWindow(model); ok {
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
//     family aliases, plus any OpenRouter slash id.
func sourcesForKind(kind string) map[models.Source]bool {
	switch kind {
	case protocol.KindClaude:
		return map[models.Source]bool{models.SourceBuiltin: true}
	default:
		return map[models.Source]bool{
			models.SourceUserConfig: true,
			models.SourceBuiltin:    true,
			models.SourceAlias:      true,
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
			row.ExpiresAt = f.expiresAt
			row.KnowledgeCutoff = f.knowledgeCutoff
			row.AgenticIndex = f.agenticIndex
			if f.created > 0 {
				v := f.created
				row.Created = &v
			}
		}
		out = append(out, row)
	}
	return out
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

// catalogFactsByID flattens the daemon's already-warm catalog into a join
// table. One Rows() call rather than per-id lookups: each of those resolves
// and locks, so a ~300-model catalog would cost 300 of each.
func (c *Controller) catalogFactsByID() map[string]catalogFacts {
	if c.catalog == nil {
		return nil
	}
	rows := c.catalog.Rows()
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
			promptUSD:       r.PromptUSD,
			completionUSD:   r.CompletionUSD,
			cacheReadUSD:    r.CacheReadUSD,
			cacheWriteUSD:   r.CacheWriteUSD,
		}
		out["openrouter/"+r.ID] = f
		out[r.ID] = f // the spine spells OpenRouter ids both ways
	}
	return out
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

	facts := c.catalogFactsByID()
	if want[models.SourceOpenRouter] && c.catalog != nil {
		seen := make(map[string]bool, len(spine))
		for _, m := range spine {
			seen[m.ID] = true
		}
		for _, r := range c.catalog.Rows() {
			id := "openrouter/" + r.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			spine = append(spine, models.Model{
				ID: id, Provider: "openrouter", Model: r.ID,
				Name: r.Name, Source: models.SourceOpenRouter,
			})
		}
	}

	rows := filterRowsForKind(filterByProvider(decorateRows(spine, facts), provider), kind)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// presetEntry is the JSON shape of one entry in the presets file.
// The full Preset struct lives in cmd/rafiki/presets.go; this is a minimal copy
// to avoid cross-package coupling in v1.
type presetEntry struct {
	Model  string            `json:"model,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// presetsFile is the JSON shape of the top-level presets object.
type presetsFile struct {
	Presets map[string]presetEntry `json:"presets"`
}

// ListPresets reads the presets file at paths.PresetsFile() and returns
// presets that satisfy the label filter. labels and hasLabel use the same
// AND-match semantics as ctrl_list: every k=v in labels must match the
// preset's labels map, and every key in hasLabel must be present.
//
// An absent or empty presets file returns an empty slice (not an error) so
// callers always get a well-formed response.
func (c *Controller) ListPresets(labels map[string]string, hasLabel []string) ([]protocol.PresetInfo, error) {
	path := paths.PresetsFile()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []protocol.PresetInfo{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pf presetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Sort preset names for deterministic output.
	names := make([]string, 0, len(pf.Presets))
	for name := range pf.Presets {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]protocol.PresetInfo, 0, len(names))
	for _, name := range names {
		p := pf.Presets[name]
		if !matchesPresetLabelFilter(p.Labels, labels, hasLabel) {
			continue
		}
		out = append(out, protocol.PresetInfo{
			Name:   name,
			Model:  p.Model,
			Labels: p.Labels,
		})
	}
	return out, nil
}

// matchesPresetLabelFilter returns true when the preset's labels satisfy both
// the AND-match required map and the key-presence hasLabels list.  Unlike the
// child matchesLabelFilter, nil preset labels are handled gracefully so presets
// without any labels can still be returned when the filter is empty.
func matchesPresetLabelFilter(presetLabels, required map[string]string, hasLabels []string) bool {
	for k, v := range required {
		if presetLabels[k] != v {
			return false
		}
	}
	for _, k := range hasLabels {
		if _, ok := presetLabels[k]; !ok {
			return false
		}
	}
	return true
}
