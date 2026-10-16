// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"
)

// writeEmbedding answers with one vector of the given length, shaped like an
// OpenAI /embeddings response.
func writeEmbedding(w http.ResponseWriter, n int) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": []any{map[string]any{"index": 0, "embedding": make([]float32, n)}},
	})
}

func TestCheckOpenAIModels(t *testing.T) {
	ck := assert.NewCollecting(t)
	var mu sync.Mutex
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ck.NoError(checkOpenAIModels(context.Background(), srv.URL, "k"), "200 with a key")
	mu.Lock()
	ck.Eq("/models", gotPath, "path")
	ck.Eq("Bearer k", gotAuth, "Authorization when a key is given")
	mu.Unlock()

	ck.NoError(checkOpenAIModels(context.Background(), srv.URL, ""), "200 without a key")
	mu.Lock()
	ck.Eq("", gotAuth, "no Authorization when keyless")
	mu.Unlock()

	// A trailing slash on the base URL must still reach /models.
	ck.NoError(checkOpenAIModels(context.Background(), srv.URL+"/", "k"), "trailing slash")
	mu.Lock()
	ck.Eq("/models", gotPath, "trailing-slash path")
	mu.Unlock()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	err := checkOpenAIModels(context.Background(), unauthorized.URL, "")
	ck.Require().Error(err, "401 must be an error")
	ck.StrContains(err.Error(), "HTTP 401", "error")
}

func TestOllamaModels(t *testing.T) {
	ck := assert.NewCollecting(t)

	r := newFakeRunner()
	r.script([]string{"ollama", "list"},
		"NAME                ID              SIZE      MODIFIED\n"+
			"llama3:latest        abc123          4.7 GB    2 days ago\n"+
			"qwen2.5:7b           def456          4.4 GB    3 days ago\n")
	models, err := ollamaModels(context.Background(), r)
	ck.Require().NoError(err, "ollamaModels")
	ck.EqDeep([]string{"llama3:latest", "qwen2.5:7b"}, models, "models in order")

	headerOnly := newFakeRunner()
	headerOnly.script([]string{"ollama", "list"}, "NAME  ID  SIZE  MODIFIED\n")
	models, err = ollamaModels(context.Background(), headerOnly)
	ck.Require().NoError(err, "ollamaModels header only")
	ck.Empty(models, "header-only output yields no models")
}

func TestOllamaPull(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := newFakeRunner()
	ck.Require().NoError(ollamaPull(context.Background(), r, "qwen3.5:9b"), "ollamaPull")
	ck.Require().Len(r.interCalls, 1, "one interactive call")
	ck.EqDeep([]string{"ollama", "pull", "qwen3.5:9b"}, r.interCalls[0].argv, "argv")
}

func TestProbeEmbeddingsLearnsDims(t *testing.T) {
	ck := assert.NewCollecting(t)
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		ck.NoError(json.NewDecoder(r.Body).Decode(&b), "decode request body")
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		writeEmbedding(w, 768)
	}))
	defer srv.Close()

	dims, err := probeEmbeddings(context.Background(), srv.URL, "", "m")
	ck.Require().NoError(err, "probeEmbeddings")
	ck.Eq(768, dims, "dims")
	mu.Lock()
	defer mu.Unlock()
	ck.Require().Len(bodies, 2, "two probe calls")
	ck.NotHasKey(bodies[0], "dimensions", "first call must omit dimensions")
	ck.Eq(float64(768), bodies[1]["dimensions"].(float64), "second call asks for the learned dims")
}

func TestProbeEmbeddingsRefusesOver2000(t *testing.T) {
	ck := assert.NewCollecting(t)
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		writeEmbedding(w, 3072)
	}))
	defer srv.Close()

	dims, err := probeEmbeddings(context.Background(), srv.URL, "", "big")
	ck.Require().Error(err, "probeEmbeddings over 2000")
	ck.StrContains(err.Error(), "at most 2000", "error")
	ck.Eq(0, dims, "dims on error")
	mu.Lock()
	defer mu.Unlock()
	ck.Eq(1, calls, "no second request after the dimension refusal")
}

func TestProbeEmbeddingsSecondCallRejected(t *testing.T) {
	ck := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		ck.NoError(json.NewDecoder(r.Body).Decode(&b), "decode request body")
		if _, ok := b["dimensions"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("no dimensions please"))
			return
		}
		writeEmbedding(w, 3)
	}))
	defer srv.Close()

	_, err := probeEmbeddings(context.Background(), srv.URL, "", "m")
	ck.Require().Error(err, "probeEmbeddings with a rejecting endpoint")
	ck.StrContains(err.Error(), "rejected rafiki's embeddings request", "error")
}

func TestProbeEmbeddingsSendsBearer(t *testing.T) {
	ck := assert.NewCollecting(t)
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		writeEmbedding(w, 4)
	}))
	defer srv.Close()

	dims, err := probeEmbeddings(context.Background(), srv.URL, "k", "m")
	ck.Require().NoError(err, "probeEmbeddings")
	ck.Eq(4, dims, "dims")
	mu.Lock()
	defer mu.Unlock()
	ck.Require().Len(auths, 2, "both probe calls")
	ck.Eq("Bearer k", auths[0], "first call Authorization")
	ck.Eq("Bearer k", auths[1], "second call Authorization")
}
