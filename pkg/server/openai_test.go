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
	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// End-to-end against a REAL capture store (RAFIKI_TEST_DSN): the OpenAI face
// decomposes the exchange into conversation_message rows, so the turn row
// keeps protocol/openai and the usage/cost accounting but no longer carries the
// full request body.
func TestChatCompletionsProxyStoresNoRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Require().Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err)
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_openai_noreq_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	c.Require().NoError(err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.Require().NoError(err)
	t.Cleanup(pool.Close)
	c.Require().NoError(store.Migrate(ctx, pool))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cc-1","model":"openai/gpt-4o",`+
			`"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi back"}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewChatCompletionsProxy(capture.NewCaptureStore(pool), nil,
		[]OpenAIUpstream{{Name: "openrouter", BaseURL: upstream.URL, APIKey: "or-key"}},
		nil, "openrouter", logger)

	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Rafiki-Session", "chat-sess-no-req")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	c.Require().Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())

	var reqNull bool
	var protocol string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT request IS NULL, protocol FROM conversations.conversation_turn LIMIT 1`).Scan(&reqNull, &protocol))
	c.True(reqNull, "conversation_turn.request should be NULL; the exchange is decomposed into messages")
	c.Eq("openai", protocol, "protocol")
}
