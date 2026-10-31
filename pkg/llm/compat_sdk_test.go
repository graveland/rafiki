// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"

	"github.com/multigres/testkit/assert"
)

// TestRequestFromSDKRoundTripsBreakpoints pins the one thing the SDK → native
// conversion must not lose this wave: the cache breakpoints the client placed
// on the params. RequestFromSDK derives the policy from the markers, and the
// Anthropic codec re-places them by the same rule — so the encoded body equals
// the golden the markers were recorded into.
func TestRequestFromSDKRoundTripsBreakpoints(t *testing.T) {
	ck := assert.NewAborting(t)

	// long-history is the case whose markers matter most (a 40-message turn
	// with two message breakpoints); plain carries a system prompt, so it also
	// covers the system marker.
	for _, name := range []string{"long-history", "plain"} {
		var tc msgtest.Case
		for _, c := range msgtest.Corpus() {
			if c.Name == name {
				tc = c
				break
			}
		}
		ck.Require().NotEmpty(tc.Name, "the corpus has no %s case", name)

		params := corpusParams(t, tc)
		req, extra, err := msganthropic.RequestFromSDK(params)
		ck.NoError(err, "RequestFromSDK(%s)", name)

		// The policy came back with the markers the params carried. A case with
		// no system blocks has no system marker to derive one from.
		if len(tc.Req.System) > 0 {
			ck.Eq(tc.Req.Cache.SystemTTL, req.Cache.SystemTTL, "%s: system TTL", name)
		}
		ck.Eq(tc.Req.Cache.MessagesTTL, req.Cache.MessagesTTL, "%s: messages TTL", name)
		// The count is derived from the markers actually on the wire, which can be
		// fewer than the policy asked for (a one-message turn has one place to put
		// one). The golden body below is the real oracle for placement.
		ck.True(req.Cache.Breakpoints >= 1 && req.Cache.Breakpoints <= tc.Req.Cache.Breakpoints,
			"%s: derived %d breakpoints from a policy asking for %d", name, req.Cache.Breakpoints, tc.Req.Cache.Breakpoints)

		body, err := msganthropic.Encode(req, msg.EncodeOptions{Extra: extra})
		ck.NoError(err, "encode %s", name)
		compareCanonical(t, name+".anthropic.json", msgtest.Golden(filepath.Join("golden", name+".anthropic.json")), body)
	}
}

// anthropicFixtureStream opens the SDK's own decoder over a recorded SSE body —
// the reference path this wave's adapter has to reproduce.
func anthropicFixtureStream(body []byte) *ssestream.Stream[anthropic.MessageStreamEventUnion] {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](ssestream.NewDecoder(resp), nil)
}

// TestCompatStreamPreservesProviderAndFinalBlocks pins the adapter's fidelity:
// driving the SDK handler and accumulator through native events must produce
// the same message the SDK's own decoder produces from the same bytes —
// including the served-by provider, a text block's citations, a tool_use's
// caller, and an opaque server-tool block whose input only exists in its
// finalized form.
func TestCompatStreamPreservesProviderAndFinalBlocks(t *testing.T) {
	for _, name := range []string{
		"anthropic-tooluse.sse",
		"anthropic-citations.sse",
		"anthropic-caller.sse",
		"anthropic-servertool.sse",
	} {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			body := msgtest.Golden(filepath.Join("responses", name))

			want, wantTypes := accumulateSDK(t, anthropicFixtureStream(body))

			got, gotTypes := accumulateAdapted(t, body)

			c := assert.NewCollecting(t)
			c.True(len(wantTypes) > 0, "the reference stream carried no events")
			// The SEQUENCES are allowed to differ: a block whose input only exists
			// in its finalized form (a server-tool block) is replayed as one delta
			// where the wire carried several. What must match is the result.
			c.True(len(gotTypes) > 0, "the adapter emitted no events")

			ck.Eq(want.ID, got.ID, "id")
			ck.Eq(string(want.Model), string(got.Model), "model")
			ck.Eq(string(want.StopReason), string(got.StopReason), "stop reason")
			ck.Eq(ProviderOf(want), ProviderOf(got), "provider")
			if name == "anthropic-servertool.sse" {
				// The fixture carries OpenRouter's served-by field; the guard reads it
				// off the accumulated message, so it must survive the round trip.
				ck.Eq("Together", ProviderOf(got), "the served-by provider was lost")
			}
			if name == "anthropic-citations.sse" {
				ck.StrContains(blockJSON(t, got), "citations", "the text block's citations were lost")
			}
			// The caller fixture needs no field-level assertion: the SDK's own
			// accumulator re-marshals a block at content_block_stop and drops a
			// field its union does not model, so the REFERENCE loses the caller
			// too. The equality above is the guarantee that the adapter adds no
			// loss of its own. (The native path keeps it: Ext carries the residue.)

			wantJSON, err := json.Marshal(want)
			ck.NoError(err, "marshal the reference message")
			gotJSON, err := json.Marshal(got)
			ck.NoError(err, "marshal the adapted message")
			compareCanonical(t, name+" (native -> sdk)", wantJSON, gotJSON)
		})
	}
}

// blockJSON renders every content block of an accumulated message as JSON, for
// a substring check that a block kept a field the SDK's typed form does not
// model.
func blockJSON(t *testing.T, m *anthropic.Message) string {
	t.Helper()
	var sb bytes.Buffer
	for _, b := range m.Content {
		sb.WriteString(b.RawJSON())
	}
	return sb.String()
}

// accumulateSDK drains an SDK stream the way the client does, returning the
// message and the event types it saw.
func accumulateSDK(t *testing.T, stream *ssestream.Stream[anthropic.MessageStreamEventUnion]) (*anthropic.Message, []string) {
	t.Helper()
	ck := assert.NewAborting(t)
	defer func() { ck.NoError(stream.Close(), "close stream") }()
	acc := anthropic.Message{}
	var types []string
	for stream.Next() {
		ev := stream.Current()
		types = append(types, ev.Type)
		FixEmptyToolInput(&ev)
		SanitizeInvalidAccumulatedInput(&acc, ev)
		ck.NoError(acc.Accumulate(ev), "accumulate %s", ev.Type)
		FixAccumulatedEmptyToolInput(&acc, ev)
	}
	ck.NoError(stream.Err(), "stream error")
	return &acc, types
}

// accumulateAdapted drains the codec's decoder through the adapter and the SDK
// accumulator — the path sendStreamingAttempt takes.
func accumulateAdapted(t *testing.T, body []byte) (*anthropic.Message, []string) {
	t.Helper()
	ck := assert.NewAborting(t)
	dec := msganthropic.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)))
	defer func() { ck.NoError(dec.Close(), "close decoder") }()

	acc := anthropic.Message{}
	adapter := &msganthropic.SDKEventAdapter{}
	var types []string
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		ck.NoError(err, "next event")
		events, aerr := adapter.Add(ev)
		ck.NoError(aerr, "adapt %T", ev)
		for _, sdkEv := range events {
			types = append(types, sdkEv.Type)
			FixEmptyToolInput(&sdkEv)
			SanitizeInvalidAccumulatedInput(&acc, sdkEv)
			ck.NoError(acc.Accumulate(sdkEv), "accumulate %s", sdkEv.Type)
			FixAccumulatedEmptyToolInput(&acc, sdkEv)
		}
	}
	return &acc, types
}
