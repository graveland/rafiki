package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

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
