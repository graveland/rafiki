package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Normalize rewrites stored message content into the shape the codec works in:
// an array of block objects, with the transport-only and shorthand forms
// expanded.
//
// It is generic JSON rather than typed blocks, so it works on a block the codec
// does not model at all. The substitutions are:
//
//   - a bare string becomes one text block;
//   - null becomes an empty array;
//   - every block object, at any depth (including inside a tool_result's
//     content), loses cache_control, which is a transport hint and not content;
//   - every block object loses a key whose value is empty — "", null, or an
//     empty array — because such a key is a marshalling artefact rather than
//     content: our own storage path writes the SDK's union without omitzero, so
//     real rows carry "id": "" and "data": "" siblings, and the API emits
//     "citations": []. Dropping them on BOTH sides of the codec's
//     self-verification is what keeps such a block typed instead of degrading it
//     to Opaque. A plain data object inside a block — a tool_use's input, an
//     image's source — keeps its own keys, empty or not, because they belong to
//     the tool and not to the block envelope;
//   - a tool_result whose content is a string gets one text block in its place,
//     and an explicit "is_error": false is removed, since it means the same as
//     its absence.
func Normalize(content []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("anthropic: normalize: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("anthropic: normalize: trailing data after JSON value")
	}
	if v == nil {
		return []byte("[]"), nil
	}
	out, err := json.Marshal(normalizeValue(v))
	if err != nil {
		return nil, fmt.Errorf("anthropic: normalize: %w", err)
	}
	return out, nil
}

// normalizeValue normalises one decoded JSON value in place: a bare string is
// content in the shorthand form, so it becomes a one-block array.
func normalizeValue(v any) any {
	if s, ok := v.(string); ok {
		return []any{textBlock(s)}
	}
	return normalizeElement(v, true)
}

// normalizeElement normalises one nested JSON value in place. isBlock says
// whether v is a block object (or an array of them), which is what decides
// whether the block-level rules apply. It deliberately omits the bare-string
// rule: an ordinary string field such as a block's "type" is a value, not
// content.
func normalizeElement(v any, isBlock bool) any {
	switch t := v.(type) {
	case []any:
		for i, el := range t {
			t[i] = normalizeElement(el, isBlock)
		}
		return t
	case map[string]any:
		normalizeObject(t, isBlock)
		return t
	}
	return v
}

// normalizeObject applies the block-level rules to obj in place, recursing into
// its values so a nested block is normalised by the same rules. A block's
// tool_result content holds blocks; every other value is plain data and is
// recursed into as such.
func normalizeObject(obj map[string]any, isBlock bool) {
	typ, _ := obj["type"].(string)
	for k, val := range obj {
		if k == "cache_control" {
			delete(obj, k)
			continue
		}
		if isBlock && emptyValue(val) {
			delete(obj, k)
			continue
		}
		if k == "content" && typ == "tool_result" {
			if s, ok := val.(string); ok {
				obj[k] = []any{textBlock(s)}
				continue
			}
		}
		obj[k] = normalizeElement(val, isBlock && k == "content" && typ == "tool_result")
	}
	if typ == "tool_result" {
		if isErr, ok := obj["is_error"].(bool); ok && !isErr {
			delete(obj, "is_error")
		}
	}
}

// emptyValue reports whether v carries no information: the empty string, null,
// or an empty array. A false boolean and a zero number are values, not absences,
// so they are kept.
func emptyValue(v any) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case nil:
		return true
	case []any:
		return len(t) == 0
	}
	return false
}

func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}
