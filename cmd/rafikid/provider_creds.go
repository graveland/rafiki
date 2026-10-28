// SPDX-License-Identifier: Apache-2.0

package main

// Per-owner provider credentials: the daemon side of a ChatGPT subscription
// login.
//
// A provider of kind openai-responses-chatgpt is authenticated by the OWNING
// USER's login, never by an environment key and never by a daemon-wide
// fallback (pkg/providers refuses api_key_env on that kind for exactly this
// reason). This file is the whole of that machinery on the daemon side:
//
//   - logins: BeginProviderLogin / CompleteProviderLogin / DeleteProviderCredential,
//     the ProviderLoginBackend the Connect control plane calls. The tokens a
//     login produces never leave the daemon; the RPC answers with a URL, then
//     with the provider and account id.
//   - the access-token cache the ChatGPT senders draw from, keyed by
//     (owner, provider) and coalesced through a singleflight so 20 children of
//     one owner make one upstream refresh, not 20.
//   - the idle sweep that refreshes a credential nobody has used for 8 days,
//     which is codex's own cadence and the reason a user who leaves children
//     idle for weeks does not come back to an expired refresh token.
//
// Three rules shape the refresh path, and each exists because breaking it
// loses a credential:
//
//  1. Refresh tokens are single use. The issuer rotates on every use, so a
//     token that came back MUST be stored even when the response that carried
//     it was an error (chatgptauth.Refresh's contract).
//  2. A refresh runs on the DAEMON's lifetime context with a deadline, never
//     on the turn's context: a cancelled turn must not abandon a rotated token
//     between the issuer's answer and the store's write, while a hung upstream
//     must not leak a goroutine forever.
//  3. A save that is not a login must never go through Store.Put. Put clears
//     dead_at, so a background retry through Put would resurrect a credential a
//     logout deleted. Store.Refresh writes the token columns and leaves the
//     tombstone alone, which is what makes "retry the save" and "the row is
//     gone" both safe.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"go.graveland.dev/rafiki/pkg/chatgptauth"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providercreds"
	"go.graveland.dev/rafiki/pkg/providers"
)

const (
	// accessWindow is how much of an access token's life must remain for the
	// cache to answer without refreshing. Codex uses the same five minutes: a
	// request that starts with less than this left can still be in flight when
	// the token dies.
	accessWindow = 5 * time.Minute

	// refreshTimeout bounds one refresh, and one revoke. It is a deadline on
	// the daemon's lifetime context, not the caller's: the caller may give up
	// (its turn was cancelled) while the refresh keeps running to the store.
	refreshTimeout = 2 * time.Minute

	// sweepInterval and sweepAge are the idle sweep: once an hour, refresh
	// every credential last refreshed more than 8 days ago. Codex refreshes on
	// the same cadence, so a token that is never used still never expires.
	sweepInterval = time.Hour
	sweepAge      = 8 * 24 * time.Hour

	// loginBindingTTL is how long a begun login can be completed. It is
	// deliberately SHORTER than chatgptauth's own pending TTL (10 minutes): the
	// binding is what answers NotFound for another owner's login id, so it must
	// be the first to forget, never the one still holding an id the engine has
	// already expired.
	loginBindingTTL = 9 * time.Minute

	// persistTries and the delay bounds are the ErrNotPersisted retry: 1s
	// doubling to 1 minute, at most 10 attempts.
	persistTries     = 10
	persistRetryBase = time.Second
	persistRetryMax  = time.Minute

	// chatGPTAccountHeader is where the account id the login belongs to rides.
	// The ChatGPT backend wants it beside the bearer, and the credential store
	// (which knows it) is the only thing that has to.
	chatGPTAccountHeader = "chatgpt-account-id"
)

// credKey names one owner's credential.
type credKey struct {
	owner    string
	provider string
}

// cachedAccess is one access token as the daemon holds it in memory.
type cachedAccess struct {
	token     string
	accountID string
	expiry    time.Time
}

// loginBinding is which owner began a login id. chatgptauth binds the id to its
// owner as well, but its refusal is an unexported error the daemon cannot
// classify, so the binding lives here too: this map is what lets an id belonging
// to another owner answer the SAME NotFound as an unknown one.
type loginBinding struct {
	owner    string
	provider string
	expires  time.Time
}

// providerCreds owns every per-user provider credential in this daemon.
type providerCreds struct {
	store providercreds.Store // nil when RAFIKI_SECRETS_KEY or the DB is unavailable
	auth  *chatgptauth.Engine
	why   string // non-empty = unavailable, the reason shown to users

	// providers is the live registry, read per login so a reloaded set is seen
	// without rebuilding this struct. Nil in a test that only exercises tokens.
	providers func() *providers.Set

	// mu guards cache and logins. They share one lock because both are small
	// maps read on paths that already take it once.
	mu    sync.Mutex
	cache map[credKey]cachedAccess
	// logins is the owner binding of every login in flight, pruned on Begin.
	logins map[string]loginBinding

	sf singleflight.Group // coalesces concurrent refreshes per (owner, provider)

	// bg is the daemon's lifetime context. Refreshes, the sweep and the
	// post-logout revoke run on it, never on the caller's ctx.
	bg context.Context

	// retryDelay is the first delay of the ErrNotPersisted retry; zero means
	// persistRetryBase. A test sets it so the retry is not a one-second sleep.
	retryDelay time.Duration
}

// newProviderCreds builds the daemon's credential machinery and starts its
// idle sweep. store is nil and why non-empty when the key or the database is
// missing: the daemon then runs with this capability unavailable and the reason
// in every refusal, which is the documented degradation (never a plaintext
// fallback, never a startup failure).
func newProviderCreds(bg context.Context, store providercreds.Store, auth *chatgptauth.Engine, why string, set func() *providers.Set) *providerCreds {
	if bg == nil {
		bg = context.Background()
	}
	p := &providerCreds{
		store:     store,
		auth:      auth,
		why:       why,
		providers: set,
		cache:     make(map[credKey]cachedAccess),
		logins:    make(map[string]loginBinding),
		bg:        bg,
	}
	if store != nil && auth != nil {
		go p.sweepLoop()
	}
	return p
}

// Available reports whether provider credentials can be used at all, and why
// not when they cannot. The reason is safe to show a user: it names the missing
// setting, not a token.
func (p *providerCreds) Available() (bool, string) {
	if p == nil {
		return false, "provider credentials are not configured"
	}
	if p.why != "" {
		return false, p.why
	}
	if p.store == nil {
		return false, "provider credentials are unavailable: the daemon has no credential store"
	}
	if p.auth == nil {
		return false, "provider credentials are unavailable: no ChatGPT login engine"
	}
	return true, ""
}

// deadErr is the one thing a user is told when a credential cannot be used. It
// names the fix, because the alternative is a 401 from a request that looked
// configured.
func deadErr(provider string) error {
	return fmt.Errorf("ChatGPT login for %s expired or missing; run: rafiki providers login %s", provider, provider)
}

// errRefreshReused rolls the refresh transaction back so the caller can re-read
// the row ONCE. It never reaches a user: the re-read either adopts another
// holder's rotation or marks the credential dead.
var errRefreshReused = errors.New("provider creds: refresh token reused; re-read the stored credential")

// errUnknownProviderLogin is the single answer for a login id that is absent,
// expired, or was begun by a different owner. Naming which one would let a
// caller learn that another user's login exists.
var errUnknownProviderLogin = &connectapi.ControllerError{
	Code:    protocol.ErrNotFound,
	Message: "unknown or expired provider login; start the login again",
}

// ─── Token source ────────────────────────────────────────────────────────────

// providerTokenSource is one (owner, provider) view of the cache, and what a
// ChatGPT sender holds. It carries no state of its own: everything lives in the
// daemon-wide providerCreds, so two senders for the same owner share one cache
// and one singleflight.
type providerTokenSource struct {
	creds    *providerCreds
	owner    string
	provider string
}

var _ llm.TokenSource = providerTokenSource{}

// TokenSourceFor returns the TokenSource for one owner's provider credential.
func (p *providerCreds) TokenSourceFor(owner, provider string) llm.TokenSource {
	return providerTokenSource{creds: p, owner: owner, provider: provider}
}

func (s providerTokenSource) Token(ctx context.Context, force bool) (string, http.Header, error) {
	return s.creds.token(ctx, s.owner, s.provider, force)
}

// token answers one request's credential.
//
// The cache answers anything with more than accessWindow of life left. Anything
// else refreshes through the singleflight, and the CALLER's context only governs
// how long the caller waits: the refresh itself runs on p.bg, so a cancelled
// turn returns promptly while the rotated token still reaches the store. DoChan
// (never Do) is what makes that possible: Do blocks with no ctx.Done arm.
func (p *providerCreds) token(ctx context.Context, owner, provider string, force bool) (string, http.Header, error) {
	if ok, why := p.Available(); !ok {
		return "", nil, errors.New(why)
	}
	key := credKey{owner: owner, provider: provider}
	if !force {
		if tok, ok := p.cached(key); ok {
			return tok.token, accountHeader(tok.accountID), nil
		}
	}
	reason := chatgptauth.ReasonProactiveExpiry
	if force {
		reason = chatgptauth.ReasonUnauthorizedRecovery
	}
	ch := p.sf.DoChan(owner+"|"+provider, func() (any, error) {
		return p.refresh(owner, provider, force, reason)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return "", nil, r.Err
		}
		tok, ok := r.Val.(cachedAccess)
		if !ok || tok.token == "" {
			return "", nil, deadErr(provider)
		}
		return tok.token, accountHeader(tok.accountID), nil
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

// accountHeader is the extra header the ChatGPT backend wants, or nil when the
// login carried no account id.
func accountHeader(accountID string) http.Header {
	if accountID == "" {
		return nil
	}
	h := http.Header{}
	h.Set(chatGPTAccountHeader, accountID)
	return h
}

// HasCredential reports whether this owner has a live credential for the
// provider. A dead one counts as no credential: the owner has to log in again,
// and a spawn refused for it must say so. An unavailable store answers false
// rather than an error: Available is the gate that distinguishes the two, and a
// caller that skipped it would otherwise report a daemon fault as a missing
// login.
func (p *providerCreds) HasCredential(ctx context.Context, owner, provider string) (bool, error) {
	if p == nil || p.store == nil {
		return false, nil
	}
	cred, err := p.store.Get(ctx, owner, provider)
	switch {
	case errors.Is(err, providercreds.ErrNotFound), errors.Is(err, providercreds.ErrDead):
		return false, nil
	case err != nil:
		return false, err
	}
	return !cred.Dead && cred.RefreshToken != "", nil
}

// refresh performs one refresh, under the store's row lock, and returns the
// access token the caller should use.
//
// force means "the caller was refused with this token, so refresh even if it
// looks healthy": the 401 recovery path. reason is what the issuer is told; the
// sweep passes scheduled, everything from a turn passes the reason its force
// implies.
func (p *providerCreds) refresh(owner, provider string, force bool, reason chatgptauth.RefreshReason) (cachedAccess, error) {
	if ok, why := p.Available(); !ok {
		return cachedAccess{}, errors.New(why)
	}
	key := credKey{owner: owner, provider: provider}
	// The deadline is on the daemon's lifetime context: nothing else about this
	// call can outlive the process, and nothing about it dies with a turn.
	ctx, cancel := context.WithTimeout(p.bg, refreshTimeout)
	defer cancel()

	// sent is the refresh token this attempt spends, and rotated is a NEW one
	// the issuer handed back alongside an error. Both are captured from the
	// callback for use after the transaction ends: a rotated token must be
	// saved even though the response it came with was a failure.
	var sent, rotated string

	next, err := p.store.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		sent = cur.RefreshToken
		if !force && usableAccess(cur.AccessToken, cur.AccessExpiry) {
			// Another holder (another daemon, another goroutine) refreshed
			// already: the row is the newer answer, and rewriting it would hide
			// a credential going stale.
			return cur, nil
		}
		tokens, rerr := p.auth.Refresh(ctx, cur.RefreshToken, reason)
		switch {
		case rerr == nil:
			if tokens.AccountID != "" && cur.AccountID != "" && tokens.AccountID != cur.AccountID {
				// Never switch accounts silently: the owner's spend and their
				// conversation history belong to the account they logged in as.
				return cur, fmt.Errorf("%w: refreshed login belongs to a different ChatGPT account", providercreds.ErrDead)
			}
			out := cur
			out.AccessToken = tokens.AccessToken
			out.AccessExpiry = tokens.Expiry
			if tokens.RefreshToken != "" {
				out.RefreshToken = tokens.RefreshToken
			}
			if tokens.AccountID != "" {
				out.AccountID = tokens.AccountID
			}
			return out, nil
		case errors.Is(rerr, chatgptauth.ErrRefreshReused):
			// The issuer says this token was already spent. It may have been
			// another holder of this same row, so roll back and re-read once.
			return cur, errRefreshReused
		case errors.Is(rerr, chatgptauth.ErrRefreshRejected):
			return cur, fmt.Errorf("%w: %v", providercreds.ErrDead, rerr)
		}
		// Transient: the row is untouched and the turn's own retry handles it.
		// The issuer may still have rotated the refresh token on the way, and
		// the old one is already dead upstream, so the new one is captured for
		// a save outside this transaction.
		if tokens.RefreshToken != "" && tokens.RefreshToken != cur.RefreshToken {
			rotated = tokens.RefreshToken
		}
		return cur, rerr
	})

	switch {
	case errors.Is(err, errRefreshReused):
		return p.rereadAfterReuse(ctx, key, sent)
	case errors.Is(err, providercreds.ErrNotPersisted):
		if stored, ok := usableCredential(next); ok {
			// The upstream has already invalidated the old refresh token, so
			// the new one must not be dropped: turns keep working from memory
			// while the write is retried in the background.
			p.cachePut(key, accessFrom(stored))
			slog.Error("provider credential refresh not persisted",
				"owner", owner, "provider", provider, "error", err)
			go p.persistEventually(owner, provider, stored, sent)
			return accessFrom(stored), nil
		}
		return cachedAccess{}, deadErr(provider)
	case errors.Is(err, providercreds.ErrDead):
		// The stored reason is worth a log line even though the user gets the
		// actionable form: "refused" and "this login is now another account" send
		// an operator to different places.
		slog.Warn("provider credential is no longer usable",
			"owner", owner, "provider", provider, "error", err)
		p.evict(key)
		return cachedAccess{}, deadErr(provider)
	case errors.Is(err, providercreds.ErrNotFound):
		p.evict(key)
		return cachedAccess{}, deadErr(provider)
	case err != nil:
		if rotated != "" {
			p.persistRotated(owner, provider, sent, rotated)
		}
		return cachedAccess{}, err
	}

	stored, ok := usableCredential(next)
	if !ok {
		p.evict(key)
		return cachedAccess{}, deadErr(provider)
	}
	p.cachePut(key, accessFrom(stored))
	return accessFrom(stored), nil
}

// usableCredential rejects a stored credential that cannot serve a request.
func usableCredential(c providercreds.Credential) (providercreds.Credential, bool) {
	if c.AccessToken == "" || c.Dead {
		return c, false
	}
	return c, true
}

// rereadAfterReuse is the second and final half of the refresh_token_reused
// path: read the row once more and adopt whatever it holds. The token was
// already spent when we sent it, so either another holder of this row rotated
// it (their token is the working one) or this credential is dead. There is no
// third attempt.
func (p *providerCreds) rereadAfterReuse(ctx context.Context, key credKey, sent string) (cachedAccess, error) {
	next, err := p.store.Refresh(ctx, key.owner, key.provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		if cur.RefreshToken != sent {
			return cur, nil
		}
		return cur, fmt.Errorf("%w: %v", providercreds.ErrDead, chatgptauth.ErrRefreshReused)
	})
	switch {
	case errors.Is(err, providercreds.ErrDead), errors.Is(err, providercreds.ErrNotFound):
		p.evict(key)
		return cachedAccess{}, deadErr(key.provider)
	case err != nil:
		return cachedAccess{}, err
	}
	stored, ok := usableCredential(next)
	if !ok {
		p.evict(key)
		return cachedAccess{}, deadErr(key.provider)
	}
	p.cachePut(key, accessFrom(stored))
	return accessFrom(stored), nil
}

// persistRotated saves a refresh token the issuer rotated alongside an error.
// The old token is already dead upstream, so dropping the new one kills a
// credential that still works.
//
// It goes through Store.Refresh rather than Put: Put clears dead_at, and this is
// a save, not a login.
func (p *providerCreds) persistRotated(owner, provider, sent, rotated string) {
	if p.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(p.bg, refreshTimeout)
	defer cancel()
	_, err := p.store.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		if cur.RefreshToken != sent {
			// Someone already stored something newer; theirs wins.
			return cur, nil
		}
		out := cur
		out.RefreshToken = rotated
		return out, nil
	})
	if err != nil {
		slog.Error("provider credential rotated token not persisted",
			"owner", owner, "provider", provider, "error", err)
	}
}

// persistEventually retries the write a refresh could not persist.
//
// Put is deliberately not used: it clears dead_at, so a retry through Put would
// resurrect a row a logout deleted or a refusal tombstoned. Store.Refresh writes
// the token columns only, and answers ErrNotFound when the row is gone, which is
// the answer that stops the retry rather than restarting the credential.
func (p *providerCreds) persistEventually(owner, provider string, want providercreds.Credential, sent string) {
	delay := p.retryDelay
	if delay <= 0 {
		delay = persistRetryBase
	}
	for attempt := 1; attempt <= persistTries; attempt++ {
		select {
		case <-p.bg.Done():
			slog.Error("provider credential refresh not persisted; daemon shutting down",
				"owner", owner, "provider", provider)
			return
		case <-time.After(delay):
		}
		ctx, cancel := context.WithTimeout(p.bg, refreshTimeout)
		wrote := false
		_, err := p.store.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
			if cur.RefreshToken != sent {
				return cur, nil
			}
			wrote = true
			return want, nil
		})
		cancel()
		switch {
		case err == nil && wrote:
			slog.Info("provider credential refresh persisted after retry",
				"owner", owner, "provider", provider, "attempts", attempt)
			return
		case err == nil:
			slog.Info("provider credential already holds a newer token; nothing to save",
				"owner", owner, "provider", provider)
			return
		case errors.Is(err, providercreds.ErrNotFound):
			// A logout deleted the row while this retried. It stays deleted.
			slog.Info("provider credential row is gone; refreshed token not persisted",
				"owner", owner, "provider", provider)
			return
		case errors.Is(err, providercreds.ErrDead):
			slog.Info("provider credential is dead; refreshed token not persisted",
				"owner", owner, "provider", provider, "error", err)
			return
		}
		slog.Warn("provider credential refresh retry failed",
			"owner", owner, "provider", provider, "attempt", attempt, "error", err)
		delay *= 2
		if delay > persistRetryMax {
			delay = persistRetryMax
		}
	}
	slog.Error("provider credential refresh not persisted; giving up",
		"owner", owner, "provider", provider, "tries", persistTries)
}

// ─── Idle sweep ──────────────────────────────────────────────────────────────

// sweepLoop refreshes idle credentials once an hour until the daemon stops.
func (p *providerCreds) sweepLoop() {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-p.bg.Done():
			return
		case <-t.C:
			p.sweepOnce(p.bg)
		}
	}
}

// sweepOnce refreshes every credential ListStale reports. It goes through the
// same singleflight as a turn, so a sweep and a request for one owner coalesce
// instead of spending the same single-use token twice. Failures are logged and
// the sweep moves on: the refusals are recorded where they happen (dead_at).
func (p *providerCreds) sweepOnce(ctx context.Context) {
	if p.store == nil {
		return
	}
	keys, err := p.store.ListStale(ctx, sweepAge)
	if err != nil {
		slog.Warn("provider credential sweep: list stale failed", "error", err)
		return
	}
	for _, k := range keys {
		owner, provider := k.Owner, k.Provider
		ch := p.sf.DoChan(owner+"|"+provider, func() (any, error) {
			return p.refresh(owner, provider, true, chatgptauth.ReasonScheduled)
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				slog.Warn("provider credential sweep: refresh failed",
					"owner", owner, "provider", provider, "error", r.Err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// ─── Cache ───────────────────────────────────────────────────────────────────

// cached returns the in-memory access token when it can serve a request.
func (p *providerCreds) cached(key credKey) (cachedAccess, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tok, ok := p.cache[key]
	if !ok || !usableAccess(tok.token, tok.expiry) {
		return cachedAccess{}, false
	}
	return tok, true
}

func (p *providerCreds) cachePut(key credKey, tok cachedAccess) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[credKey]cachedAccess)
	}
	p.cache[key] = tok
}

// evict forgets one owner's cached token, after a logout, a refusal, or a
// credential the store no longer holds.
func (p *providerCreds) evict(key credKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, key)
}

// usableAccess reports whether an access token has more than accessWindow of
// life left.
func usableAccess(token string, expiry time.Time) bool {
	return token != "" && expiry.After(time.Now().Add(accessWindow))
}

func accessFrom(c providercreds.Credential) cachedAccess {
	return cachedAccess{token: c.AccessToken, accountID: c.AccountID, expiry: c.AccessExpiry}
}

// ─── Provider login backend ──────────────────────────────────────────────────

var _ connectapi.ProviderLoginBackend = (*providerCreds)(nil)

// loginProvider resolves a provider that authenticates by the owner's login.
// An unknown name and a name whose kind does not use login authentication get
// the SAME InvalidArgument answer: a client has no business learning which
// providers a daemon has configured from an error it cannot act on.
func (p *providerCreds) loginProvider(name string) (providers.Provider, error) {
	if p.providers != nil {
		if set := p.providers(); set != nil {
			if prov, ok := set.Get(name); ok && prov.Kind == providers.KindOpenAIResponsesChatGPT {
				return prov, nil
			}
		}
	}
	return providers.Provider{}, &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: fmt.Sprintf("provider %q does not use login authentication", name),
	}
}

// usesProviderLogins reports whether the registry holds a provider that
// authenticates by a user's login. It gates the startup warning: a daemon whose
// providers.toml has no ChatGPT-kind provider has nothing to warn about.
func usesProviderLogins(set *providers.Set) bool {
	if set == nil {
		return false
	}
	for _, name := range set.Names() {
		if p, ok := set.Get(name); ok && p.Kind == providers.KindOpenAIResponsesChatGPT {
			return true
		}
	}
	return false
}

// BeginProviderLogin starts a browser login for one owner. Tokens never cross
// this seam: the caller gets the URL to open and the loopback parameters to
// listen on.
func (p *providerCreds) BeginProviderLogin(ctx context.Context, ownerUserID, provider string) (connectapi.ProviderLoginBegin, error) {
	if ok, why := p.Available(); !ok {
		return connectapi.ProviderLoginBegin{}, &connectapi.ControllerError{
			Code: protocol.ErrUnavailable, Message: why,
		}
	}
	if _, err := p.loginProvider(provider); err != nil {
		return connectapi.ProviderLoginBegin{}, err
	}
	// Begin itself takes no context: it mints a login id and an authorize URL
	// in memory, and the pending entry has its own TTL.
	loginID, authorizeURL, err := p.auth.Begin(ownerUserID, provider)
	if err != nil {
		// Uncoded on purpose: the cause is chatgptauth's (an entropy failure or
		// the in-flight cap), and its text is not ours to forward. The handler
		// mapper logs it and the wire answers internal.
		return connectapi.ProviderLoginBegin{}, fmt.Errorf("start the %s login: %w", provider, err)
	}
	p.bindLogin(loginID, loginBinding{
		owner:    ownerUserID,
		provider: provider,
		expires:  time.Now().Add(loginBindingTTL),
	})
	return connectapi.ProviderLoginBegin{
		LoginID:       loginID,
		AuthorizeURL:  authorizeURL,
		CallbackPath:  chatgptauth.CallbackPath,
		RedirectPort:  uint32(chatgptauth.CallbackPort),
		RedirectHosts: []string{chatgptauth.CallbackHost},
	}, nil
}

// CompleteProviderLogin finishes a login and stores its credential.
//
// The login id is bound to the owner that began it: a different owner, an
// unknown id and an expired one all answer the same NotFound, so a caller can
// never learn that another user's login exists. The engine applies the same
// rule, but its refusal is unexported, so the binding is kept here too, where it
// can be given the right wire answer.
func (p *providerCreds) CompleteProviderLogin(ctx context.Context, ownerUserID, loginID, callbackQuery string) (string, string, error) {
	if ok, why := p.Available(); !ok {
		return "", "", &connectapi.ControllerError{Code: protocol.ErrUnavailable, Message: why}
	}
	if _, ok := p.takeLogin(ownerUserID, loginID); !ok {
		return "", "", errUnknownProviderLogin
	}
	provider, tokens, err := p.auth.Complete(ctx, ownerUserID, loginID, callbackQuery)
	if err != nil {
		return "", "", err
	}
	if tokens.RefreshToken == "" {
		// An access token alone dies within the hour and cannot be renewed; the
		// daemon would keep a credential that can only fail later.
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: "the issuer returned no refresh token; the login cannot be kept",
		}
	}
	cred := providercreds.Credential{
		OwnerUserID:  ownerUserID,
		Provider:     provider,
		AccountID:    tokens.AccountID,
		RefreshToken: tokens.RefreshToken,
		AccessToken:  tokens.AccessToken,
		AccessExpiry: tokens.Expiry,
		LastRefresh:  time.Now(),
	}
	if err := p.store.Put(ctx, cred); err != nil {
		return "", "", fmt.Errorf("store the %s login: %w", provider, err)
	}
	p.evict(credKey{owner: ownerUserID, provider: provider})
	slog.Info("provider login stored",
		"owner", ownerUserID, "provider", provider, "account_id", tokens.AccountID)
	return provider, tokens.AccountID, nil
}

// DeleteProviderCredential forgets an owner's login and revokes it upstream.
// The revoke is best effort: the row is already gone, and a failure there must
// not look like a failed logout.
func (p *providerCreds) DeleteProviderCredential(ctx context.Context, ownerUserID, provider string) error {
	if ok, why := p.Available(); !ok {
		return &connectapi.ControllerError{Code: protocol.ErrUnavailable, Message: why}
	}
	deleted, err := p.store.Delete(ctx, ownerUserID, provider)
	if errors.Is(err, providercreds.ErrNotFound) {
		return &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("no login is stored for provider %q", provider),
		}
	}
	if err != nil {
		return fmt.Errorf("delete the %s credential: %w", provider, err)
	}
	p.evict(credKey{owner: ownerUserID, provider: provider})
	if deleted.RefreshToken == "" {
		// The row existed but could not be decrypted with the current key: the
		// logout happened, and there is no token to revoke.
		return nil
	}
	// On the daemon's context with a deadline: a stuck issuer must not hang a
	// logout, and a client that disconnects mid-call must not cancel a revoke
	// the daemon already committed to.
	rctx, cancel := context.WithTimeout(p.bg, refreshTimeout)
	defer cancel()
	if err := p.auth.Revoke(rctx, deleted.RefreshToken); err != nil {
		slog.Warn("provider credential revoke failed after logout",
			"owner", ownerUserID, "provider", provider, "error", err)
	}
	return nil
}

// bindLogin records which owner began a login id, pruning expired entries and
// any earlier login for the same (owner, provider): a user who restarts a login
// must not leave an id behind that would still complete.
func (p *providerCreds) bindLogin(loginID string, b loginBinding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logins == nil {
		p.logins = make(map[string]loginBinding)
	}
	now := time.Now()
	for id, rec := range p.logins {
		if now.After(rec.expires) || (rec.owner == b.owner && rec.provider == b.provider) {
			delete(p.logins, id)
		}
	}
	p.logins[loginID] = b
}

// takeLogin consumes an owner's login binding. A binding that is absent,
// expired or belongs to a DIFFERENT owner answers false, and the other owner's
// binding is left in place: knowing an id buys nobody the ability to cancel
// someone else's login.
func (p *providerCreds) takeLogin(owner, loginID string) (loginBinding, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.logins[loginID]
	if !ok {
		return loginBinding{}, false
	}
	if time.Now().After(rec.expires) {
		delete(p.logins, loginID)
		return loginBinding{}, false
	}
	if rec.owner != owner {
		return loginBinding{}, false
	}
	delete(p.logins, loginID)
	return rec, true
}
