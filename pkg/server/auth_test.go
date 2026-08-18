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
	lastOwnerUserID  string
	lastAuthorUserID string
}

func (s *recordingStore) EnsureConversationByExternalRef(ctx context.Context, ref capture.ConversationRef) (string, error) {
	s.lastOwnerUserID = ref.OwnerUserID
	return "conv-1", nil
}

func (s *recordingStore) InsertTurnIntent(ctx context.Context, t capture.TurnIntent) (string, time.Time, error) {
	s.lastAuthorUserID = t.AuthorUserID
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

// The proxy persists the USER ID, never the username: the username is
// resolved at read time through the users FK, and Identity.Username exists for
// logs and CLI output only.
func TestMessagesProxyAuthenticatorSetsOwnerUserID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &recordingStore{}
	p := NewMessagesProxy(nil, staticAuthenticator{id: &Identity{UserID: "9f1c7b2e-0000-4000-8000-000000000001", Username: "brent"}}, "real-key", upstream.URL, "", nil, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	p.ServeHTTP(rec, req)

	const wantID = "9f1c7b2e-0000-4000-8000-000000000001"
	if fs.lastOwnerUserID != wantID || fs.lastAuthorUserID != wantID {
		t.Errorf("owner_user_id=%q author_user_id=%q, want %s for both (from Authenticator)",
			fs.lastOwnerUserID, fs.lastAuthorUserID, wantID)
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

	if fs.lastOwnerUserID != "" || fs.lastAuthorUserID != "" {
		t.Errorf("owner_user_id=%q author_user_id=%q, want empty/empty (anonymous)",
			fs.lastOwnerUserID, fs.lastAuthorUserID)
	}
}

// ConversationTokens satisfies proxyStore. The fake records nothing, so the
// rollup is empty and cost_total reflects only the turn being logged.
func (s *recordingStore) ConversationTokens(context.Context, string) ([]capture.ModelTokens, error) {
	return nil, nil
}
