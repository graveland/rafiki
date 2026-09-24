// SPDX-License-Identifier: Apache-2.0

// Package embed is the HTTP client for a self-contained, OpenAI-shaped
// /embeddings endpoint (the [embeddings] table in providers.toml). It
// implements recall.Embedder: the daemon's indexer feeds it windows,
// summaries and memories in batches and stores the vectors for recall.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/recall"
)

// Client posts batches of inputs to one OpenAI-shaped /embeddings endpoint.
type Client struct {
	cfg providers.EmbeddingsConfig
	hc  *http.Client
}

var _ recall.Embedder = (*Client)(nil)

// New builds a client for cfg. A nil hc means http.DefaultClient.
func New(cfg providers.EmbeddingsConfig, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{cfg: cfg, hc: hc}
}

// Model is the model id sent with every request.
func (c *Client) Model() string { return c.cfg.Model }

// Dimensions is the vector length every response vector must have.
func (c *Client) Dimensions() int { return c.cfg.Dimensions }

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
}

type embedResponse struct {
	Data []embedVector `json:"data"`
}

type embedVector struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embed returns one vector per input, ordered by the server's per-vector
// index field. Empty input is a no-op returning (nil, nil): callers batch by
// recall.EmbedBatchSize and an empty batch must not produce a request.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.cfg.Model, Input: inputs, Dimensions: c.cfg.Dimensions})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Authorization only when a credential resolves; a keyless endpoint gets
	// no header at all. Accept-Encoding is deliberately NOT set: the transport
	// adds it itself and transparently gunzips — setting it by hand disables
	// that decompression and yields raw gzip bytes with no error.
	if key := c.cfg.APIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: POST %s: %w", c.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embed: %s: %s", resp.Status, bodySnippet(b))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}
	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w: %s", err, bodySnippet(raw))
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embed: response has %d vectors for %d inputs", len(out.Data), len(inputs))
	}
	vecs := make([][]float32, len(inputs))
	for _, v := range out.Data {
		if v.Index < 0 || v.Index >= len(inputs) {
			return nil, fmt.Errorf("embed: vector index %d out of range for %d inputs", v.Index, len(inputs))
		}
		if len(v.Embedding) != c.cfg.Dimensions {
			return nil, fmt.Errorf("embed: vector %d has %d dimensions, want %d", v.Index, len(v.Embedding), c.cfg.Dimensions)
		}
		vecs[v.Index] = v.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embed: response missing vector for input %d", i)
		}
	}
	return vecs, nil
}

// bodySnippet returns at most the first 512 bytes of a body, for inclusion in
// errors: the "fails loudly" contract — the operator sees what the server
// actually said rather than a bare status code.
func bodySnippet(b []byte) string {
	if len(b) > 512 {
		return string(b[:512])
	}
	return string(b)
}
