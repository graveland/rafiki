// SPDX-License-Identifier: Apache-2.0

package batch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	batch "go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/batch/batchtest"
)

// This file is the EXTERNAL test package (batch_test) because it runs the
// batchtest conformance suite against the in-memory store: batchtest imports
// pkg/batch, so an in-package test file importing it would be an import
// cycle. Everything the tests need is on the exported surface; the fake
// store's clock is injected through batch.NewMemStoreWithClock.

// fakeClock is the injected clock shared by the batcher and the store, so the
// recovery sweep's window math lines up with the row timestamps it reads.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// submitRecord is one captured POST /v1/batches. batchID is the id the fake
// server answered with ("" when the response carried no batch, e.g. an error
// status) — the request body cannot contain it.
type submitRecord struct {
	endpoint string
	model    string
	requests []batch.BatchRequest
	raw      []byte
	batchID  string
}

// harness wires a Batcher to an in-memory store and a fake OpenRouter server.
// Endpoints answer with sane defaults (submit → fresh in_progress batch,
// GET → the same batch still in_progress, list → empty) and tests override
// behavior per case; every field access is guarded, because handlers run on
// the server's goroutines.
type harness struct {
	t      *testing.T
	clock  *fakeClock
	ms     *batch.MemStore
	srv    *httptest.Server
	api    *batch.OpenRouter
	b      *batch.Batcher
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	submits     []submitRecord
	gets        []string
	lists       int
	nextBatchID int

	submitFn func(raw []byte) (int, string)
	getFn    func(id string) (int, string)
	listFn   func() (int, string)
	postHook func(raw []byte)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: newFakeClock()}
	h.ms = batch.NewMemStoreWithClock(h.clock.Now)
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	h.api = batch.NewOpenRouter(h.srv.URL, "sk-test", h.srv.Client())
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	h.b = batch.New(h.ms, h.api, batch.Options{Window: 15 * time.Millisecond, Poll: 15 * time.Millisecond, Now: h.clock.Now})
	return h
}

func (h *harness) start() {
	h.t.Helper()
	go h.b.Start(h.ctx)
}

func (h *harness) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/batches":
		h.serveSubmit(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/batches":
		h.serveList(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/batches/"):
		h.serveGet(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *harness) serveSubmit(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	if h.postHook != nil {
		h.postHook(raw)
	}
	fn := h.submitFn
	h.mu.Unlock()

	status, body := http.StatusOK, ""
	switch {
	case fn != nil:
		status, body = fn(raw)
	default:
		h.mu.Lock()
		h.nextBatchID++
		body = h.batchBodyLocked(fmt.Sprintf("batch-%d", h.nextBatchID), "in_progress")
		h.mu.Unlock()
	}

	h.mu.Lock()
	var parsed struct {
		Endpoint string               `json:"endpoint"`
		Model    string               `json:"model"`
		Requests []batch.BatchRequest `json:"requests"`
	}
	_ = json.Unmarshal(raw, &parsed)
	var respID struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &respID)
	h.submits = append(h.submits, submitRecord{
		endpoint: parsed.Endpoint,
		model:    parsed.Model,
		requests: parsed.Requests,
		raw:      raw,
		batchID:  respID.ID,
	})
	h.mu.Unlock()

	w.WriteHeader(status)
	if body != "" {
		_, _ = io.WriteString(w, body)
	}
}

func (h *harness) serveGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/batches/")
	h.mu.Lock()
	h.gets = append(h.gets, id)
	fn := h.getFn
	h.mu.Unlock()

	status, body := http.StatusOK, ""
	switch {
	case fn != nil:
		status, body = fn(id)
	default:
		h.mu.Lock()
		body = h.batchBodyLocked(id, "in_progress")
		h.mu.Unlock()
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (h *harness) serveList(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.lists++
	fn := h.listFn
	h.mu.Unlock()

	status, body := http.StatusOK, `{"data":[],"has_more":false}`
	if fn != nil {
		status, body = fn()
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// batchBodyLocked renders a batch object; callers hold h.mu.
func (h *harness) batchBodyLocked(id, status string) string {
	b, err := json.Marshal(batch.Batch{ID: id, Status: status, CreatedAt: h.clock.Now().Unix()})
	if err != nil {
		return `{"id":"` + id + `","status":"` + status + `"}`
	}
	return string(b)
}

func (h *harness) setSubmit(fn func(raw []byte) (int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.submitFn = fn
}

func (h *harness) setGet(fn func(id string) (int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.getFn = fn
}

func (h *harness) setListFn(fn func() (int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listFn = fn
}

// setListJSON makes the listing return exactly these batches, one page.
func (h *harness) setListJSON(batches []batch.Batch) {
	payload, err := json.Marshal(batch.BatchList{Data: batches})
	if err != nil {
		h.t.Fatalf("marshal list: %v", err)
	}
	h.setListFn(func() (int, string) { return http.StatusOK, string(payload) })
}

func (h *harness) setBatchJSON(id string, b batch.Batch) {
	payload, err := json.Marshal(b)
	if err != nil {
		h.t.Fatalf("marshal batch: %v", err)
	}
	h.setGet(func(got string) (int, string) {
		if got == id {
			return http.StatusOK, string(payload)
		}
		return http.StatusNotFound, "no such batch"
	})
}

// nextSubmitBody allocates a fresh batch id and returns its in_progress body.
func (h *harness) nextSubmitBody() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextBatchID++
	return h.batchBodyLocked(fmt.Sprintf("batch-%d", h.nextBatchID), "in_progress")
}

func (h *harness) completeBatch(id string, results ...batch.BatchResult) {
	h.setBatchJSON(id, batch.Batch{ID: id, Status: "completed", CreatedAt: h.clock.Now().Unix(), Results: results})
}

func (h *harness) failBatch(id, status, msg string) {
	h.setBatchJSON(id, batch.Batch{ID: id, Status: status, CreatedAt: h.clock.Now().Unix(), Error: &batch.APIError{Message: msg}})
}

func (h *harness) submitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.submits)
}

func (h *harness) submitsSnapshot() []submitRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]submitRecord(nil), h.submits...)
}

func (h *harness) listCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lists
}

type parkRes struct {
	msg *anthropic.Message
	err error
}

func (h *harness) park(customID, model string) <-chan parkRes {
	return h.parkParams(customID, model, testParams(model))
}

func (h *harness) parkParams(customID, model string, params anthropic.MessageNewParams) <-chan parkRes {
	return h.parkCtx(h.ctx, customID, model, params)
}

func (h *harness) parkCtx(ctx context.Context, customID, model string, params anthropic.MessageNewParams) <-chan parkRes {
	ch := make(chan parkRes, 1)
	go func() {
		msg, err := h.b.Park(ctx, customID, model, params)
		ch <- parkRes{msg: msg, err: err}
	}()
	return ch
}

func (h *harness) await(t *testing.T, ch <-chan parkRes, d time.Duration) parkRes {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(d):
		t.Fatalf("park did not return within %v", d)
		return parkRes{}
	}
}

// waitSubmits blocks until at least n submits are recorded, then returns the
// snapshot. All parks under test insert their rows BEFORE h.start, so every
// window tick sees the full group.
func (h *harness) waitSubmits(t *testing.T, n int, d time.Duration) []submitRecord {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if h.submitCount() >= n {
			return h.submitsSnapshot()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("only %d submits within %v, want %d", h.submitCount(), d, n)
	return nil
}

func (h *harness) waitInState(t *testing.T, st batch.State, n int, d time.Duration) []batch.Row {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		rows, err := h.ms.InState(context.Background(), st)
		if err == nil && len(rows) >= n {
			return rows
		}
		time.Sleep(2 * time.Millisecond)
	}
	rows, _ := h.ms.InState(context.Background(), st)
	t.Fatalf("only %d rows in state %q within %v", len(rows), st, d)
	return nil
}

// seedSubmitting inserts a row already in batch.StateSubmitting, as if the previous
// process died between MarkSubmitting and MarkSubmitted.
func (h *harness) seedSubmitting(t *testing.T, customID, model string) batch.Row {
	t.Helper()
	row, err := h.ms.Insert(context.Background(), batch.Row{
		CustomID:  customID,
		Model:     model,
		State:     batch.StateQueued,
		Request:   json.RawMessage(`{"max_tokens":32}`),
		CreatedAt: h.clock.Now(),
		UpdatedAt: h.clock.Now(),
	})
	if err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	if err := h.ms.MarkSubmitting(context.Background(), []int64{row.ID}); err != nil {
		t.Fatalf("seed MarkSubmitting: %v", err)
	}
	return row
}

// seedSubmitted continues the seeding to batch.StateSubmitted with a batch id.
func (h *harness) seedSubmitted(t *testing.T, customID, model, batchID string) batch.Row {
	t.Helper()
	row := h.seedSubmitting(t, customID, model)
	if err := h.ms.MarkSubmitted(context.Background(), []int64{row.ID}, batchID); err != nil {
		t.Fatalf("seed MarkSubmitted: %v", err)
	}
	return row
}

func testParams(model string) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 32,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
	}
}

func testParamsWithExtras(model string) anthropic.MessageNewParams {
	params := testParams(model)
	params.SetExtraFields(map[string]any{
		"stream":   false,
		"provider": map[string]any{"order": []string{"vendor"}},
	})
	return params
}

// msgBody is a minimal Messages-API response body for message id.
func msgBody(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","type":"message","role":"assistant","model":"vendor/m",` +
		`"content":[{"type":"text","text":"hi from ` + id + `"}],"stop_reason":"end_turn",` +
		`"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
}

func TestMemStoreConformance(t *testing.T) {
	batchtest.Run(t, func(t *testing.T) batch.Store { return batch.NewMemStore() })
}

func TestBatchCoalescesPerModel(t *testing.T) {
	h := newHarness(t)
	const (
		modelA = "vendor/a:batch"
		modelB = "vendor/b:batch"
	)
	_ = h.park("conv-1", modelA)
	_ = h.park("conv-2", modelA)
	_ = h.park("conv-3", modelB)
	// All three rows must be queued before the loop starts, so the first
	// window coalesces them.
	h.waitInState(t, batch.StateQueued, 3, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 2, 5*time.Second)
	if len(submits) != 2 {
		t.Fatalf("submits = %d, want 2 (one per model)", len(submits))
	}
	perModel := map[string]int{}
	for _, s := range submits {
		if s.endpoint != "/v1/messages" {
			t.Errorf("submit endpoint = %q, want %q", s.endpoint, "/v1/messages")
		}
		perModel[s.model] += len(s.requests)
	}
	if perModel[modelA] != 2 || perModel[modelB] != 1 {
		t.Fatalf("requests per model = %v, want %s:2 %s:1", perModel, modelA, modelB)
	}
}

func TestBatchSubmittingBeforePost(t *testing.T) {
	h := newHarness(t)
	ids := []string{"conv-1", "conv-2"}
	for _, id := range ids {
		_ = h.park(id, "vendor/m:batch")
	}
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.mu.Lock()
	h.postHook = func(raw []byte) {
		var parsed struct {
			Requests []batch.BatchRequest `json:"requests"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Errorf("post hook: %v", err)
			return
		}
		for _, req := range parsed.Requests {
			row, ok, err := h.ms.Live(context.Background(), req.CustomID)
			if err != nil || !ok {
				t.Errorf("live(%s): ok=%v err=%v", req.CustomID, ok, err)
				continue
			}
			if row.State != batch.StateSubmitting {
				t.Errorf("row %s state = %q at POST time, want submitting", req.CustomID, row.State)
			}
		}
	}
	h.mu.Unlock()
	h.start()
	h.waitSubmits(t, 1, 5*time.Second)
}

func TestBatchBodyShape(t *testing.T) {
	h := newHarness(t)
	const model = "vendor/m:batch"
	_ = h.parkParams("conv-1", model, testParamsWithExtras(model))
	h.waitInState(t, batch.StateQueued, 1, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	var body struct {
		Endpoint string `json:"endpoint"`
		Model    string `json:"model"`
		Requests []struct {
			CustomID string          `json:"custom_id"`
			Body     json.RawMessage `json:"body"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(submits[0].raw, &body); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	if body.Endpoint != "/v1/messages" {
		t.Errorf("endpoint = %q, want /v1/messages", body.Endpoint)
	}
	if body.Model != model {
		t.Errorf("batch model = %q, want the :batch id %q", body.Model, model)
	}
	if len(body.Requests) != 1 || body.Requests[0].CustomID != "conv-1" {
		t.Fatalf("requests = %+v", body.Requests)
	}
	var callBody map[string]any
	if err := json.Unmarshal(body.Requests[0].Body, &callBody); err != nil {
		t.Fatalf("decode call body: %v", err)
	}
	for _, banned := range []string{"model", "stream", "provider"} {
		if _, ok := callBody[banned]; ok {
			t.Errorf("call body contains %q", banned)
		}
	}
	for _, kept := range []string{"max_tokens", "messages"} {
		if _, ok := callBody[kept]; !ok {
			t.Errorf("call body lost %q", kept)
		}
	}
}

func TestBatchResultsOutOfOrder(t *testing.T) {
	h := newHarness(t)
	ch1 := h.park("conv-1", "vendor/m:batch")
	ch2 := h.park("conv-2", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	batchID := submits[0].batchID
	// Results arrive reversed: conv-2 first.
	h.completeBatch(batchID,
		batch.BatchResult{CustomID: "conv-2", Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_2")}},
		batch.BatchResult{CustomID: "conv-1", Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_1")}},
	)
	res1 := h.await(t, ch1, 5*time.Second)
	res2 := h.await(t, ch2, 5*time.Second)
	if res1.err != nil || res1.msg == nil || res1.msg.ID != "msg_1" {
		t.Errorf("conv-1: msg=%+v err=%v", res1.msg, res1.err)
	}
	if res2.err != nil || res2.msg == nil || res2.msg.ID != "msg_2" {
		t.Errorf("conv-2: msg=%+v err=%v", res2.msg, res2.err)
	}
}

func TestBatchWholeBatchFailureFansOut(t *testing.T) {
	h := newHarness(t)
	ch1 := h.park("conv-1", "vendor/m:batch")
	ch2 := h.park("conv-2", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	h.failBatch(submits[0].batchID, "failed", "upstream exploded")
	for i, ch := range []<-chan parkRes{ch1, ch2} {
		res := h.await(t, ch, 5*time.Second)
		var be *batch.Error
		if !errors.As(res.err, &be) {
			t.Fatalf("waiter %d: err %v (%T) is not *batch.Error", i, res.err, res.err)
		}
		if !strings.Contains(res.err.Error(), "upstream exploded") {
			t.Errorf("waiter %d: error = %q, want the batch error message", i, res.err.Error())
		}
	}
}

func TestBatchPerRequestErrorIsolated(t *testing.T) {
	h := newHarness(t)
	ch1 := h.park("conv-1", "vendor/m:batch")
	ch2 := h.park("conv-2", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	h.completeBatch(submits[0].batchID,
		batch.BatchResult{CustomID: "conv-1", Error: &batch.APIError{Message: "quota exceeded"}},
		batch.BatchResult{CustomID: "conv-2", Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_2")}},
	)
	res1 := h.await(t, ch1, 5*time.Second)
	res2 := h.await(t, ch2, 5*time.Second)
	var be *batch.Error
	if !errors.As(res1.err, &be) || !strings.Contains(res1.err.Error(), "quota exceeded") {
		t.Errorf("conv-1: err = %v, want *batch.Error with the item's error", res1.err)
	}
	if res2.err != nil || res2.msg == nil || res2.msg.ID != "msg_2" {
		t.Errorf("conv-2: msg=%+v err=%v, want the successful result", res2.msg, res2.err)
	}
}

func TestBatchAdoptSubmitted(t *testing.T) {
	h := newHarness(t)
	h.seedSubmitted(t, "conv-1", "vendor/m:batch", "batch-9")
	h.completeBatch("batch-9",
		batch.BatchResult{CustomID: "conv-1", Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_9")}},
	)
	ch := h.park("conv-1", "vendor/m:batch")
	h.start()
	res := h.await(t, ch, 5*time.Second)
	if res.err != nil || res.msg == nil || res.msg.ID != "msg_9" {
		t.Fatalf("park: msg=%+v err=%v", res.msg, res.err)
	}
	if got := h.submitCount(); got != 0 {
		t.Errorf("submits = %d, want 0 (adoption must not POST)", got)
	}
}

func TestBatchAdoptCompleted(t *testing.T) {
	h := newHarness(t)
	row := h.seedSubmitted(t, "conv-1", "vendor/m:batch", "batch-9")
	if err := h.ms.Complete(context.Background(), row.ID, msgBody("msg_9")); err != nil {
		t.Fatalf("seed Complete: %v", err)
	}
	h.start()
	msg, err := h.b.Park(h.ctx, "conv-1", "vendor/m:batch", testParams("vendor/m:batch"))
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if msg == nil || msg.ID != "msg_9" {
		t.Errorf("Park returned %+v, want the stored message", msg)
	}
	if got := h.submitCount(); got != 0 {
		t.Errorf("submits = %d, want 0 (completed rows return without a POST)", got)
	}
}

func TestBatchFailedRowRetries(t *testing.T) {
	h := newHarness(t)
	row, err := h.ms.Insert(context.Background(), batch.Row{
		CustomID:  "conv-1",
		Model:     "vendor/m:batch",
		State:     batch.StateQueued,
		Request:   json.RawMessage(`{"max_tokens":32}`),
		CreatedAt: h.clock.Now(),
		UpdatedAt: h.clock.Now(),
	})
	if err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	if err := h.ms.MarkSubmitting(context.Background(), []int64{row.ID}); err != nil {
		t.Fatalf("seed MarkSubmitting: %v", err)
	}
	if err := h.ms.Fail(context.Background(), row.ID, "boom"); err != nil {
		t.Fatalf("seed Fail: %v", err)
	}
	ch := h.park("conv-1", "vendor/m:batch")
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	if got := h.submitCount(); got != 1 {
		t.Errorf("submits = %d, want exactly one POST for the fresh row", got)
	}
	h.waitInState(t, batch.StateSubmitted, 1, 5*time.Second)
	failed, _ := h.ms.InState(context.Background(), batch.StateFailed)
	if len(failed) != 0 {
		t.Errorf("InState(failed) = %+v, want empty (old row tombstoned)", failed)
	}
	submitted, _ := h.ms.InState(context.Background(), batch.StateSubmitted)
	if len(submitted) != 1 || submitted[0].CustomID != "conv-1" || submitted[0].ID == row.ID {
		t.Errorf("InState(submitted) = %+v, want one fresh row for conv-1", submitted)
	}
	h.completeBatch(submits[0].batchID,
		batch.BatchResult{CustomID: "conv-1", Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_new")}},
	)
	res := h.await(t, ch, 5*time.Second)
	if res.err != nil || res.msg == nil || res.msg.ID != "msg_new" {
		t.Errorf("park: msg=%+v err=%v", res.msg, res.err)
	}
}

func TestBatchRecoverySubmittingMatched(t *testing.T) {
	h := newHarness(t)
	h.seedSubmitting(t, "conv-1", "vendor/m:batch")
	h.setListJSON([]batch.Batch{{
		ID:        "batch-7",
		Status:    "completed",
		CreatedAt: h.clock.Now().Unix(),
		Results: []batch.BatchResult{{
			CustomID: "conv-1",
			Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_7")},
		}},
	}})
	h.clock.Advance(10 * time.Second)
	ch := h.park("conv-1", "vendor/m:batch")
	h.start()
	res := h.await(t, ch, 5*time.Second)
	if res.err != nil || res.msg == nil || res.msg.ID != "msg_7" {
		t.Fatalf("park: msg=%+v err=%v", res.msg, res.err)
	}
	rows, _ := h.ms.InState(context.Background(), batch.StateCompleted)
	if len(rows) != 1 || rows[0].ProviderBatchID != "batch-7" {
		t.Errorf("InState(completed) = %+v, want the row adopted onto batch-7", rows)
	}
	if got := h.submitCount(); got != 0 {
		t.Errorf("submits = %d, want 0 (recovery must not resubmit)", got)
	}
}

func TestBatchRecoverySubmittingNeverLanded(t *testing.T) {
	h := newHarness(t)
	h.seedSubmitting(t, "conv-1", "vendor/m:batch")
	// Past the grace window with an empty listing: the POST never landed.
	h.clock.Advance(400 * time.Second)
	_ = h.park("conv-1", "vendor/m:batch")
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	rows := h.waitInState(t, batch.StateSubmitted, 1, 5*time.Second)
	if len(rows) != 1 || rows[0].ProviderBatchID != submits[0].batchID {
		t.Errorf("InState(submitted) = %+v, want the requeued row submitted once", rows)
	}
	// One POST only: the requeue must not have resubmitted a phantom batch.
	time.Sleep(60 * time.Millisecond)
	if got := h.submitCount(); got != 1 {
		t.Errorf("submits = %d after several windows, want 1", got)
	}
}

func TestBatchRecoverySubmittingWaitsOnNonTerminal(t *testing.T) {
	h := newHarness(t)
	h.seedSubmitting(t, "conv-1", "vendor/m:batch")
	h.setListJSON([]batch.Batch{{
		ID:        "batch-3",
		Status:    "in_progress",
		CreatedAt: h.clock.Now().Unix(),
		Results: []batch.BatchResult{{
			CustomID: "conv-1",
			Response: &batch.BatchResponse{StatusCode: 200, Body: msgBody("msg_3")},
		}},
	}})
	h.clock.Advance(10 * time.Second)
	_ = h.park("conv-1", "vendor/m:batch")
	h.start()
	time.Sleep(80 * time.Millisecond) // several poll+recovery cycles
	rows, _ := h.ms.InState(context.Background(), batch.StateSubmitting)
	if len(rows) != 1 || rows[0].CustomID != "conv-1" {
		t.Errorf("InState(submitting) = %+v, want the row still waiting", rows)
	}
	if got := h.listCount(); got < 1 {
		t.Errorf("list calls = %d, want the sweep to have listed", got)
	}
	if got := h.submitCount(); got != 0 {
		t.Errorf("submits = %d, want 0", got)
	}
}

func TestBatchPost429Requeues(t *testing.T) {
	h := newHarness(t)
	_ = h.park("conv-1", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 1, 5*time.Second)
	var calls atomic.Int32
	h.setSubmit(func(raw []byte) (int, string) {
		if calls.Add(1) == 1 {
			return http.StatusTooManyRequests, "rate limited"
		}
		return http.StatusOK, h.nextSubmitBody()
	})
	h.start()
	submits := h.waitSubmits(t, 2, 5*time.Second)
	rows := h.waitInState(t, batch.StateSubmitted, 1, 5*time.Second)
	if len(rows) != 1 || rows[0].ProviderBatchID != submits[1].batchID {
		t.Errorf("InState(submitted) = %+v, want the row on the second POST's batch", rows)
	}
}

func TestBatchPost4xxFailsRows(t *testing.T) {
	h := newHarness(t)
	ch1 := h.park("conv-1", "vendor/m:batch")
	ch2 := h.park("conv-2", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.setSubmit(func(raw []byte) (int, string) {
		return http.StatusUnprocessableEntity, `{"error":{"message":"bad custom_id"}}`
	})
	h.start()
	res1 := h.await(t, ch1, 5*time.Second)
	res2 := h.await(t, ch2, 5*time.Second)
	for i, res := range []parkRes{res1, res2} {
		var be *batch.Error
		if !errors.As(res.err, &be) || !strings.Contains(res.err.Error(), "bad custom_id") {
			t.Errorf("waiter %d: err = %v, want *batch.Error with the response body text", i, res.err)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if got := h.submitCount(); got != 1 {
		t.Errorf("submits = %d, want 1 (a 422 must not be retried)", got)
	}
	failed, _ := h.ms.InState(context.Background(), batch.StateFailed)
	if len(failed) != 2 {
		t.Errorf("InState(failed) = %+v, want both rows failed", failed)
	}
}

func TestBatchPost5xxLeavesSubmitting(t *testing.T) {
	h := newHarness(t)
	_ = h.park("conv-1", "vendor/m:batch")
	_ = h.park("conv-2", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 2, 5*time.Second)
	h.setSubmit(func(raw []byte) (int, string) {
		return http.StatusInternalServerError, "boom"
	})
	h.start()
	h.waitSubmits(t, 1, 5*time.Second)
	time.Sleep(60 * time.Millisecond) // several windows
	if got := h.submitCount(); got != 1 {
		t.Errorf("submits = %d, want 1 (a 5xx leaves the rows submitting)", got)
	}
	rows, _ := h.ms.InState(context.Background(), batch.StateSubmitting)
	if len(rows) != 2 {
		t.Errorf("InState(submitting) = %+v, want both rows left for recovery", rows)
	}
}

func TestBatchCtxCancelKeepsRow(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.parkCtx(ctx, "conv-1", "vendor/m:batch", testParams("vendor/m:batch"))
	h.waitInState(t, batch.StateQueued, 1, 5*time.Second)
	cancel()
	res := h.await(t, ch, 5*time.Second)
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Park err = %v, want context.Canceled", res.err)
	}
	rows, _ := h.ms.InState(context.Background(), batch.StateQueued)
	if len(rows) != 1 || rows[0].CustomID != "conv-1" {
		t.Fatalf("InState(queued) = %+v, want the row kept for a resumed child", rows)
	}
}

func TestBatchErrorIsPlain(t *testing.T) {
	h := newHarness(t)
	ch := h.park("conv-1", "vendor/m:batch")
	h.waitInState(t, batch.StateQueued, 1, 5*time.Second)
	h.start()
	submits := h.waitSubmits(t, 1, 5*time.Second)
	h.failBatch(submits[0].batchID, "failed", "upstream exploded")
	res := h.await(t, ch, 5*time.Second)
	var be *batch.Error
	if !errors.As(res.err, &be) {
		t.Fatalf("delivered error %v (%T) does not satisfy errors.As(*batch.Error)", res.err, res.err)
	}
	var ae *anthropic.Error
	if errors.As(res.err, &ae) {
		t.Fatalf("delivered error masquerades as *anthropic.Error: %v", res.err)
	}
	if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
		t.Fatalf("delivered error wraps a context error: %v", res.err)
	}
}
