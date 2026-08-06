// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestFixEmptyToolInput(t *testing.T) {
	tests := []struct {
		name    string
		rawJSON string
		fixed   bool
	}{
		{
			name:    "tool_use with empty string input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":""}}`,
			fixed:   true,
		},
		{
			name:    "tool_use with object input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":{"path":"/foo"}}}`,
			fixed:   false,
		},
		{
			name:    "server_tool_use with empty string input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"toolu_01","name":"read","input":""}}`,
			fixed:   true,
		},
		{
			name:    "text block (not tool_use)",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			fixed:   false,
		},
		{
			name:    "message_start (not content_block_start)",
			rawJSON: `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-3","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`,
			fixed:   false,
		},
		{
			name:    "tool_use with null input",
			rawJSON: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read","input":null}}`,
			fixed:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ev anthropic.MessageStreamEventUnion
			if err := json.Unmarshal([]byte(tt.rawJSON), &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			FixEmptyToolInput(&ev)

			// Verify the event can be accumulated without error.
			var acc anthropic.Message
			if aerr := acc.Accumulate(ev); aerr != nil {
				t.Fatalf("accumulate: %v", aerr)
			}
			// For content_block_start events, also simulate the corresponding
			// content_block_stop to trigger the marshal path that fails when
			// input is an empty string.
			if ev.Type == "content_block_start" {
				stopEv := anthropic.MessageStreamEventUnion{}
				if err := json.Unmarshal([]byte(`{"type":"content_block_stop","index":0}`), &stopEv); err != nil {
					t.Fatalf("unmarshal stop: %v", err)
				}
				if aerr := acc.Accumulate(stopEv); aerr != nil {
					t.Fatalf("accumulate content_block_stop: %v", aerr)
				}
			}
		})
	}
}
