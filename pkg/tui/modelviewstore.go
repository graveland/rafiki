// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"go.graveland.dev/rafiki/pkg/clientstate"
)

// This file is the ONLY place the cockpit's in-memory modelView meets its
// stored form. The store itself (pkg/clientstate) knows nothing about
// modelField or boundStop -- it holds labels -- so a new column or a new
// threshold changes this file and nothing under pkg/clientstate.

func fieldByName(name string) (modelField, bool) {
	for f := modelField(0); f < modelFieldCount; f++ {
		if f.String() == name {
			return f, true
		}
	}
	return 0, false
}

func stopByLabel(stops []boundStop, label string) (int, bool) {
	for i, s := range stops {
		if s.label == label {
			return i, true
		}
	}
	return 0, false
}

// toStored flattens a view for the state file.
func toStored(v modelView) *clientstate.ModelView {
	out := &clientstate.ModelView{
		ToolsOnly: v.toolsOnly, VisionOnly: v.visionOnly, ThinkingOnly: v.thinkingOnly,
		Bounds: map[string]clientstate.Bound{},
	}
	for _, k := range v.keys {
		out.Keys = append(out.Keys, clientstate.SortKey{Field: k.field.String(), Desc: k.desc})
	}
	for f, b := range v.bounds {
		if !b.set() {
			continue
		}
		var sb clientstate.Bound
		if mins := minStops(f); b.minIx > 0 && b.minIx < len(mins) {
			sb.Min = mins[b.minIx].label
		}
		if maxs := maxStops(f); b.maxIx > 0 && b.maxIx < len(maxs) {
			sb.Max = maxs[b.maxIx].label
		}
		out.Bounds[f.String()] = sb
	}
	return out
}

// fromStored rebuilds a view, dropping anything it no longer recognises.
func fromStored(p *clientstate.ModelView) modelView {
	if p == nil {
		return defaultModelView()
	}
	v := modelView{
		toolsOnly: p.ToolsOnly, visionOnly: p.VisionOnly, thinkingOnly: p.ThinkingOnly,
		bounds: map[modelField]bound{},
	}
	for _, k := range p.Keys {
		f, ok := fieldByName(k.Field)
		if !ok {
			continue
		}
		v.keys = append(v.keys, sortKey{field: f, desc: k.Desc})
	}
	for name, sb := range p.Bounds {
		f, ok := fieldByName(name)
		if !ok {
			continue
		}
		var b bound
		if sb.Min != "" {
			if ix, ok := stopByLabel(minStops(f), sb.Min); ok {
				b.minIx = ix
			}
		}
		if sb.Max != "" {
			if ix, ok := stopByLabel(maxStops(f), sb.Max); ok {
				b.maxIx = ix
			}
		}
		if b.set() {
			v.bounds[f] = b
		}
	}
	// A document that decoded to nothing orderable still needs a total order,
	// or the list comes back in whatever order the daemon happened to send.
	if len(v.keys) == 0 {
		v.keys = []sortKey{{field: colModel}}
	}
	return v
}

// loadModelView reads the remembered query for one profile, falling back to
// the default. Per-profile because a daemon's model catalog is per-profile:
// a remembered query built against one provider set is not necessarily valid
// against another's.
func loadModelView(profileName string) modelView {
	return fromStored(clientstate.LoadScoped(clientstate.Scope{Profile: profileName}).ModelView)
}

// saveModelView persists the query for one profile.
//
// Through UpdateScoped, not SaveScoped: writing the whole document from here
// would drop every section this package has never heard of.
func saveModelView(profileName string, v modelView) {
	clientstate.UpdateScoped(clientstate.Scope{Profile: profileName}, func(s *clientstate.State) { s.ModelView = toStored(v) })
}
