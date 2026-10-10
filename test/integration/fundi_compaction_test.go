// SPDX-License-Identifier: Apache-2.0

package integration_test

// Fundi context compaction, end to end on a real daemon engine through a fake
// Anthropic seat: the proactive threshold trigger, the verbatim tail, the
// restart that resumes from the working set, and the reactive overflow net.
//
// The seat speaks SSE (a plain-JSON 200 parses as an empty message with no
// error — see restart_resume_test.go). The daemon's model catalog is the real
// on-disk snapshot (routing.FileSnapshotStore at paths.CacheDir()/
// "openrouter_catalog.json"), so the test points the daemon's XDG_CACHE_HOME at
// a scratch dir and pre-writes a fresh snapshot carrying the child's model with
// a controlled context window. That is the daemon's documented cache, not a
// bypass: compactionContextWindow reads exactly this catalog, and the test
// asserts the compaction actually happened.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// compactionModel is the child's model id. The catalog indexes OpenRouter-native
// ids, so the snapshot's entry uses the same id and the provider-local id the
// engine passes to compactionContextWindow ("mini") resolves to it through the
// catalog's native-Anthropic mapping — exactly the production path.
const compactionModel = "anthropic/mini"

// compactionWindow and compactionUsageInput are chosen together so the proactive
// trigger fires on the intended turn and NOT again after compaction: the
// reported usage leaves less than the 40k headroom buffer (200_000 - 199_000),
// while the post-compaction working set's byte estimate is far below the window,
// so a resumed or continued conversation does not re-compact.
const (
	compactionWindow     = 200_000
	compactionUsageInput = 199_000
)

// compactionPromptFirstLine is the first line of llm's compaction prompt
// (pkg/llm/compact_prompt.go). The seat recognises a compaction request by a
// text block starting with it.
const compactionPromptFirstLine = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."

// ─── fake Anthropic seat ─────────────────────────────────────────────────────

// seatMessage is one request message's text blocks.
type seatMessage struct {
	role  string
	texts []string
}

// seatRequest is one recorded request: enough to tell a compaction request from
// a real one and to read the opening message.
type seatRequest struct {
	model        string
	isCompaction bool
	msgs         []seatMessage
	errored      bool
}

func (r *seatRequest) firstText() string {
	if len(r.msgs) == 0 {
		return ""
	}
	return strings.Join(r.msgs[0].texts, "\n")
}

// compactionSeat answers /v1/messages with SSE: a normal end_turn reply for
// ordinary turns, and a summary for a compaction request (a text block starting
// with the compaction prompt's first line). Options:
//
//   - blockOnSummary: a real request whose first message carries the summary is
//     held until release is closed, so the child stays mid-turn (the restart
//     test needs a live turn to auto-resume).
//   - overflowAtRows > 0: the first real request whose message count reaches it
//     is answered HTTP 400 "prompt is too long" ONCE.
type compactionSeat struct {
	srv            *httptest.Server
	summaryText    string
	blockOnSummary bool
	overflowAtRows int
	// realUsageInput is the input_tokens an ordinary reply reports. It drives
	// the proactive trigger: with compactionUsageInput the next check sits
	// under the headroom buffer; a small value never triggers it (the overflow
	// test's setup).
	realUsageInput int
	release        chan struct{}

	mu         sync.Mutex
	reqs       []*seatRequest
	overflowed bool
}

func newCompactionSeat(t *testing.T, marker string) *compactionSeat {
	t.Helper()
	s := &compactionSeat{
		summaryText:    "SUMMARY-" + marker,
		realUsageInput: compactionUsageInput,
		release:        make(chan struct{}),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *compactionSeat) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req := s.parse(body)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	overflow := s.overflowAtRows > 0 && !s.overflowed && !req.isCompaction && len(req.msgs) >= s.overflowAtRows
	if overflow {
		s.overflowed = true
		req.errored = true
	}
	s.mu.Unlock()

	if req.isCompaction {
		// The summary call is NON-streaming: llm.compact clears the stream
		// handler, so this response must be a plain JSON Message, not SSE.
		s.replyJSON(w, "<analysis>a</analysis><summary>"+s.summaryText+"</summary>", compactionUsageInput)
		return
	}
	if overflow {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 999999 tokens > 200000 maximum"}}`)
		return
	}
	if s.blockOnSummary && strings.Contains(req.firstText(), s.summaryText) {
		select {
		case <-s.release:
		case <-time.After(120 * time.Second):
		}
	}
	s.replySSE(w, "ok", s.realUsageInput)
}

func (s *compactionSeat) parse(body []byte) *seatRequest {
	var wire struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &wire)
	req := &seatRequest{model: wire.Model}
	for _, m := range wire.Messages {
		sm := seatMessage{role: m.Role}
		for _, c := range m.Content {
			if c.Type != "text" {
				continue
			}
			sm.texts = append(sm.texts, c.Text)
			if strings.HasPrefix(c.Text, compactionPromptFirstLine) {
				req.isCompaction = true
			}
		}
		req.msgs = append(req.msgs, sm)
	}
	return req
}

// replyJSON writes one complete NON-streaming Messages response (the shape a
// compaction summary call expects).
func (s *compactionSeat) replyJSON(w http.ResponseWriter, text string, inputTokens int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body, _ := json.Marshal(map[string]any{
		"id":          "msg_c",
		"type":        "message",
		"role":        "assistant",
		"model":       "mini",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": inputTokens, "output_tokens": 5},
	})
	_, _ = w.Write(body)
}

// replySSE writes one complete streaming Messages response.
func (s *compactionSeat) replySSE(w http.ResponseWriter, text string, inputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	delta, _ := json.Marshal(text)
	events := []string{
		"event: message_start\n" + fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":%d,"output_tokens":1}}}`, inputTokens) + "\n\n",
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\n" + fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, delta) + "\n\n",
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n",
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	for _, ev := range events {
		_, _ = io.WriteString(w, ev)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (s *compactionSeat) snapshot() []*seatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*seatRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func (s *compactionSeat) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *compactionSeat) compactionCount() int {
	n := 0
	for _, r := range s.snapshot() {
		if r.isCompaction {
			n++
		}
	}
	return n
}

func (s *compactionSeat) dump() string {
	var b strings.Builder
	for i, r := range s.snapshot() {
		fmt.Fprintf(&b, "  [%d] compaction=%v errored=%v model=%s msgs=%d first=%q\n",
			i, r.isCompaction, r.errored, r.model, len(r.msgs), truncateForLog(r.firstText(), 120))
	}
	return b.String()
}

// ─── daemon fixtures ─────────────────────────────────────────────────────────

// writeCompactionCatalog writes the daemon's on-disk model catalog snapshot at
// <cacheHome>/rafiki/openrouter_catalog.json with one fresh entry, so the
// proactive trigger sees modelID's window without any network fetch.
func writeCompactionCatalog(t *testing.T, cacheHome, modelID string, window int) {
	t.Helper()
	dir := filepath.Join(cacheHome, "rafiki")
	assert.NewAborting(t).NoError(os.MkdirAll(dir, 0o755), "mkdir cache dir")
	snap := map[string]any{
		"fetched": time.Now().UTC().Format(time.RFC3339Nano),
		"models": []any{map[string]any{
			"id":                   modelID,
			"context_length":       window,
			"supported_parameters": []string{"tools"},
			"architecture":         map[string]any{"input_modalities": []string{"text"}},
		}},
	}
	b, err := json.Marshal(snap)
	assert.NewAborting(t).NoError(err, "marshal catalog snapshot")
	assert.NewAborting(t).NoError(os.WriteFile(filepath.Join(dir, "openrouter_catalog.json"), b, 0o600), "write catalog snapshot")
}

// writeCompactionProviders points the "anthropic" provider at the fake seat.
// It is keyless (no api_key_env), so the daemon sends without a credential.
func writeCompactionProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
base_url = %q
`, baseURL)
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write providers fixture")
	return path
}

// compactionEnv is the per-daemon environment a compaction test boots with: the
// fake provider, the scratch catalog cache, and the event-buffer debounce the
// restart tests use.
func compactionEnv(providers, cacheHome string) []string {
	return append(noRealProviderEnv(),
		"RAFIKI_PROVIDERS="+providers,
		"XDG_CACHE_HOME="+cacheHome,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)
}

// spawnCompactionChild spawns a fundi child on the fake model. prefill entries
// (with cwd as the workspace) require a live executor.
func spawnCompactionChild(t *testing.T, d *daemon, cwd string, prefill []*rafikiv1.PrefillRead) string {
	t.Helper()
	ck := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:            cwd,
		Kind:           protocol.KindFundi,
		Model:          compactionModel,
		RecordRequests: true,
		Prefill:        prefill,
	}))
	ck.NoError(err, "spawn failed")
	ck.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

// promptCompactionChild sends one prompt frame.
func promptCompactionChild(t *testing.T, d *daemon, childID, id, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := d.control(t).SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: fmt.Sprintf(`{"type":"prompt","id":%q,"message":%q}`, id, text),
	}))
	assert.NewAborting(t).NoError(err, "SendFrame prompt %s", id)
}

// waitSeatRequests blocks until the seat has recorded at least n requests.
func waitSeatRequests(t *testing.T, seat *compactionSeat, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if seat.requestCount() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("seat never saw %d request(s); saw %d:\n%s", n, seat.requestCount(), seat.dump())
}

// compactionStream watches a child's durable events so a turn's completion is
// observed as the durable agent_status=idle event rather than by polling the
// status, which can still read the PREVIOUS turn's idle.
type compactionStream struct {
	es  *eventStream
	idx int
}

func openCompactionStream(t *testing.T, d *daemon, childID string) *compactionStream {
	t.Helper()
	client := d.control(t)
	var watermark int32
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c := getChild(t, client, childID); c.LatestOrdinal != nil {
			watermark = *c.LatestOrdinal
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	es := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: watermark - 1}, watermark)
	return &compactionStream{es: es}
}

func (cs *compactionStream) waitIdle(t *testing.T, childID string) {
	t.Helper()
	_, cs.idx = cs.es.waitEventAfter(t, cs.idx, agentStatusEvent(childID, "idle"), 30*time.Second)
}

func (cs *compactionStream) waitBoundary(t *testing.T) {
	t.Helper()
	cs.es.waitEvent(t, func(ev *rafikiv1.Event) bool { return ev.GetCompactionBoundary() != nil }, 10*time.Second)
}

// hasError reports whether any live event buffered so far is a durable error —
// the native witness of a failed turn (the framed agent_error has no durable
// form in GetHistory, which renders messages only).
func (cs *compactionStream) hasError() bool {
	cs.es.mu.Lock()
	defer cs.es.mu.Unlock()
	for _, ev := range cs.es.events {
		if ev.GetError() != nil {
			return true
		}
	}
	return false
}

// driveUntilCompaction prompts the child one turn at a time until the seat sees
// a compaction request, then waits for the compaction turn's real request to
// reach the seat. blockOnSummary seats hold that request, so the caller sees the
// child streaming; an unblocked seat answers it, so the caller sees it idle.
func driveUntilCompaction(t *testing.T, d *daemon, seat *compactionSeat, cs *compactionStream, childID, marker string) {
	t.Helper()
	for i := 1; i <= 5; i++ {
		before := seat.requestCount()
		promptCompactionChild(t, d, childID, fmt.Sprintf("u%d", i), fmt.Sprintf("prompt-%d-%s", i, marker))
		waitSeatRequests(t, seat, before+1, 30*time.Second)
		if seat.compactionCount() >= 1 {
			// The compaction request has been answered; the real request that
			// follows it is either in flight (blocked) or answered.
			waitSeatRequests(t, seat, before+2, 30*time.Second)
			return
		}
		cs.waitIdle(t, childID)
	}
	t.Fatalf("no compaction request after 5 prompts:\n%s", seat.dump())
}

// ─── DB helpers ──────────────────────────────────────────────────────────────

// compactionHorizon returns the conversation's resume_from_ordinal (0 when
// NULL) and the kind of the row at it ("" when NULL).
func compactionHorizon(t *testing.T, dsn, conversationID string) (int, string) {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	ctx := context.Background()
	var horizon *int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT resume_from_ordinal FROM conversations.conversation WHERE id = $1::uuid`,
		conversationID).Scan(&horizon), "read horizon")
	if horizon == nil {
		return 0, ""
	}
	var kind *string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT kind FROM conversations.conversation_message WHERE conversation_id = $1::uuid AND ordinal = $2`,
		conversationID, *horizon).Scan(&kind), "read horizon row kind")
	if kind == nil {
		return *horizon, ""
	}
	return *horizon, *kind
}

// getHistoryEvents serves the durable tier for a child (the same read rafiki
// logs uses: full, unfiltered history converted to native events).
func getHistoryEvents(t *testing.T, client rafikiv1connect.ControlClient, childID string) []*rafikiv1.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.GetHistory(ctx, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
	assert.NewAborting(t).NoError(err, "GetHistory")
	return resp.Msg.GetEvents()
}

// eventText returns the text of a message event's text blocks.
func eventText(ev *rafikiv1.Event) string {
	var sb strings.Builder
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_UserMessage:
		for _, b := range p.UserMessage.GetContent() {
			if tb := b.GetText(); tb != nil {
				sb.WriteString(tb.GetText())
			}
		}
	case *rafikiv1.Event_AssistantMessage:
		for _, b := range p.AssistantMessage.GetContent() {
			if tb := b.GetText(); tb != nil {
				sb.WriteString(tb.GetText())
			}
		}
	}
	return sb.String()
}

func countHistoryText(events []*rafikiv1.Event, want string) int {
	n := 0
	for _, ev := range events {
		if strings.Contains(eventText(ev), want) {
			n++
		}
	}
	return n
}

// ─── tests ───────────────────────────────────────────────────────────────────

// TestCompactionFundiChildCompactsAndKeepsAnswering drives a fundi child past
// the model's window and pins the whole proactive path: exactly one compaction
// request, a post-compaction request opening with the summary, the stored
// horizon pointing at a compaction_summary row, the full pre-compaction history
// still readable, and a live CompactionBoundary event.
func TestCompactionFundiChildCompactsAndKeepsAnswering(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	marker := uniqueSuffix()
	seat := newCompactionSeat(t, marker)
	cacheHome := t.TempDir()
	writeCompactionCatalog(t, cacheHome, compactionModel, compactionWindow)
	providers := writeCompactionProviders(t, seat.srv.URL)

	d := bootDaemonDB(t, nextDaemonID(), compactionEnv(providers, cacheHome)...)
	childID := spawnCompactionChild(t, d, t.TempDir(), nil)
	waitForStatus(t, d, childID, "idle", 30*time.Second)

	cs := openCompactionStream(t, d, childID)
	driveUntilCompaction(t, d, seat, cs, childID, marker)
	cs.waitIdle(t, childID)
	convID := resumeConversationID(t, dsn, childID)
	horizon, kind := compactionHorizon(t, dsn, convID)

	reqs := seat.snapshot()
	var compactionIdx = -1
	for i, r := range reqs {
		if r.isCompaction {
			compactionIdx = i
		}
	}
	ck.Eq(1, seat.compactionCount(), "compaction-prompt requests:\n%s", seat.dump())
	ck.True(compactionIdx >= 0, "no compaction request:\n%s", seat.dump())
	ck.True(compactionIdx+1 < len(reqs), "no real request after the compaction request:\n%s", seat.dump())
	next := reqs[compactionIdx+1]
	ck.False(next.isCompaction, "the request after compaction must be a real one")
	ck.StrContains(next.firstText(), "SUMMARY-"+marker,
		"the post-compaction request must open with the summary:\n%s", seat.dump())

	ck.NotEq(0, horizon, "resume_from_ordinal must move off 0")
	ck.Eq("compaction_summary", kind, "the row at the horizon")
	events := getHistoryEvents(t, d.control(t), childID)
	for i := 1; i <= 3; i++ {
		ck.Eq(1, countHistoryText(events, fmt.Sprintf("prompt-%d-%s", i, marker)),
			"pre-compaction prompt %d must still appear in the full history", i)
	}

	cs.waitBoundary(t)
}

// TestCompactionFundiDisplayShowsTailOnce pins that a full-history reader shows
// the verbatim tail copy exactly once: eventconv skips compaction_tail rows, so
// a tail marker survives only through its original row.
func TestCompactionFundiDisplayShowsTailOnce(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	marker := uniqueSuffix()
	seat := newCompactionSeat(t, marker)
	cacheHome := t.TempDir()
	writeCompactionCatalog(t, cacheHome, compactionModel, compactionWindow)
	providers := writeCompactionProviders(t, seat.srv.URL)

	d := bootDaemonDB(t, nextDaemonID(), compactionEnv(providers, cacheHome)...)
	childID := spawnCompactionChild(t, d, t.TempDir(), nil)
	waitForStatus(t, d, childID, "idle", 30*time.Second)

	cs := openCompactionStream(t, d, childID)
	driveUntilCompaction(t, d, seat, cs, childID, marker)
	cs.waitIdle(t, childID)

	convID := resumeConversationID(t, dsn, childID)
	horizon, kind := compactionHorizon(t, dsn, convID)
	ck.Eq("compaction_summary", kind, "the row at the horizon")

	// The tail is non-empty: the summary replaced only the oldest row, so the
	// second prompt's marker is carried by BOTH an original row and a copy.
	tailMarker := "prompt-2-" + marker
	events := getHistoryEvents(t, d.control(t), childID)
	ck.Eq(1, countHistoryText(events, tailMarker),
		"the tail's marker text must appear exactly once in the full history (tail copies are skipped)")
	ck.Eq(1, countHistoryText(events, "prompt-1-"+marker),
		"the replaced first prompt must still appear exactly once (its original row)")

	// Prove the tail really is non-empty: the row after the summary is a
	// compaction_tail copy.
	tailKind := rowKindAt(t, dsn, convID, horizon+1)
	ck.Eq("compaction_tail", tailKind, "the row after the summary must be a tail copy")
}

// TestCompactionFundiRestartResumesFromWorkingSet compacts, restarts the daemon
// with the child mid-turn, and pins that the successor's auto-resume re-issues
// the turn on the WORKING set: the first post-restart request opens with the
// summary, not the original first prompt, and no second compaction runs. The
// prefilled subtest covers a child whose history starts with a pre-fill, which
// must resume rather than be skipped by the pre-fill classifier.
func TestCompactionFundiRestartResumesFromWorkingSet(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_DSN") == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	t.Run("plain", func(t *testing.T) { testCompactionRestartResumes(t, false) })
	t.Run("prefilled", func(t *testing.T) { testCompactionRestartResumes(t, true) })
}

func testCompactionRestartResumes(t *testing.T, prefill bool) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")

	marker := uniqueSuffix()
	seat := newCompactionSeat(t, marker)
	seat.blockOnSummary = true
	cacheHome := t.TempDir()
	writeCompactionCatalog(t, cacheHome, compactionModel, compactionWindow)
	providers := writeCompactionProviders(t, seat.srv.URL)

	cwd := t.TempDir()
	env := compactionEnv(providers, cacheHome)
	daemonID := nextDaemonID()
	var childPrefill []*rafikiv1.PrefillRead
	if prefill {
		env = append(env, "RAFIKI_EXECUTORS_ENABLED=1")
		seed := filepath.Join(cwd, "seed.md")
		ck.NoError(os.WriteFile(seed, []byte("prefill seed "+marker+"\n"), 0o600), "write seed file")
		childPrefill = []*rafikiv1.PrefillRead{{Path: "seed.md"}}
	}

	d1 := bootDaemonDB(t, daemonID, env...)
	if prefill {
		startLocalExecutor(t, d1, dsn, cwd)
	}
	childID := spawnCompactionChild(t, d1, cwd, childPrefill)
	waitForStatus(t, d1, childID, "idle", 30*time.Second)
	if prefill {
		// Prove the pre-fill actually ran: the seed file's contents must be in
		// the conversation before the first turn. Without this the subtest
		// could pass while silently exercising nothing.
		ck.True(conversationHasText(t, dsn, resumeConversationID(t, dsn, childID), "prefill seed "+marker),
			"the pre-fill must have read the seed file into the conversation")
	}

	cs := openCompactionStream(t, d1, childID)
	driveUntilCompaction(t, d1, seat, cs, childID, marker)
	waitForStatus(t, d1, childID, string(protocol.StatusStreaming), 30*time.Second)
	ck.Eq(1, seat.compactionCount(), "one compaction before the restart:\n%s", seat.dump())

	// SIGKILL mid-turn, then boot the successor against the same tree.
	beforeRestart := seat.requestCount()
	_ = d1.proc.Process.Signal(syscall.SIGKILL)
	_ = d1.proc.Wait()
	d2 := restartResumeDaemon(t, d1, daemonID, providers, env...)

	// The auto-resume re-issues the interrupted turn on the working set; the
	// seat blocks it (its first message is the summary), so it is observable.
	waitSeatRequests(t, seat, beforeRestart+1, 30*time.Second)

	reqs := seat.snapshot()
	resumed := reqs[len(reqs)-1]
	ck.False(resumed.isCompaction, "the resumed request must be a real one:\n%s", seat.dump())
	ck.StrContains(resumed.firstText(), "SUMMARY-"+marker,
		"the resumed request must open with the summary, not the original first prompt:\n%s", seat.dump())
	ck.False(strings.Contains(resumed.firstText(), "prompt-1-"+marker),
		"the resumed request must not carry the replaced first prompt:\n%s", seat.dump())
	ck.Eq(1, seat.compactionCount(), "no second compaction after the restart:\n%s", seat.dump())

	close(seat.release)
	waitForStatus(t, d2, childID, "idle", 30*time.Second)
}

// TestCompactionFundiOverflowNetRecovers pins the reactive overflow net: a seat
// that rejects the first real request as "prompt is too long" ONCE, then answers
// normally, drives exactly one compaction and a clean turn — the child ends
// without an agent_error.
func TestCompactionFundiOverflowNetRecovers(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	marker := uniqueSuffix()
	seat := newCompactionSeat(t, marker)
	// A small reported usage keeps the PROACTIVE trigger dormant, so the only
	// way compaction can happen is the reactive net. The overflow lands on the
	// first real request with enough history to be compactable (5 messages: two
	// completed turns plus the new prompt).
	seat.realUsageInput = 10
	seat.overflowAtRows = 5
	cacheHome := t.TempDir()
	writeCompactionCatalog(t, cacheHome, compactionModel, compactionWindow)
	providers := writeCompactionProviders(t, seat.srv.URL)

	d := bootDaemonDB(t, nextDaemonID(), compactionEnv(providers, cacheHome)...)
	childID := spawnCompactionChild(t, d, t.TempDir(), nil)
	waitForStatus(t, d, childID, "idle", 30*time.Second)

	cs := openCompactionStream(t, d, childID)
	// Two complete turns build the history, then the third overflows.
	for i := 1; i <= 3; i++ {
		promptCompactionChild(t, d, childID, fmt.Sprintf("u%d", i), fmt.Sprintf("prompt-%d-%s", i, marker))
		cs.waitIdle(t, childID)
	}
	waitForStatus(t, d, childID, "idle", 30*time.Second)

	ck.Eq(1, seat.compactionCount(), "exactly one compaction request:\n%s", seat.dump())

	var errored bool
	for _, r := range seat.snapshot() {
		if r.errored {
			errored = true
		}
	}
	ck.True(errored, "the seat must have rejected one request as too large:\n%s", seat.dump())
	// The compaction request precedes the successful retry.
	reqs := seat.snapshot()
	var compactionIdx = -1
	for i, r := range reqs {
		if r.isCompaction {
			compactionIdx = i
		}
	}
	ck.True(compactionIdx >= 0 && compactionIdx+1 < len(reqs), "no retry after the compaction:\n%s", seat.dump())
	ck.StrContains(reqs[compactionIdx+1].firstText(), "SUMMARY-"+marker,
		"the retry must run on the compacted history:\n%s", seat.dump())
	ck.False(reqs[compactionIdx+1].errored, "the retry must succeed:\n%s", seat.dump())

	// The child ends the turn without an agent_error: no durable error event was
	// published for the overflow turn.
	ck.False(cs.hasError(), "the overflow turn must not publish an agent_error")
}

// conversationHasText reports whether any persisted message in the conversation
// contains want.
func conversationHasText(t *testing.T, dsn, conversationID, want string) bool {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	var n int
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid AND content::text LIKE '%' || $2 || '%'`,
		conversationID, want).Scan(&n), "search conversation for %q", want)
	return n > 0
}

// rowKindAt returns the kind of a conversation_message row at ordinal ("" when
// NULL).
func rowKindAt(t *testing.T, dsn, conversationID string, ordinal int) string {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	var kind *string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(),
		`SELECT kind FROM conversations.conversation_message WHERE conversation_id = $1::uuid AND ordinal = $2`,
		conversationID, ordinal).Scan(&kind), "read row kind at %d", ordinal)
	if kind == nil {
		return ""
	}
	return *kind
}

// ─── local executor (for the pre-fill variant) ───────────────────────────────

// startLocalExecutor mints a token and runs a native `rafiki executor serve`
// that reverse-dials the daemon's executor unix socket, rooted at root. A
// pre-fill reads through the executor's read tool, so the child needs one.
func startLocalExecutor(t *testing.T, d *daemon, dsn, root string) {
	t.Helper()
	c := assert.NewAborting(t)

	pool := openPool(t, dsn)
	store := executorsdb.NewPostgresStore(pool)
	token, err := store.MintToken(context.Background(), executors.NewToken{
		Labels:        map[string]string{"machine": "compaction-local"},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	pool.Close()
	c.NoError(err, "mint executor token")

	execSock := filepath.Join(d.homeDir, "rafiki", "executor.sock")
	home := t.TempDir()
	cred := filepath.Join(t.TempDir(), "cred")
	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect-socket", execSock,
		"--enroll-token", token,
		"--credential-file", cred,
		"--root", root,
	)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_RUNTIME_DIR="+home,
		"XDG_STATE_HOME="+home,
		"XDG_DATA_HOME="+home,
		"XDG_CONFIG_HOME="+home,
	)
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	c.NoError(cmd.Start(), "start local executor")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd:              root,
			Kind:             protocol.KindFundi,
			Model:            compactionModel,
			ExecutorSelector: "env=definitely-not-a-match",
		}))
		cancel()
		if err != nil && strings.Contains(err.Error(), "1 live executor(s)") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("local executor never became live\nexecutor stderr:\n%s", stderr.tail(4000))
}

// waitHorizonKind polls until the row at the conversation's resume horizon has
// the given kind (a manual /compact or /clear is asynchronous: the prompt is
// queued behind the engine's turn queue).
func waitHorizonKind(t *testing.T, dsn, conversationID, kind string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		horizon, got := compactionHorizon(t, dsn, conversationID)
		if got == kind {
			return horizon
		}
		if time.Now().After(deadline) {
			t.Fatalf("horizon row kind = %q after 30s; want %q", got, kind)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSlashCompactAndClearOnFundiChild drives /compact then /clear through a
// real daemon: /compact makes one summary request and moves the horizon onto a
// compaction_summary row without a turn; /clear moves it onto a kind='clear'
// row, after which the next turn's request carries none of the earlier text,
// while the full history still shows every prompt.
func TestSlashCompactAndClearOnFundiChild(t *testing.T) {
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	marker := uniqueSuffix()
	seat := newCompactionSeat(t, marker)
	seat.realUsageInput = 1_000 // never trips the proactive trigger
	cacheHome := t.TempDir()
	writeCompactionCatalog(t, cacheHome, compactionModel, compactionWindow)
	providers := writeCompactionProviders(t, seat.srv.URL)

	d := bootDaemonDB(t, nextDaemonID(), compactionEnv(providers, cacheHome)...)
	childID := spawnCompactionChild(t, d, t.TempDir(), nil)
	waitForStatus(t, d, childID, "idle", 30*time.Second)
	cs := openCompactionStream(t, d, childID)

	for i := 1; i <= 3; i++ {
		before := seat.requestCount()
		promptCompactionChild(t, d, childID, fmt.Sprintf("u%d", i), fmt.Sprintf("prompt-%d-%s", i, marker))
		waitSeatRequests(t, seat, before+1, 30*time.Second)
		cs.waitIdle(t, childID)
	}
	ck.Eq(0, seat.compactionCount(), "no compaction before /compact:\n%s", seat.dump())
	convID := resumeConversationID(t, dsn, childID)

	requestsBefore := seat.requestCount()
	promptCompactionChild(t, d, childID, "c1", "/compact")
	waitHorizonKind(t, dsn, convID, "compaction_summary")
	cs.waitBoundary(t)
	ck.Eq(1, seat.compactionCount(), "/compact must make exactly one summary request:\n%s", seat.dump())
	ck.Eq(requestsBefore+1, seat.requestCount(), "/compact must not run a turn:\n%s", seat.dump())

	promptCompactionChild(t, d, childID, "c2", "/clear")
	horizon := waitHorizonKind(t, dsn, convID, "clear")
	ck.NotEq(0, horizon, "the clear horizon must move off 0")

	before := seat.requestCount()
	promptCompactionChild(t, d, childID, "u4", "after-clear-"+marker)
	waitSeatRequests(t, seat, before+1, 30*time.Second)
	cs.waitIdle(t, childID)
	reqs := seat.snapshot()
	last := reqs[len(reqs)-1]
	ck.False(last.isCompaction, "the request after /clear must be a real turn")
	for _, m := range last.msgs {
		for _, text := range m.texts {
			ck.False(strings.Contains(text, "prompt-1-"+marker) || strings.Contains(text, "SUMMARY-"+marker),
				"a request after /clear must carry nothing from before it:\n%s", seat.dump())
		}
	}
	ck.StrContains(last.firstText(), "cleared", "the post-clear request must open with the clear boundary:\n%s", seat.dump())

	events := getHistoryEvents(t, d.control(t), childID)
	ck.Eq(1, countHistoryText(events, "prompt-1-"+marker), "pre-clear history must stay readable")
	ck.Eq(1, countHistoryText(events, "after-clear-"+marker), "the post-clear prompt must appear once")
}
