// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const maxDiffPaths = 8

// describeOccupantMismatch says WHY a replayed request message was refused,
// without quoting any of it: roles, block types, and the JSON key paths whose
// values differ in the first differing block. It rides ErrOrdinalOccupied into
// logs and the turn's error text, where message content must not go.
func describeOccupantMismatch(storedRole, role string, stored, replay []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "role stored=%s replay=%s", storedRole, role)
	sb, sok := canonicalBlocks(stored)
	rb, rok := canonicalBlocks(replay)
	if !sok || !rok {
		fmt.Fprintf(&b, "; unparseable content (stored ok=%t, replay ok=%t)", sok, rok)
		return b.String()
	}
	fmt.Fprintf(&b, "; blocks stored=%v replay=%v", blockTypes(sb), blockTypes(rb))
	for i := 0; i < len(sb) && i < len(rb); i++ {
		var paths []string
		diffPaths(sb[i], rb[i], "", &paths)
		if len(paths) > 0 {
			fmt.Fprintf(&b, "; block %d differs at %s", i, strings.Join(paths, ","))
			break
		}
	}
	return b.String()
}

// canonicalBlocks is the form capture compares: the SDK round-trip when the
// content is Anthropic blocks, otherwise the cache_control-stripped JSON.
func canonicalBlocks(content []byte) ([]any, bool) {
	var raw []byte
	if blocks, ok := anthropicContentBlocks(content); ok {
		var err error
		if raw, err = json.Marshal(blocks); err != nil {
			return nil, false
		}
	} else if raw, ok = normalizeMessage(content); !ok {
		return nil, false
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

func blockTypes(blocks []any) []string {
	types := make([]string, len(blocks))
	for i, blk := range blocks {
		if m, ok := blk.(map[string]any); ok {
			types[i], _ = m["type"].(string)
		}
	}
	return types
}

func diffPaths(a, b any, path string, out *[]string) {
	if len(*out) >= maxDiffPaths {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := make(map[string]struct{}, len(am)+len(bm))
		for k := range am {
			keys[k] = struct{}{}
		}
		for k := range bm {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			diffPaths(am[k], bm[k], path+"/"+k, out)
		}
		return
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok {
		for i := 0; i < len(as) || i < len(bs); i++ {
			var ae, be any
			if i < len(as) {
				ae = as[i]
			}
			if i < len(bs) {
				be = bs[i]
			}
			diffPaths(ae, be, fmt.Sprintf("%s[%d]", path, i), out)
		}
		return
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		*out = append(*out, path)
	}
}
