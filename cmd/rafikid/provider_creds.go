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
// Five rules shape the refresh path, and each exists because breaking it loses
// a credential:
//
//  1. Refresh tokens are single use. The issuer rotates on every use, so a
//     token that came back MUST be stored even when the response that carried
//     it was an error (chatgptauth.Refresh's contract).
//  2. A rotation the database did not accept is HANDED OFF, not dropped: it
//     lives in `pending` until a background save lands it, and while it is there
//     no caller may spend the token it already replaced (that comes back
//     refresh_token_reused and tombstones a login that works).
//  3. A refresh runs on the DAEMON's lifetime context with a deadline, never
//     on the turn's context: a cancelled turn must not abandon a rotated token
//     between the issuer's answer and the store's write, while a hung upstream
//     must not leak a goroutine forever.
//  4. A save that is not a login must never go through Store.Put. Put clears
//     dead_at, so a background retry through Put would resurrect a credential a
//     logout deleted. Store.Refresh writes the token columns and leaves the
//     tombstone alone, which is what makes "retry the save" and "the row is
//     gone" both safe.
//  5. Shutdown drains every flight and save (Wait) before the pool closes, and
//     the saves' final writes ignore the cancelled daemon context
//     (context.WithoutCancel), because a rotation that lands during shutdown is
//     exactly the one most likely to be lost.

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

	// redactedToken is what a token field prints as. Nothing here ever renders
	// one: a token reaching a log line, an error message or a panic trace is a
	// credential leak.
	redactedToken = "<redacted>"
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

// String redacts the access token, for the same reason
// providercreds.Credential does: %v on a struct holding a live secret is a
// leak, and fmt prefers Stringer to the struct's own rendering.
func (c cachedAccess) String() string {
	return fmt.Sprintf("providerCreds.cachedAccess{account:%s access:%s expiry:%s}",
		c.accountID, redactedToken, c.expiry.UTC().Format(time.RFC3339))
}

// LogValue keeps the token out of structured logs, so slog.Any on one logs a
// redacted group rather than the live secret. It satisfies slog.LogValuer.
func (c cachedAccess) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("account", c.accountID),
		slog.String("access_token", redactedToken),
		slog.Time("access_expiry", c.expiry),
	)
}

// pendingRotation is a refresh the issuer already completed but the database did
// not accept: sent is the refresh token that was spent, want is the credential
// the issuer handed back in its place.
//
// It is the design's "never lose a rotated token" rule made concrete. While it
// is set, a refresh that finds the row STILL holding sent adopts want instead of
// spending sent a second time: the issuer would answer refresh_token_reused, the
// re-read would find the same spent token, and the credential would be
// tombstoned while the working token sat in the retry goroutine.
type pendingRotation struct {
	sent string
	want providercreds.Credential
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

	// mu guards cache, gen, pending and logins. They share one lock because
	// each is a small map read on paths that already take it once, and because
	// no path here ever calls the store while holding it (so it is always the
	// innermost lock).
	mu    sync.Mutex
	cache map[credKey]cachedAccess
	// gen is the eviction generation per key. A refresh in flight captures it
	// before it starts and a cachePut is discarded unless it still matches, so
	// a logout or a re-login always beats a refresh that started earlier.
	gen map[credKey]uint64
	// pending holds rotations whose database write has not landed yet.
	pending map[credKey]pendingRotation
	// logins is the owner binding of every login in flight, pruned on Begin.
	logins map[string]loginBinding

	sf singleflight.Group // coalesces concurrent refreshes per (owner, provider)

	// bg is the daemon's lifetime context. Refreshes, the sweep and the
	// post-logout revoke run on it, never on the caller's ctx.
	bg context.Context

	// retryDelay is the first delay of the ErrNotPersisted retry; zero means
	// persistRetryBase. A test sets it so the retry is not a one-second sleep.
	retryDelay time.Duration

	// workMu serializes a new flight against the draining flag Wait sets; wg
	// counts flights, saves and the sweeper so shutdown can drain them before
	// the database pool closes.
	workMu   sync.Mutex
	draining bool
	wg       sync.WaitGroup
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
		gen:       make(map[credKey]uint64),
		pending:   make(map[credKey]pendingRotation),
		logins:    make(map[string]loginBinding),
		bg:        bg,
	}
	if store != nil && auth != nil {
		// Counted before the struct is returned, so a Wait can never race it.
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.sweepLoop()
		}()
	}
	return p
}

// Wait drains every refresh flight, background save and the sweeper. The caller
// cancels the daemon context first, which stops the sweeper and makes each save
// attempt its write immediately, and calls this BEFORE closing the database
// pool: a rotation that landed during shutdown must still reach the row.
func (p *providerCreds) Wait() {
	if p == nil {
		return
	}
	p.workMu.Lock()
	p.draining = true
	p.workMu.Unlock()
	p.wg.Wait()
}

// startWork registers one refresh flight. It refuses once Wait has begun, so no
// work is added to the WaitGroup after the drain started.
func (p *providerCreds) startWork() bool {
	p.workMu.Lock()
	defer p.workMu.Unlock()
	if p.draining {
		return false
	}
	p.wg.Add(1)
	return true
}

// addSave registers one background save. Unlike startWork it is allowed while
// draining: a save is the last chance for a rotation that already happened, and
// Wait waits for it. It can only be called from work that is itself counted (a
// flight, or another save), so the counter is never zero when it runs.
func (p *providerCreds) addSave() {
	p.wg.Add(1)
}

// errProviderCredsDraining is what a flight answers once shutdown has begun. A
// turn at that point is failing anyway; the point is that no new upstream
// refresh starts while the pool is being drained.
var errProviderCredsDraining = errors.New("provider credentials: the daemon is shutting down")

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

// flightKey separates a 401 recovery from an ordinary refresh. A forced caller
// that joined an ordinary flight would be handed the very token the server just
// refused, which is a guaranteed second 401 rather than a recovery. The sweep
// shares the recovery key on purpose: both want an upstream refresh.
func flightKey(owner, provider string, force bool) string {
	if force {
		return owner + "|" + provider + "|recovery"
	}
	return owner + "|" + provider + "|refresh"
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
	ch := p.sf.DoChan(flightKey(owner, provider, force), func() (any, error) {
		if !p.startWork() {
			return nil, errProviderCredsDraining
		}
		defer p.wg.Done()
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
	// The generation is read BEFORE the store work: anything that forgets this
	// credential while the refresh runs (a logout, a re-login) bumps it, and the
	// cache write at the end is then discarded.
	gen := p.generation(key)
	// The deadline is on the daemon's lifetime context: nothing else about this
	// call can outlive the process, and nothing about it dies with a turn.
	ctx, cancel := context.WithTimeout(p.bg, refreshTimeout)
	defer cancel()

	// sent is the refresh token this attempt spends; rotated is a credential
	// carrying a NEW refresh token the issuer handed back alongside an error.
	// Both are captured from the callback for use after the transaction ends.
	var (
		sent    string
		rotated providercreds.Credential
	)

	next, err := p.store.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		sent = cur.RefreshToken
		// A rotation this daemon already completed whose write has not landed:
		// while the row still holds the spent token, adopt the rotation. Sending
		// `sent` again would come back refresh_token_reused and tombstone a
		// login that works.
		if pend, ok := p.pendingFor(key); ok && pend.sent == cur.RefreshToken {
			cur = pend.want
			if !force {
				// A routine caller is still holding the access token that worked:
				// keep it, and let this write land the rotation so the NEXT
				// refresh has a usable refresh token.
				return cur, nil
			}
			// A 401 recovery must not be handed back the token that just failed.
			// The sender allows a forced refresh one retry, and a second 401 is
			// terminal, so returning the stale access token turns a recoverable
			// 401 into a failed turn: continue into a real refresh, using the
			// rotation's (valid) refresh token rather than the spent one.
		} else if !force && usableAccess(cur.AccessToken, cur.AccessExpiry) {
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
		// the old one is already dead upstream, so that credential is captured
		// for a hand-off outside this transaction.
		if tokens.RefreshToken != "" && tokens.RefreshToken != cur.RefreshToken {
			rotated = cur
			rotated.RefreshToken = tokens.RefreshToken
		}
		return cur, rerr
	})

	switch {
	case errors.Is(err, errRefreshReused):
		return p.rereadAfterReuse(ctx, key, gen, sent)
	case errors.Is(err, providercreds.ErrNotPersisted):
		if stored, ok := usableCredential(next); ok {
			// The upstream has already invalidated the old refresh token, so
			// the new one must not be dropped: turns keep working from memory
			// while the write is retried, and the pending hand-off stops any
			// other refresh from spending the token it replaced.
			p.cachePut(key, accessFrom(stored), gen)
			slog.Error("provider credential refresh not persisted",
				"owner", owner, "provider", provider, "error", err)
			p.handOffRotation(key, sent, stored)
			return accessFrom(stored), nil
		}
		p.clearPending(key, sent)
		return cachedAccess{}, deadErr(provider)
	case errors.Is(err, providercreds.ErrDead):
		// The stored reason is worth a log line even though the user gets the
		// actionable form: "refused" and "this login is now another account" send
		// an operator to different places.
		slog.Warn("provider credential is no longer usable",
			"owner", owner, "provider", provider, "error", err)
		p.forget(key)
		return cachedAccess{}, deadErr(provider)
	case errors.Is(err, providercreds.ErrNotFound):
		p.forget(key)
		return cachedAccess{}, deadErr(provider)
	case err != nil:
		if rotated.RefreshToken != "" {
			p.handOffRotation(key, sent, rotated)
		}
		return cachedAccess{}, err
	}

	// The rotation is now in the row, so there is nothing left to hand off.
	p.clearPending(key, sent)
	stored, ok := usableCredential(next)
	if !ok {
		p.forget(key)
		return cachedAccess{}, deadErr(provider)
	}
	p.cachePut(key, accessFrom(stored), gen)
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
func (p *providerCreds) rereadAfterReuse(ctx context.Context, key credKey, gen uint64, sent string) (cachedAccess, error) {
	next, err := p.store.Refresh(ctx, key.owner, key.provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		if cur.RefreshToken != sent {
			return cur, nil
		}
		return cur, fmt.Errorf("%w: %v", providercreds.ErrDead, chatgptauth.ErrRefreshReused)
	})
	switch {
	case errors.Is(err, providercreds.ErrDead), errors.Is(err, providercreds.ErrNotFound):
		p.forget(key)
		return cachedAccess{}, deadErr(key.provider)
	case err != nil:
		return cachedAccess{}, err
	}
	stored, ok := usableCredential(next)
	if !ok {
		p.forget(key)
		return cachedAccess{}, deadErr(key.provider)
	}
	p.cachePut(key, accessFrom(stored), gen)
	return accessFrom(stored), nil
}

// handOffRotation records a rotation the database did not accept and starts the
// save that will finish it. The pending entry is the hand-off: while it is set,
// any refresh that finds the row still holding the spent token adopts this
// credential rather than spending that token again.
func (p *providerCreds) handOffRotation(key credKey, sent string, want providercreds.Credential) {
	p.setPending(key, pendingRotation{sent: sent, want: want})
	p.addSave()
	go func() {
		defer p.wg.Done()
		p.persistUntilSaved(key, sent, want)
	}()
}

// persistUntilSaved retries the write a refresh could not persist, 1s doubling
// to a 1 minute cap, at most persistTries times.
//
// Put is deliberately not used: it clears dead_at, so a retry through Put would
// resurrect a row a logout deleted or a refusal tombstoned. Store.Refresh writes
// the token columns only, and answers ErrNotFound when the row is gone, which is
// the answer that stops the retry.
//
// Every attempt writes with context.WithoutCancel: the daemon may be shutting
// down, which is exactly when an unpersisted rotation is most likely, and the
// write must still land (Wait holds shutdown until this returns).
func (p *providerCreds) persistUntilSaved(key credKey, sent string, want providercreds.Credential) {
	delay := p.retryDelay
	if delay <= 0 {
		delay = persistRetryBase
	}
	for attempt := 1; attempt <= persistTries; attempt++ {
		select {
		case <-time.After(delay):
		case <-p.bg.Done():
			// Shutting down. Stop waiting out the backoff and try the write now:
			// the pool is still open, and this is the last chance to keep the
			// rotation.
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(p.bg), refreshTimeout)
		wrote := false
		_, err := p.store.Refresh(ctx, key.owner, key.provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
			if cur.RefreshToken != sent {
				// Somebody else moved the row on: their token is newer than the
				// one this save carries, so writing ours would undo a re-login
				// (or another daemon's rotation).
				return cur, nil
			}
			wrote = true
			return want, nil
		})
		cancel()
		switch {
		case err == nil && wrote:
			p.clearPending(key, sent)
			slog.Info("provider credential refresh persisted after retry",
				"owner", key.owner, "provider", key.provider, "attempts", attempt)
			return
		case err == nil:
			p.clearPending(key, sent)
			slog.Info("provider credential already holds a newer token; nothing to save",
				"owner", key.owner, "provider", key.provider)
			return
		case errors.Is(err, providercreds.ErrNotFound):
			// A logout deleted the row while this retried. It stays deleted.
			p.clearPending(key, sent)
			slog.Info("provider credential row is gone; refreshed token not persisted",
				"owner", key.owner, "provider", key.provider)
			return
		case errors.Is(err, providercreds.ErrDead):
			p.clearPending(key, sent)
			slog.Error("provider credential is dead; the refreshed token was lost",
				"owner", key.owner, "provider", key.provider)
			return
		}
		slog.Warn("provider credential refresh retry failed",
			"owner", key.owner, "provider", key.provider, "attempt", attempt, "error", err)
		delay *= 2
		if delay > persistRetryMax {
			delay = persistRetryMax
		}
	}
	p.clearPending(key, sent)
	slog.Error("provider credential rotation lost: the refreshed token could not be persisted",
		"owner", key.owner, "provider", key.provider, "tries", persistTries)
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
		ch := p.sf.DoChan(flightKey(owner, provider, true), func() (any, error) {
			if !p.startWork() {
				return nil, errProviderCredsDraining
			}
			defer p.wg.Done()
			return p.refresh(owner, provider, true, chatgptauth.ReasonScheduled)
		})
		select {
		case r := <-ch:
			if r.Err != nil && !errors.Is(r.Err, errProviderCredsDraining) {
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

// generation reads a key's eviction generation.
func (p *providerCreds) generation(key credKey) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen[key]
}

// cachePut stores an access token only if the key has not been forgotten since
// the refresh that produced it started. Without that test a refresh in flight
// across a logout would put the logged-out account's token back into the cache
// and the next turn would keep working, while the design promises it fails with
// the re-login error; the same race would resurrect a previous account's token
// after a re-login.
func (p *providerCreds) cachePut(key credKey, tok cachedAccess, gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen[key] != gen {
		return
	}
	if p.cache == nil {
		p.cache = make(map[credKey]cachedAccess)
	}
	p.cache[key] = tok
}

// forget drops everything this daemon remembers about one key's credential, and
// by bumping the generation it makes every refresh already in flight discard its
// own cache write. A login, a logout, a refusal and a vanished row all end here.
func (p *providerCreds) forget(key credKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, key)
	delete(p.pending, key)
	if p.gen == nil {
		p.gen = make(map[credKey]uint64)
	}
	p.gen[key]++
}

// pendingFor returns the rotation waiting to be saved for this key.
func (p *providerCreds) pendingFor(key credKey) (pendingRotation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pend, ok := p.pending[key]
	return pend, ok
}

func (p *providerCreds) setPending(key credKey, pend pendingRotation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[credKey]pendingRotation)
	}
	p.pending[key] = pend
}

// clearPending drops the hand-off for one key, and only if it is still the
// rotation described by sent: a newer hand-off must never be cleared by an older
// one finishing.
func (p *providerCreds) clearPending(key credKey, sent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pend, ok := p.pending[key]; ok && pend.sent == sent {
		delete(p.pending, key)
	}
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
	if tokens.AccountID == "" {
		// Every ChatGPT request carries the account id, and a credential is
		// pinned to the account it was logged in as (a refresh resolving to
		// another account is refused). A login that cannot be pinned cannot
		// honour that pin, so it is refused here rather than discovered on the
		// first turn.
		return "", "", &connectapi.ControllerError{
			Code: protocol.ErrFailedPrecondition,
			Message: "the issuer returned no ChatGPT account id; a credential is pinned to the " +
				"account it was logged in as, so the login cannot be kept",
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
	// A login replaces whatever this daemon remembered for the key, including a
	// previous account's cached token and any rotation still waiting to be
	// saved: the row now holds the login, so neither may be served or written
	// back.
	p.forget(credKey{owner: ownerUserID, provider: provider})
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
	p.forget(credKey{owner: ownerUserID, provider: provider})
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
