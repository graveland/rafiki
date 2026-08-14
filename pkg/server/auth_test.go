// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/capture"
)

type recordingStore struct {
	lastOwner  string
	lastAuthor string
}

func (s *recordingStore) EnsureConversationByExternalRef(ctx context.Context, ref capture.ConversationRef) (string, error) {
	s.lastOwner = ref.Owner
	return "conv-1", nil
}

func (s *recordingStore) InsertTurnIntent(ctx context.Context, t capture.TurnIntent) (string, time.Time, error) {
	s.lastAuthor = t.Author
	return "turn-1", time.Unix(0, 0), nil
}

func (s *recordingStore) CompleteTurn(ctx context.Context, r capture.TurnResult) error { return nil }
func (s *recordingStore) FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error {
	return nil
}
func (s *recordingStore) DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) (int, error) {
	return 0, nil
}
func (s *recordingStore) AppendResponseMessage(ctx context.Context, convID, turnID string, createdAt time.Time, ordinal int, canonical []byte, in, out int64, stopReason string) error {
	return nil
}

type staticAuthenticator struct{ id *Identity }

func (a staticAuthenticator) Identify(*http.Request) *Identity { return a.id }

func TestMessagesProxyAuthenticatorSetsOwner(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &recordingStore{}
	p := NewMessagesProxy(nil, staticAuthenticator{id: &Identity{Username: "brent"}}, "real-key", upstream.URL, "", nil, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	p.ServeHTTP(rec, req)

	if fs.lastOwner != "brent" || fs.lastAuthor != "brent" {
		t.Errorf("owner=%q author=%q, want brent/brent (from Authenticator)", fs.lastOwner, fs.lastAuthor)
	}
}

func TestMessagesProxyNilAuthenticatorIsAnonymous(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &recordingStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	p.ServeHTTP(rec, req)

	if fs.lastOwner != "" || fs.lastAuthor != "" {
		t.Errorf("owner=%q author=%q, want empty/empty (anonymous)", fs.lastOwner, fs.lastAuthor)
	}
}

// ConversationTokens satisfies proxyStore. The fake records nothing, so the
// rollup is empty and cost_total reflects only the turn being logged.
func (s *recordingStore) ConversationTokens(context.Context, string) ([]capture.ModelTokens, error) {
	return nil, nil
}
