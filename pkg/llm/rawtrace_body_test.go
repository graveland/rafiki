// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// recordingRoundTripper records the request body it is handed and answers a
// fixed 200. It stands in for the network, so a test can prove what actually
// left the process.
type recordingRoundTripper struct {
	mu       sync.Mutex
	body     []byte
	encoding string
	resp     *http.Response
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.body = body
	rt.encoding = req.Header.Get("Content-Encoding")
	rt.mu.Unlock()
	return rt.resp, nil
}

// captureThroughTransport runs one request through headerCaptureTransport and
// returns the capture plus the transport that saw the wire bytes.
func captureThroughTransport(t *testing.T, body []byte, contentEncoding string) (*rawTraceHeaders, *recordingRoundTripper) {
	t.Helper()
	ck := assert.NewAborting(t)
	h := &rawTraceHeaders{}
	ctx := context.WithValue(context.Background(), rawTraceHeaderKey{}, h)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://example.test/v1/messages", bytes.NewReader(body))
	ck.NoError(err, "build request")
	ck.NotNil(req.GetBody, "the fixture needs a replayable body")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	rt := &recordingRoundTripper{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Test-Upstream-Marker": []string{"seen-it"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}
	tr := headerCaptureTransport{base: rt}
	resp, err := tr.RoundTrip(req)
	ck.NoError(err, "round trip")
	ck.NotNil(resp, "response")
	ck.NoError(resp.Body.Close(), "close response body")
	return h, rt
}

func gzipBody(t *testing.T, b []byte) []byte {
	t.Helper()
	ck := assert.NewAborting(t)
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(b)
	ck.NoError(err, "gzip write")
	ck.NoError(w.Close(), "gzip close")
	return buf.Bytes()
}

func zstdBody(t *testing.T, b []byte) []byte {
	t.Helper()
	ck := assert.NewAborting(t)
	enc, err := zstd.NewWriter(nil)
	ck.NoError(err, "zstd writer")
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// TestRawTraceRecordsOpenAIChatWireBody pins the body a raw trace stores for a
// non-Anthropic dialect: the actual OpenAI Chat Completions JSON that went over
// the wire (never Anthropic params, never a compressed blob), the dialect's own
// path, the real headers, and the native response form.
func TestRawTraceRecordsOpenAIChatWireBody(t *testing.T) {
	ck := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck.Eq("/chat/completions", r.URL.Path, "upstream path")
		body, err := io.ReadAll(r.Body)
		ck.NoError(err, "read upstream body")
		ck.StrContains(string(body), `"model":"glm-5.3-flash"`, "upstream body")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test-Upstream-Marker", "seen-it")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer srv.Close()

	set := &providers.Set{
		DefaultProvider: "oa",
		Providers: map[string]providers.Provider{
			"oa": {Name: "oa", Kind: providers.KindOpenAI, BaseURL: srv.URL},
		},
	}
	c, err := NewClient(WithProviders(set), WithLogger(testLogger(t)))
	ck.Require().NoError(err, "NewClient")
	rec := newFakeRawTraceRecorder()
	c.rawTrace = rec

	req := &msg.Request{Model: "oa/glm-5.3-flash", MaxTokens: 8,
		Messages: []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}}}
	resp, err := c.send(context.Background(), SendMeta{}, req, nil)
	ck.Require().NoError(err, "send")
	ck.Require().NotNil(resp, "response")

	got := rec.awaitOne(t)
	ck.Eq("/chat/completions", got.ReqPath, "recorded request path")

	var body map[string]json.RawMessage
	ck.Require().NoError(json.Unmarshal(got.ReqBody, &body),
		"the recorded request body must be the dialect's JSON, got %s", got.ReqBody)
	var model string
	ck.Require().NoError(json.Unmarshal(body["model"], &model), "recorded model")
	ck.Eq("glm-5.3-flash", model, "recorded model")
	ck.HasKey(body, "messages", "recorded request body")
	ck.NotHasKey(body, "provider", "an openai-chat body carries no OpenRouter provider object")

	var reqHeaders map[string]string
	ck.Require().NoError(json.Unmarshal(got.ReqHeaders, &reqHeaders), "request headers")
	ck.Eq("application/json", reqHeaders["Content-Type"], "recorded wire header")

	var respHeaders map[string]string
	ck.Require().NoError(json.Unmarshal(got.RespHeaders, &respHeaders), "response headers")
	ck.Eq("seen-it", respHeaders["X-Test-Upstream-Marker"], "recorded response header")

	var respBody map[string]json.RawMessage
	ck.Require().NoError(json.Unmarshal(got.RespBody, &respBody),
		"the recorded response body must be the native form, got %s", got.RespBody)
	for _, key := range []string{"id", "model", "stop_reason", "usage", "content"} {
		ck.HasKey(respBody, key, "recorded response body")
	}
}

// TestRawTraceDecompressesRequestBody pins the decompression rule: the wire
// carries the compressed body, the trace carries the JSON it stands for. The
// upstream must still receive the ORIGINAL compressed bytes — the capture reads
// its own replay, never the outbound body.
func TestRawTraceDecompressesRequestBody(t *testing.T) {
	original := []byte(`{"model":"glm-5.3-flash","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	cases := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{"gzip", "gzip", gzipBody},
		{"zstd", "zstd", zstdBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			compressed := tc.compress(t, original)
			c.True(len(compressed) > 0, "the fixture compressed to nothing")

			h, rt := captureThroughTransport(t, compressed, tc.encoding)

			rt.mu.Lock()
			wire, enc := rt.body, rt.encoding
			rt.mu.Unlock()
			c.EqDeep(compressed, wire, "the upstream must receive the ORIGINAL compressed bytes")
			c.Eq(tc.encoding, enc, "the upstream must see the Content-Encoding it was sent")

			c.Eq("", h.captureErr, "capture error")
			c.EqDeep(original, h.reqBody, "the capture must hold the DECOMPRESSED body")
			c.Eq("/v1/messages", h.reqPath, "captured path")
			c.Eq(tc.encoding, h.req.Get("Content-Encoding"),
				"the captured headers describe the transport, not the stored body")
		})
	}
}

// TestRawTraceDecompressionLimit pins the bound: a body that expands past
// maxTraceBody is stored truncated, with the truncation recorded as a
// diagnostic — never fully expanded.
func TestRawTraceDecompressionLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	original := []byte(`{"text":"` + strings.Repeat("a", maxTraceBody+4096) + `"}`)
	compressed := gzipBody(t, original)
	c.True(len(compressed) < maxTraceBody, "the fixture must compress below the limit: %d bytes", len(compressed))

	h, _ := captureThroughTransport(t, compressed, "gzip")
	c.Len(h.reqBody, maxTraceBody, "the stored body must be truncated at the limit")
	c.StrContains(h.captureErr, "truncated", "the truncation must be recorded as a diagnostic")
}

// TestRawTraceDecodeFailureIsBestEffort pins the failure rule: an encoding this
// package cannot decode omits the body and records why, and the send itself is
// untouched — the upstream still receives exactly the bytes it would have.
func TestRawTraceDecodeFailureIsBestEffort(t *testing.T) {
	original := []byte(`{"model":"m","max_tokens":8}`)

	t.Run("unsupported encoding", func(t *testing.T) {
		c := assert.NewCollecting(t)
		h, rt := captureThroughTransport(t, original, "br")
		c.Nil(h.reqBody, "an undecodable body must be omitted, never stored as binary")
		c.StrContains(h.captureErr, "unsupported content-encoding", "capture error")
		rt.mu.Lock()
		wire := rt.body
		rt.mu.Unlock()
		c.EqDeep(original, wire, "the send must be unaltered")
	})

	t.Run("corrupt gzip", func(t *testing.T) {
		c := assert.NewCollecting(t)
		h, rt := captureThroughTransport(t, original, "gzip")
		c.Nil(h.reqBody, "an undecodable body must be omitted")
		c.StrContains(h.captureErr, "gzip", "capture error")
		rt.mu.Lock()
		wire := rt.body
		rt.mu.Unlock()
		c.EqDeep(original, wire, "the send must be unaltered")
	})
}

// TestRawTraceBodyCaptureIsGatedOnTheRecorder pins that capture costs nothing
// when it is off: a client without a raw-trace recorder attaches no capture
// point, so the transport reads no request body and decompresses nothing for a
// record nobody will write.
func TestRawTraceBodyCaptureIsGatedOnTheRecorder(t *testing.T) {
	c := assert.NewAborting(t)

	off := &Client{}
	ctx, hdrs := off.rawTraceContext(context.Background())
	c.Nil(hdrs, "capture off must attach no capture point")
	c.Nil(ctx.Value(rawTraceHeaderKey{}), "capture off must attach no capture point")

	// With no capture point on the context, the transport reads no body: the
	// replayable body is left untouched.
	reads := 0
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://example.test/v1/messages", bytes.NewReader([]byte(`{}`)))
	c.NoError(err, "build request")
	req.GetBody = func() (io.ReadCloser, error) {
		reads++
		return io.NopCloser(strings.NewReader(`{}`)), nil
	}
	res, err := headerCaptureTransport{base: &recordingRoundTripper{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}}.RoundTrip(req)
	c.NoError(err, "round trip")
	c.NoError(res.Body.Close(), "close response body")
	c.Eq(0, reads, "no request body is read with capture off")

	on := &Client{rawTrace: newFakeRawTraceRecorder()}
	_, hdrs = on.rawTraceContext(context.Background())
	c.NotNil(hdrs, "capture on must attach a capture point")
}
