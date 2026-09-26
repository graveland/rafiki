// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestResumeTextLine(t *testing.T) {
	cases := []struct {
		name    string
		childID string
		found   string
		want    string
	}{
		{"named", "c_9", "worker", "resumed c_9 (worker)\n"},
		{"name could not be resolved", "c_9", "", "resumed c_9\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderResume(&buf, tc.childID, tc.found, &rafikiv1.ResumeResponse{ChildId: "c_9"}, outputTable); err != nil {
				t.Fatalf("renderResume: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// The Resume response's canonical protojson is the JSON contract now — pretty
// in -j, one compact line in -J. The wire response carries the child id only
// (the framed payload's SpawnResponseData extras are gone from the wire).
func TestResumeJSONProtojson(t *testing.T) {
	resp := &rafikiv1.ResumeResponse{ChildId: "c_9"}

	var buf bytes.Buffer
	if err := renderResume(&buf, "c_9", "worker", proto.Message(resp), outputJSON); err != nil {
		t.Fatalf("renderResume json: %v", err)
	}
	const wantJSON = "{\n  \"childId\": \"c_9\"\n}\n"
	if buf.String() != wantJSON {
		t.Errorf("json output = %q, want %q", buf.String(), wantJSON)
	}

	// JSONL: the record as one compact line.
	var jl bytes.Buffer
	if err := renderResume(&jl, "c_9", "", proto.Message(resp), outputJSONL); err != nil {
		t.Fatalf("renderResume jsonl: %v", err)
	}
	if jl.String() != `{"childId":"c_9"}`+"\n" {
		t.Errorf("jsonl output = %q, want one compact line", jl.String())
	}
}
