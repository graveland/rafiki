// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestFixEmptyToolInputAndAccumulate(t *testing.T) {
	tests := []struct {
		name    string
		rawJSON string
		fixed   bool // whether FixEmptyToolInput on the event mutated it
		needFix bool // whether the accumulated ContentBlockUnion needed FixAccumulatedEmptyToolInput
		deltas  []string
	}{
		{
			name:    "tool_use with empty string input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":""}}`,
			fixed:   true,
			needFix: true,
			deltas:  []string{`{"path":"/foo"}`},
		},
		{
			name:    "tool_use with object input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":{"path":"/foo"}}}`,
			fixed:   false,
			needFix: false,
			deltas:  nil,
		},
		{
			name:    "server_tool_use with empty string input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"toolu_01","name":"read","input":""}}`,
			fixed:   true,
			needFix: true,
			deltas:  []string{`{"path":"/bar"}`},
		},
		{
			name:    "text block (not tool_use)",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			fixed:   false,
			needFix: false,
			deltas:  nil,
		},
		{
			name:    "message_start (not content_block_start)",
			rawJSON: `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-3","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`,
			fixed:   false,
			needFix: false,
			deltas:  nil,
		},
		{
			name:    "tool_use with null input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":null}}`,
			fixed:   false,
			needFix: false,
			deltas:  []string{`{"path":"/baz"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ev anthropic.MessageStreamEventUnion
			if err := json.Unmarshal([]byte(tt.rawJSON), &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			// FixEmptyToolInput operates on the typed event fields (Input any).
			// The typed-input fix is redundant because the SDK's Accumulate
			// ignores it (it reconstructs ContentBlockUnion from RawJSON), but
			// we keep it as a defense-in-depth measure.
			FixEmptyToolInput(&ev)

			// Accumulate the content_block_start.
			var acc anthropic.Message
			if aerr := acc.Accumulate(ev); aerr != nil {
				t.Fatalf("accumulate content_block_start: %v", aerr)
			}

			// FixAccumulatedEmptyToolInput patches the ContentBlockUnion.Input
			// (json.RawMessage) that the SDK constructed from RawJSON.
			fixedAcc := FixAccumulatedEmptyToolInput(&acc, ev)
			if fixedAcc != tt.needFix {
				t.Errorf("FixAccumulatedEmptyToolInput returned %v, want %v", fixedAcc, tt.needFix)
			}

			// Send InputJSONDelta events (the real trigger for the bug).
			for i, deltaJSON := range tt.deltas {
				// The delta JSON is embedded as a JSON string inside partial_json,
				// so its quotes must be escaped.
				escaped := strings.ReplaceAll(deltaJSON, `"`, `\"`)
				deltaRaw := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + escaped + `"}}`
				var deltaEv anthropic.MessageStreamEventUnion
				if err := json.Unmarshal([]byte(deltaRaw), &deltaEv); err != nil {
					t.Fatalf("unmarshal delta %d: %v", i, err)
				}
				if aerr := acc.Accumulate(deltaEv); aerr != nil {
					t.Fatalf("accumulate input_json_delta %d: %v", i, aerr)
				}
			}

			// content_block_stop triggers the marshal path that fails when
			// Input is corrupt (e.g. ""{"path":"/foo"}). Only send for
			// content_block_start events — message_start has no content block.
			if ev.Type == "content_block_start" {
				stopEv := anthropic.MessageStreamEventUnion{}
				if err := json.Unmarshal([]byte(`{"type":"content_block_stop","index":0}`), &stopEv); err != nil {
					t.Fatalf("unmarshal stop: %v", err)
				}
				SanitizeInvalidAccumulatedInput(&acc, stopEv)
				if aerr := acc.Accumulate(stopEv); aerr != nil {
					t.Fatalf("accumulate content_block_stop: %v", aerr)
				}
			}
		})
	}
}

// TestSanitizeInvalidAccumulatedInput covers corruption that the two
// "" -> {} fixes above don't target: an Input left as truncated/malformed
// JSON (e.g. by an interrupted delta sequence) rather than the empty-string
// seed value they specifically look for. Without the sanitize call,
// acc.Accumulate on the stop event fails with "unexpected end of JSON input"
// because json.RawMessage.MarshalJSON validates its bytes as JSON.
func TestSanitizeInvalidAccumulatedInput(t *testing.T) {
	tests := []struct {
		name      string
		input     json.RawMessage
		wantValid bool // whether Input is untouched (nil or already valid)
	}{
		{name: "truncated object", input: json.RawMessage(`{"co`), wantValid: false},
		{name: "empty non-nil", input: json.RawMessage{}, wantValid: false},
		{name: "nil left alone", input: nil, wantValid: true},
		{name: "valid object left alone", input: json.RawMessage(`{"path":"/foo"}`), wantValid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := anthropic.Message{
				Content: []anthropic.ContentBlockUnion{{Type: "tool_use", ID: "toolu_01", Name: "read", Input: tt.input}},
			}
			stopEv := anthropic.MessageStreamEventUnion{}
			if err := json.Unmarshal([]byte(`{"type":"content_block_stop","index":0}`), &stopEv); err != nil {
				t.Fatalf("unmarshal stop: %v", err)
			}

			SanitizeInvalidAccumulatedInput(&acc, stopEv)

			if tt.wantValid {
				if string(acc.Content[0].Input) != string(tt.input) {
					t.Errorf("Input mutated: got %q, want untouched %q", acc.Content[0].Input, tt.input)
				}
			} else if !json.Valid(acc.Content[0].Input) {
				t.Errorf("Input still invalid after sanitize: %q", acc.Content[0].Input)
			}

			if aerr := acc.Accumulate(stopEv); aerr != nil {
				t.Fatalf("accumulate content_block_stop after sanitize: %v", aerr)
			}
		})
	}
}

// TestSanitizeInvalidAccumulatedInput_MessageStop covers the message_stop
// path, which marshals the whole accumulated message rather than only the
// last content block.
func TestSanitizeInvalidAccumulatedInput_MessageStop(t *testing.T) {
	acc := anthropic.Message{
		Content: []anthropic.ContentBlockUnion{
			{Type: "text", Text: "hi"},
			{Type: "tool_use", ID: "toolu_01", Name: "read", Input: json.RawMessage(`{"pa`)},
		},
	}
	stopEv := anthropic.MessageStreamEventUnion{}
	if err := json.Unmarshal([]byte(`{"type":"message_stop"}`), &stopEv); err != nil {
		t.Fatalf("unmarshal message_stop: %v", err)
	}

	SanitizeInvalidAccumulatedInput(&acc, stopEv)

	if aerr := acc.Accumulate(stopEv); aerr != nil {
		t.Fatalf("accumulate message_stop after sanitize: %v", aerr)
	}
}
