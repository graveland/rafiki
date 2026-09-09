// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestResumeTextLine(t *testing.T) {
	raw := json.RawMessage(`{"childId":"c_9","model":"m"}`)
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
			if err := renderResume(&buf, tc.childID, tc.found, raw, outputTable); err != nil {
				t.Fatalf("renderResume: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// The ctrl_resume payload rides SpawnResponseData, shared with ctrl_spawn, so
// its JSON must keep passing through untouched — the pre-tables encoding was
// exactly Encoder(SetIndent)+Encode over the raw response.
func TestResumeJSONRawPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"childId":"c_9","sessionId":"s_1","model":"m"}`)

	var buf bytes.Buffer
	if err := renderResume(&buf, "c_9", "worker", raw, outputJSON); err != nil {
		t.Fatalf("renderResume json: %v", err)
	}
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		t.Fatalf("encode reference: %v", err)
	}
	if buf.String() != want.String() {
		t.Errorf("json output changed:\nold: %s\nnew: %s", want.String(), buf.String())
	}

	// JSONL: the record as one compact line.
	var jl bytes.Buffer
	if err := renderResume(&jl, "c_9", "", raw, outputJSONL); err != nil {
		t.Fatalf("renderResume jsonl: %v", err)
	}
	if jl.String() != `{"childId":"c_9","sessionId":"s_1","model":"m"}`+"\n" {
		t.Errorf("jsonl output = %q, want one compact line", jl.String())
	}
}
