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
	return normalizeElement(v)
}

// normalizeElement normalises one nested JSON value in place. It deliberately
// omits the bare-string rule: an ordinary string field such as a block's "type"
// is a value, not content.
func normalizeElement(v any) any {
	switch t := v.(type) {
	case []any:
		for i, el := range t {
			t[i] = normalizeElement(el)
		}
		return t
	case map[string]any:
		normalizeObject(t)
		return t
	}
	return v
}

// normalizeObject applies the block-level rules to obj in place, recursing into
// its values so a nested block is normalised by the same rules.
func normalizeObject(obj map[string]any) {
	typ, _ := obj["type"].(string)
	for k, val := range obj {
		if k == "cache_control" {
			delete(obj, k)
			continue
		}
		if k == "content" && typ == "tool_result" {
			if s, ok := val.(string); ok {
				obj[k] = []any{textBlock(s)}
				continue
			}
		}
		obj[k] = normalizeElement(val)
	}
	if typ == "tool_result" {
		if isErr, ok := obj["is_error"].(bool); ok && !isErr {
			delete(obj, "is_error")
		}
	}
}

func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}
