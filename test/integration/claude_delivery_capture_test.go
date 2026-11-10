// SPDX-License-Identifier: Apache-2.0

package integration_test

// The capture half of the delivery acknowledgement, end to end: a REAL daemon
// forwards a REAL child's /v1/messages request to a fake upstream the operator's
// providers.toml names, so the whole capture path runs — begin, decompose,
// store, report, gate release, publish — and the test can hold it open.
//
// This is the file the proxy face's `base_url` support exists for. Before it,
// `[providers.anthropic] base_url` moved every sender and not the Messages
// face, so a harness could not point the face at a local server and the claim
// this file checks — that the acknowledgement arrives BEFORE the capture, and
// that an older capture publishes without the newly acknowledged message — had
// no live proof at all.
//
// What is proven here, in order:
//
//  1. the receipt for message 1 arrives while message 1's own request is
//     blocked at the upstream, i.e. before any capture exists;
//  2. a second message sent MID-TURN is acknowledged immediately, still while
//     that request is blocked;
//  3. releasing request 1 publishes message 1's row and NOT message 2's — an
//     older in-flight capture cannot contain a message acknowledged after it
//     was built;
//  4. the next request carries message 2 WRAPPED in Claude Code's mid-turn
//     prose, and its capture publishes that wrapped row — which is exactly why
//     the acknowledgement names a row id instead of matching text.
//
// The two link tests below take the same harness one step further: the row the
// receipt named is applied to the captured USER MESSAGE, so a client can tell
// which published row its own send produced. History is the other half of that
// contract — the stored row is drawn identically with or without the link, so
// every history read carries an EMPTY message_id.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// ─── the fake upstream ───────────────────────────────────────────────────────

// captureUpstream is an Anthropic Messages endpoint the daemon forwards to
// because providers.toml declares it as [providers.anthropic] base_url. Every
// request parks until its gate is closed, so a test controls exactly when a
// capture can complete.
type captureUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies [][]byte
	gates  map[int]chan struct{}
	// arrived receives the index of each request as it reaches the handler, so
	// a test waits on the REQUEST, never on a clock.
	arrived chan int
}

func newCaptureUpstream(t *testing.T) *captureUpstream {
	t.Helper()
	u := &captureUpstream{gates: map[int]chan struct{}{}, arrived: make(chan int, 16)}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

// gate returns the channel that holds request n until it is closed.
func (u *captureUpstream) gate(n int) chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.gates[n] == nil {
		u.gates[n] = make(chan struct{})
	}
	return u.gates[n]
}

// body returns the request body the upstream received for request n.
func (u *captureUpstream) body(n int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n < 0 || n >= len(u.bodies) {
		return ""
	}
	return string(u.bodies[n])
}

// expectRequest blocks until the upstream receives request n, and fails with
// the configured base_url named when the daemon never forwards one.
func (u *captureUpstream) expectRequest(t *testing.T, n int) {
	t.Helper()
	select {
	case got := <-u.arrived:
		assert.NewAborting(t).Eq(n, got, "the request the upstream sees")
	case <-time.After(60 * time.Second):
		t.Fatalf("the daemon never forwarded the child's request %d to the configured base_url; providers.toml pointed at %s", n, u.srv.URL)
	}
}

func (u *captureUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	raw := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}

	u.mu.Lock()
	n := len(u.bodies)
	u.bodies = append(u.bodies, raw)
	gate := u.gates[n]
	u.mu.Unlock()
	u.arrived <- n
	if gate != nil {
		<-gate
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, ev := range []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_capture_` + fmt.Sprint(n) + `","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	} {
		_, _ = w.Write([]byte(ev))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// writeAnthropicBaseProviders writes the providers.toml that points the
// anthropic provider — and with it the proxy face — at baseURL.
func writeAnthropicBaseProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
base_url = %q
api_key_env = "ANTHROPIC_API_KEY"
`, baseURL)
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write providers fixture")
	return path
}

// ─── event helpers ───────────────────────────────────────────────────────────

// userMessageMatching matches a durable UserMessage event for childID whose
// text satisfies predicate.
func userMessageMatching(childID string, predicate func(string) bool) func(*rafikiv1.Event) bool {
	return func(ev *rafikiv1.Event) bool {
		return ev.GetChildId() == childID && ev.GetUserMessage() != nil && predicate(eventText(ev))
	}
}

// childUserTexts returns every user-message text the stream has buffered for
// the child, live or replayed — a diagnosis and an assertion source.
func childUserTexts(stream *eventStream, childID string) []string {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	var out []string
	for _, ev := range stream.events {
		if ev.GetChildId() == childID && ev.GetUserMessage() != nil {
			out = append(out, eventText(ev))
		}
	}
	return out
}

// pendingCaptureTurn returns the id of the child's ONE pending capture turn.
// "pending" is the state InsertTurnIntent writes BEFORE the upstream is called,
// so a pending turn PROVES the proxy is mid-capture — not merely holding a
// request the child has not sent yet.
func pendingCaptureTurn(t *testing.T, childID string) string {
	t.Helper()
	c := assert.NewAborting(t)
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	var ids []string
	c.NoError(pool.QueryRow(context.Background(), `
		SELECT coalesce(array_agg(t.id::text), '{}')
		  FROM conversations.conversation_turn t
		  JOIN conversations.conversation c ON c.id = t.conversation_id
		 WHERE c.external_ref = $1 AND t.status::text = 'pending'`, childID).Scan(&ids),
		"read the pending capture turn for %s", childID)
	c.EqDeep(1, len(ids), "pending capture turns for the child")
	return ids[0]
}

// captureTurnStatus reports one capture turn's status.
func captureTurnStatus(t *testing.T, turnID string) string {
	t.Helper()
	c := assert.NewAborting(t)
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	var status string
	c.NoError(pool.QueryRow(context.Background(),
		`SELECT status::text FROM conversations.conversation_turn WHERE id = $1::uuid`, turnID).Scan(&status),
		"read capture turn %s", turnID)
	return status
}

// ─── the test ────────────────────────────────────────────────────────────────

// TestClaudeDeliveryCapturePublishesOnlyAfterTheReceipt is the whole ordering
// story in one session: two messages, one capture held open across both.
func TestClaudeDeliveryCapturePublishesOnlyAfterTheReceipt(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	up := newCaptureUpstream(t)
	firstGate := up.gate(0)
	secondGate := up.gate(1)

	d, _ := claudeDeliveryDaemon(t,
		"RAFIKI_PROVIDERS="+writeAnthropicBaseProviders(t, up.srv.URL),
		"FAKE_CLAUDE_TURNS=1",
		// A bare Anthropic id, so the request stays on the native-Anthropic route
		// the configured base_url governs.
		"FAKE_CLAUDE_REQUEST_MODEL=claude-haiku-4-5",
	)
	client := d.control(t)
	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	// ── 1. message one, acknowledged before its capture can happen ───────────
	const first = "the first message"
	firstRow := sendPromptRow(t, client, childID, first)
	firstReceipt := stream.waitEvent(t, deliveredEvent(childID, firstRow), 60*time.Second)
	c.Eq(firstRow, firstReceipt.GetMessageDelivered().GetMessageId(), "the receipt names the row Send returned")

	select {
	case n := <-up.arrived:
		c.Eq(0, n, "the first request the upstream sees")
	case <-time.After(60 * time.Second):
		t.Fatalf("the daemon never forwarded the child's request to the configured base_url; providers.toml pointed at %s", up.srv.URL)
	}
	// The capture is genuinely IN FLIGHT at the proxy — not a request the child
	// merely has not sent yet: beginCapture has already resolved the child's
	// conversation and written the turn intent, and the proxy is parked on the
	// upstream with nothing stored and nothing reported.
	firstCaptureTurn := pendingCaptureTurn(t, childID)
	c.EqDeep(0, len(childUserTexts(stream, childID)), "a transcript row was published while the capture was still blocked")

	// ── 2. message two, sent MID-TURN, acknowledged with the capture blocked ─
	const second = "a message sent mid-turn"
	secondRow := sendPromptRow(t, client, childID, second)
	secondReceipt := stream.waitEvent(t, deliveredEvent(childID, secondRow), 60*time.Second)
	c.Eq(secondRow, secondReceipt.GetMessageDelivered().GetMessageId(), "the mid-turn receipt names its own row")
	c.NotEq(firstRow, secondRow, "the two sends are distinct inbox rows")
	c.EqDeep(0, len(childUserTexts(stream, childID)), "a transcript row was published while the capture was still blocked")

	// ── 3. release the FIRST capture: it publishes message one, not two ─────
	close(firstGate)
	firstRowEvent := stream.waitEvent(t, userMessageMatching(childID, func(s string) bool {
		return strings.Contains(s, first)
	}), 60*time.Second)

	// Less(want, got) asserts got < want: the receipt precedes the row it
	// announced, and that row is the capture's.
	c.Less(firstRowEvent.GetOrdinal(), firstReceipt.GetOrdinal(),
		"the receipt (ordinal %d) must precede the captured row (ordinal %d)",
		firstReceipt.GetOrdinal(), firstRowEvent.GetOrdinal())

	// Pin the timeline in the LOG rather than inferring it from the order the
	// test happened to do things: message two was acknowledged while this
	// capture was still blocked, so its receipt is a LOWER ordinal than the row
	// that capture has just published.
	c.Require().NotNil(firstRowEvent.Ordinal, "the captured row carries no ordinal")
	c.Require().NotNil(secondReceipt.Ordinal, "the mid-turn receipt carries no ordinal")
	c.Less(firstRowEvent.GetOrdinal(), secondReceipt.GetOrdinal(),
		"the mid-turn receipt (ordinal %d) must precede the first capture's row (ordinal %d): it was acknowledged while that capture was blocked",
		secondReceipt.GetOrdinal(), firstRowEvent.GetOrdinal())

	// The capture that was in flight has now COMPLETED — the same turn, from
	// pending to complete — which is what "released" means on this path.
	c.Eq("complete", captureTurnStatus(t, firstCaptureTurn),
		"the blocked capture's turn must complete once the upstream answers")

	// The older in-flight capture was built before message two existed, so its
	// request cannot carry it — and its capture must not publish it.
	c.NotStrContains(up.body(0), second, "the first request carries the mid-turn message")
	for _, text := range childUserTexts(stream, childID) {
		c.NotStrContains(text, second, "a capture published a message it never carried: %q", text)
	}

	// ── 4. the NEXT capture carries message two, WRAPPED ────────────────────
	select {
	case n := <-up.arrived:
		c.Eq(1, n, "the second request the upstream sees")
	case <-time.After(90 * time.Second):
		t.Fatalf("the child never sent a second request; buffered user rows: %q", childUserTexts(stream, childID))
	}
	secondBody := up.body(1)
	c.StrContains(secondBody, "while you were working", "the mid-turn request's wrapper")
	c.StrContains(secondBody, second, "the wrapper keeps the user's original words")

	close(secondGate)
	wrapped := stream.waitEvent(t, userMessageMatching(childID, func(s string) bool {
		return strings.Contains(s, "while you were working")
	}), 90*time.Second)
	c.StrContains(eventText(wrapped), second, "the published row keeps the original text inside the wrapper")
	c.NotEq(second, strings.TrimSpace(eventText(wrapped)),
		"the published row is the user's text verbatim; the wrapper is the reason a text match fails")

	// The receipt for message two still preceded the row that carries it.
	c.Less(wrapped.GetOrdinal(), secondReceipt.GetOrdinal(),
		"the mid-turn receipt (ordinal %d) must precede its captured row (ordinal %d)",
		secondReceipt.GetOrdinal(), wrapped.GetOrdinal())
}

// ─── the link ────────────────────────────────────────────────────────────────

// TestClaudeDeliveryCaptureCarriesTheMessageID pins both halves of the link: the
// captured user message a send produced names the inbox row Send returned, and
// the same message read back from history names nothing — the link describes the
// live publish, never the stored row.
func TestClaudeDeliveryCaptureCarriesTheMessageID(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	up := newCaptureUpstream(t)
	firstGate := up.gate(0)

	d, _ := claudeDeliveryDaemon(t,
		"RAFIKI_PROVIDERS="+writeAnthropicBaseProviders(t, up.srv.URL),
		"FAKE_CLAUDE_TURNS=1",
		"FAKE_CLAUDE_REQUEST_MODEL=claude-haiku-4-5",
	)
	client := d.control(t)
	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	const prompt = "name the inbox row this prompt delivered"
	rowID := sendPromptRow(t, client, childID, prompt)
	receipt := stream.waitEvent(t, deliveredEvent(childID, rowID), 60*time.Second)
	c.Eq(rowID, receipt.GetMessageDelivered().GetMessageId(), "the receipt names the row Send returned")

	up.expectRequest(t, 0)
	c.EqDeep(0, len(childUserTexts(stream, childID)), "a transcript row was published while the capture was still blocked")

	close(firstGate)
	published := stream.waitEvent(t, userMessageMatching(childID, func(s string) bool {
		return strings.TrimSpace(s) == prompt
	}), 60*time.Second)
	c.Eq(rowID, published.GetUserMessage().GetMessageId(),
		"the captured user message must name the inbox row it delivered")

	hist, err := client.GetHistory(context.Background(), connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
	c.NoError(err, "GetHistory")
	var stored int
	for _, ev := range hist.Msg.GetEvents() {
		if ev.GetUserMessage() == nil || !strings.Contains(eventText(ev), prompt) {
			continue
		}
		stored++
		c.Eq("", ev.GetUserMessage().GetMessageId(),
			"history carries the message link; every client draws the stored row, so the link stays on the live publish")
	}
	c.Eq(1, stored, "user messages in history carrying the prompt")
}

// TestClaudeDeliveryCaptureTwoIdenticalSendsLinkInOrder sends the SAME words
// twice in two turns: text cannot tell the two rows apart, so the ids must, and
// position decides which send each captured message names.
func TestClaudeDeliveryCaptureTwoIdenticalSendsLinkInOrder(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)

	up := newCaptureUpstream(t)
	firstGate := up.gate(0)
	secondGate := up.gate(1)

	d, _ := claudeDeliveryDaemon(t,
		"RAFIKI_PROVIDERS="+writeAnthropicBaseProviders(t, up.srv.URL),
		"FAKE_CLAUDE_TURNS=1",
		"FAKE_CLAUDE_REQUEST_MODEL=claude-haiku-4-5",
	)
	client := d.control(t)
	childID := spawnClaudeChild(t, d)
	stream := d.openEvents(t, childEventsSubject(childID), nil, eventWatermark(t, client, childID))

	const prompt = "the same words twice"

	firstRow := sendPromptRow(t, client, childID, prompt)
	stream.waitEvent(t, deliveredEvent(childID, firstRow), 60*time.Second)
	up.expectRequest(t, 0)
	close(firstGate)
	first := stream.waitEvent(t, userMessageMatching(childID, func(s string) bool {
		return strings.Contains(s, prompt)
	}), 60*time.Second)
	c.Eq(firstRow, first.GetUserMessage().GetMessageId(), "the first captured message names the first send")

	// BARRIER: the child must be idle again before the second send, or the
	// fixture wraps it as a mid-turn message and the second turn is not a turn.
	stream.waitEvent(t, agentStatusEvent(childID, "idle"), 60*time.Second)

	secondRow := sendPromptRow(t, client, childID, prompt)
	stream.waitEvent(t, deliveredEvent(childID, secondRow), 60*time.Second)
	up.expectRequest(t, 1)
	close(secondGate)
	second := stream.waitEvent(t, func(ev *rafikiv1.Event) bool {
		return ev.GetChildId() == childID && ev.GetUserMessage() != nil &&
			ev.GetUserMessage().GetMessageId() == secondRow
	}, 60*time.Second)
	c.StrContains(eventText(second), prompt, "the second captured row keeps the user's words")
	c.NotEq(firstRow, secondRow, "the two sends are distinct inbox rows")

	var ids []string
	stream.mu.Lock()
	for _, ev := range stream.events {
		if ev.GetChildId() != childID || ev.GetUserMessage() == nil || !stream.isLive(ev) {
			continue
		}
		ids = append(ids, ev.GetUserMessage().GetMessageId())
	}
	stream.mu.Unlock()
	c.EqDeep([]string{firstRow, secondRow}, ids, "captured user messages name the two sends in send order")
}
