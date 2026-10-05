// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// sampleChild is a message exercising every protojson rendering quirk the
// helpers must normalize: a Timestamp (rendered RFC3339), a map, and an unset
// field (omitted).
func sampleChild() *rafikiv1.ChildSummary {
	return &rafikiv1.ChildSummary{
		ChildId:   "c_1",
		Name:      "alpha",
		Kind:      "fundi",
		Status:    "idle",
		StartedAt: timestamppb.New(time.UnixMilli(1700000000000)),
		Labels:    map[string]string{"rafiki/parent": "c_0"},
	}
}

func TestProtoOutEmitProtoJSONIsCanonicalAndStable(t *testing.T) {
	c := assert.NewAborting(t)
	// The exact bytes, so detrand's per-binary whitespace coin flip can never
	// leak through: protojson output that was not re-indented would differ
	// between rebuilds of the test binary and fail this golden comparison.
	const want = `{
  "childId": "c_1",
  "name": "alpha",
  "kind": "fundi",
  "status": "idle",
  "startedAt": "2023-11-14T22:13:20Z",
  "labels": {
    "rafiki/parent": "c_0"
  }
}
`

	var first, second bytes.Buffer
	c.NoError(emitProto(&first, sampleChild(), outputJSON), "emitProto json")
	c.NoError(emitProto(&second, sampleChild(), outputJSON), "emitProto json (again)")
	c.Eq(want, first.String(), "emitProto json =\n")
	// Same message marshalled twice gives identical bytes.
	c.Eq(second.String(), first.String(), "emitProto json not stable:\n")
}

func TestProtoOutEmitProtoJSONLOneCompactLine(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	c.NoError(emitProto(&out, sampleChild(), outputJSONL), "emitProto jsonl")
	got := out.String()
	want := `{"childId":"c_1","name":"alpha","kind":"fundi","status":"idle","startedAt":"2023-11-14T22:13:20Z","labels":{"rafiki/parent":"c_0"}}` + "\n"
	c.Eq(want, got, "emitProto jsonl")
	c.False(strings.Contains(got, "\n") && !strings.HasSuffix(got, "\n"), "emitProto jsonl split across lines: %q", got)
}

func TestProtoOutEmitProtoRowsJSONEnvelope(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(emitProtoRows(&out, rows, outputJSON), "emitProtoRows json")
	c.Eq(want, out.String(), "emitProtoRows json =\n")
}

func TestProtoOutEmitProtoRowsJSONLHasNoEnvelope(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []*rafikiv1.ChildSummary{
		{ChildId: "c_1", Name: "alpha"},
		{ChildId: "c_2", Name: "beta"},
		{ChildId: "c_3"},
	}
	var out bytes.Buffer
	c.NoError(emitProtoRows(&out, rows, outputJSONL), "emitProtoRows jsonl")
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	c.Len(lines, len(rows), "got %d lines for %d rows: %q", len(lines), len(rows), out.String())
	for i, line := range lines {
		c.NotStrContains(line, "rows", "line %d carries an envelope", i)
		c.False(!strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}"), "line %d is not a bare JSON object: %q", i, line)
	}
	wantFirst := `{"childId":"c_1","name":"alpha"}`
	c.Eq(wantFirst, lines[0], "first line")
	wantLast := `{"childId":"c_3"}`
	c.Eq(wantLast, lines[2], "last line")
}

func TestProtoOutEmitProtoRowsEmptyList(t *testing.T) {
	c := assert.NewAborting(t)
	var rows []*rafikiv1.ChildSummary
	var jsonOut bytes.Buffer
	c.NoError(emitProtoRows(&jsonOut, rows, outputJSON), "emitProtoRows json (empty)")
	c.Eq("{\n  \"rows\": []\n}\n", jsonOut.String(), "emitProtoRows json (empty) =")

	var jsonlOut bytes.Buffer
	c.NoError(emitProtoRows(&jsonlOut, rows, outputJSONL), "emitProtoRows jsonl (empty)")
	c.Eq("", jsonlOut.String(), "emitProtoRows jsonl (empty)")
}
