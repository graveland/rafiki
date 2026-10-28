// SPDX-License-Identifier: Apache-2.0

package chatgptauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// pending is one in-flight browser login: everything Complete needs to replay
// the code exchange exactly as the authorize request made it.
type pending struct {
	ownerUserID string
	provider    string
	state       string
	verifier    string
	expires     time.Time
}

// errUnknownLogin is the single answer for an absent, expired, or
// other-owner login id. Naming which of the three it was would let a caller
// learn that another user's login exists and is live.
var errUnknownLogin = errors.New("chatgptauth: unknown or expired login; start the login again")

// Begin starts a login for (ownerUserID, provider) and returns the login id
// Complete expects plus the authorize URL to open in a browser.
//
// The caller must match the login id to the owner it was begun for: Complete
// refuses an id presented by anyone else. A second Begin for the same pair
// replaces the first, so a user who restarts a login does not accumulate
// entries that could still be completed with an older state.
func (e *Engine) Begin(ownerUserID, provider string) (loginID, authorizeURL string, err error) {
	if ownerUserID == "" {
		return "", "", errors.New("chatgptauth: Begin needs an owner user id")
	}
	if provider == "" {
		return "", "", errors.New("chatgptauth: Begin needs a provider name")
	}
	loginID, err = randomToken()
	if err != nil {
		return "", "", err
	}
	state, err := randomToken()
	if err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()

	e.mu.Lock()
	now := e.now()
	// Sweep on every Begin: the map is bounded only by this plus the cap. The
	// pair's previous entry goes the same way, which is what makes a second
	// Begin replace the first.
	for id, pend := range e.pending {
		if now.After(pend.expires) || (pend.ownerUserID == ownerUserID && pend.provider == provider) {
			delete(e.pending, id)
		}
	}
	if len(e.pending) >= maxPendingLogins {
		e.mu.Unlock()
		return "", "", fmt.Errorf(
			"chatgptauth: too many logins in progress (%d); finish one or wait %s",
			maxPendingLogins, pendingTTL)
	}
	e.pending[loginID] = pending{
		ownerUserID: ownerUserID,
		provider:    provider,
		state:       state,
		verifier:    verifier,
		expires:     now.Add(pendingTTL),
	}
	e.mu.Unlock()

	// Exactly the parameters `codex login` sends, originator included: the
	// authorize step is the one place a different originator has not been
	// shown to be accepted.
	authorizeURL = e.cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"),
		oauth2.SetAuthURLParam("codex_cli_simplified_flow", "true"),
		oauth2.SetAuthURLParam("originator", "codex_cli_rs"),
	)
	return loginID, authorizeURL, nil
}

// Complete validates a login callback and exchanges its code for tokens.
//
// The owner's pending entry is consumed whether or not the exchange succeeds:
// a login is single-use, so a replayed callback can never mint a second
// credential. An id belonging to ANOTHER owner is refused without consuming
// it, so knowing (or guessing) a login id buys nobody the ability to cancel
// someone else's login, and that refusal is identical to an absent id's.
func (e *Engine) Complete(ctx context.Context, ownerUserID, loginID, callbackQuery string) (provider string, t Tokens, err error) {
	now := e.now()
	e.mu.Lock()
	pend, ok := e.pending[loginID]
	if !ok || now.After(pend.expires) || pend.ownerUserID != ownerUserID {
		e.mu.Unlock()
		return "", Tokens{}, errUnknownLogin
	}
	// The owner's entry is consumed on first use, before anything that can
	// fail: a callback that reaches this point never works twice.
	delete(e.pending, loginID)
	e.mu.Unlock()

	q, err := url.ParseQuery(callbackQuery)
	if err != nil {
		return "", Tokens{}, fmt.Errorf("chatgptauth: malformed login callback query: %w", err)
	}
	// The issuer's own refusal, verbatim (truncated). It is an answer, not an
	// outage: the user declined, or the grant was refused.
	if idpErr := q.Get("error"); idpErr != "" {
		msg := "chatgptauth: the issuer refused the login: " + truncate(idpErr, errorTextLimit)
		if desc := q.Get("error_description"); desc != "" {
			msg += ": " + truncate(desc, errorTextLimit)
		}
		return "", Tokens{}, errors.New(msg)
	}
	// Constant-time: state is the anti-CSRF binding between Begin and
	// Complete, and the callback is reachable by anything that can reach the
	// loopback port.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(pend.state)) != 1 {
		return "", Tokens{}, errors.New("chatgptauth: state mismatch on the login callback")
	}
	code := q.Get("code")
	if code == "" {
		return "", Tokens{}, errors.New("chatgptauth: login callback is missing the authorization code")
	}

	// x/oauth2 takes its HTTP client from the context, not the Config, so the
	// engine's client has to be carried there for the exchange to use it.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, e.http)
	tok, err := e.cfg.Exchange(ctx, code, oauth2.VerifierOption(pend.verifier))
	if err != nil {
		return "", Tokens{}, fmt.Errorf("chatgptauth: authorization code exchange failed: %w", err)
	}
	t, err = tokensFrom(tok)
	if err != nil {
		return "", Tokens{}, err
	}
	return pend.provider, t, nil
}

// tokensFrom converts an x/oauth2 token from the code exchange.
func tokensFrom(tok *oauth2.Token) (Tokens, error) {
	if tok == nil || tok.AccessToken == "" {
		return Tokens{}, errors.New("chatgptauth: the token endpoint returned no access token")
	}
	t := Tokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	}
	if raw, ok := tok.Extra("id_token").(string); ok && raw != "" {
		t.IDToken = raw
		if id, err := AccountIDFromIDToken(raw); err == nil {
			t.AccountID = id
		}
	}
	return t, nil
}

// randomToken is 32 random bytes, base64url: a login id or a state.
func randomToken() (string, error) {
	b := make([]byte, entropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("chatgptauth: generate login entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
