// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// sampleChild is a message exercising every protojson rendering quirk the
// helpers must normalize: an int64 (rendered as a string), a map, and an
// unset field (omitted).
func sampleChild() *rafikiv1.ChildSummary {
	return &rafikiv1.ChildSummary{
		ChildId:   "c_1",
		Name:      "alpha",
		Kind:      "fundi",
		Status:    "idle",
		StartedAt: 1700000000000,
		Labels:    map[string]string{"rafiki/parent": "c_0"},
	}
}

func TestProtoOutEmitProtoJSONIsCanonicalAndStable(t *testing.T) {
	// The exact bytes, so detrand's per-binary whitespace coin flip can never
	// leak through: protojson output that was not re-indented would differ
	// between rebuilds of the test binary and fail this golden comparison.
	const want = `{
  "childId": "c_1",
  "name": "alpha",
  "kind": "fundi",
  "status": "idle",
  "startedAt": "1700000000000",
  "labels": {
    "rafiki/parent": "c_0"
  }
}
`

	var first, second bytes.Buffer
	if err := emitProto(&first, sampleChild(), outputJSON); err != nil {
		t.Fatalf("emitProto json: %v", err)
	}
	if err := emitProto(&second, sampleChild(), outputJSON); err != nil {
		t.Fatalf("emitProto json (again): %v", err)
	}
	if first.String() != want {
		t.Fatalf("emitProto json =\n%s\nwant\n%s", first.String(), want)
	}
	// Same message marshalled twice gives identical bytes.
	if first.String() != second.String() {
		t.Fatalf("emitProto json not stable:\n%s\nvs\n%s", first.String(), second.String())
	}
}

func TestProtoOutEmitProtoJSONLOneCompactLine(t *testing.T) {
	var out bytes.Buffer
	if err := emitProto(&out, sampleChild(), outputJSONL); err != nil {
		t.Fatalf("emitProto jsonl: %v", err)
	}
	got := out.String()
	want := `{"childId":"c_1","name":"alpha","kind":"fundi","status":"idle","startedAt":"1700000000000","labels":{"rafiki/parent":"c_0"}}` + "\n"
	if got != want {
		t.Fatalf("emitProto jsonl = %q, want %q", got, want)
	}
	if strings.Contains(got, "\n") && !strings.HasSuffix(got, "\n") {
		t.Fatalf("emitProto jsonl split across lines: %q", got)
	}
}

func TestProtoOutEmitProtoRowsJSONEnvelope(t *testing.T) {
	rows := []*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha"},
		{ChildId: "c_2", Name: "beta"},
	}
	const want = `{
  "rows": [
    {
      "childId": "c_1",
      "name": "alpha"
    },
    {
      "childId": "c_2",
      "name": "beta"
    }
  ]
}
`
	var out bytes.Buffer
	if err := emitProtoRows(&out, rows, outputJSON); err != nil {
		t.Fatalf("emitProtoRows json: %v", err)
	}
	if out.String() != want {
		t.Fatalf("emitProtoRows json =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestProtoOutEmitProtoRowsJSONLHasNoEnvelope(t *testing.T) {
	rows := []*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha"},
		{ChildId: "c_2", Name: "beta"},
		{ChildId: "c_3"},
	}
	var out bytes.Buffer
	if err := emitProtoRows(&out, rows, outputJSONL); err != nil {
		t.Fatalf("emitProtoRows jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("got %d lines for %d rows: %q", len(lines), len(rows), out.String())
	}
	for i, line := range lines {
		if strings.Contains(line, "rows") {
			t.Fatalf("line %d carries an envelope: %q", i, line)
		}
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Fatalf("line %d is not a bare JSON object: %q", i, line)
		}
	}
	wantFirst := `{"childId":"c_1","name":"alpha"}`
	if lines[0] != wantFirst {
		t.Fatalf("first line = %q, want %q", lines[0], wantFirst)
	}
	wantLast := `{"childId":"c_3"}`
	if lines[2] != wantLast {
		t.Fatalf("last line = %q, want %q (unset fields are omitted)", lines[2], wantLast)
	}
}

func TestProtoOutEmitProtoRowsEmptyList(t *testing.T) {
	var rows []*rafikiv1.ChildSummary
	var jsonOut bytes.Buffer
	if err := emitProtoRows(&jsonOut, rows, outputJSON); err != nil {
		t.Fatalf("emitProtoRows json (empty): %v", err)
	}
	if got := jsonOut.String(); got != "{\n  \"rows\": []\n}\n" {
		t.Fatalf("emitProtoRows json (empty) = %q", got)
	}

	var jsonlOut bytes.Buffer
	if err := emitProtoRows(&jsonlOut, rows, outputJSONL); err != nil {
		t.Fatalf("emitProtoRows jsonl (empty): %v", err)
	}
	if got := jsonlOut.String(); got != "" {
		t.Fatalf("emitProtoRows jsonl (empty) = %q, want no output", got)
	}
}
