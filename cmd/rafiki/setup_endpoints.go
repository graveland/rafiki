// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/embed"
	"go.graveland.dev/rafiki/pkg/providers"
)

// ollamaBaseURL is where setup looks for a local Ollama. A package var so
// tests point it at an httptest.Server; tests must never reach a real Ollama.
var ollamaBaseURL = "http://localhost:11434"

const (
	// ollamaSuggestedChatModel is offered when Ollama has no models: small,
	// tool-capable (fundi needs tool calls). Checked against ollama.com/library 2026-10-16.
	ollamaSuggestedChatModel = "qwen3.5:9b"
	// ollamaSuggestedEmbedModel is the suggested local embedding model: 137M
	// params, 768 dims, 8k context — runs on almost any machine.
	ollamaSuggestedEmbedModel = "nomic-embed-text:v1.5"
	// openrouterEmbeddingsURL and openrouterSuggestedEmbedModel are the
	// OpenRouter embeddings defaults: 1536 dims, 8k context, accepts dimensions.
	openrouterEmbeddingsURL       = "https://openrouter.ai/api/v1/embeddings"
	openrouterSuggestedEmbedModel = "openai/text-embedding-3-small"
	// maxEmbeddingDims is pgvector's HNSW limit on vector(N): recalldb's
	// EnsureVectorIndexes builds HNSW on embedding::vector(dims).
	maxEmbeddingDims = 2000
	// embedProbeInput is the text the dimension probe embeds.
	embedProbeInput = "rafiki setup embedding probe"
)

// checkOpenAIModels GETs strings.TrimRight(baseURL, "/") + "/models" with a 10s
// timeout and "Authorization: Bearer <apiKey>" when apiKey != "". Only a 200 is
// success; anything else is fmt.Errorf("HTTP %d", code), or the transport
// error. Never sets Accept-Encoding.
func checkOpenAIModels(ctx context.Context, baseURL, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	// Accept-Encoding is deliberately NOT set: the transport adds it itself and
	// transparently gunzips, and setting it by hand disables that.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ollamaAnswering reports whether checkOpenAIModels(ctx, ollamaBaseURL+"/v1", "") succeeds.
func ollamaAnswering(ctx context.Context) bool {
	return checkOpenAIModels(ctx, ollamaBaseURL+"/v1", "") == nil
}

// ollamaModels runs `ollama list` through r and returns the first
// whitespace-separated field of every non-empty line after the header line,
// in order. Empty output, or only a header, → empty slice, nil error.
func ollamaModels(ctx context.Context, r runner) ([]string, error) {
	out, _, err := r.Run(ctx, nil, "ollama", "list")
	if err != nil {
		return nil, err
	}
	var models []string
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // the header line ("NAME ID SIZE MODIFIED")
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		models = append(models, fields[0])
	}
	return models, nil
}

// ollamaPull runs `ollama pull <model>` through r.RunInteractive so its
// progress streams to the terminal.
func ollamaPull(ctx context.Context, r runner, model string) error {
	return r.RunInteractive(ctx, "ollama", "pull", model)
}

// probeEmbeddings learns an embeddings endpoint's native dimensions and proves
// the daemon's real request works. Call 1 POSTs
// {"model":model,"input":[embedProbeInput]} with no "dimensions" field,
// Content-Type application/json, Bearer auth when apiKey != "", 30s timeout; a
// non-200 or an empty data[0].embedding is an error. dims =
// len(data[0].embedding). dims > maxEmbeddingDims → error naming the model and
// the 2000 limit. Call 2 is
// embed.New(providers.EmbeddingsConfig{URL: url, Model: model, Dimensions:
// dims}, client).Embed(ctx, []string{embedProbeInput}) — exactly the request
// the daemon sends (pkg/embed always sends "dimensions"). Its error is wrapped
// as "the endpoint rejected rafiki's embeddings request: %w".
func probeEmbeddings(ctx context.Context, url, apiKey, model string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": []string{embedProbeInput},
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	// No Accept-Encoding and, crucially, no "dimensions": the first call must
	// learn the model's native length.
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("embeddings probe: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("embeddings probe: decode response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return 0, fmt.Errorf("embeddings probe: the endpoint returned no vector")
	}
	dims := len(out.Data[0].Embedding)
	if dims > maxEmbeddingDims {
		return 0, fmt.Errorf("embedding model %s returns %d dimensions; rafiki's vector index supports at most 2000", model, dims)
	}

	// Call 2 is the daemon's exact request. pkg/embed resolves its key from the
	// environment (EmbeddingsConfig.APIKey()), but setup holds the key value,
	// not an env var it can set — so the credential rides the transport.
	client := &http.Client{Transport: embedProbeTransport{apiKey: apiKey, base: http.DefaultTransport}}
	c := embed.New(providers.EmbeddingsConfig{URL: url, Model: model, Dimensions: dims}, client)
	if _, err := c.Embed(ctx, []string{embedProbeInput}); err != nil {
		return 0, fmt.Errorf("the endpoint rejected rafiki's embeddings request: %w", err)
	}
	return dims, nil
}

// embedProbeTransport wraps a base RoundTripper and sets a fixed bearer credential
// on a CLONE of each request, so a caller's request is never mutated. A keyless
// probe passes the request through untouched.
type embedProbeTransport struct {
	apiKey string
	base   http.RoundTripper
}

func (t embedProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.apiKey == "" {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return t.base.RoundTrip(clone)
}
