// SPDX-License-Identifier: Apache-2.0

package chatgptauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// jsonServer answers every request with a fixed status and body, and records
// the request it saw. The body is written on the server goroutine, so tests
// assert on it only after the call has returned.
type jsonServer struct {
	srv         *httptest.Server
	status      int
	body        string
	gotMethod   string
	gotPath     string
	gotCT       string
	gotBody     string
	gotAuthHead string
}

func newJSONServer(t *testing.T, status int, body string) *jsonServer {
	t.Helper()
	f := &jsonServer{status: status, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotMethod = r.Method
		f.gotPath = r.URL.Path
		f.gotCT = r.Header.Get("Content-Type")
		f.gotAuthHead = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		f.gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ─── Refresh: error classification ──────────────────────────────────────────

func TestRefreshClassifiesErrors(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantRejected bool
		wantReused   bool
	}{
		{name: "401 is permanent", status: 401, body: ``, wantRejected: true},
		{name: "invalid_grant string", status: 400, body: `{"error":"invalid_grant"}`, wantRejected: true},
		{name: "expired nested code", status: 400, body: `{"error":{"code":"refresh_token_expired"}}`, wantRejected: true},
		{name: "invalidated string", status: 400, body: `{"error":"refresh_token_invalidated"}`, wantRejected: true},
		{name: "reused nested code", status: 400, body: `{"error":{"code":"refresh_token_reused"}}`, wantReused: true},
		{name: "reused string", status: 400, body: `{"error":"refresh_token_reused"}`, wantReused: true},
		{name: "500 is transient", status: 500, body: `{"error":"internal_server_error"}`},
		{name: "unparseable body is transient", status: 400, body: `not json`},
		{name: "unknown code is transient", status: 400, body: `{"error":"something_else"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			fake := newJSONServer(t, tt.status, tt.body)
			e := engineFor(fake.srv)

			_, err := e.Refresh(context.Background(), "refresh-1", ReasonProactiveExpiry)
			c.Error(err, "Refresh must fail")
			c.Eq(tt.wantRejected || tt.wantReused, errors.Is(err, ErrRefreshRejected),
				"errors.Is(err, ErrRefreshRejected)")
			c.Eq(tt.wantReused, errors.Is(err, ErrRefreshReused),
				"errors.Is(err, ErrRefreshReused)")
		})
	}
}

// ─── Refresh: request shape ─────────────────────────────────────────────────

func TestRefreshSendsJSON(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newJSONServer(t, 200, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":60}`)
	e := engineFor(fake.srv)

	tok, err := e.Refresh(context.Background(), "refresh-1", ReasonScheduled)
	c.NoError(err, "Refresh")

	c.Eq(http.MethodPost, fake.gotMethod)
	c.Eq("/oauth/token", fake.gotPath)
	c.Eq("application/json", fake.gotCT)
	c.Eq("", fake.gotAuthHead, "a public client sends no basic auth header")

	var body struct {
		ClientID      string          `json:"client_id"`
		GrantType     string          `json:"grant_type"`
		RefreshToken  string          `json:"refresh_token"`
		RefreshReason string          `json:"refresh_reason"`
		Scope         json.RawMessage `json:"scope"`
	}
	c.NoError(json.Unmarshal([]byte(fake.gotBody), &body), "request body is JSON")
	c.Eq(ClientID, body.ClientID)
	c.Eq("refresh_token", body.GrantType)
	c.Eq("refresh-1", body.RefreshToken)
	c.Eq("scheduled", body.RefreshReason)
	c.Empty(body.Scope, "refresh must not send a scope")

	c.Eq("access-2", tok.AccessToken)
	c.Eq("refresh-2", tok.RefreshToken)
}

func TestRefreshKeepsFieldsWhenOmitted(t *testing.T) {
	c := assert.NewAborting(t)
	exp := time.Unix(2000000000, 0).UTC()
	access := mkJWT(t, map[string]any{"exp": 2000000000})
	fake := newJSONServer(t, 200, `{"access_token":"`+access+`"}`)
	e := engineFor(fake.srv)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	tok, err := e.Refresh(context.Background(), "refresh-old", ReasonForced)
	c.NoError(err, "Refresh")

	c.Eq(access, tok.AccessToken)
	c.Eq("refresh-old", tok.RefreshToken, "an omitted refresh token keeps the one we sent")
	c.Eq("", tok.IDToken, "no id_token in the response")
	c.Eq("", tok.AccountID, "no account id without an id_token")
	c.True(tok.Expiry.Equal(exp), "expiry from the access token's exp claim")
}

func TestRefreshDefaultsToAnHour(t *testing.T) {
	c := assert.NewAborting(t)
	// An opaque access token carries no exp claim, so the assumed lifetime is
	// the only thing left to fall back on.
	fake := newJSONServer(t, 200, `{"access_token":"opaque-access-token"}`)
	e := engineFor(fake.srv)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	tok, err := e.Refresh(context.Background(), "refresh-old", ReasonForced)
	c.NoError(err, "Refresh")
	c.True(tok.Expiry.Equal(now.Add(time.Hour)), "expiry defaults to now + 1h")
}

func TestRefreshRotatedTokenSurvivesMissingAccessToken(t *testing.T) {
	// A rotation with no access token: the old refresh token is already dead
	// upstream, so the new one has to come back with the error. The MIXED-CASE
	// row matters as much as the exact one: encoding/json reads
	// {"Refresh_Token": ...} into a refresh_token field, so that shape is
	// honoured by this package and must be redacted like the exact key.
	tests := []struct {
		name string
		body func(token string) string
	}{
		{name: "exact key", body: func(token string) string {
			return `{"refresh_token":"` + token + `"}`
		}},
		{name: "mixed case key", body: func(token string) string {
			return `{"Refresh_Token":"` + token + `"}`
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			const token = "rotated-refresh-token-value"
			fake := newJSONServer(t, 200, tt.body(token))
			e := engineFor(fake.srv)

			tok, err := e.Refresh(context.Background(), "refresh-1", ReasonForced)
			c.Error(err, "a response with no access token is a failure")
			c.NotErrorIs(err, ErrRefreshRejected, "and not a permanent one")
			c.Eq(token, tok.RefreshToken, "the rotated token must ride back with the error")

			// The message ends up in a log line, so the token must not be in it.
			c.NotStrContains(err.Error(), token, "the error must not leak the rotated token")
			c.StrContains(err.Error(), "HTTP 200")
			c.StrContains(err.Error(), "<redacted>")
		})
	}
}

func TestRefreshErrorBodyIsRedacted(t *testing.T) {
	c := assert.NewAborting(t)
	const token = "a-token-that-must-not-be-logged"
	fake := newJSONServer(t, 400, `{"error":{"code":"slow_down"},"access_token":"`+token+`"}`)
	e := engineFor(fake.srv)

	_, err := e.Refresh(context.Background(), "refresh-1", ReasonForced)
	c.Error(err, "Refresh must fail")
	c.NotErrorIs(err, ErrRefreshRejected, "slow_down is transient")
	c.NotStrContains(err.Error(), token, "an error body's token must not reach the message")
	c.StrContains(err.Error(), "<redacted>")
	c.StrContains(err.Error(), "HTTP 400")
}

func TestRefreshExpiresInBeatsJWTExp(t *testing.T) {
	c := assert.NewAborting(t)
	// The access token claims a lifetime to 2033; expires_in says 60 seconds.
	// expires_in wins, or the caller would hold a token long past its real end
	// (or refresh one the issuer considers fresh).
	access := mkJWT(t, map[string]any{"exp": 2000000000})
	fake := newJSONServer(t, 200, `{"access_token":"`+access+`","expires_in":60}`)
	e := engineFor(fake.srv)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	tok, err := e.Refresh(context.Background(), "refresh-1", ReasonForced)
	c.NoError(err, "Refresh")
	c.True(tok.Expiry.Equal(now.Add(60*time.Second)),
		"expiry = %s, want %s (from expires_in, not the JWT exp claim)", tok.Expiry, now.Add(60*time.Second))
}

// ─── Revoke ─────────────────────────────────────────────────────────────────

func TestRevokeBestEffort(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "accepted", status: 200},
		{name: "refused", status: 500, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			fake := newJSONServer(t, tt.status, `{}`)
			e := engineFor(fake.srv)

			err := e.Revoke(context.Background(), "refresh-1")
			c.Eq(tt.wantErr, err != nil, "Revoke error")

			c.Eq(http.MethodPost, fake.gotMethod)
			c.Eq("/oauth/revoke", fake.gotPath)
			c.Eq("application/json", fake.gotCT)
			var body struct {
				ClientID      string `json:"client_id"`
				Token         string `json:"token"`
				TokenTypeHint string `json:"token_type_hint"`
			}
			c.NoError(json.Unmarshal([]byte(fake.gotBody), &body), "revoke body is JSON")
			c.Eq(ClientID, body.ClientID)
			c.Eq("refresh-1", body.Token)
			c.Eq("refresh_token", body.TokenTypeHint)
		})
	}
}

// ─── AccountIDFromIDToken ───────────────────────────────────────────────────

func TestAccountIDFromIDToken(t *testing.T) {
	tests := []struct {
		name    string
		token   func(t *testing.T) string
		want    string
		wantErr bool
	}{
		{
			name: "namespaced claim wins",
			token: func(t *testing.T) string {
				return mkJWT(t, map[string]any{
					authClaim:            map[string]any{"chatgpt_account_id": "acct-nested"},
					"chatgpt_account_id": "acct-top",
				})
			},
			want: "acct-nested",
		},
		{
			name: "top level fallback",
			token: func(t *testing.T) string {
				return mkJWT(t, map[string]any{"chatgpt_account_id": "acct-top"})
			},
			want: "acct-top",
		},
		{
			name: "namespaced object without the field falls back",
			token: func(t *testing.T) string {
				return mkJWT(t, map[string]any{
					authClaim:            map[string]any{"other": "x"},
					"chatgpt_account_id": "acct-top",
				})
			},
			want: "acct-top",
		},
		{
			name: "neither claim",
			token: func(t *testing.T) string {
				return mkJWT(t, map[string]any{"sub": "user-1"})
			},
			wantErr: true,
		},
		{
			name:    "malformed JWT",
			token:   func(*testing.T) string { return "not-a-jwt" },
			wantErr: true,
		},
		{
			name: "payload is not base64url",
			token: func(*testing.T) string {
				return "header.!!!!.signature"
			},
			wantErr: true,
		},
		{
			name: "payload is not an object",
			token: func(*testing.T) string {
				return "header." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".signature"
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got, err := AccountIDFromIDToken(tt.token(t))
			if tt.wantErr {
				c.Error(err, "AccountIDFromIDToken must fail")
				return
			}
			c.NoError(err, "AccountIDFromIDToken")
			c.Eq(tt.want, got)
		})
	}
}

// TestTokensRedactSecrets pins the leak-closing rule for the issuer's own answer
// type: %v/%s/%+v go through String, %#v through GoString, slog through
// LogValue, and none of the three tokens may appear in any of them.
func TestTokensRedactSecrets(t *testing.T) {
	c := assert.NewAborting(t)
	tok := Tokens{
		AccessToken:  "access-token-must-not-print",
		RefreshToken: "refresh-token-must-not-print",
		IDToken:      "id-token-must-not-print",
		Expiry:       time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC),
		AccountID:    "acct_1",
	}

	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		out := fmt.Sprintf(format, tok)
		for _, secret := range []string{tok.AccessToken, tok.RefreshToken, tok.IDToken} {
			c.NotStrContains(out, secret, "%s leaked %q: %s", format, secret, out)
		}
		c.StrContains(out, "<redacted>", "%s does not mark the tokens redacted: %s", format, out)
		c.StrContains(out, tok.AccountID, "%s dropped the non-secret fields: %s", format, out)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("tokens", "tokens", tok)
	for _, secret := range []string{tok.AccessToken, tok.RefreshToken, tok.IDToken} {
		c.NotStrContains(buf.String(), secret, "slog leaked %q: %s", secret, buf.String())
	}
	c.StrContains(buf.String(), "<redacted>", "slog does not mark the tokens redacted: %s", buf.String())
	c.StrContains(buf.String(), tok.AccountID, "slog dropped the non-secret fields: %s", buf.String())
}
