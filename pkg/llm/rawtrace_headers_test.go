// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
)

// fakeRawTraceRecorder captures Insert calls without a database, so
// recordRawTrace's output can be asserted directly. recordRawTrace's insert
// runs in a detached goroutine, so tests must receive off ch rather than read
// a field synchronously.
type fakeRawTraceRecorder struct {
	ch chan rawtrace.RawHTTPRequest
}

func newFakeRawTraceRecorder() *fakeRawTraceRecorder {
	return &fakeRawTraceRecorder{ch: make(chan rawtrace.RawHTTPRequest, 8)}
}

func (f *fakeRawTraceRecorder) Insert(_ context.Context, r rawtrace.RawHTTPRequest) error {
	f.ch <- r
	return nil
}

func (f *fakeRawTraceRecorder) awaitOne(t *testing.T) rawtrace.RawHTTPRequest {
	t.Helper()
	select {
	case r := <-f.ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for raw trace insert")
		return rawtrace.RawHTTPRequest{}
	}
}

// TestRecordRawTraceCapturesRealHeaders pins the actual bug: recordRawTrace
// used to hand the store two hardcoded literals
// (`{"Content-Type":"application/json"}` for the request, `{}` for the
// response) regardless of what was really sent or received. A fundi-sourced
// raw_http_request row should instead show the credential header REDACTED
// (present, not dropped) and the upstream's own response header, neither of
// which the old stub could ever produce.
func TestRecordRawTraceCapturesRealHeaders(t *testing.T) {
	const testKeyEnv = "RAFIKI_TEST_RAWTRACE_KEY"
	t.Setenv(testKeyEnv, "sk-secret-value")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-secret-value" {
			t.Errorf("upstream saw x-api-key = %q, want the real key (redaction must happen at capture, not on the wire)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test-Upstream-Marker", "seen-it")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	set := &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic": {Name: "anthropic", Kind: providers.KindAnthropic, BaseURL: srv.URL, APIKeyEnv: testKeyEnv},
		},
	}
	c, err := NewClient(WithProviders(set), WithLogger(testLogger(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	rec := newFakeRawTraceRecorder()
	c.rawTrace = rec

	_, err = c.SendParams(context.Background(), SendMeta{}, anthropic.MessageNewParams{
		Model: "test-model", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}

	got := rec.awaitOne(t)

	var reqHeaders map[string]string
	if err := json.Unmarshal(got.ReqHeaders, &reqHeaders); err != nil {
		t.Fatalf("ReqHeaders not valid JSON: %v (%s)", err, got.ReqHeaders)
	}
	if reqHeaders["X-Api-Key"] != "<redacted>" {
		t.Errorf("req X-Api-Key = %q, want <redacted> (present, not the real key, not dropped)", reqHeaders["X-Api-Key"])
	}

	var respHeaders map[string]string
	if err := json.Unmarshal(got.RespHeaders, &respHeaders); err != nil {
		t.Fatalf("RespHeaders not valid JSON: %v (%s)", err, got.RespHeaders)
	}
	if respHeaders["X-Test-Upstream-Marker"] != "seen-it" {
		t.Errorf("resp X-Test-Upstream-Marker = %q, want seen-it (the old stub could never carry an arbitrary upstream header)", respHeaders["X-Test-Upstream-Marker"])
	}
}
