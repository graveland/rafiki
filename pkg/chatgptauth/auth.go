// SPDX-License-Identifier: Apache-2.0

// Package chatgptauth is the OAuth engine for ChatGPT subscription logins: the
// PKCE browser flow, the refresh that rotates the token, revocation, and the
// account id every ChatGPT-backed request has to carry.
//
// It holds no storage and no database handle. The daemon decides what to
// persist; this package only talks to the issuer and keeps the login ids of
// in-flight browser logins until they are completed or expire.
package chatgptauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
)

// The ChatGPT subscription login, as the codex CLI performs it. These values
// are fixed by the issuer's registered client, not by a rafiki deployment
// choice: a different client id would be a different login.
const (
	ClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	Issuer       = "https://auth.openai.com"
	AuthorizeURL = Issuer + "/oauth/authorize" // captured from `codex login` (spike 1)
	TokenURL     = Issuer + "/oauth/token"
	RevokeURL    = Issuer + "/oauth/revoke"
	Scopes       = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	CallbackPort = 1455
	CallbackPath = "/auth/callback"
	CallbackHost = "127.0.0.1" // the registered redirect host, captured from `codex login`

	// authClaim is the id_token claim object holding the ChatGPT account id.
	authClaim = "https://api.openai.com/auth"

	// accountIDField is the field of authClaim carrying it. It has a
	// top-level twin, which AccountIDFromIDToken falls back to.
	accountIDField = "chatgpt_account_id"

	// pendingTTL is how long a Begin stays completable. Short on purpose: a
	// pending login holds a live PKCE verifier that anyone who can reach the
	// loopback callback could otherwise replay at leisure.
	pendingTTL = 10 * time.Minute

	// maxPendingLogins bounds the pending map. Begin is reachable by any
	// client the daemon serves, so nothing but this cap stops a loop of
	// Begins from growing the map without limit.
	maxPendingLogins = 1024

	// entropyBytes per generated login id and state. 32 bytes of randomness,
	// base64url, each.
	entropyBytes = 32

	// defaultExpiry is the access-token lifetime assumed when a refresh
	// response states neither expires_in nor a JWT exp claim.
	defaultExpiry = time.Hour

	// responseBodyLimit caps how much of a token or revoke response is read.
	// These bodies are a few hundred bytes; anything larger is not ours.
	responseBodyLimit = 1 << 20

	// errorTextLimit caps how much of an unparseable response body reaches an
	// error message.
	errorTextLimit = 200
)

// Tokens is one credential as the issuer returns it. Fields the issuer omitted
// stay zero, and the caller keeps whatever it already had stored: a refresh
// response is allowed to be a bare access token.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Expiry       time.Time
	AccountID    string // from the id_token's https://api.openai.com/auth claim, field chatgpt_account_id
}

// ErrRefreshRejected: the refresh token is permanently unusable; the user must log in again.
var ErrRefreshRejected = errors.New("chatgptauth: refresh token rejected; log in again")

// ErrRefreshReused is the refresh_token_reused case of ErrRefreshRejected (errors.Is holds for
// both). It is distinguishable because the caller re-reads storage once before giving up:
// another daemon may have rotated the token between our read and our call.
var ErrRefreshReused = fmt.Errorf("%w (refresh_token_reused)", ErrRefreshRejected)

// RefreshReason is sent as refresh_reason, as codex does.
type RefreshReason string

// The reasons codex distinguishes when it refreshes.
const (
	ReasonProactiveExpiry      RefreshReason = "proactive_expiry"
	ReasonScheduled            RefreshReason = "scheduled"
	ReasonUnauthorizedRecovery RefreshReason = "unauthorized_recovery"
	ReasonForced               RefreshReason = "forced"
)

// permanentErrorCodes are the issuer's own names for a refresh token that can
// never work again. refresh_token_reused is handled separately: it may be
// another daemon's rotation rather than a dead login.
var permanentErrorCodes = map[string]bool{
	"refresh_token_expired":     true,
	"refresh_token_invalidated": true,
	"invalid_grant":             true,
}

// Engine performs ChatGPT logins and token refreshes. One instance serves the
// whole daemon and is safe for concurrent use.
type Engine struct {
	cfg       oauth2.Config
	revokeURL string
	http      *http.Client

	mu      sync.Mutex
	pending map[string]pending

	// now is injectable for tests; every expiry decision goes through it.
	now func() time.Time
}

// New builds an Engine against the real issuer. A nil httpClient means
// http.DefaultClient.
func New(httpClient *http.Client) *Engine {
	return NewWithEndpoints(httpClient, AuthorizeURL, TokenURL, RevokeURL)
}

// NewWithEndpoints is New with the three issuer endpoints replaced, which is
// what tests use to point the engine at an httptest server.
func NewWithEndpoints(httpClient *http.Client, authorizeURL, tokenURL, revokeURL string) *Engine {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	e := &Engine{
		revokeURL: revokeURL,
		http:      httpClient,
		pending:   make(map[string]pending),
		now:       time.Now,
	}
	// AuthStyleInParams: this is a public client with no secret, and the
	// issuer expects client_id in the form body (as codex sends it), not as
	// an empty-password basic-auth header. Pinning the style also keeps
	// x/oauth2 from probing the endpoint with a doomed first request.
	e.cfg = oauth2.Config{
		ClientID: ClientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:   authorizeURL,
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: redirectURI(),
		Scopes:      strings.Fields(Scopes),
	}
	return e
}

// redirectURI is the registered loopback redirect, and the ONE value used both
// in the authorize URL and in the code exchange: the issuer treats a mismatch
// between the two as a code-replay signal and refuses the exchange.
func redirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", CallbackHost, CallbackPort, CallbackPath)
}

// refreshRequest is the JSON body codex sends to refresh a token. It is
// hand-written rather than x/oauth2's form encoding because the issuer wants
// JSON here, and because x/oauth2's TokenSource would cache a token the
// database owns (refresh tokens are single-use and rotated under a row lock).
type refreshRequest struct {
	ClientID      string        `json:"client_id"`
	GrantType     string        `json:"grant_type"`
	RefreshToken  string        `json:"refresh_token"`
	RefreshReason RefreshReason `json:"refresh_reason"`
}

// refreshResponse is deliberately all-optional: the issuer may answer with
// only an access token, and every omitted field means "unchanged".
type refreshResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	ExpiresIn    flexInt `json:"expires_in"`
}

// flexInt accepts expires_in as a JSON number or a quoted number. Providers
// disagree on the encoding, and a shape mismatch must not fail a refresh that
// otherwise succeeded: an unparseable value reads as absent.
type flexInt int64

// UnmarshalJSON decodes a number or a string holding one. Garbage and null
// read as 0, which callers treat as "not reported".
func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	*f = flexInt(int64(n))
	return nil
}

// Refresh trades a refresh token for a new access token.
//
// A permanent failure wraps ErrRefreshRejected (or is ErrRefreshReused); every
// other failure (network, 5xx, an unparseable body, an unrecognised error
// code) is transient and wraps neither, because the caller's remedy differs:
// a transient error is retried, a permanent one sends the user back to login.
//
// A NON-EMPTY RefreshToken in the returned Tokens MUST be persisted by the
// caller even when err is non-nil. The issuer rotates on use: a response that
// carried a new refresh token and no access token has already invalidated the
// old one, so discarding the new token kills a credential that still works.
func (e *Engine) Refresh(ctx context.Context, refreshToken string, reason RefreshReason) (Tokens, error) {
	if refreshToken == "" {
		// Never retryable: there is nothing to refresh with.
		return Tokens{}, fmt.Errorf("%w: no refresh token", ErrRefreshRejected)
	}
	if reason == "" {
		reason = ReasonForced
	}
	body, err := json.Marshal(refreshRequest{
		ClientID:      ClientID,
		GrantType:     "refresh_token",
		RefreshToken:  refreshToken,
		RefreshReason: reason,
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgptauth: encode refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint.TokenURL, bytes.NewReader(body))
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgptauth: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("chatgptauth: refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit))
	if readErr != nil {
		return Tokens{}, fmt.Errorf("chatgptauth: read refresh response: %w", readErr)
	}

	// Classify before looking at the status, in codex's order: the body's own
	// code is the more specific answer, and it can arrive on a 400 or a 401.
	code := errorCode(raw)
	if code == "refresh_token_reused" {
		return Tokens{}, ErrRefreshReused
	}
	if permanentErrorCodes[code] {
		return Tokens{}, fmt.Errorf("%w (%s)", ErrRefreshRejected, code)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return Tokens{}, fmt.Errorf("%w (HTTP 401)", ErrRefreshRejected)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Tokens{}, fmt.Errorf("chatgptauth: refresh failed: %s", describeBody(resp.StatusCode, raw))
	}
	var out refreshResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Tokens{}, fmt.Errorf("chatgptauth: refresh response is not JSON: %w", err)
	}
	if out.AccessToken == "" {
		// A 2xx with nothing usable in it is an upstream fault, not a dead
		// login: the next attempt may well succeed. A rotated token that
		// arrived without one is still OURS to keep: the issuer has already
		// invalidated the old token, so it rides back with the error (see the
		// contract on Refresh). The body never reaches the message unredacted.
		return Tokens{RefreshToken: out.RefreshToken},
			fmt.Errorf("chatgptauth: refresh response carried no access_token: %s",
				describeBody(resp.StatusCode, raw))
	}

	t := Tokens{
		AccessToken:  out.AccessToken,
		RefreshToken: refreshToken, // kept unless the issuer rotated it
		IDToken:      out.IDToken,
	}
	if out.RefreshToken != "" {
		t.RefreshToken = out.RefreshToken
	}
	switch {
	case out.ExpiresIn > 0:
		t.Expiry = e.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	default:
		if exp, ok := expiryFromJWT(out.AccessToken); ok {
			t.Expiry = exp
		} else {
			t.Expiry = e.now().Add(defaultExpiry)
		}
	}
	if out.IDToken != "" {
		// An id_token that does not parse leaves AccountID empty on purpose:
		// the caller keeps the account id it already stored rather than
		// failing a refresh that produced a usable access token.
		if id, err := AccountIDFromIDToken(out.IDToken); err == nil {
			t.AccountID = id
		}
	}
	return t, nil
}

// Revoke asks the issuer to invalidate a refresh token. Best effort by
// contract: the caller logs a failure and moves on, because the daemon has
// already deleted the only copy it kept.
func (e *Engine) Revoke(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return errors.New("chatgptauth: revoke called with no refresh token")
	}
	body, err := json.Marshal(struct {
		ClientID      string `json:"client_id"`
		Token         string `json:"token"`
		TokenTypeHint string `json:"token_type_hint"`
	}{ClientID: ClientID, Token: refreshToken, TokenTypeHint: "refresh_token"})
	if err != nil {
		return fmt.Errorf("chatgptauth: encode revoke request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.revokeURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("chatgptauth: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("chatgptauth: revoke request failed: %w", err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused; the answer itself carries
	// nothing this daemon acts on.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("chatgptauth: revoke failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// AccountIDFromIDToken returns the ChatGPT account id an id_token carries.
//
// The signature is NOT verified. The token arrived in the same TLS response as
// the access token it accompanies, straight from the issuer, so it is already
// as trustworthy as the access token itself; verifying it would mean fetching
// and pinning a JWKS for a claim only this daemon ever reads.
func AccountIDFromIDToken(idToken string) (string, error) {
	payload, err := jwtPayload(idToken)
	if err != nil {
		return "", err
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("chatgptauth: id_token payload is not a JSON object: %w", err)
	}
	if raw, ok := claims[authClaim]; ok {
		var nested struct {
			AccountID string `json:"chatgpt_account_id"`
		}
		if err := json.Unmarshal(raw, &nested); err == nil && nested.AccountID != "" {
			return nested.AccountID, nil
		}
	}
	var top struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(payload, &top); err == nil && top.AccountID != "" {
		return top.AccountID, nil
	}
	return "", fmt.Errorf("chatgptauth: id_token carries no %s.%s claim", authClaim, accountIDField)
}

// expiryFromJWT reads an access token's exp claim, the fallback for a refresh
// response that omits expires_in.
func expiryFromJWT(accessToken string) (time.Time, bool) {
	payload, err := jwtPayload(accessToken)
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0).UTC(), true
}

// jwtPayload decodes the payload segment of a JWT without touching its
// signature. The segment is base64url without padding; padded input is
// accepted too, since some issuers pad and it costs one retry to allow.
func jwtPayload(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("chatgptauth: token is not a JWT (want 3 dot-separated parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err == nil {
		return payload, nil
	}
	payload, perr := base64.URLEncoding.DecodeString(parts[1])
	if perr != nil {
		return nil, fmt.Errorf("chatgptauth: JWT payload is not base64url: %w", err)
	}
	return payload, nil
}

// errorCode reads the issuer's error code from either shape it uses: a string
// error field, or a nested object carrying a code.
func errorCode(raw []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Error) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(body.Error, &s); err == nil {
		return s
	}
	var obj struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body.Error, &obj); err == nil {
		return obj.Code
	}
	return ""
}

// tokenFields are the response fields whose values are credentials. They are
// redacted before a body ever reaches an error message.
var tokenFields = []string{"access_token", "refresh_token", "id_token"}

// describeBody names an HTTP status and, when the body parses as a JSON
// object, its token values redacted. Nothing else of the body is reported: an
// error message ends up in a log line, and a response can echo the credential
// it was about to hand out.
//
// Keys match CASE-INSENSITIVELY, which is the point: encoding/json reads
// {"Refresh_Token": ...} into a refresh_token struct field, so a token written
// with a different case is one this package honours and has to redact.
func describeBody(status int, raw []byte) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Sprintf("HTTP %d", status)
	}
	for key := range fields {
		if isTokenField(key) {
			fields[key] = json.RawMessage(`"<redacted>"`)
		}
	}
	redacted, err := marshalPlain(fields)
	if err != nil {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, truncate(redacted, errorTextLimit))
}

// isTokenField reports whether a body key names a credential, ignoring case.
func isTokenField(key string) bool {
	for _, field := range tokenFields {
		if strings.EqualFold(key, field) {
			return true
		}
	}
	return false
}

// marshalPlain encodes v without HTML escaping, so the "<redacted>"
// placeholder reads as itself in an error message rather than as \u003c... .
func marshalPlain(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// truncate caps s to n bytes on a rune boundary. Error text can quote a
// response body, and a body is not this daemon's to trust.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
