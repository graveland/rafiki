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
	}
	if scopes := strings.Fields(answer.Scope); len(scopes) > 0 {
		result.Scopes = scopes
	}
	return result, nil
}

// clientAuthStyle is how one request presents the client credentials.
type clientAuthStyle int

const (
	// clientAuthAuto tries the header form and falls back to the body form.
	clientAuthAuto clientAuthStyle = iota
	clientAuthParams
	clientAuthHeader
)

func clientAuthStyleOf(style AuthStyle) clientAuthStyle {
	switch style {
	case AuthStyleInParams:
		return clientAuthParams
	case AuthStyleInHeader:
		return clientAuthHeader
	default:
		return clientAuthAuto
	}
}

// postToken performs one token endpoint request under the grant's stored client
// authentication style, over the guarded client.
func (e *Engine) postToken(ctx context.Context, endpoint string, reg Registration, form url.Values) (*http.Response, error) {
	style := clientAuthStyleOf(reg.AuthStyle)
	if style != clientAuthAuto {
		return e.tokenRequest(ctx, endpoint, reg, form, style)
	}
	// Auto-detect is the oauth2 library's own probe: header form first, then the
	// body form if the endpoint refuses the credentials' placement. It is the
	// last resort — a stored registration normally records which method its
	// issuance chose.
	resp, err := e.tokenRequest(ctx, endpoint, reg, form, clientAuthHeader)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	resp.Body.Close()
	return e.tokenRequest(ctx, endpoint, reg, form, clientAuthParams)
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
	if style == clientAuthHeader && (reg.ClientID != "" || reg.ClientSecret != "") {
		// RFC 6749 section 2.3.1: form-encode before base64.
		req.SetBasicAuth(url.QueryEscape(reg.ClientID), url.QueryEscape(reg.ClientSecret))
	}
	resp, err := e.policy.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: contacting the token endpoint: %w", err)
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
		return tokenResponse{}, fmt.Errorf("mcpoauth: reading the token endpoint answer: %w", err)
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

// revokeDetached best-effort revokes a token the store detached during a logout.
//
// It runs AFTER the local invalidation has committed and can only ever fail
// quietly: the rows are gone, the generation is tombstoned, and a revoke that
// does not answer is never a reason to restore anything. It is bounded by
// RevocationTimeout because the server may be the very thing the user logged out
// of.
func (e *Engine) revokeDetached(ctx context.Context, rev Revocation) {
	if rev.Registration.RevocationEndpoint == "" || (rev.RefreshToken == "" && rev.AccessToken == "") {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RevocationTimeout)
	defer cancel()
	token, hint := rev.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = rev.AccessToken, "access_token"
	}
	if err := e.policy.ValidateDestination(ctx, rev.Registration.RevocationEndpoint); err != nil {
		e.log.Debug("mcp oauth revocation skipped", "resource", rev.Resource, "reason", "endpoint is outside [mcp_oauth]")
		return
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", hint)
	resp, err := e.postToken(ctx, rev.Registration.RevocationEndpoint, rev.Registration, form)
	if err != nil {
		e.log.Debug("mcp oauth revocation failed", "resource", rev.Resource, "reason", "request failed")
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e.log.Debug("mcp oauth revocation refused", "resource", rev.Resource, "status", resp.StatusCode)
	}
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
