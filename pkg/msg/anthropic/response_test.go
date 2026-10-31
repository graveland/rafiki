package anthropic_test

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// TestResponseJSONUsageByPresence pins the storage boundary's presence rule for
// every count, Reasoning included: a count the provider did not report is
// OMITTED from the recorded body, and a reported zero stays a recorded zero.
func TestResponseJSONUsageByPresence(t *testing.T) {
	c := assert.NewAborting(t)
	resp := &msg.Response{
		ID:         "msg_1",
		Model:      "m",
		StopReason: msg.StopEndTurn,
		Usage: msg.Usage{
			Input:     msg.Ptr(0),
			Output:    msg.Ptr(5),
			Reasoning: msg.Ptr(0),
		},
	}
	body, err := anthropic.ResponseJSON(resp)
	c.NoError(err, "ResponseJSON")

	var stored struct {
		StopReason string                     `json:"stop_reason"`
		Usage      map[string]json.RawMessage `json:"usage"`
	}
	c.NoError(json.Unmarshal(body, &stored), "unmarshal the recorded body")
	c.Eq("end_turn", stored.StopReason, "a modelled stop reason keeps its wire spelling")
	c.HasKey(stored.Usage, "input_tokens", "a reported zero is recorded")
	c.Eq("0", string(stored.Usage["input_tokens"]), "a reported zero stays zero")
	c.HasKey(stored.Usage, "output_tokens", "a reported count is recorded")
	c.HasKey(stored.Usage, "reasoning_tokens", "a decoded reasoning count must reach the record")
	c.Eq("0", string(stored.Usage["reasoning_tokens"]), "a reported reasoning zero stays zero")
	c.NotHasKey(stored.Usage, "cache_read_input_tokens", "an unreported count is omitted, never recorded as zero")
	c.NotHasKey(stored.Usage, "cache_creation_input_tokens", "an unreported count is omitted, never recorded as zero")
}
