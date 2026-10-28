// Package convtree defines the content-addressed identity of a conversation
// message: its canonical form and the chained path hash.
//
// A conversation message is identified by where it sits, not by its row id: a
// node's hash covers the hash of its parent slot and the canonical form of the
// message itself, so two histories that agree on every message produce the
// same path hashes regardless of how they were captured, replayed or
// re-marshalled on the way to storage.
package convtree

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
)

// HashVersion is the version of the hashing scheme, mixed into every node
// hash so a change to the canonical form or the chaining rule cannot collide
// with hashes written by an older version.
const HashVersion int16 = 1

// The kinds of node a chain can start from. A root's parent slot is a
// RootSeed rather than another node's hash.
const (
	RootKindRoot       = "root"
	RootKindCompaction = "compaction_summary"
	RootKindClear      = "clear"
)

// Hash is a path hash: the identity of one node in a conversation tree.
type Hash [32]byte

// Msg is one message as stored: role and the JSON of its content.
type Msg struct {
	Role    string
	Content []byte
}

// Canon returns the canonical form of one message: the JSON array
// [role, content] after the content has been normalised.
//
// The normalisation is generic JSON, not the API's typed blocks, because a
// round trip through the SDK's content union loses the fields that make two
// tool results or two images different (tool_result.content,
// tool_result.is_error, image.source). A bare JSON string is treated as a
// single text block, which is how the API accepts a shorthand prompt and how
// the stored form spells it.
//
// Normalisation drops cache_control (a transport hint, not content) and every
// null-valued key (an explicit null and an absent key mean the same thing),
// and json.Marshal sorts map keys, so key order and whitespace in the stored
// JSON cannot change the hash.
func Canon(role string, content []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing data after JSON value")
		}
		return nil, err
	}
	if s, ok := v.(string); ok {
		v = []any{map[string]any{"type": "text", "text": s}}
	}
	return json.Marshal([]any{role, normalize(v)})
}

// Equal reports whether two messages canonicalise equal. A message that
// cannot be canonicalised is equal to nothing, including another invalid one.
func Equal(a, b Msg) bool {
	ca, err := Canon(a.Role, a.Content)
	if err != nil {
		return false
	}
	cb, err := Canon(b.Role, b.Content)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// RootSeed returns the parent slot a root chain starts from.
//
// The seed is derived from the derived-from hash only for a clear, and only a
// clear: a clear boundary is a constant (store.ClearBoundaryText), so two
// clears are distinguishable only by the node they were cut from. A
// compaction summary is unique model output, so an equal summary under an
// equal history IS the same history and must hash the same — and seeding
// compaction roots from derived-from would make the backfill disagree with
// live capture, where the summary names the reply it summarised rather than
// the last row before it. Every other root kind ignores derivedFrom.
func RootSeed(kind string, derivedFrom *Hash) Hash {
	var d Hash
	if kind == RootKindClear && derivedFrom != nil {
		d = *derivedFrom
	}
	return sha256.Sum256(append(append([]byte("root"), kind...), d[:]...))
}

// NodeHash returns the path hash of one message appended under parentSlot,
// which is the parent's path hash or, for a root, its RootSeed.
func NodeHash(parentSlot Hash, m Msg) (Hash, error) {
	canon, err := Canon(m.Role, m.Content)
	if err != nil {
		return Hash{}, fmt.Errorf("convtree: canon: %w", err)
	}
	h := sha256.New()
	h.Write([]byte{byte(HashVersion)})
	h.Write(parentSlot[:])
	h.Write(canon)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// Chain returns the path hashes of msgs appended in order under parentSlot:
// h[0] from parentSlot, h[i] from h[i-1].
func Chain(parentSlot Hash, msgs []Msg) ([]Hash, error) {
	out := make([]Hash, len(msgs))
	slot := parentSlot
	for i, m := range msgs {
		h, err := NodeHash(slot, m)
		if err != nil {
			return nil, err
		}
		out[i] = h
		slot = h
	}
	return out, nil
}

// normalize drops cache_control and null-valued keys from every object and
// recurses into the rest, in place.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "cache_control" || val == nil {
				delete(t, k)
				continue
			}
			t[k] = normalize(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalize(val)
		}
		return t
	}
	return v
}
