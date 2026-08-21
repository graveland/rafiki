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
	"go.graveland.dev/rafiki/pkg/routing"
)

// ContextWindow resolves model against the daemon's shared model catalog.
// ok is false when the daemon has no catalog (c.catalog is nil — the proxy
// face failed to start, or a build path that never wired one) or the model
// isn't in the current snapshot.
func (c *Controller) ContextWindow(model string) (contextLen, maxCompletion int, ok bool) {
	if c.catalog == nil {
		return 0, 0, false
	}
	return c.catalog.ContextWindow(model)
}

// ModelInfo answers ctrl_model_info from the daemon's already-warm catalog.
// Never returns an error: an unknown model and an unconfigured catalog are
// both Known=false, which is what every caller degrades on.
func (c *Controller) ModelInfo(model string) protocol.ModelInfoResponseData {
	out := protocol.ModelInfoResponseData{Model: model}
	ctxLen, maxComp, ok := c.ContextWindow(model)
	if !ok {
		return out
	}
	out.Known = true
	out.ContextWindow = ctxLen
	out.MaxCompletionTokens = maxComp
	out.AutoCompactWindow = routing.AutoCompactWindow(ctxLen, maxComp)
	if c.catalog != nil {
		if id, ok := c.catalog.ResolveID(model); ok {
			out.ResolvedID = id
		}
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
