// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// This file does the two upstream calls that touch a credential the daemon
// already holds: the refresh and the revocation.
//
// Both are deliberately hand-written rather than delegated to golang.org/x/
// oauth2's refreshing token source. That library's refresher sends only
// grant_type and refresh_token, and it hides the rotation inside a cached token
// source — two things this design cannot accept. A refresh here carries the
// RFC 8707 `resource` the grant is bound to (so an authorization server cannot
// mint a token for a different resource), and the rotation is returned to the
// store to persist under its intent version, so the value a caller ends up using
// is the one the database committed, never a token held only in memory.

// refresh is the store's RefreshFunc: one bounded upstream refresh for the grant
// the store just read under its row lock.
//
// It is called at most once per committed refresh intent, with cur being the
// authoritative stored grant. Every identity field stays the row's; only token
// material and (when the issuer says so) the granted scopes come back.
func (e *Engine) refresh(ctx context.Context, cur Grant) (RefreshResult, error) {
	endpoint := cur.Registration.TokenEndpoint
	if endpoint == "" {
		return RefreshResult{}, errors.New("mcpoauth: the stored grant has no token endpoint")
	}
	if cur.Token.RefreshToken == "" {
		// Nothing to spend and nothing to replay: this generation needs a login.
		return RefreshResult{}, fmt.Errorf("%w: the stored grant has no refresh token", ErrLoginRequired)
	}
	if err := e.policy.ValidateDestination(ctx, endpoint); err != nil {
		return RefreshResult{}, fmt.Errorf("mcpoauth: token endpoint %q: %w", endpoint, err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cur.Token.RefreshToken)
	// RFC 8707: the token is asked for THIS resource. Without it an
	// authorization server is entitled to answer with a token minted for
	// whatever the client was originally registered against.
	form.Set("resource", cur.Resource)

	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()

	resp, err := e.postToken(ctx, endpoint, cur.Registration, form)
	if err != nil {
		return RefreshResult{}, err
	}
	answer, err := decodeTokenResponse(resp)
	if err != nil {
		return RefreshResult{}, err
	}
	switch {
	case answer.ErrorCode == "invalid_grant":
		// The refresh token is dead: revoked, expired or already spent. That is
		// a login, not a retry.
		return RefreshResult{}, fmt.Errorf("%w: the refresh token was refused", ErrLoginRequired)
	case answer.ErrorCode != "":
		return RefreshResult{}, fmt.Errorf("mcpoauth: token refresh was refused (error %q)", safeOAuthCode(answer.ErrorCode))
	case answer.AccessToken == "":
		return RefreshResult{}, errors.New("mcpoauth: token refresh returned no access token")
	case answer.TokenType != "" && !isBearerType(answer.TokenType):
		return RefreshResult{}, fmt.Errorf("mcpoauth: token refresh returned token type %q, which this client cannot use", answer.TokenType)
	}

	result := RefreshResult{
		AccessToken: answer.AccessToken,
		// An omitted replacement keeps the stored token: the store applies that
		// rule, so an empty string here means "unchanged", never "revoked".
		RefreshToken: answer.RefreshToken,
		TokenType:    answer.TokenType,
	}
	if answer.ExpiresIn > 0 {
		result.Expiry = e.now().Add(time.Duration(answer.ExpiresIn) * time.Second)
	} else {
		// No lifetime at all — absent, zero or negative. The store treats a zero
		// expiry as "never refreshes", which would hand out a token nothing can
		// renew the moment the server stops honoring it. Expiring it immediately
		// is the honest reading: the next acquisition refreshes again, and the
		// rotated refresh token this answer may carry has already been persisted
		// by the time it does.
		result.Expiry = e.now()
	}
	if scopes := strings.Fields(answer.Scope); len(scopes) > 0 {
		result.Scopes = scopes
	}
	return result, nil
}

// clientAuthStyle is how one request presents the client credentials.
type clientAuthStyle int

const (
	// clientAuthParams sends client_id (and the secret, when there is one) in the
	// form body. It is also the public/none style: a client with no secret sends
	// only its id.
	clientAuthParams clientAuthStyle = iota
	// clientAuthHeader sends them as HTTP Basic, which requires a secret.
	clientAuthHeader
)

// clientAuthStyleOf resolves ONE style for a refresh, before the request is
// sent.
//
// There is deliberately no detection pass. A refresh token is single-use: a first
// attempt that the server refuses has already spent it, so retrying the same
// rotation under another credential placement cannot succeed — it can only make a
// second request with a dead token. What we resolve instead:
//
//   - A client with no secret is a public client (RFC 7591 "none"): its id goes in
//     the body, without a secret and without an Authorization header. Sending
//     Basic with an empty secret is not a thing a correct server accepts.
//   - A stored style the login established (in_params / in_header) is honoured.
//   - Anything else (AuthStyleAuto, or an unmodelled value) is a confidential
//     client whose method discovery did not name; RFC 7591's default for that is
//     client_secret_basic, chosen once and not second-guessed.
func clientAuthStyleOf(reg Registration) clientAuthStyle {
	if reg.ClientSecret == "" {
		return clientAuthParams
	}
	if reg.AuthStyle == AuthStyleInParams {
		return clientAuthParams
	}
	return clientAuthHeader
}

// postToken performs exactly one token endpoint request under the resolved client
// authentication style, over the guarded client.
func (e *Engine) postToken(ctx context.Context, endpoint string, reg Registration, form url.Values) (*http.Response, error) {
	return e.tokenRequest(ctx, endpoint, reg, form, clientAuthStyleOf(reg))
}

func (e *Engine) tokenRequest(ctx context.Context, endpoint string, reg Registration, form url.Values, style clientAuthStyle) (*http.Response, error) {
	body := make(url.Values, len(form)+2)
	for k, v := range form {
		body[k] = append([]string(nil), v...)
	}
	switch style {
	case clientAuthParams:
		if reg.ClientID != "" {
			body.Set("client_id", reg.ClientID)
		}
		if reg.ClientSecret != "" {
			body.Set("client_secret", reg.ClientSecret)
		}
	}
	encoded := body.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: building the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if style == clientAuthHeader && reg.ClientSecret != "" {
		// RFC 6749 section 2.3.1: form-encode before base64. A public client never
		// reaches this branch: Basic with an empty secret is a credential the server
		// should refuse, and it is not a credential this daemon sends.
		req.SetBasicAuth(url.QueryEscape(reg.ClientID), url.QueryEscape(reg.ClientSecret))
	}
	resp, err := e.policy.Client().Do(req)
	if err != nil {
		// Authored: a transport error names the URL it dialled, and this one may be
		// an endpoint a project entry chose. The caller learns the step failed; the
		// dial's own text goes no further.
		return nil, errors.New("mcpoauth: the token endpoint could not be reached")
	}
	return resp, nil
}

// tokenResponse is a token endpoint answer (RFC 6749 section 5.1) or its error
// form (section 5.2).
type tokenResponse struct {
	AccessToken  string
	TokenType    string
	RefreshToken string
	ExpiresIn    int64
	Scope        string
	ErrorCode    string
}

// decodeTokenResponse reads a token endpoint answer without ever quoting its
// body: an error body is attacker-influenced text that may repeat the
// authorization code or a token.
func decodeTokenResponse(resp *http.Response) (tokenResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		// Never wrapped: a body read can fail with a transport or codec error whose
		// text is not ours to repeat.
		return tokenResponse{}, fmt.Errorf("mcpoauth: the token endpoint answered status %d and the answer could not be read", resp.StatusCode)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" || mediaType == "text/plain" {
		values, perr := url.ParseQuery(string(body))
		if perr != nil {
			return tokenResponse{}, fmt.Errorf("mcpoauth: the token endpoint answered status %d with an unreadable body", resp.StatusCode)
		}
		answer := tokenResponse{
			AccessToken:  values.Get("access_token"),
			TokenType:    values.Get("token_type"),
			RefreshToken: values.Get("refresh_token"),
			Scope:        values.Get("scope"),
			ErrorCode:    values.Get("error"),
		}
		if v, cerr := strconv.ParseInt(values.Get("expires_in"), 10, 64); cerr == nil {
			answer.ExpiresIn = v
		}
		return checkTokenAnswer(resp.StatusCode, answer)
	}

	var wire struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		ErrorCode    string `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return tokenResponse{}, fmt.Errorf("mcpoauth: the token endpoint answered status %d with a body that is not a token response", resp.StatusCode)
	}
	return checkTokenAnswer(resp.StatusCode, tokenResponse{
		AccessToken:  wire.AccessToken,
		TokenType:    wire.TokenType,
		RefreshToken: wire.RefreshToken,
		ExpiresIn:    wire.ExpiresIn,
		Scope:        wire.Scope,
		ErrorCode:    wire.ErrorCode,
	})
}

// checkTokenAnswer refuses a non-2xx answer that carries no OAuth error code,
// so a proxy's HTML error page can never be mistaken for a token.
func checkTokenAnswer(status int, answer tokenResponse) (tokenResponse, error) {
	if status >= 200 && status <= 299 {
		return answer, nil
	}
	if answer.ErrorCode == "" {
		return tokenResponse{}, fmt.Errorf("mcpoauth: the token endpoint answered status %d", status)
	}
	return answer, nil
}

// isBearerType reports whether a token type is usable as an Authorization header
// value by this client.
func isBearerType(tokenType string) bool {
	return strings.EqualFold(tokenType, "bearer") ||
		strings.EqualFold(tokenType, "bearer_token") ||
		strings.EqualFold(tokenType, "bearer-token")
}

// RevokeDetached best-effort revokes credentials the store detached during a
// local invalidation — a logout, or the cleanup that follows a removed user.
//
// It exists so a caller that has already committed the invalidation can offer the
// token back to the authorization server without that offer being able to change
// the outcome: the rows are gone, the generation is tombstoned, and a revocation
// that fails or hangs is never a reason to restore anything or to fail the
// removal. The returned error is for the caller to log, and it is authored (the
// resource and a status, never the token or a response body).
//
// Each revocation is bounded by RevocationTimeout, the same budget a logout's
// revoke uses, and the loop stops as soon as the caller's own context ends. A
// caller that wants one bound for the whole batch passes a deadline of its own.
func (e *Engine) RevokeDetached(ctx context.Context, revocations []Revocation) error {
	if len(revocations) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// The local invalidation has already committed: none of this may be abandoned
	// because the request that triggered it went away.
	base := context.WithoutCancel(ctx)
	var errs []error
	for _, rev := range revocations {
		if ctx.Err() != nil {
			break
		}
		itemCtx, cancel := context.WithTimeout(base, RevocationTimeout)
		err := e.revokeOne(itemCtx, rev)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// revokeDetached is the logout path: one revocation, bounded, logged and never
// returned. It is the same attempt RevokeDetached makes.
func (e *Engine) revokeDetached(ctx context.Context, rev Revocation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RevocationTimeout)
	defer cancel()
	if err := e.revokeOne(ctx, rev); err != nil {
		e.log.Debug("mcp oauth revocation failed", "resource", displayURL(rev.Resource), "reason", err.Error())
	}
}

// revokeOne makes one bounded revocation attempt and describes the outcome.
//
// Nothing here logs or returns the token, and nothing here touches the store: a
// revocation is an offer to a server that may already have forgotten it, so the
// only thing it may change is what that server believes.
func (e *Engine) revokeOne(ctx context.Context, rev Revocation) error {
	endpoint := rev.Registration.RevocationEndpoint
	if endpoint == "" {
		// The issuer advertised none. Revocation is then skipped, never guessed.
		return nil
	}
	token, hint := rev.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = rev.AccessToken, "access_token"
	}
	if token == "" {
		return nil
	}
	if err := e.policy.ValidateDestination(ctx, endpoint); err != nil {
		return fmt.Errorf("%w: revocation endpoint for %s", ErrDestinationDenied, displayURL(rev.Resource))
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", hint)
	resp, err := e.postToken(ctx, endpoint, rev.Registration, form)
	if err != nil {
		return fmt.Errorf("mcpoauth: revocation for %s could not be sent", displayURL(rev.Resource))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mcpoauth: revocation for %s was refused (status %d)", displayURL(rev.Resource), resp.StatusCode)
	}
	return nil
}

// revokeProvisional drops token material from a login that will never be
// committed — the store refused the grant, so nothing local references it.
func (e *Engine) revokeProvisional(f *flow, reg Registration, tok Token) {
	e.revokeDetached(f.ctx, Revocation{
		OwnerUserID:  f.owner,
		Resource:     f.boundResource(),
		Registration: reg,
		RefreshToken: tok.RefreshToken,
		AccessToken:  tok.AccessToken,
	})
}
