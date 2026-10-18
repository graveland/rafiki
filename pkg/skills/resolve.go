// SPDX-License-Identifier: Apache-2.0

package skills

import "sort"

// Layer is one rung of skill precedence. Higher wins: a user's own row
// shadows a global one, and a global one shadows the core row shipped in the
// binary. The values are deliberately 1-based so a zero Layer is "unset"
// rather than a silently valid weakest layer.
type Layer int

const (
	LayerCore Layer = iota + 1 // non-zero: no zero-value layer
	LayerGlobal
	LayerUser
)

// LayerOf reports which layer a row belongs to: core if it carries the
// daemon's own provenance, user if it has an owner, global otherwise. Source
// is checked first, so a core row is core even if it somehow carries an
// owner.
func LayerOf(r Record) Layer {
	if r.Source == CoreSource {
		return LayerCore
	}
	if r.OwnerUserID != "" {
		return LayerUser
	}
	return LayerGlobal
}

// Resolve returns the skills ownerUserID sees, sorted by (Namespace, Name).
// Rows owned by any other user are ignored; ownerUserID "" means no user
// layer. A namespace present in a layer (any row, enabled or not) replaces
// that namespace from every lower layer, whole — except DefaultNamespace,
// which is resolved per name. Disabled rows are dropped last.
//
// "Present" is about the row's existence, never its content: a disabled
// override still hides the lower-layer skill of the same name, which is how
// switching a core skill off is expressed. The input is not mutated and the
// result is freshly allocated, so callers may pass the store's List output
// straight through.
func Resolve(rows []Record, ownerUserID string) []Record {
	// Bucket by layer first; index l-1 because Layer is 1-based.
	var byLayer [3][]Record
	for _, r := range rows {
		if r.OwnerUserID != "" && r.OwnerUserID != ownerUserID {
			continue // another user's rows are invisible to this owner
		}
		l := LayerOf(r)
		byLayer[l-1] = append(byLayer[l-1], r)
	}

	// Walk strongest to weakest. What a higher layer carries shadows the
	// same namespace (or, for DefaultNamespace, the same name) below it.
	var (
		shadowedNS   = make(map[string]bool)
		shadowedName = make(map[string]bool)
		resolved     []Record
	)
	for l := LayerUser; l >= LayerCore; l-- {
		layer := byLayer[l-1]
		if len(layer) == 0 {
			continue
		}

		// Claims are per layer, collected before filtering: rows in one
		// layer never shadow each other.
		layerNS := make(map[string]bool)
		layerName := make(map[string]bool)
		for _, r := range layer {
			if r.Namespace == DefaultNamespace {
				layerName[r.Name] = true
			} else {
				layerNS[r.Namespace] = true
			}
		}

		for _, r := range layer {
			if r.Namespace == DefaultNamespace {
				if !shadowedName[r.Name] {
					resolved = append(resolved, r)
				}
				continue
			}
			if !shadowedNS[r.Namespace] {
				resolved = append(resolved, r)
			}
		}

		for ns := range layerNS {
			shadowedNS[ns] = true
		}
		for name := range layerName {
			shadowedName[name] = true
		}
	}

	out := make([]Record, 0, len(resolved))
	for _, r := range resolved {
		if r.Enabled {
			out = append(out, r)
		}
	}

	// Stable so two rows that tie on (namespace, name) — an override pair
	// that somehow shares a layer — keep the store's own order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
