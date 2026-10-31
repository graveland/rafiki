// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
	"go.graveland.dev/rafiki/pkg/msg/openaichat"
	"go.graveland.dev/rafiki/pkg/msg/responses"

	"github.com/multigres/testkit/assert"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite msgtest goldens")

// TestGoldenCorpus pins what Conversation.assemble builds: a request assembled
// from the conversation's defaults must encode to the same recorded bodies the
// codecs were built against. The codecs' own tests own the corpus for their
// dialects; what this adds is the assembly hop, which is where a dropped block,
// a lost cache policy or an unmerged extra field would show up as a wire change.
func TestGoldenCorpus(t *testing.T) {
	for _, tc := range msgtest.Corpus() {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			req := corpusRequest(t, tc)
			extra := tc.Extra

			anthropicBody, err := msganthropic.Encode(req, msg.EncodeOptions{Extra: extra})
			ck.NoError(err, "encode anthropic body")
			writeGolden(t, tc.Name+".anthropic.json", anthropicBody)

			// The Chat Completions goldens were recorded before the codec merged
			// extra top-level fields, so the extras ride this comparison
			// separately (pkg/msg/openaichat pins the merge itself).
			chatBody, err := openaichat.Encode(req, msg.EncodeOptions{})
			ck.NoError(err, "encode openai chat body")
			writeGolden(t, tc.Name+".openaichat.json", chatBody)

			chatStreamBody, err := openaichat.Encode(req, msg.EncodeOptions{Stream: true})
			ck.NoError(err, "encode openai chat stream body")
			writeGolden(t, tc.Name+".openaichat-stream.json", chatStreamBody)

			// The Responses goldens were recorded before the codec merged extras,
			// so those are lifted out of the built body and checked on their own.
			apiBody, err := responses.Codec{Mode: responses.ModeAPI}.Encode(req, msg.EncodeOptions{
				SessionID: "sess-1",
				Extra:     extra,
			})
			ck.NoError(err, "encode responses api body")
			writeGoldenWithExtra(t, tc.Name+".responses-api.json", apiBody, tc.Extra)

			chatGPTBody, err := responses.Codec{
				Mode:       responses.ModeChatGPT,
				DevMessage: chatGPTInstructionsAsDeveloperMessage,
			}.Encode(req, msg.EncodeOptions{Stream: true, SessionID: "sess-1", Extra: extra})
			ck.NoError(err, "encode responses chatgpt body")
			writeGoldenWithExtra(t, tc.Name+".responses-chatgpt.json", chatGPTBody, tc.Extra)
		})
	}
}

// corpusRequest assembles the native request the client would build for tc,
// through Conversation.assemble and the corpus's own defaults.
func corpusRequest(t *testing.T, tc msgtest.Case) *msg.Request {
	t.Helper()
	req := tc.Req
	policy := CachePolicy{
		SystemTTL:   CacheTTL(req.Cache.SystemTTL),
		MessagesTTL: CacheTTL(req.Cache.MessagesTTL),
		Breakpoints: req.Cache.Breakpoints,
	}
	conv := &Conversation{cfg: convConfig{
		model:          req.Model,
		system:         req.System,
		temperature:    req.Temperature,
		topK:           req.TopK,
		maxTokens:      req.MaxTokens,
		thinkingBudget: req.ThinkingBudget,
		cache:          &policy,
	}}
	return conv.assemble(req.Messages, sendConfig{
		maxTokens:  req.MaxTokens,
		tools:      req.Tools,
		toolChoice: req.ForceTool,
	})
}

// corpusParams is the SDK-shaped form of corpusRequest: the native request the
// client would build, encoded and decoded back into the SDK's params. It is the
// input the SDK → native compatibility hop (RequestFromSDK, used by SendParams)
// is exercised against, and going through the codec is what gives it the same
// cache markers a real request would carry.
func corpusParams(t *testing.T, tc msgtest.Case) anthropic.MessageNewParams {
	t.Helper()
	ck := assert.NewAborting(t)
	body, err := msganthropic.Encode(corpusRequest(t, tc), msg.EncodeOptions{})
	ck.NoError(err, "encode corpus request")
	var params anthropic.MessageNewParams
	ck.NoError(json.Unmarshal(body, &params), "decode corpus params")
	if len(tc.Extra) > 0 {
		params.SetExtraFields(tc.Extra)
	}
	return params
}

// writeGoldenWithExtra compares a built body with its golden after lifting the
// case's own top-level extras out of it: the recorded golden predates the
// codec's extra merge, and the merge itself is pinned by the codec's tests.
func writeGoldenWithExtra(t *testing.T, name string, body []byte, extra map[string]any) {
	t.Helper()
	if *updateGolden || len(extra) == 0 {
		writeGolden(t, name, body)
		return
	}
	ck := assert.NewAborting(t)
	var got map[string]any
	ck.NoError(json.Unmarshal(body, &got), "%s: built body is not a JSON object", name)
	for k, v := range extra {
		ck.EqDeep(v, got[k], "%s: extra field %q", name, k)
		delete(got, k)
	}
	stripped, err := json.Marshal(got)
	ck.NoError(err, "%s: re-encode stripped body", name)
	writeGolden(t, name, stripped)
}

func writeGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	path := msgtest.GoldenPath(filepath.Join("golden", name))
	if *updateGolden {
		pretty, err := prettyJSON(body)
		if err != nil {
			t.Fatalf("pretty-print golden %s: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, pretty, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update-golden to record it)", name, err)
	}
	compareCanonical(t, name, want, body)
}

func prettyJSON(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// compareCanonical compares two JSON documents by value, never by bytes: key
// order and whitespace are not part of what a codec promises.
func compareCanonical(t *testing.T, name string, want, got []byte) {
	t.Helper()
	ck := assert.NewAborting(t)
	var w, g any
	ck.NoError(json.Unmarshal(want, &w), "%s: golden is not JSON", name)
	ck.NoError(json.Unmarshal(got, &g), "%s: built body is not JSON", name)
	ck.EqDeep(w, g, "%s does not match its golden (only an intended change justifies -update-golden)", name)
}
