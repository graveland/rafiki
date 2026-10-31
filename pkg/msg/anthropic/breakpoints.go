package anthropic

import "go.graveland.dev/rafiki/pkg/msg"

// Position locates one cache breakpoint: the message index and, within that
// message, the content-block index the marker belongs on.
type Position struct{ Message, Block int }

// lookbackStride spaces consecutive breakpoints. The API's cache lookback is
// 20 blocks, so a breakpoint no more than 15 blocks behind the previous one is
// always reachable.
const lookbackStride = 15

// Breakpoints returns where the moving prompt-cache markers belong in req: the
// last cacheable block, then each earlier one at least lookbackStride blocks
// back, up to req.Cache.Breakpoints (clamped to [1, 3]). The result is in
// request order — ascending by message, then by block — and nil when
// req.Cache.MessagesTTL asks for no caching.
//
// Placement walks the same view Encode sends, so a block the foreign-block rule
// removes is never picked: a marker on a dropped block would be dropped with it
// and the turn would carry no marker at all. Positions are the caller's own
// block indices, which is what Encode maps onto the filtered messages.
//
// A block is cacheable unless it is a thinking block, which the API refuses to
// carry cache_control on. Every block that survives filtering counts toward the
// stride, cacheable or not, because the stride exists to keep a marker inside
// the lookback window of the one before it.
//
// The function is pure: it reads req and returns positions, and the codec
// itself uses it to place the markers it encodes. Callers who need to know
// where a request's markers landed call it rather than re-deriving the rule.
func Breakpoints(req *msg.Request) []Position {
	if req == nil {
		return nil
	}
	filtered, remap := dropForeign(req.Messages)
	out := breakpoints(filtered, req.Cache)
	for i, p := range out {
		out[i].Block = sourceIndex(remap[p.Message], p.Block)
	}
	return out
}

// breakpoints places the markers over msgs, the filtered view Encode will send,
// and returns positions in that view.
func breakpoints(msgs []msg.Message, cache msg.CachePolicy) []Position {
	if cache.MessagesTTL == msg.CacheOff || cache.MessagesTTL == "" {
		return nil
	}
	limit := cache.Breakpoints
	if limit < 1 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}

	var out []Position
	placed, walked := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		content := msgs[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			walked++
			if !cacheableBlock(content[j]) {
				continue
			}
			if placed >= limit || (placed > 0 && walked < lookbackStride) {
				continue
			}
			out = append(out, Position{Message: i, Block: j})
			placed++
			walked = 0
		}
	}
	// The walk is backward, so the positions are too.
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// sourceIndex maps a block's index in a filtered message back to its index in
// the message the caller wrote. remap carries -1 for a dropped block, so a
// filtered block that survives has exactly one source index.
func sourceIndex(remap []int, filtered int) int {
	for orig, f := range remap {
		if f == filtered {
			return orig
		}
	}
	return -1
}

// cacheableBlock reports whether b may carry a cache breakpoint. A thinking
// block — typed, or kept opaque as redacted_thinking — cannot: the API rejects
// cache_control on it.
func cacheableBlock(b msg.Block) bool {
	switch t := b.(type) {
	case msg.Thinking:
		return false
	case msg.Opaque:
		switch msg.OpaqueType(t) {
		case "thinking", "redacted_thinking":
			return false
		}
	}
	return true
}
