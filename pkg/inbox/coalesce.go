// SPDX-License-Identifier: Apache-2.0

package inbox

import "fmt"

// BatchConfig caps one delivered batch. Zero means unlimited for both fields.
type BatchConfig struct {
	MaxFragments     int
	MaxBytesPerFlush int
}

// Batch is one frame's worth of inbox rows.
//
// IDs is every row the batch ACCOUNTS FOR, which is not the same as every row
// whose text survived: a row superseded by a later write on the same key, or
// dropped by a cap, is still acked. Leaving it pending would redeliver it
// forever, and it has already been represented — by the newer text, or by the
// omission marker.
type Batch struct {
	ChildID string
	Mode    Mode
	Source  string
	IDs     []string
	Frags   []string
}

// Coalesce turns pending rows into the batches to deliver, in delivery order.
//
// Direct messages (Source == "") are never coalesced: each is its own batch,
// in arrival order, because a human's two prompts are two turns' worth of
// intent and merging them silently rewrites what was said. Fragments ARE
// coalesced, per source, which is the whole reason eventbuf exists — five
// workers finishing together cost one turn rather than five.
//
// This is a pure function over rows, which is the point: the coalesced shape
// is recomputed at delivery instead of persisted, so a crash mid-debounce
// loses nothing and re-coalesces correctly on restart.
func Coalesce(rows []Inbound, cfg BatchConfig) []Batch {
	var out []Batch

	type group struct {
		mode     Mode
		ids      []string
		keyed    map[string]string
		keyOrder []string
		unkeyed  []string
	}
	groups := make(map[string]*group)
	var groupOrder []string

	for _, r := range rows {
		if r.Source == "" {
			b := Batch{ChildID: r.ChildID, Mode: r.Mode, IDs: []string{r.ID}}
			if r.Mode != ModeAbort {
				b.Frags = []string{r.Text}
			}
			out = append(out, b)
			continue
		}
		g := groups[r.Source]
		if g == nil {
			g = &group{mode: ModePrompt, keyed: make(map[string]string)}
			groups[r.Source] = g
			groupOrder = append(groupOrder, r.Source)
		}
		g.ids = append(g.ids, r.ID)
		// Sticky steer, expressed as data rather than as buffer state: a group
		// holding any steer delivers as a steer, so a steer deferred behind a
		// later prompt cannot be quietly downgraded to a prompt.
		if r.Mode == ModeSteer {
			g.mode = ModeSteer
		}
		if r.Key != "" {
			if _, seen := g.keyed[r.Key]; !seen {
				g.keyOrder = append(g.keyOrder, r.Key)
			}
			g.keyed[r.Key] = r.Text
			continue
		}
		g.unkeyed = append(g.unkeyed, r.Text)
	}

	for _, source := range groupOrder {
		g := groups[source]
		var frags []string
		for _, k := range g.keyOrder {
			frags = append(frags, g.keyed[k])
		}
		frags = append(frags, g.unkeyed...)
		out = append(out, Batch{
			ChildID: rows[0].ChildID,
			Mode:    g.mode,
			Source:  source,
			IDs:     g.ids,
			Frags:   applyCaps(frags, cfg),
		})
	}
	return out
}

// applyCaps enforces MaxFragments and THEN MaxBytesPerFlush. Both apply: an
// early return from the fragment cap would skip the byte budget exactly when
// the batch is largest.
func applyCaps(all []string, cfg BatchConfig) []string {
	total := len(all)
	omitted := 0

	if cfg.MaxFragments > 0 && total > cfg.MaxFragments {
		kept := cfg.MaxFragments - 1
		if kept < 0 {
			kept = 0
		}
		omitted = total - kept
		all = all[:kept]
	}

	if cfg.MaxBytesPerFlush > 0 {
		used := 0
		for i, s := range all {
			if used+len(s) > cfg.MaxBytesPerFlush {
				omitted += len(all) - i
				all = all[:i]
				break
			}
			used += len(s)
		}
	}

	if omitted > 0 {
		all = append(all, fmt.Sprintf("[%d fragment(s) omitted]", omitted))
	}
	return all
}
