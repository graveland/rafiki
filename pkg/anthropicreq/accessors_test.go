// SPDX-License-Identifier: Apache-2.0

package anthropicreq

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/claudethread"
)

// The oracles below are the implementations each accessor replaced, copied
// verbatim from the call sites this package took over (pkg/server's
// requestsStream, pkg/capture's splitEnvelopeAndBreakpoints/requestMessages and
// messageHasCacheControl). They are test-only on purpose: they are what "same
// behaviour" means, so a change to a rule must be made in two places — the
// accessor and its oracle — which is the point.
//
// The thread-signal oracles are not copies: pkg/claudethread still exports its
// byte-slice form for the OpenAI face, and that is the oracle.

func oracleRequestsStream(body []byte) bool {
	var s struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &s)
	return s.Stream
}

func oracleModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// oracleMessageHasCacheControl is the three-branch rule: a block array is
// inspected per block, a plain string is never a breakpoint, and anything else
// falls back to a conservative substring check.
func oracleMessageHasCacheControl(content json.RawMessage) bool {
	var blocks []struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			if len(b.CacheControl) > 0 {
				return true
			}
		}
		return false
	}
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return false
	}
	return bytes.Contains(content, []byte(`"cache_control"`))
}

func oracleSplitEnvelopeAndBreakpoints(reqBody []byte) (envelope []byte, breakpoints []byte, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &m); err != nil {
		return nil, nil, err
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	idxs := []int{}
	if raw, ok := m["messages"]; ok {
		if err := json.Unmarshal(raw, &msgs); err == nil {
			for i, mm := range msgs {
				if oracleMessageHasCacheControl(mm.Content) {
					idxs = append(idxs, i)
				}
			}
		}
	}
	delete(m, "messages")
	envelope, err = json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	breakpoints, err = json.Marshal(idxs)
	if err != nil {
		return nil, nil, err
	}
	return envelope, breakpoints, nil
}

func oracleRequestMessages(reqBody []byte) ([]Message, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return nil, err
	}
	msgs := make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = Message{Role: m.Role, Content: m.Content}
	}
	return msgs, nil
}

// TestRequestAccessorsMatchHelpers pins every accessor to the helper it
// replaced, over every fixture.
func TestRequestAccessorsMatchHelpers(t *testing.T) {
	c := assert.NewCollecting(t)
	fixtures := bodyFixtures(t)
	c.Require().Greater(0, len(fixtures), "no fixtures were collected")

	for name, body := range fixtures {
		req, perr := Parse(body)

		// Stream / Model / the thread signals answer a zero result for a body
		// that does not decode, exactly as their whole-body decodes did.
		c.Eq(oracleRequestsStream(body), req.Stream(), "%s: Stream", name)
		c.Eq(oracleModel(body), req.Model(), "%s: Model", name)
		c.Eq(claudethread.PreviousMessageID(body), req.PreviousMessageID(), "%s: PreviousMessageID", name)
		wantBilling, wantOK := claudethread.BillingFromRequest(body)
		gotBilling, gotOK := req.Billing()
		c.EqDeep(wantBilling, gotBilling, "%s: Billing", name)
		c.Eq(wantOK, gotOK, "%s: Billing ok", name)
		c.Eq(claudethread.DeclaresClientTools(body), req.DeclaresClientTools(), "%s: DeclaresClientTools", name)

		// Envelope and breakpoints: byte-identical, and an error exactly when the
		// old splitter errored.
		wantEnv, wantBP, wantErr := oracleSplitEnvelopeAndBreakpoints(body)
		gotEnv, gotBP, gotErr := req.EnvelopeAndBreakpoints()
		c.Eq(wantErr != nil, gotErr != nil, "%s: split error presence", name)
		if perr == nil {
			// A body that decodes at all must produce the same envelope; the
			// pre-parse splitter could also read a body whose only defect is a
			// non-string role (which Parse accepts and Messages refuses).
			c.Eq(string(wantEnv), string(gotEnv), "%s: envelope", name)
			c.Eq(string(wantBP), string(gotBP), "%s: breakpoints", name)
		}

		// Messages: same messages, and the same "not decodable" verdict.
		wantMsgs, wantMErr := oracleRequestMessages(body)
		gotMsgs, gotMErr := req.Messages()
		c.Eq(wantMErr != nil, gotMErr != nil, "%s: messages error presence", name)
		if wantMErr == nil {
			// A nil and an empty slice are the same answer here: capture
			// re-allocates the tree messages, so only the content matters.
			c.Eq(len(wantMsgs), len(gotMsgs), "%s: message count", name)
			for i := range wantMsgs {
				c.EqDeep(wantMsgs[i], gotMsgs[i], "%s: message %d", name, i)
			}
		}
	}
}

// TestMessagesErrorKeepsCaptureVocabulary pins the error prefix capture's log
// lines and failed turns have always carried.
func TestMessagesErrorKeepsCaptureVocabulary(t *testing.T) {
	c := assert.NewCollecting(t)

	unparsable, err := Parse([]byte("not json"))
	c.Require().Error(err, "Parse must report the decode failure")
	_, merr := unparsable.Messages()
	c.Require().Error(merr, "Messages on an unparseable body")
	c.StrContains(merr.Error(), "decompose: unmarshal request:", "Messages error prefix")
	_, _, eerr := unparsable.EnvelopeAndBreakpoints()
	c.Require().Error(eerr, "EnvelopeAndBreakpoints on an unparseable body")
	c.StrContains(eerr.Error(), "split envelope: unmarshal request:", "split error prefix")

	// A body that decodes but whose role is not a string: Messages refuses it,
	// the breakpoint scan still reads it, exactly as the whole-body helpers did.
	odd := mustParse(t, `{"messages":[{"role":1,"content":[{"type":"text","text":"x","cache_control":{}}]}]}`)
	_, merr = odd.Messages()
	c.Require().Error(merr, "a non-string role must fail the strict decode")
	c.StrContains(merr.Error(), "decompose: unmarshal request:", "Messages error prefix")
	_, bps, eerr := odd.EnvelopeAndBreakpoints()
	c.Require().NoError(eerr, "the splitter never required a string role")
	c.Eq("[0]", string(bps), "breakpoints of a message with a non-string role")
}

// TestUnparseableBodyAnswersZeroes pins Parse's documented contract: a Request
// is always returned, forwarding the body verbatim, so the proxy's
// forward-anyway path survives.
func TestUnparseableBodyAnswersZeroes(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"model":`)
	req, err := Parse(body)
	c.Require().Error(err, "Parse must report the decode failure")
	c.True(req != nil, "Parse must still return a usable Request")
	c.Eq(string(body), string(req.Raw()), "an unparseable body must be forwarded verbatim")
	c.Eq("", req.Model(), "Model")
	c.Eq(false, req.Stream(), "Stream")
	c.Eq("", req.PreviousMessageID(), "PreviousMessageID")
	c.Eq(false, req.DeclaresClientTools(), "DeclaresClientTools")
	_, ok := req.Billing()
	c.Eq(false, ok, "Billing")
	c.Eq("", req.PrefixHash(), "PrefixHash")
}

// TestWithFieldsResetsCaches pins the rewrite contract: WithFields returns a new
// Request whose accessors read the FORWARDED bytes, so a cached answer from the
// inbound body can never leak into what the upstream sees.
func TestWithFieldsResetsCaches(t *testing.T) {
	c := assert.NewCollecting(t)
	req := mustParse(t, `{"model":"claude-old","messages":[{"role":"user","content":"original"}]}`)

	// Prime every cache off the inbound body.
	msgs, err := req.Messages()
	c.Require().NoError(err, "Messages")
	c.Eq(1, len(msgs), "messages read primed")
	c.Eq("claude-old", req.Model(), "model read primed")
	_ = req.PrefixHash()

	fields := req.Fields()
	fields["model"] = json.RawMessage(`"claude-new"`)
	fields["messages"] = json.RawMessage(`[{"role":"user","content":"rewritten"}]`)
	rewritten, err := req.WithFields(fields)
	c.Require().NoError(err, "WithFields")

	c.StrContains(string(rewritten.Raw()), `"claude-new"`, "the rewritten body must carry the new model")
	c.StrContains(string(rewritten.Raw()), `"rewritten"`, "the rewritten body must carry the new message")
	c.Eq("claude-new", rewritten.Model(), "the new Request must re-read the new model")
	newMsgs, err := rewritten.Messages()
	c.Require().NoError(err, "Messages on the rewrite")
	c.Eq(1, len(newMsgs), "message count after rewrite")
	c.Eq(`"rewritten"`, string(newMsgs[0].Content), "the new Request must decode the NEW raw")

	// The original Request is untouched by the rewrite.
	c.Eq("claude-old", req.Model(), "the source Request must not change")
	// And the map handed to WithFields is copied, so mutating it later cannot
	// change what the new Request forwards.
	fields["model"] = json.RawMessage(`"claude-mutated-later"`)
	c.Eq("claude-new", rewritten.Model(), "WithFields must own its map")
	c.StrContains(string(rewritten.Raw()), `"claude-new"`, "the forwarded bytes must not follow the caller's map")
}

// TestWithFieldsCarriesEveryKey pins that a rewrite keeps fields it did not edit
// byte-for-byte in value, including an integer above 2^53 that a
// map[string]any round trip would round.
func TestWithFieldsCarriesEveryKey(t *testing.T) {
	c := assert.NewCollecting(t)
	body := `{"model":"m","seed":9007199254740993,"metadata":{"user_id":"u"},"messages":[{"role":"user","content":"hi"}]}`
	req := mustParse(t, body)
	fields := req.Fields()
	fields["model"] = json.RawMessage(`"claude-resolved"`)
	rewritten, err := req.WithFields(fields)
	c.Require().NoError(err, "WithFields")

	c.StrContains(string(rewritten.Raw()), `"seed":9007199254740993`, "an integer past 2^53 must survive a rewrite")
	var got, want map[string]any
	c.Require().NoError(json.Unmarshal(rewritten.Raw(), &got), "decode rewritten body")
	c.Require().NoError(json.Unmarshal([]byte(body), &want), "decode input")
	want["model"] = "claude-resolved"
	c.EqDeep(want, got, "only the model changes")
}

// TestEnvelopeKeepsTheNullBodyShape pins the one degenerate shape the canonical
// marshal makes visible: a body of JSON null is still a body, and its envelope
// is "null" (the pre-parse splitter marshalled the nil map it was handed).
func TestEnvelopeKeepsTheNullBodyShape(t *testing.T) {
	c := assert.NewCollecting(t)
	req := mustParse(t, `null`)
	env, bps, err := req.EnvelopeAndBreakpoints()
	c.Require().NoError(err, "EnvelopeAndBreakpoints")
	c.Eq("null", string(env), "envelope of a null body")
	c.Eq("[]", string(bps), "breakpoints of a null body")

	empty := mustParse(t, `{}`)
	env, bps, err = empty.EnvelopeAndBreakpoints()
	c.Require().NoError(err, "EnvelopeAndBreakpoints")
	c.Eq("{}", string(env), "envelope of an empty object")
	c.Eq("[]", string(bps), "breakpoints of an empty object")
}

// TestMessageHasCacheControl is the structure-aware detection, moved here with
// the rule: only a real cache_control field on a content block counts, and
// plain-string content is never a breakpoint even when its text embeds the
// literal token.
func TestMessageHasCacheControl(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"block with cache_control", `[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]`, true},
		{"block without cache_control", `[{"type":"text","text":"x"}]`, false},
		{"plain string mentioning the token", `"please explain \"cache_control\" to me"`, false},
		{"plain string", `"hello"`, false},
		{"object-shaped content falls back to the substring check", `{"cache_control":{"type":"ephemeral"}}`, true},
		{"object-shaped content without the token", `{"text":"x"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := messageHasCacheControl(json.RawMessage(c.content))
			assert.NewCollecting(t).Eq(c.want, got, "messageHasCacheControl(%s) = %v, want", c.content, got)
		})
	}
}

// TestAccessorsAreLazyAndCached proves each accessor answers from the one parse:
// a second call must not decode again, which is what keeps one proxied turn at
// one full-body decode.
func TestAccessorsAreLazyAndCached(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"model":"m","stream":true,"diagnostics":{"previous_message_id":"p"},"system":[{"type":"text","text":"t"}],"tools":[{"name":"x","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`)

	DecodeCount.Top.Store(0)
	DecodeCount.Messages.Store(0)
	req, err := Parse(body)
	c.Require().NoError(err, "Parse")
	for i := 0; i < 3; i++ {
		c.Eq("m", req.Model(), "Model")
		c.Eq(true, req.Stream(), "Stream")
		c.Eq("p", req.PreviousMessageID(), "PreviousMessageID")
		c.Eq(true, req.DeclaresClientTools(), "DeclaresClientTools")
		_, _ = req.Billing()
		_, _ = req.Messages()
		_, _, _ = req.EnvelopeAndBreakpoints()
		_ = req.PrefixHash()
	}
	c.Eq(int64(1), DecodeCount.Top.Load(), "top-level decodes for one request")
	c.Eq(int64(1), DecodeCount.Messages.Load(), "messages[] decodes for one request")
}
