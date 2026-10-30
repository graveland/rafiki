// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"bytes"
	"encoding/json"
	"strings"
)

// StorableJSON makes a JSON value storable in a Postgres jsonb column, which
// cannot represent U+0000: a `\u0000` escape triggers SQLSTATE 22P05 on
// insert. Content is arbitrary conversation text, so strip U+0000 from every
// string. Values without the escape are returned verbatim — only NUL-bearing
// content pays the decode/re-encode. Invalid JSON is returned unchanged so the
// insert surfaces the real error.
//
// It is also what a node is HASHED as, so a message that differs only in NUL
// escapes is the same node.
func StorableJSON(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // preserve integer precision through the round-trip
	var v any
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(stripNUL(v))
	if err != nil {
		return b
	}
	return out
}

// stripNUL recursively removes U+0000 from every string (and map key) in a
// decoded JSON value. json.Number and other scalars pass through unchanged.
func stripNUL(v any) any {
	switch t := v.(type) {
	case string:
		if strings.IndexByte(t, 0) < 0 {
			return t
		}
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i, e := range t {
			t[i] = stripNUL(e)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[strings.ReplaceAll(k, "\x00", "")] = stripNUL(e)
		}
		return out
	default:
		return v
	}
}
