// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The export caps. They are deliberately constants rather than config: a span
// attribute is a fixed-size carrier, and a limit an operator can raise is one
// that eventually ships a megabyte to a collector.
const (
	userMessageMax    = 2048
	replySnippetRunes = 200
	toolArgLeafMax    = 256
	toolArgsMax       = 4096
	errorTextMax      = 65536
	errorTextHalf     = 32768
	hashHexLen        = 12
)

// UserMessage returns s capped at 2048 bytes on a rune boundary.
func UserMessage(s string) string {
	return s[:utf8PrefixLen(s, userMessageMax)]
}

// ReplySnippet returns the first 200 runes of s.
func ReplySnippet(s string) string {
	n := 0
	for i := range s {
		if n == replySnippetRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// ToolArgs shapes a tool call's argument JSON for export: string leaves longer
// than 256 bytes are replaced by a hash marker, and the encoded result is cut
// at 4096 bytes. truncated reports that the cut happened.
//
// Invalid JSON is treated as a single string leaf, so a non-JSON argument is
// either passed through whole or hashed, never dropped.
func ToolArgs(inputJSON string) (out string, truncated bool) {
	var v any
	if err := json.Unmarshal([]byte(inputJSON), &v); err != nil {
		v = shapeLeaf(inputJSON)
	} else {
		v = shapeValue(v)
	}
	enc, err := json.Marshal(v)
	if err != nil {
		// Unreachable: shapeValue only yields strings, numbers, bools, nil and
		// maps/slices of them. Fall back to the hash of the raw input.
		enc, _ = json.Marshal(hashLeaf(inputJSON))
	}
	if len(enc) > toolArgsMax {
		return string(enc[:utf8PrefixLen(string(enc), toolArgsMax)]), true
	}
	return string(enc), false
}

// ErrorText returns s unchanged up to 64 KiB; beyond that it keeps the first
// and last 32 KiB and marks how much was elided, so both the message head and
// the trailing cause survive.
//
// Invalid UTF-8 is replaced with U+FFFD before the cap is measured. This is
// the one capping function whose input need not have passed through a JSON or
// protobuf decoder — a provider's HTTP error body or binary-ish command output
// reaches it as raw bytes — and protobuf refuses to marshal a string field
// holding invalid UTF-8, which fails the whole export batch and loses the
// trace. The other three functions take already-decoded strings.
func ErrorText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= errorTextMax {
		return s
	}
	head := s[:utf8PrefixLen(s, errorTextHalf)]
	tail := s[utf8SuffixStart(s, errorTextHalf):]
	marker := fmt.Sprintf("\n…[%d bytes elided]…\n", len(s)-len(head)-len(tail))
	return head + marker + tail
}

// shapeValue replaces every oversized string leaf, recursing through maps and
// slices. The value is modified in place; it came from json.Unmarshal, so
// nothing else holds it.
func shapeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = shapeValue(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = shapeValue(e)
		}
		return x
	case string:
		return shapeLeaf(x)
	default: // numbers, bools, nil
		return v
	}
}

// shapeLeaf keeps a short string verbatim and replaces a long one with a hash
// marker, which keeps the shape of the arguments legible without the text.
func shapeLeaf(s string) any {
	if len(s) <= toolArgLeafMax {
		return s
	}
	return hashLeaf(s)
}

// hashLeaf is the marker standing in for an oversized string leaf.
func hashLeaf(s string) map[string]any {
	sum := sha256.Sum256([]byte(s))
	return map[string]any{
		"sha256": hex.EncodeToString(sum[:])[:hashHexLen],
		"bytes":  len(s),
	}
}

// utf8PrefixLen returns the largest n <= max such that s[:n] ends on a rune
// boundary (or len(s) when it is already short enough).
func utf8PrefixLen(s string, max int) int {
	if len(s) <= max {
		return len(s)
	}
	for n := max; n > 0; n-- {
		if utf8.RuneStart(s[n]) {
			return n
		}
	}
	return 0
}

// utf8SuffixStart returns the smallest n such that s[n:] is at most max bytes
// and begins on a rune boundary.
func utf8SuffixStart(s string, max int) int {
	n := len(s) - max
	if n <= 0 {
		return 0
	}
	for n < len(s) && !utf8.RuneStart(s[n]) {
		n++
	}
	return n
}
