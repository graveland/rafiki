// SPDX-License-Identifier: Apache-2.0

package batch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestOpenRouterDefaultsToPublicBase(t *testing.T) {
	c := assert.NewCollecting(t)
	o := NewOpenRouter("", "sk-test", nil)
	c.Eq(DefaultBaseURL, o.baseURL, "baseURL")
	o = NewOpenRouter("https://example.org/api/", "sk-test", nil)
	c.Eq("https://example.org/api", o.baseURL, "trailing slash not trimmed")
}

func TestOpenRouterSubmit(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotPath, gotMethod, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"batch-1","status":"in_progress","created_at":1700000000}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	batch, err := o.Submit(context.Background(), "vendor/m:batch", nil, []BatchRequest{
		{CustomID: "conv-1", Body: json.RawMessage(`{"max_tokens":16}`)},
	})
	c.Require().NoError(err, "Submit")
	c.False(gotMethod != http.MethodPost || gotPath != "/v1/batches", "request = %s %s, want POST /v1/batches", gotMethod, gotPath)
	c.Eq("Bearer sk-test", gotAuth, "Authorization =")
	var body struct {
		Endpoint string         `json:"endpoint"`
		Model    string         `json:"model"`
		Requests []BatchRequest `json:"requests"`
	}
	c.Require().NoError(json.Unmarshal(gotBody, &body), "decode submit body")
	c.False(body.Endpoint != messagesEndpoint || body.Model != "vendor/m:batch" || len(body.Requests) != 1 ||
		body.Requests[0].CustomID != "conv-1" || string(body.Requests[0].Body) != `{"max_tokens":16}`, "submit body = %+v", body)
	c.False(batch.ID != "batch-1" || batch.Status != "in_progress" || batch.CreatedAt != 1700000000, "decoded batch = %+v", batch)
}

func TestOpenRouterSubmitNon2xxCarriesBody(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	_, err := o.Submit(context.Background(), "vendor/m:batch", nil, nil)
	var he *httpStatusError
	c.Require().False(err == nil || !errors.As(err, &he), "Submit: want *httpStatusError, got %v", err)
	c.False(he.Status != http.StatusTooManyRequests || !strings.Contains(he.Body, "slow down"), "httpStatusError = %+v", he)
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("Error() = %q, want status and body text", err.Error())
	}
}

func TestOpenRouterBatchGet(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":"batch-7","status":"completed","created_at":1700000000,"results":[{"custom_id":"conv-1","response":{"status_code":200,"body":{"id":"msg_1"}}}]}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	b, err := o.Batch(context.Background(), "batch-7")
	c.Require().NoError(err, "Batch")
	c.Eq("/v1/batches/batch-7", gotPath, "path =")
	c.Eq("Bearer sk-test", gotAuth, "Authorization =")
	c.False(b.Status != "completed" || !b.Terminal() || len(b.Results) != 1 ||
		b.Results[0].CustomID != "conv-1" || b.Results[0].Response.StatusCode != 200, "decoded batch = %+v", b)
}

func TestOpenRouterListPagingParams(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":[{"id":"batch-9","status":"in_progress","created_at":1700000000}],"has_more":true}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	list, err := o.List(context.Background(), "batch-3", 0)
	c.Require().NoError(err, "List")
	c.Eq("after=batch-3&limit=100", gotQuery, "query =")
	c.False(!list.HasMore || len(list.Data) != 1 || list.Data[0].ID != "batch-9", "decoded list = %+v", list)
}

func TestOpenRouterTerminalStatuses(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, st := range []string{"completed", "failed", "expired", "cancelled"} {
		c.True((Batch{Status: st}).Terminal(), "status %q should be terminal", st)
	}
	for _, st := range []string{"", "validating", "in_progress", "queued"} {
		c.False((Batch{Status: st}).Terminal(), "status %q should not be terminal", st)
	}
}

// TestBatchRoutingEnvelopeCarriesProviderOnly pins the parked call's wire
// shape (Task 0.1's probe): the provider object rides the submit ENVELOPE's
// top level — next to endpoint/model, BEFORE requests (the stream-parser
// returns 400 when requests appears first) — narrowed to {"only": [...]},
// never the live path's full prefs (sort, data_collection, zdr, ignore are
// each a submit-time 400 "Unrecognized key"), and per-call bodies carry no
// provider key. A nil provider omits the key entirely.
func TestBatchRoutingEnvelopeCarriesProviderOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"batch-1","status":"in_progress","created_at":1700000000}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	batch, err := o.Submit(context.Background(), "vendor/m:batch", json.RawMessage(`{"only":["deepinfra"]}`), []BatchRequest{
		{CustomID: "conv-1", Body: json.RawMessage(`{"max_tokens":16}`)},
	})
	c.Require().NoError(err, "Submit")
	c.Eq("batch-1", batch.ID, "batch decoded")

	// Key order: provider BEFORE requests, so the stream-parser never sees
	// requests first.
	raw := string(gotBody)
	idxProvider := strings.Index(raw, `"provider"`)
	idxRequests := strings.Index(raw, `"requests"`)
	c.True(idxProvider >= 0 && idxRequests > idxProvider,
		"provider must precede requests in the submit body: %s", raw)

	var envelope struct {
		Endpoint string         `json:"endpoint"`
		Model    string         `json:"model"`
		Provider map[string]any `json:"provider"`
		Requests []BatchRequest `json:"requests"`
	}
	c.Require().NoError(json.Unmarshal(gotBody, &envelope), "decode submit envelope")
	c.Len(envelope.Provider, 1, "provider carries exactly one key, got %v", envelope.Provider)
	var onlyList []any
	onlyList = append(onlyList, envelope.Provider["only"].([]any)...)
	c.EqDiff([]any{"deepinfra"}, onlyList, "the only list")
	c.False(strings.Contains(string(envelope.Requests[0].Body), "provider"),
		"the per-call body must carry no provider key: %s", envelope.Requests[0].Body)

	// Nil provider: the key is omitted, not an empty object.
	_, err = o.Submit(context.Background(), "vendor/m:batch", nil, []BatchRequest{
		{CustomID: "conv-2", Body: json.RawMessage(`{"max_tokens":16}`)},
	})
	c.Require().NoError(err, "Submit (nil provider)")
	c.NotStrContains(string(gotBody), `"provider"`, "nil provider omits the key entirely: %s", gotBody)
}
