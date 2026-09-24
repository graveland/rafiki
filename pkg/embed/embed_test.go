// SPDX-License-Identifier: Apache-2.0

package embed_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/embed"
	"go.graveland.dev/rafiki/pkg/providers"
)

func startServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

type sentBody struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
}

func outOfOrderData(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"data":[` +
		`{"index":1,"embedding":[4,5,6]},` +
		`{"index":0,"embedding":[1,2,3]}]}`))
}

func TestEmbedOrdersByIndexAndSendsBearer(t *testing.T) {
	t.Setenv("EMBED_TEST_API_KEY", "sekrit")
	var mu sync.Mutex
	var gotAuth, gotCT string
	var gotBody sentBody
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		outOfOrderData(w)
	})
	c := embed.New(providers.EmbeddingsConfig{
		URL:        srv.URL,
		APIKeyEnv:  "EMBED_TEST_API_KEY",
		Model:      "bge-large",
		Dimensions: 3,
	}, nil)
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[0][2] != 3 || vecs[1][0] != 4 || vecs[1][2] != 6 {
		t.Errorf("vecs = %v, want [[1 2 3] [4 5 6]] (ordered by the server's index field)", vecs)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekrit")
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody.Model != "bge-large" || len(gotBody.Input) != 2 ||
		gotBody.Input[0] != "a" || gotBody.Input[1] != "b" || gotBody.Dimensions != 3 {
		t.Errorf("request body = %+v, want model bge-large, input [a b], dimensions 3", gotBody)
	}
}

// Authorization must be present iff a credential resolves: no header at all
// for an empty api_key_env, and none for a named-but-unset variable (an unset
// key surfaces as an upstream 401, not a client-side error).
func TestEmbedOmitsAuthorizationWithoutKey(t *testing.T) {
	cases := []struct {
		name      string
		apiKeyEnv string
	}{
		{name: "empty api_key_env"},
		{name: "named env unset", apiKeyEnv: "EMBED_TEST_UNSET_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
			})
			c := embed.New(providers.EmbeddingsConfig{
				URL: srv.URL, APIKeyEnv: tc.apiKeyEnv, Model: "m", Dimensions: 1,
			}, nil)
			if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if gotAuth != "" {
				t.Errorf("Authorization = %q, want no header", gotAuth)
			}
		})
	}
}

func TestEmbedRejectsWrongDimensions(t *testing.T) {
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]}]}`))
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 2}, nil)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("Embed succeeded, want a dimensions error")
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error = %q, want it to name the dimension mismatch", err)
	}
}

func TestEmbedRejectsWrongVectorCount(t *testing.T) {
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 2}, nil)
	_, err := c.Embed(context.Background(), []string{"x", "y"})
	if err == nil || !strings.Contains(err.Error(), "1 vectors for 2 inputs") {
		t.Errorf("error = %v, want a count mismatch naming both sides", err)
	}
}

// The "fails loudly" contract: a non-2xx error carries the server's status
// AND its body, truncated to the first 512 bytes.
func TestEmbedNon2xxIncludesBody(t *testing.T) {
	const body = "upstream exploded"
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(body))
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 1}, nil)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}
	if msg := err.Error(); !strings.Contains(msg, "embed: 502") || !strings.Contains(msg, body) {
		t.Errorf("error = %q, want status and body %q", msg, body)
	}
}

func TestEmbedNon2xxBodyTruncated(t *testing.T) {
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("x", 600)))
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 1}, nil)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, strings.Repeat("x", 512)) {
		t.Errorf("error %q lacks the first 512 bytes of the body", msg)
	}
	if strings.Contains(msg, strings.Repeat("x", 513)) {
		t.Errorf("error %q carries more than the first 512 bytes of the body", msg)
	}
}

func TestEmbedDecodeFailureIncludesBody(t *testing.T) {
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 1}, nil)
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("Embed succeeded, want a decode error")
	}
	if msg := err.Error(); !strings.Contains(msg, "decode response") || !strings.Contains(msg, "not json at all") {
		t.Errorf("error = %q, want a decode failure naming the body", msg)
	}
}

func TestEmbedEmptyInputNoRequest(t *testing.T) {
	called := false
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 1}, nil)
	for _, in := range [][]string{nil, {}} {
		vecs, err := c.Embed(context.Background(), in)
		if err != nil || vecs != nil {
			t.Errorf("Embed(%v) = %v, %v; want nil, nil", in, vecs, err)
		}
	}
	if called {
		t.Error("server received a request for an empty batch")
	}
}

// A gzipped body with Content-Encoding: gzip must decode transparently. The
// transport only gunzips responses it asked to compress itself, so this pins
// the rule that the client never sets Accept-Encoding by hand (which would
// disable decompression and hand the JSON decoder raw gzip bytes).
func TestEmbedGzipResponse(t *testing.T) {
	srv := startServer(t, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
		if err := zw.Close(); err != nil {
			t.Errorf("gzip close: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	})
	c := embed.New(providers.EmbeddingsConfig{URL: srv.URL, Model: "m", Dimensions: 2}, nil)
	vecs, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("Embed with a gzipped body: %v", err)
	}
	if len(vecs) != 1 || vecs[0][0] != 1 || vecs[0][1] != 2 {
		t.Errorf("vecs = %v, want [[1 2]]", vecs)
	}
}
