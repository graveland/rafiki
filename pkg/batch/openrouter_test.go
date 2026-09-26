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
)

func TestOpenRouterDefaultsToPublicBase(t *testing.T) {
	o := NewOpenRouter("", "sk-test", nil)
	if o.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", o.baseURL, DefaultBaseURL)
	}
	o = NewOpenRouter("https://example.org/api/", "sk-test", nil)
	if o.baseURL != "https://example.org/api" {
		t.Errorf("trailing slash not trimmed: %q", o.baseURL)
	}
}

func TestOpenRouterSubmit(t *testing.T) {
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
	batch, err := o.Submit(context.Background(), "vendor/m:batch", []BatchRequest{
		{CustomID: "conv-1", Body: json.RawMessage(`{"max_tokens":16}`)},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/batches" {
		t.Errorf("request = %s %s, want POST /v1/batches", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	var body struct {
		Endpoint string         `json:"endpoint"`
		Model    string         `json:"model"`
		Requests []BatchRequest `json:"requests"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	if body.Endpoint != messagesEndpoint || body.Model != "vendor/m:batch" || len(body.Requests) != 1 ||
		body.Requests[0].CustomID != "conv-1" || string(body.Requests[0].Body) != `{"max_tokens":16}` {
		t.Errorf("submit body = %+v", body)
	}
	if batch.ID != "batch-1" || batch.Status != "in_progress" || batch.CreatedAt != 1700000000 {
		t.Errorf("decoded batch = %+v", batch)
	}
}

func TestOpenRouterSubmitNon2xxCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	_, err := o.Submit(context.Background(), "vendor/m:batch", nil)
	var he *httpStatusError
	if err == nil || !errors.As(err, &he) {
		t.Fatalf("Submit: want *httpStatusError, got %v", err)
	}
	if he.Status != http.StatusTooManyRequests || !strings.Contains(he.Body, "slow down") {
		t.Errorf("httpStatusError = %+v", he)
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("Error() = %q, want status and body text", err.Error())
	}
}

func TestOpenRouterBatchGet(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":"batch-7","status":"completed","created_at":1700000000,"results":[{"custom_id":"conv-1","response":{"status_code":200,"body":{"id":"msg_1"}}}]}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	b, err := o.Batch(context.Background(), "batch-7")
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if gotPath != "/v1/batches/batch-7" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if b.Status != "completed" || !b.Terminal() || len(b.Results) != 1 ||
		b.Results[0].CustomID != "conv-1" || b.Results[0].Response.StatusCode != 200 {
		t.Errorf("decoded batch = %+v", b)
	}
}

func TestOpenRouterListPagingParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":[{"id":"batch-9","status":"in_progress","created_at":1700000000}],"has_more":true}`))
	}))
	defer srv.Close()

	o := NewOpenRouter(srv.URL, "sk-test", srv.Client())
	list, err := o.List(context.Background(), "batch-3", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotQuery != "after=batch-3&limit=100" {
		t.Errorf("query = %q", gotQuery)
	}
	if !list.HasMore || len(list.Data) != 1 || list.Data[0].ID != "batch-9" {
		t.Errorf("decoded list = %+v", list)
	}
}

func TestOpenRouterTerminalStatuses(t *testing.T) {
	for _, st := range []string{"completed", "failed", "expired", "cancelled"} {
		if !(Batch{Status: st}).Terminal() {
			t.Errorf("status %q should be terminal", st)
		}
	}
	for _, st := range []string{"", "validating", "in_progress", "queued"} {
		if (Batch{Status: st}).Terminal() {
			t.Errorf("status %q should not be terminal", st)
		}
	}
}
