// SPDX-License-Identifier: Apache-2.0

// Package batch is the daemon-wide parked-call batcher: the durable row state
// machine (Store), the OpenRouter Batch API client (OpenRouter), and the
// Batcher that coalesces parked calls per model over a short window, submits
// one batch per model, polls batches to their terminal status, and delivers
// each outcome to the parked caller.
//
// The package is pgx-free and must not import pkg/llm. pkg/llm holds the
// Batcher through a single-method interface,
//
//	Park(ctx context.Context, customID, model string, params anthropic.MessageNewParams) (*anthropic.Message, error)
//
// and the durable Store implementation lives in pkg/batchdb.
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Error is the only error type the Batcher delivers for a failed parked call.
// It must stay PLAIN — never wrap an *anthropic.Error, context.DeadlineExceeded
// or a net error in it — because pkg/agentloop retries errors that look like
// those, and a retried parked send would resubmit an already-created batch.
// Every message is a string assembled from the batch outcome.
type Error struct {
	Msg string
}

// Error renders "batch: " + Msg.
func (e *Error) Error() string { return "batch: " + e.Msg }

// Default scheduling knobs for Options.
const (
	DefaultWindow = 30 * time.Second
	DefaultPoll   = 60 * time.Second
)

// The recovery sweep's candidate window: a submitting row's POST must have
// landed as a batch created_at in [UpdatedAt-60s, UpdatedAt+300s] to count,
// and a row with zero candidates requeues only once Now() has passed the
// grace window, because a listing can lag a just-created batch.
const (
	submitMatchBefore = 60 * time.Second
	submitGraceWindow = 300 * time.Second
	// maxListPages bounds how many listing pages one recovery sweep walks.
	maxListPages = 50
)

// Options tunes the Batcher; zero fields take defaults.
type Options struct {
	// Window is how long queued calls coalesce before one batch per model is
	// submitted. Default DefaultWindow.
	Window time.Duration
	// Poll is how often submitted batches are polled and submitting rows
	// recovered. Default DefaultPoll.
	Poll time.Duration
	// Now reads the wall clock for recovery window math. Default time.Now.
	// The Store stamps row times with its own clock; tests that exercise
	// recovery inject the same fake clock into both.
	Now func() time.Time
}

// Batcher parks Messages-API calls in the provider Batch API and delivers
// each outcome to the parked caller. One Batcher owns its Store and runs all
// provider traffic from the single goroutine Start spawns; Park only decides,
// inserts and waits.
type Batcher struct {
	store Store
	api   *OpenRouter
	opts  Options
	now   func() time.Time

	// mu guards waiters and serializes every Park decision (Live/adopt/
	// Insert/registration) against the delivery that ends a wait, so a
	// result either lands before the Live check (which then sees the final
	// row) or after the registration (and then finds the waiter).
	mu      sync.Mutex
	waiters map[string][]chan parkOutcome
}

// parkOutcome is what a waiter receives: the decoded message, or the plain
// *Error the row failed with. err is typed error, not *Error, so a nil stays
// an untyped nil — a typed (*Error)(nil) would surface as a non-nil error at
// Park's return and fail every successful park.
type parkOutcome struct {
	msg *anthropic.Message
	err error
}

// New returns a Batcher over store, submitting through api (nil disables all
// provider traffic; adoption and waiting still work) and applying defaults to
// opts's zero fields.
func New(store Store, api *OpenRouter, opts Options) *Batcher {
	if opts.Window <= 0 {
		opts.Window = DefaultWindow
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Batcher{
		store:   store,
		api:     api,
		opts:    opts,
		now:     now,
		waiters: make(map[string][]chan parkOutcome),
	}
}

// Start runs the batcher until ctx ends: one recovery sweep up front (a
// restart must resolve rows left submitting by the previous process before
// anything else), a submit window every Options.Window, and a result poll
// plus a recovery sweep every Options.Poll. It BLOCKS; run it as
// `go b.Start(ctx)`.
func (b *Batcher) Start(ctx context.Context) {
	b.recoverSubmitting(ctx)
	windowC := time.NewTicker(b.opts.Window)
	defer windowC.Stop()
	pollC := time.NewTicker(b.opts.Poll)
	defer pollC.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-windowC.C:
			b.flushQueued(ctx)
		case <-pollC.C:
			b.pollSubmitted(ctx)
			b.recoverSubmitting(ctx)
		}
	}
}

// Park parks one Messages-API call. It returns when the call's batch result
// is delivered, or ctx is cancelled.
//
// A parked call is adopted, not duplicated: if a live row for customID
// already exists, Park registers on it — completed rows return the stored
// response with no POST, pending rows wait, failed rows are tombstoned and
// replaced by a fresh queued row. On ctx cancellation the row stays and
// completes later (a resumed child adopts it by custom_id); ctx.Err() is
// returned raw, never wrapped in *Error.
func (b *Batcher) Park(ctx context.Context, customID, model string, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	body, err := requestJSON(params)
	if err != nil {
		return nil, err
	}
	// The adopt-or-insert decision can rarely need a second pass: the only
	// writer that could race it is another Batcher process sharing the
	// store (in-process Parks are serialized by b.mu), and its row may turn
	// out to be a failed one to tombstone first. Eight passes is already
	// far beyond anything reachable.
	for attempt := 0; attempt < 8; attempt++ {
		waitCh, done, err := b.adoptOrInsert(ctx, customID, model, body)
		switch {
		case err != nil:
			return nil, err
		case done != nil:
			return done.msg, done.err
		case waitCh != nil:
			return b.wait(ctx, waitCh)
		}
	}
	return nil, &Error{Msg: fmt.Sprintf("custom_id %q could not be adopted or inserted", customID)}
}

// adoptOrInsert performs Park's decision under b.mu: return a resolved
// outcome (done), a channel to wait on (waitCh), or neither (retry the
// decision — the existing row was failed and needs tombstoning first).
func (b *Batcher) adoptOrInsert(ctx context.Context, customID, model string, body json.RawMessage) (<-chan parkOutcome, *parkOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	row, ok, err := b.store.Live(ctx, customID)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		switch row.State {
		case StateCompleted:
			msg, ferr := decodeMessage(row.Response)
			if ferr != nil {
				return nil, &parkOutcome{err: ferr}, nil
			}
			return nil, &parkOutcome{msg: msg}, nil
		case StateQueued, StateSubmitting, StateSubmitted:
			return b.registerLocked(customID), nil, nil
		case StateFailed:
			// A failed row is tombstoned and replaced by a fresh queued
			// row: a parked call must not return a stale failure that a
			// new submit could still succeed on.
			if err := b.store.Tombstone(ctx, row.ID); err != nil {
				return nil, nil, err
			}
		}
	}
	row, err = b.store.Insert(ctx, Row{
		CustomID:  customID,
		Model:     model,
		State:     StateQueued,
		Request:   body,
		CreatedAt: b.now(),
		UpdatedAt: b.now(),
	})
	if err != nil {
		// Another writer (a second Batcher process sharing this store)
		// inserted the same custom_id first: adopt whatever it wrote.
		live, ok, lerr := b.store.Live(ctx, customID)
		if lerr != nil || !ok {
			return nil, nil, err
		}
		switch live.State {
		case StateCompleted:
			msg, ferr := decodeMessage(live.Response)
			if ferr != nil {
				return nil, &parkOutcome{err: ferr}, nil
			}
			return nil, &parkOutcome{msg: msg}, nil
		case StateFailed:
			return nil, nil, nil // retry: tombstone it on the next pass
		default:
			return b.registerLocked(customID), nil, nil
		}
	}
	return b.registerLocked(customID), nil, nil
}

// wait blocks on the waiter channel and ctx. Exactly one of the two fires:
// delivery removes the waiter before sending, so no outcome is ever lost to
// a cancelled ctx — that waiter simply leaves a buffered value behind.
func (b *Batcher) wait(ctx context.Context, ch <-chan parkOutcome) (*anthropic.Message, error) {
	select {
	case out := <-ch:
		if out.err != nil {
			return nil, out.err
		}
		return out.msg, nil
	case <-ctx.Done():
		// The row stays and completes later; a resumed child adopts it.
		return nil, ctx.Err()
	}
}

// registerLocked adds a waiter channel for customID. Callers hold b.mu. The
// channel is buffered and receives at most one outcome under b.mu.
func (b *Batcher) registerLocked(customID string) <-chan parkOutcome {
	ch := make(chan parkOutcome, 1)
	b.waiters[customID] = append(b.waiters[customID], ch)
	return ch
}

// deliver hands one outcome to every waiter registered for customID and
// removes them. The send never blocks: a waiter that left via ctx leaves its
// buffered value behind.
func (b *Batcher) deliver(customID string, out parkOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chans := b.waiters[customID]
	delete(b.waiters, customID)
	for _, ch := range chans {
		select {
		case ch <- out:
		default:
		}
	}
}

// flushQueued coalesces every queued row into one batch per model: each
// group is marked submitting BEFORE its POST, so a crash between POST and
// MarkSubmitted lands in the recovery sweep instead of a blind resubmission.
func (b *Batcher) flushQueued(ctx context.Context) {
	if b.api == nil {
		return
	}
	rows, err := b.store.InState(ctx, StateQueued)
	if err != nil || len(rows) == 0 {
		return
	}
	groups := make(map[string][]Row)
	for _, r := range rows {
		groups[r.Model] = append(groups[r.Model], r)
	}
	models := make([]string, 0, len(groups))
	for m := range groups {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		group := groups[model]
		ids := make([]int64, len(group))
		for i, r := range group {
			ids[i] = r.ID
		}
		// BEFORE the POST: from here on the outcome is unknown until the
		// POST proves it, and only the recovery sweep may resolve a row
		// left in this state.
		if err := b.store.MarkSubmitting(ctx, ids); err != nil {
			return // store outage; the next window retries
		}
		requests := make([]BatchRequest, len(group))
		for i, r := range group {
			requests[i] = BatchRequest{CustomID: r.CustomID, Body: r.Request}
		}
		batch, err := b.api.Submit(ctx, model, requests)
		if err != nil {
			b.submitFailed(ctx, group, err)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if err := b.store.MarkSubmitted(ctx, ids, batch.ID); err != nil {
			return // store outage; the recovery sweep matches the batch
		}
	}
}

// submitFailed classifies a failed POST by what it PROVES about the batch.
func (b *Batcher) submitFailed(ctx context.Context, group []Row, err error) {
	var he *httpStatusError
	if !errors.As(err, &he) {
		// Transport error with no response: the POST may have created the
		// batch. Leave the rows submitting for the recovery sweep —
		// requeueing here could double-bill.
		return
	}
	switch {
	case he.Status == http.StatusTooManyRequests:
		// 429 proves nothing was created: requeue for the next window.
		_ = b.store.Requeue(ctx, idsOf(group))
	case he.Status >= 400 && he.Status < 500:
		// A validation error (e.g. the 422 for a bad custom_id) fails
		// identically on every retry, so requeueing would loop forever.
		for _, row := range group {
			b.failRow(ctx, row, he.Body)
		}
	default:
		// 5xx and anything else: the POST may have created the batch.
		// Leave the rows submitting for the recovery sweep.
	}
}

// pollSubmitted GETs each distinct provider batch id among submitted rows and
// applies terminal outcomes. A failed GET proves nothing about the batch, so
// its rows simply stay submitted and the next poll retries.
func (b *Batcher) pollSubmitted(ctx context.Context) {
	if b.api == nil {
		return
	}
	rows, err := b.store.InState(ctx, StateSubmitted)
	if err != nil || len(rows) == 0 {
		return
	}
	seen := make(map[string]bool)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ProviderBatchID != "" && !seen[r.ProviderBatchID] {
			seen[r.ProviderBatchID] = true
			ids = append(ids, r.ProviderBatchID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		batch, err := b.api.Batch(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if !batch.Terminal() {
			continue
		}
		b.applyTerminal(ctx, batch)
		if ctx.Err() != nil {
			return
		}
	}
}

// applyTerminal applies one terminal batch to its submitted rows: results
// per call when completed, a batch-level failure fan-out otherwise.
func (b *Batcher) applyTerminal(ctx context.Context, batch Batch) {
	if batch.Status == "completed" {
		b.applyResults(ctx, batch)
		return
	}
	b.failRowsOfBatch(ctx, batch.ID, batchFailureMessage(batch))
}

// batchFailureMessage builds the failure text for a whole failed batch.
func batchFailureMessage(batch Batch) string {
	msg := ""
	if batch.Error != nil {
		msg = batch.Error.Message
	}
	if msg == "" {
		return "batch " + batch.Status
	}
	return "batch " + batch.Status + ": " + msg
}

// applyResults matches results to submitted rows by custom_id — NEVER by
// index, results arrive out of order — completing or failing each row and
// delivering each outcome.
func (b *Batcher) applyResults(ctx context.Context, batch Batch) {
	rows, err := b.store.InState(ctx, StateSubmitted)
	if err != nil {
		return
	}
	byCustom := make(map[string]Row)
	for _, r := range rows {
		if r.ProviderBatchID == batch.ID {
			byCustom[r.CustomID] = r
		}
	}
	for _, res := range batch.Results {
		row, ok := byCustom[res.CustomID]
		if !ok {
			continue // already resolved, tombstoned, or not ours
		}
		if res.Response != nil && res.Response.StatusCode == http.StatusOK && len(res.Response.Body) > 0 {
			msg, ferr := decodeMessage(res.Response.Body)
			if ferr != nil {
				// An undecodable body is stored as the row's failure:
				// completing it would poison every future adoption.
				b.failRow(ctx, row, ferr.Msg)
				continue
			}
			if err := b.store.Complete(ctx, row.ID, res.Response.Body); err != nil {
				return // store outage; the next poll re-applies
			}
			b.deliver(row.CustomID, parkOutcome{msg: msg})
			continue
		}
		b.failRow(ctx, row, resultErrorMessage(res))
	}
}

// resultErrorMessage builds the failure text for one result item that did not
// succeed: the item's error message when present, else its status code.
func resultErrorMessage(res BatchResult) string {
	if res.Error != nil && res.Error.Message != "" {
		return res.Error.Message
	}
	if res.Response != nil && res.Response.StatusCode != http.StatusOK && res.Response.StatusCode != 0 {
		return fmt.Sprintf("status_code %d", res.Response.StatusCode)
	}
	return "no response in batch result"
}

// failRow stores the failure on one row and delivers it to that row's
// waiters. A store error leaves the row in its current state so the next
// poll re-applies the outcome; nothing is delivered that is not durable.
func (b *Batcher) failRow(ctx context.Context, row Row, msg string) {
	if err := b.store.Fail(ctx, row.ID, msg); err != nil {
		return
	}
	b.deliver(row.CustomID, parkOutcome{err: &Error{Msg: msg}})
}

// failRowsOfBatch fails every submitted row of one batch with the batch-level
// message and delivers each outcome.
func (b *Batcher) failRowsOfBatch(ctx context.Context, batchID, msg string) {
	rows, err := b.store.InState(ctx, StateSubmitted)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.ProviderBatchID != batchID {
			continue
		}
		b.failRow(ctx, r, msg)
	}
}

// recoverSubmitting resolves submitting rows — rows whose POST outcome is
// unknown, e.g. after a crash between the POST and MarkSubmitted — without
// ever resubmitting them blind. It lists recent batches newest-first (paging
// stops once created_at passes the oldest submitting row's candidate window)
// and decides per row:
//
//   - a terminal candidate whose results contain the row's custom_id: the
//     POST landed; adopt that batch id and apply its results;
//   - any non-terminal candidate: the batch may still be running; leave the
//     row for the next poll (accept the wait);
//   - all candidates terminal and none contains the custom_id (zero
//     candidates count once Now() passes the grace window): the POST never
//     landed; requeue.
func (b *Batcher) recoverSubmitting(ctx context.Context) {
	if b.api == nil {
		return
	}
	rows, err := b.store.InState(ctx, StateSubmitting)
	if err != nil || len(rows) == 0 {
		return
	}
	now := b.now()
	cutoff := now
	for _, r := range rows {
		if r.UpdatedAt.Before(cutoff) {
			cutoff = r.UpdatedAt
		}
	}
	cutoff = cutoff.Add(-submitMatchBefore)

	recent, ok := b.listRecent(ctx, cutoff)
	if !ok {
		return // the listing failed; decide nothing this sweep
	}
	for _, row := range rows {
		lo := row.UpdatedAt.Add(-submitMatchBefore)
		hi := row.UpdatedAt.Add(submitGraceWindow)
		var matched *Batch
		nonTerminal, anyCandidate := false, false
		for i := range recent {
			bt := &recent[i]
			created := time.Unix(bt.CreatedAt, 0)
			if created.Before(lo) || created.After(hi) {
				continue
			}
			anyCandidate = true
			if !bt.Terminal() {
				nonTerminal = true
				continue
			}
			if matched == nil && containsCustomID(bt, row.CustomID) {
				matched = bt
			}
		}
		switch {
		case matched != nil:
			// Positive proof the POST landed: adopt the batch id and
			// apply its results (which may also resolve sibling rows).
			if err := b.store.MarkSubmitted(ctx, []int64{row.ID}, matched.ID); err != nil {
				return
			}
			b.applyTerminal(ctx, *matched)
		case nonTerminal:
			// Accept the wait; a later poll sees the terminal status.
		case anyCandidate:
			// Every candidate is terminal and none carries the
			// custom_id: the POST never landed.
			_ = b.store.Requeue(ctx, []int64{row.ID})
		case now.After(row.UpdatedAt.Add(submitGraceWindow)):
			// Zero candidates and the grace window has passed: nothing
			// in the listing could be ours.
			_ = b.store.Requeue(ctx, []int64{row.ID})
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// listRecent walks the batch listing newest-first until a page's oldest
// created_at passes cutoff, the listing says has_more=false, or maxListPages
// pages have been read. The second return is false when the listing failed.
func (b *Batcher) listRecent(ctx context.Context, cutoff time.Time) ([]Batch, bool) {
	var recent []Batch
	after := ""
	for page := 0; page < maxListPages; page++ {
		list, err := b.api.List(ctx, after, batchListLimit)
		if err != nil {
			return nil, false
		}
		recent = append(recent, list.Data...)
		if len(list.Data) > 0 {
			oldest := time.Unix(list.Data[len(list.Data)-1].CreatedAt, 0)
			if oldest.Before(cutoff) {
				break
			}
			after = list.Data[len(list.Data)-1].ID
		}
		if !list.HasMore {
			break
		}
	}
	return recent, true
}

// containsCustomID reports whether a batch's results carry the custom_id.
func containsCustomID(batch *Batch, customID string) bool {
	for _, res := range batch.Results {
		if res.CustomID == customID {
			return true
		}
	}
	return false
}

// idsOf collects a group's row ids.
func idsOf(rows []Row) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// requestJSON marshals params to the per-call batch body: the Messages-API
// params JSON with the keys the batch envelope carries elsewhere removed —
// model (the batch-level model names it), stream (batches are
// non-streaming) and provider (a routing hint the batch endpoint does not
// accept). Values stay raw bytes, so nested message content survives the
// round trip byte for byte.
func requestJSON(params anthropic.MessageNewParams) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("batch: marshal request: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("batch: request body is not an object: %w", err)
	}
	delete(m, "model")
	delete(m, "stream")
	delete(m, "provider")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("batch: re-marshal request body: %w", err)
	}
	return out, nil
}

// decodeMessage decodes a Messages-API response body. A failure is a plain
// *Error: an undecodable stored or delivered response is a final outcome for
// the call, never something agentloop should retry.
func decodeMessage(raw json.RawMessage) (*anthropic.Message, *Error) {
	var msg anthropic.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, &Error{Msg: fmt.Sprintf("decode response: %v", err)}
	}
	return &msg, nil
}
