// SPDX-License-Identifier: Apache-2.0

package llmtest_test

import (
	"context"
	"io"
	"testing"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// Fake satisfies the sender seam it stands in for.
var _ llm.Sender = (*llmtest.Fake)(nil)

// TestFakeReplaysAResponseAsEvents pins the stream shape: draining the events
// through the accumulator reproduces the scripted response exactly, including
// the tool input, the thinking signature and the served-by provider.
func TestFakeReplaysAResponseAsEvents(t *testing.T) {
	c := assert.NewAborting(t)
	want := llmtest.FromAnthropicJSON(`{"id":"msg_1","model":"claude-x","provider":"Together",` +
		`"content":[{"type":"text","text":"hi"},` +
		`{"type":"thinking","thinking":"why","signature":"sig"},` +
		`{"type":"tool_use","id":"tu_1","name":"read","input":{"path":"a"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":3}}`)
	f := &llmtest.Fake{Responses: []*msg.Response{want}}

	dec, err := f.Stream(context.Background(), &msg.Request{Model: "claude-x"}, msg.EncodeOptions{})
	c.NoError(err, "Stream")
	acc := msg.NewAccumulator()
	for {
		ev, derr := dec.Next()
		if derr == io.EOF {
			break
		}
		c.NoError(derr, "Next")
		c.NoError(acc.Add(ev), "Add(%T)", ev)
	}
	c.NoError(dec.Close(), "Close")
	c.EqDeep(want, acc.Response(), "the replayed events must accumulate to the scripted response")
}

// TestFakeScriptsErrorsAndRepeatsTheLastEntry pins the script rules: an error
// entry answers that call, and the final response repeats once the script runs
// out.
func TestFakeScriptsErrorsAndRepeatsTheLastEntry(t *testing.T) {
	c := assert.NewAborting(t)
	boom := &msg.APIError{Status: 500, Kind: msg.ErrTransient}
	f := &llmtest.Fake{
		Errs:      []error{boom},
		Responses: []*msg.Response{llmtest.FromAnthropicJSON(`{"id":"msg_1"}`)},
	}
	_, err := f.Send(context.Background(), &msg.Request{}, msg.EncodeOptions{})
	c.Eq(error(boom), err, "the first call answers its error entry")

	for i := range 2 {
		resp, serr := f.Send(context.Background(), &msg.Request{}, msg.EncodeOptions{})
		c.NoError(serr, "call %d", i+2)
		c.Eq("msg_1", resp.ID, "the last response repeats")
	}
	c.Eq(3, f.Calls(), "every call is recorded")
}
