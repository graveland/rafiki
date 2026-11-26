// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/anthropicreq"
	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// parseTestStore returns a CaptureStore on a scratch database, skipping the test
// when no test DSN is configured. It is the harness the two capture-outcome
// tests below need: the decode counts and the failed-turn record only exist on
// the real store, because the fake one writes nothing.
func parseTestStore(t *testing.T) (*capture.CaptureStore, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Require().Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err, "admin pool")
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_bodyparse_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	c.Require().NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.Require().NoError(err, "scratch pool")
	t.Cleanup(pool.Close)
	c.Require().NoError(store.Migrate(ctx, pool), "migrate")
	return capture.NewCaptureStore(pool), pool
}

// simpleUpstream is a well-formed non-streamed Anthropic response, so the turn
// captures cleanly.
func simpleUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestServeHTTPDecodesBodyOnce pins the whole point of pkg/anthropicreq: one
// proxied turn decodes the top-level object once and the messages[] array once,
// however many consumers ask. Before the parse was shared, one request decoded
// the body about nine times.
func TestServeHTTPDecodesBodyOnce(t *testing.T) {
	c := assert.NewCollecting(t)
	cs, _ := parseTestStore(t)
	upstream := simpleUpstream(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(cs, nil, "key", upstream.URL, "claude", nil, logger)

	body := `{"model":"claude","stream":false,"diagnostics":{"previous_message_id":""},` +
		`"system":[{"type":"text","text":"plain instructions"}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

	anthropicreq.DecodeCount.Top.Store(0)
	anthropicreq.DecodeCount.Messages.Store(0)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Rafiki-Session", "sess-decode-once")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())
	c.Eq(int64(1), anthropicreq.DecodeCount.Top.Load(), "top-level decodes for one proxied turn")
	c.Eq(int64(1), anthropicreq.DecodeCount.Messages.Load(), "messages[] decodes for one proxied turn")
}

// TestServeHTTPUnparseableBodyKeepsTheForwardPath pins the shape of today's
// behaviour for a body that does not decode: it is FORWARDED upstream verbatim
// and the capture turn is begun and completed exactly as before. The
// expectation was recorded by running this request on hot-paths before the body
// was parsed once.
func TestServeHTTPUnparseableBodyKeepsTheForwardPath(t *testing.T) {
	c := assert.NewCollecting(t)
	var sawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`not json`))
	req.Header.Set("X-Rafiki-Session", "sess-bad")
	p.ServeHTTP(rec, req)

	c.Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())
	c.StrContains(rec.Body.String(), "msg_1", "the upstream's answer must reach the client, got")
	c.Eq("not json", sawBody, "the unparseable body must be forwarded verbatim, not rewritten")
	c.Eq(1, fs.intents, "turn intents")
	c.Eq(1, fs.completes, "completions")
	c.Eq(0, fs.fails, "failed turns")
	c.EqDeep([]string{"CompleteCapturedTurn"}, fs.calls, "store calls")
	c.Eq("", fs.lastIntentModel, "the intent's model (nothing was resolved)")
	c.Eq("claude", fs.lastIntentSource, "the intent's source")
	c.Eq("human", fs.lastIntentAuthorKind, "the intent's author kind")
}

// TestServeHTTPUnparseableBodyIsRecordedAsAFailedTurn pins the capture outcome
// for the same request against the REAL store: the turn lands as 'error' naming
// the decode failure, with no prefix hash and no message rows. Recorded by
// running it on hot-paths before the body was parsed once.
func TestServeHTTPUnparseableBodyIsRecordedAsAFailedTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	cs, pool := parseTestStore(t)
	upstream := simpleUpstream(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(cs, nil, "key", upstream.URL, "claude", nil, logger)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`not json`))
	req.Header.Set("X-Rafiki-Session", "sess-badbody")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	c.Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())

	ctx := context.Background()
	var status, errMsg string
	var prefixHash *string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT status, coalesce(error,''), prefix_hash FROM conversations.conversation_turn`).Scan(&status, &errMsg, &prefixHash),
		"read the turn")
	c.Eq("error", status, "turn status")
	c.Eq("capture completion failed: decompose: unmarshal request: invalid character 'o' in literal null (expecting 'u')",
		errMsg, "the failed turn's reason")
	if prefixHash != nil {
		c.Eq("", *prefixHash, "prefix hash of an unparseable request")
	}
	var msgs int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message`).Scan(&msgs), "count messages")
	c.Eq(0, msgs, "message rows a failed decompose must not write")
	var turns int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_turn`).Scan(&turns), "count turns")
	c.Eq(1, turns, "turn rows")
}
