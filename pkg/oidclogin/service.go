// SPDX-License-Identifier: Apache-2.0

package oidclogin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/users"
)

// TokenMinter is the slice of users.Store the login engine needs. Kept as an
// interface so tests can fake it and so the engine never needs the full Store.
type TokenMinter interface {
	MintToken(ctx context.Context, userID string, t users.NewToken) (users.Token, string, error)
}

// pendingLogin is one in-flight browser login: everything Complete needs to
// replay the code exchange exactly as the authorize request made it.
type pendingLogin struct {
	state, nonce, verifier, redirectURI, clientHost string
	expires                                         time.Time
}

const (
	// pendingTTL is how long a Begin stays completable. Short on purpose: a
	// pending login is unauthenticated surface holding a live PKCE verifier.
	pendingTTL = 10 * time.Minute

	// maxPendingLogins bounds the pending map — this endpoint is
	// unauthenticated, so nothing stops a loop of Begins from growing it
	// without limit except this cap.
	maxPendingLogins = 1024

	// entropyBytes per generated login id, state and nonce — 32 bytes of
	// randomness, base64url, each.
	entropyBytes = 32

	// maxClientHostLen bounds clientHost: it becomes part of a token name
	// and a log line, both of which a hostile value could otherwise poison.
	maxClientHostLen = 64

	// discoveryTimeout bounds how long one lazy discovery attempt may take.
	discoveryTimeout = 10 * time.Second

	// callbackPath is the fixed loopback redirect path of every login; only
	// the port varies.
	callbackPath = "/oidc/callback"
)

// Service implements connectapi.LoginBackend. One instance serves every face
// that mounts the Login service; it is safe for concurrent use.
type Service struct {
	cfg          *Config
	resolver     users.OIDCResolver
	minter       TokenMinter
	clientSecret string
	httpClient   *http.Client

	mu      sync.Mutex
	idp     *oidc.Provider // nil until one lazy discovery succeeds; retried after failure
	pending map[string]*pendingLogin

	// now is injectable for tests; every expiry and timestamp decision goes
	// through it.
	now func() time.Time
}

// New builds the engine. cfg is already validated (LoadConfig); the resolver
// maps verified claims to a rafiki user and the minter mints the session
// token. The client secret is resolved from cfg.ClientSecretEnv once, at
// construction — LoadConfig validated the env var as set.
func New(cfg *Config, resolver users.OIDCResolver, minter TokenMinter) *Service {
	return &Service{
		cfg:          cfg,
		resolver:     resolver,
		minter:       minter,
		clientSecret: os.Getenv(cfg.ClientSecretEnv),
		httpClient:   http.DefaultClient,
		pending:      make(map[string]*pendingLogin),
		now:          time.Now,
	}
}

// idpProvider returns the cached IdP metadata, discovering it lazily on first
// use. Discovery is NEVER done at daemon startup: an IdP that is down when
// the daemon boots must not break startup, only the login itself. A failed
// attempt is not cached — the next Begin retries.
func (s *Service) idpProvider(ctx context.Context) (*oidc.Provider, error) {
	s.mu.Lock()
	p := s.idp
	s.mu.Unlock()
	if p != nil {
		return p, nil
	}
	dctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	np, err := oidc.NewProvider(oidc.ClientContext(dctx, s.httpClient), s.cfg.Issuer)
	if err != nil {
		slog.Warn("oidclogin: identity provider discovery failed", "issuer", s.cfg.Issuer, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("identity provider unreachable"))
	}
	s.mu.Lock()
	s.idp = np
	s.mu.Unlock()
	return np, nil
}

// oauthConfig builds the client for one round trip against redirectURI. The
// redirect URI MUST be identical in the authorize URL and the exchange —
// providers treat a mismatch as a code-replay signal and refuse.
func (s *Service) oauthConfig(p *oidc.Provider, redirectURI string) oauth2.Config {
	return oauth2.Config{
		ClientID:     s.cfg.ClientID,
		ClientSecret: s.clientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       s.cfg.Scopes,
	}
}

// Begin starts a login: it returns an authorize URL the caller opens in a
// browser and the loopback port the browser will be redirected to.
//
// redirectPort is the loopback port the CLIENT has bound (or is about to
// bind). cfg.RedirectPort overrides it when set — the daemon pins the port
// and the client must rebind there, which is why the response carries
// redirect_port rather than the client trusting its own request. The port
// used is the one actually baked into redirectURI, and that is what the
// response reports.
func (s *Service) Begin(ctx context.Context, redirectPort uint32, clientHost string) (*rafikiv1.BeginLoginResponse, error) {
	if redirectPort == 0 || redirectPort > 65535 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("redirect_port must be between 1 and 65535"))
	}
	// Discovery before the pending check: a failed Begin must not burn a
	// slot, and the cap governs live logins, not discovery attempts.
	p, err := s.idpProvider(ctx)
	if err != nil {
		return nil, err
	}

	port := uint32(s.cfg.RedirectPort)
	if port == 0 {
		port = redirectPort
	}
	// An IP literal, never localhost: the browser resolves localhost through
	// its own stack (proxies, ::1 first) and the redirect must land on the
	// loopback listener the client actually bound.
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, callbackPath)

	loginID, err := randomToken()
	if err != nil {
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	host := sanitizeClientHost(clientHost)

	s.mu.Lock()
	now := s.now()
	// Sweep expired entries on every Begin — the map is bounded only by this
	// plus the cap, and this endpoint is unauthenticated.
	live := 0
	for id, pend := range s.pending {
		if now.After(pend.expires) {
			delete(s.pending, id)
		} else {
			live++
		}
	}
	if live >= maxPendingLogins {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeResourceExhausted,
			errors.New("too many logins in progress; wait a minute and try again"))
	}
	s.pending[loginID] = &pendingLogin{
		state:       state,
		nonce:       nonce,
		verifier:    verifier,
		redirectURI: redirectURI,
		clientHost:  host,
		expires:     now.Add(pendingTTL),
	}
	s.mu.Unlock()

	oc := s.oauthConfig(p, redirectURI)
	authURL := oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	return &rafikiv1.BeginLoginResponse{LoginId: loginID, AuthorizeUrl: authURL, RedirectPort: port}, nil
}

// Complete finishes a login from the callback the IdP redirected to. The
// pending entry is removed FIRST — a login is single-use, success or failure,
// so a replayed callback can never mint a second token.
func (s *Service) Complete(ctx context.Context, loginID, callbackQuery string) (*rafikiv1.CompleteLoginResponse, error) {
	now := s.now()
	s.mu.Lock()
	pend, ok := s.pending[loginID]
	if ok {
		delete(s.pending, loginID)
	}
	s.mu.Unlock()
	if !ok || now.After(pend.expires) {
		return nil, connect.NewError(connect.CodeNotFound,
			errors.New("login expired or already used; run 'rafiki login' again"))
	}

	q, err := url.ParseQuery(callbackQuery)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("malformed callback query"))
	}
	// The IdP's own refusal, verbatim (truncated). It is an answer, not an
	// outage: the caller declined or the IdP refused the grant.
	if e := q.Get("error"); e != "" {
		msg := "identity provider refused: " + truncate(e, 200)
		if desc := q.Get("error_description"); desc != "" {
			msg += ": " + truncate(desc, 200)
		}
		return nil, s.refuse(msg)
	}
	// Constant-time: state is the anti-CSRF binding between Begin and
	// Complete; a timing-probeable compare would let an attacker on the
	// loopback link iteratively forge it.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(pend.state)) != 1 {
		return nil, s.refuse("state mismatch")
	}
	code := q.Get("code")
	if code == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("callback is missing the authorization code"))
	}

	p, err := s.idpProvider(ctx)
	if err != nil {
		return nil, err
	}
	oc := s.oauthConfig(p, pend.redirectURI)
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(pend.verifier))
	if err != nil {
		slog.Warn("oidclogin: token exchange failed", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("identity provider unreachable"))
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("identity provider sent no id_token"))
	}
	// Verify checks signature, iss, aud and exp. The nonce is checked right
	// after — go-oidc surfaces it but deliberately leaves replay-binding to
	// the caller.
	idToken, err := p.Verifier(&oidc.Config{ClientID: s.cfg.ClientID}).
		Verify(oidc.ClientContext(ctx, s.httpClient), rawIDToken)
	if err != nil {
		slog.Warn("oidclogin: ID token verification failed", "error", err)
		return nil, s.refuse("invalid ID token from identity provider")
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(pend.nonce)) != 1 {
		return nil, s.refuse("nonce mismatch")
	}

	var claims struct {
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("identity provider sent unreadable claims"))
	}

	email, err := users.NormalizeEmail(claims.Email)
	if err != nil || email == "" {
		return nil, s.refuse("identity provider sent no usable email claim")
	}
	domain := email[strings.LastIndexByte(email, '@')+1:]
	if !s.domainAllowed(domain) {
		return nil, s.refuse(fmt.Sprintf("email domain %s is not accepted by this daemon", domain))
	}
	// email_verified: honor false; absent means the IdP does not assert
	// verification and is accepted. Any shape other than JSON booleans or
	// the strings "true"/"false" is treated as absent — IdPs disagree on the
	// encoding and a number or a foreign string must not silently refuse
	// every user of an otherwise healthy IdP.
	if verified, present := parseEmailVerified(claims.EmailVerified); present && !verified {
		return nil, s.refuse("email " + email + " is not verified at the identity provider")
	}

	u, err := s.resolver.ResolveOIDC(ctx, users.OIDCClaims{
		Issuer:  idToken.Issuer,
		Subject: idToken.Subject,
		Email:   email,
	})
	switch {
	case err == nil:
	case errors.Is(err, users.ErrOIDCNoUser):
		return nil, s.refuse(fmt.Sprintf(
			"no rafiki user for %s; ask an admin to run 'rafikid user create --email %s' or 'rafikid user update'",
			email, email))
	case errors.Is(err, users.ErrOIDCConflict):
		return nil, s.refuse("this email is already linked to a different account at this identity provider")
	default:
		// Not a sentinel — the store could not check. An outage, never a
		// refusal: surfacing it as "no user" would tell a real user their
		// account does not exist because a database blipped.
		slog.Warn("oidclogin: identity resolution failed", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("could not resolve the login against the user database"))
	}

	minted, secret, err := s.minter.MintToken(ctx, u.ID, users.NewToken{
		Name:   fmt.Sprintf("login %s %s", pend.clientHost, now.UTC().Format("2006-01-02")),
		Origin: users.OriginOIDC,
		TTL:    time.Duration(s.cfg.SessionTTL),
	})
	switch {
	case err == nil:
	case errors.Is(err, users.ErrNotFound):
		return nil, s.refuse("rafiki user no longer exists")
	default:
		slog.Warn("oidclogin: token mint failed", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("could not mint a session token"))
	}

	var expiresAt *timestamppb.Timestamp
	if minted.ExpiresAt != nil {
		expiresAt = timestamppb.New(*minted.ExpiresAt)
	}
	slog.Info("oidclogin: login succeeded",
		"username", minted.Username, "token_id", minted.ID, "client_host", pend.clientHost)
	return &rafikiv1.CompleteLoginResponse{Token: secret, Username: minted.Username, ExpiresAt: expiresAt}, nil
}

// domainAllowed compares for EXACT equality: a suffix or subdomain match
// would let anyone who controls evil.graveland.dev (or graveland.dev.evil.com)
// register an account at the configured domain and log in.
func (s *Service) domainAllowed(domain string) bool {
	for _, d := range s.cfg.EmailDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// refuse logs the reason and returns it as the wire error. The reason never
// contains the authorization code or the minted token.
func (s *Service) refuse(reason string) error {
	slog.Warn("oidclogin: login refused", "reason", reason)
	return connect.NewError(connect.CodePermissionDenied, errors.New(reason))
}

// parseEmailVerified decodes email_verified: JSON true/false or the strings
// "true"/"false". present is false for an absent claim and for any other
// shape, both of which mean "not asserted". JSON null decodes into *bool as
// nil WITHOUT error — decoded into a plain bool it would read as a present
// false and refuse every login of an IdP that emits it, so the pointer is
// what keeps null on the absent path.
func parseEmailVerified(raw json.RawMessage) (verified, present bool) {
	if len(raw) == 0 {
		return false, false
	}
	var bp *bool
	if err := json.Unmarshal(raw, &bp); err == nil {
		if bp == nil {
			return false, false
		}
		return *bp, true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch str {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// randomToken is 32 random bytes, base64url — a login id, a state or a nonce.
func randomToken() (string, error) {
	b := make([]byte, entropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate login entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sanitizeClientHost strips control characters and truncates to 64 bytes —
// clientHost becomes part of a token name and a log line, both of which a
// hostile value could otherwise poison (newlines in a log, terminal escapes).
func sanitizeClientHost(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxClientHostLen {
		s = s[:maxClientHostLen]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// truncate caps s to n bytes on a rune boundary.
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
