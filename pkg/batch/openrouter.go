// SPDX-License-Identifier: Apache-2.0

package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the OpenRouter API root used when NewOpenRouter gets
	// an empty base URL.
	DefaultBaseURL = "https://openrouter.ai/api"
	// messagesEndpoint is the Batch API endpoint each submitted call targets.
	messagesEndpoint = "/v1/messages"
	// batchListLimit is the page size used when listing batches.
	batchListLimit = 100
)

// OpenRouter is a minimal client for the OpenRouter Batch API: submit one
// batch, fetch one batch, and page the batch listing. It carries no state
// machine of its own — the Batcher owns every decision; this type only speaks
// HTTP.
type OpenRouter struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// NewOpenRouter returns a client rooted at baseURL (DefaultBaseURL when
// empty), authenticating with apiKey. hc may be nil, which selects a client
// with a two-minute timeout; every request also carries the caller's ctx.
func NewOpenRouter(baseURL, apiKey string, hc *http.Client) *OpenRouter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &OpenRouter{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      hc,
	}
}

// BatchRequest is one per-call entry of a submit body.
type BatchRequest struct {
	CustomID string          `json:"custom_id"`
	Body     json.RawMessage `json:"body"`
}

// APIError is the provider's error object, carried both by a failed batch
// (batch level) and by an individual result item.
type APIError struct {
	Code    any    `json:"code,omitempty"`
	Message string `json:"message"`
}

// BatchResponse is the per-result response envelope of a completed batch.
type BatchResponse struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
}

// BatchResult is one result entry of a terminal batch. Exactly one of
// Response (a HTTP status plus the Messages-API body) and Error is meaningful.
type BatchResult struct {
	CustomID string         `json:"custom_id"`
	Response *BatchResponse `json:"response,omitempty"`
	Error    *APIError      `json:"error,omitempty"`
}

// Batch is the provider batch object, restricted to the fields the Batcher
// reads.
type Batch struct {
	ID        string        `json:"id"`
	Status    string        `json:"status"`
	CreatedAt int64         `json:"created_at"` // unix seconds
	Error     *APIError     `json:"error,omitempty"`
	Results   []BatchResult `json:"results,omitempty"`
}

// Terminal reports whether the batch has reached a final status:
// completed, failed, expired or cancelled.
func (b Batch) Terminal() bool {
	switch b.Status {
	case "completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

// BatchList is one page of the batch listing.
type BatchList struct {
	Data    []Batch `json:"data"`
	HasMore bool    `json:"has_more"`
}

// httpStatusError is a non-2xx Batch API response. Status is what the request
// PROVED (e.g. a 429 proves nothing was created; a 422 proves the batch would
// fail identically on every retry); Body is the response body text, which the
// Batcher stores on failed rows and delivers to waiters.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("batch API returned %d: %s", e.Status, e.Body)
}

// Submit creates one batch from requests against batchModel (the `:batch`
// model id) on the /v1/messages endpoint. A non-2xx response is returned as
// an *httpStatusError carrying the response body text.
func (o *OpenRouter) Submit(ctx context.Context, batchModel string, requests []BatchRequest) (Batch, error) {
	payload, err := json.Marshal(struct {
		Endpoint string         `json:"endpoint"`
		Model    string         `json:"model"`
		Requests []BatchRequest `json:"requests"`
	}{Endpoint: messagesEndpoint, Model: batchModel, Requests: requests})
	if err != nil {
		return Batch{}, fmt.Errorf("batch: marshal submit body: %w", err)
	}
	var out Batch
	err = o.do(ctx, http.MethodPost, "/v1/batches", payload, &out)
	if err != nil {
		return Batch{}, err
	}
	return out, nil
}

// Batch fetches one batch by provider id.
func (o *OpenRouter) Batch(ctx context.Context, id string) (Batch, error) {
	var out Batch
	err := o.do(ctx, http.MethodGet, "/v1/batches/"+url.PathEscape(id), nil, &out)
	if err != nil {
		return Batch{}, err
	}
	return out, nil
}

// List fetches one page of the batch listing, newest first. after continues
// the page at (exclusive of) the given batch id; limit is the page size
// (batchListLimit when <= 0).
func (o *OpenRouter) List(ctx context.Context, after string, limit int) (BatchList, error) {
	if limit <= 0 {
		limit = batchListLimit
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if after != "" {
		q.Set("after", after)
	}
	var out BatchList
	err := o.do(ctx, http.MethodGet, "/v1/batches?"+q.Encode(), nil, &out)
	if err != nil {
		return BatchList{}, err
	}
	return out, nil
}

// do performs one authenticated request and decodes the 2xx body into out.
func (o *OpenRouter) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, rd)
	if err != nil {
		return fmt.Errorf("batch: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		// Transport error, no response: the caller cannot know whether the
		// request landed (Submit relies on this distinction).
		return fmt.Errorf("batch: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("batch: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpStatusError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("batch: decode %s %s: %w", method, path, err)
	}
	return nil
}
