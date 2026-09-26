// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The -o json/-j and -J contract for Connect-backed verbs: the canonical
// protojson of the response message, no client-side output structs. protojson
// deliberately randomises its whitespace (internal/detrand, seeded per binary)
// to discourage string-matching its output, so both helpers normalise the
// bytes through encoding/json — Compact for JSONL, Indent for pretty — which
// makes the output canonical and byte-stable for a given message regardless
// of which coin flip this binary was built with.

// marshalProtoJSON marshals m as canonical, compact protojson: camelCase
// names, fields in field-number order, no injected spaces.
func marshalProtoJSON(m proto.Message) ([]byte, error) {
	b, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("protojson: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return nil, fmt.Errorf("compact protojson: %w", err)
	}
	return compact.Bytes(), nil
}

// emitProto writes one proto message as pretty JSON (outputJSON) or one
// compact line (outputJSONL). Any other mode is treated as pretty JSON: the
// helpers are only reached from a caller that already branched on
// -o json/-j/-J and renders its own table for auto/table.
//
// The pretty form is the canonical protojson re-indented with the two-space
// indent writeJSON uses, so both encodings agree on field names, order and
// value shapes (protojson renders int64 as a string — accepted, it is what
// rafiki-py sees too).
func emitProto(w io.Writer, m proto.Message, mode outputMode) error {
	b, err := marshalProtoJSON(m)
	if err != nil {
		return err
	}
	if mode == outputJSONL {
		if _, err := w.Write(b); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	}
	return writeIndentedJSON(w, b)
}

// emitProtoRows writes a list of proto messages: outputJSON → the envelope
// {"rows":[...]} pretty; outputJSONL → one compact message per line with NO
// envelope, so a piped consumer reads one record per line (writeJSONL's
// contract, carried over for protojson rows). Row order is preserved; an
// empty list writes `{"rows": []}` in JSON mode and nothing at all in JSONL.
func emitProtoRows[T proto.Message](w io.Writer, rows []T, mode outputMode) error {
	if mode == outputJSONL {
		for _, row := range rows {
			b, err := marshalProtoJSON(row)
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
		return nil
	}
	parts := make([]string, len(rows))
	for i, row := range rows {
		b, err := marshalProtoJSON(row)
		if err != nil {
			return err
		}
		parts[i] = string(b)
	}
	// json.Indent re-validates the joined bytes, so a marshal bug surfaces as
	// an error rather than as malformed output.
	env := []byte(`{"rows":[` + strings.Join(parts, ",") + `]}`)
	return writeIndentedJSON(w, env)
}

// writeIndentedJSON re-indents canonical JSON bytes with writeJSON's two-space
// indent and appends the newline json.Encoder.Encode would.
func writeIndentedJSON(w io.Writer, b []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return fmt.Errorf("indent json: %w", err)
	}
	out.WriteByte('\n')
	_, err := w.Write(out.Bytes())
	return err
}
