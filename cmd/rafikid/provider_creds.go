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
//  2. A rotation the database did not accept is HANDED OFF, not dropped: it is
//     recorded in `pending` from INSIDE the refresh callback, while the store's
//     row lock is still held, and stays there until a background save lands it.
//     While a record matches the row's token no caller may spend that token
//     again (the issuer would answer refresh_token_reused and tombstone a login
//     that works), and a record carries a sequence so an older save can neither
//     overwrite nor clear a newer rotation.
//  3. A refresh runs on the DAEMON's lifetime context with a deadline, never
//     on the turn's context: a cancelled turn must not abandon a rotated token
//     between the issuer's answer and the store's write, while a hung upstream
//     must not leak a goroutine forever. The issuer call itself additionally
//     ignores the daemon's OWN cancellation (context.WithoutCancel plus a fresh
//     deadline), because a graceful shutdown cancels that context and a
//     rotation already in flight would otherwise be dropped with the old token
//     spent upstream.
//  4. A save that is not a login must never go through Store.Put. Put clears
//     dead_at, so a background retry through Put would resurrect a credential a
//     logout deleted. Store.Refresh writes the token columns and leaves the
//     tombstone alone, which is what makes "retry the save" and "the row is
//     gone" both safe.
//  5. Shutdown drains every flight and save (Wait) before the pool closes, and
//     both halves of a refresh ignore the cancelled daemon context: the issuer
//     call runs on context.WithoutCancel with its own deadline, and the saves'
//     final writes do the same, because a rotation that lands during shutdown is
//     exactly the one most likely to be lost. The drain itself is BOUNDED: a
//     hung database (or a hung issuer) must not hold process exit, and what it
//     gave up on is logged by key, never by token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"go.graveland.dev/rafiki/pkg/chatgptauth"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/paths"
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

	// drainTimeout bounds Wait. A save makes at most persistTries attempts, each
	// bounded only by refreshTimeout, so a database that HANGS rather than
	// refusing a write could otherwise hold shutdown for persistTries x
	// refreshTimeout per pending key. Past this bound the rotations still unsaved
	// are logged rather than waited for: holding the process open is the worse
	// outcome.
	drainTimeout = 30 * time.Second

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

	// chatGPTModelsURL is the per-account model directory the codex CLI reads.
	// Like the Responses endpoint it is undocumented and can change.
	chatGPTModelsURL = "https://chatgpt.com/backend-api/codex/models"

	// chatgptClientVersion is the codex client version the directory request
	// claims. The server gates entries by a minimal client version, so an older
	// string silently hides models the account can run; 0.165.0 is the version
	// the captured fixtures came from. RAFIKI_CHATGPT_CLIENT_VERSION overrides
	// it.
	chatgptClientVersion = "0.165.0"

	// modelsRefreshInterval is how long a model directory stays fresh before a
	// listing asks upstream again. An hour: the directory changes when OpenAI
	// ships a model, and a listing is the only thing that reads it eagerly.
	modelsRefreshInterval = time.Hour

	// modelsResponseCap bounds a model directory body. The captured document is
	// a few hundred KB; anything past this is not a directory, and reading it
	// would only spend memory.
	modelsResponseCap = 16 << 20

	// modelsReadTimeout bounds the store read a sender's facts lookup may do when
	// nothing is warm yet. It is a bound, not a budget: the common case is a
	// memory hit that never reaches the store.
	modelsReadTimeout = 5 * time.Second

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

// GoString redacts the token in the Go-syntax rendering too: %#v consults
// GoStringer where %v consults Stringer, and a debug dump is exactly where a
// live token must not appear.
func (c cachedAccess) GoString() string {
	return fmt.Sprintf("providerCreds.cachedAccess{token:%q, accountID:%q, expiry:%s}",
		redactedToken, c.accountID, c.expiry.UTC().Format(time.RFC3339))
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
// not accept: sent is the refresh token the ROW held when the rotation was
// recorded, want is the credential the issuer handed back in its place, and seq
// orders hand-offs for one key.
//
// It is the design's "never lose a rotated token" rule made concrete. The record
// is written INSIDE the store callback, while the row lock is held, so any
// refresh that acquires the row afterwards already sees it; one that finds the
// row STILL holding sent adopts want instead of spending sent a second time. A
// save may write or clear only the record whose seq is still current, so an
// older save can never overwrite or clear a newer rotation.
type pendingRotation struct {
	seq  uint64
	sent string
	want providercreds.Credential
}

// String redacts the rotation's two tokens. fmt does not reach String() through
// an unexported field, but a pendingRotation printed as a whole (%v on the
// struct) would otherwise render both of them.
func (r pendingRotation) String() string {
	return fmt.Sprintf("providerCreds.pendingRotation{seq:%d sent:%s want:%s}",
		r.seq, redactedToken, r.want.String())
}

// GoString redacts the tokens in the Go-syntax rendering too: %#v consults
// GoStringer where %v consults Stringer, and a debug dump is exactly where a
// live token must not appear.
func (r pendingRotation) GoString() string {
	return fmt.Sprintf("providerCreds.pendingRotation{seq:%d, sent:%q, want:%#v}",
		r.seq, redactedToken, r.want)
}

// LogValue keeps the tokens out of structured logs, so slog.Any on one logs a
// redacted group. It satisfies slog.LogValuer.
func (r pendingRotation) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Uint64("seq", r.seq),
		slog.String("sent", redactedToken),
		slog.String("provider", r.want.Provider),
		slog.String("owner", r.want.OwnerUserID),
	)
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
	// pending holds rotations whose database write has not landed yet, keyed by
	// credential, and nextRotation numbers them. Only the newest record for a key
	// may be written or cleared: that is what stops an older save from undoing a
	// newer rotation (the second hand-off an adopted rotation creates).
	pending      map[credKey]pendingRotation
	nextRotation uint64
	// logins is the owner binding of every login in flight, pruned on Begin.
	logins map[string]loginBinding
	// models is every owner's parsed model directory, keyed like the cache, plus
	// when a fetch was last ATTEMPTED for it. A parse is replaced only by a fetch
	// (or by a login/logout, through forget), so it is one parse per fetch,
	// shared by the sender's facts, the child's context window and the listing.
	models map[credKey]*ownerModelsEntry

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

	// drainWait overrides drainTimeout; zero means drainTimeout. A test sets it
	// so the bound is observable without waiting 30 seconds.
	drainWait time.Duration

	// modelsURL and modelsClient override the model-directory endpoint and its
	// HTTP client; empty and nil mean the fixed ChatGPT endpoint and the default
	// client. A test sets them to an httptest server, the same way retryDelay and
	// drainWait are overridden.
	modelsURL    string
	modelsClient *http.Client
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
		models:    make(map[credKey]*ownerModelsEntry),
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
//
// The wait is BOUNDED. A save retries at most persistTries times with each
// attempt bounded only by refreshTimeout, so a database that hangs rather than
// refusing its writes would otherwise hold shutdown for hours. Past drainTimeout
// the rotations still unsaved are reported and Wait returns: the pool closes
// underneath them, and the operator sees which keys lost one.
func (p *providerCreds) Wait() {
	if p == nil {
		return
	}
	p.workMu.Lock()
	p.draining = true
	p.workMu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	limit := p.drainBound()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// Name what cannot be waited for any longer. Keys, never tokens: a
		// drain log that named the credential would be a leak.
		unsaved := p.unsavedRotations()
		slog.Error("provider credentials: shutdown drain timed out; these rotations may not be saved",
			"timeout", limit, "count", len(unsaved), "keys", strings.Join(unsaved, ","))
	}
}

// unsavedRotations names the hand-offs still waiting to be written, as
// "owner/provider" pairs, sorted so the log line is stable.
func (p *providerCreds) unsavedRotations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.pending))
	for key := range p.pending {
		keys = append(keys, key.owner+"/"+key.provider)
	}
	sort.Strings(keys)
	return keys
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

// isDraining reports whether Wait has begun.
func (p *providerCreds) isDraining() bool {
	p.workMu.Lock()
	defer p.workMu.Unlock()
	return p.draining
}

// drainBound is how long Wait waits for work to finish: drainWait in a test,
// drainTimeout in the daemon.
func (p *providerCreds) drainBound() time.Duration {
	if p.drainWait > 0 {
		return p.drainWait
	}
	return drainTimeout
}

// upstreamCtx is the context the issuer call itself runs on.
//
// It deliberately ignores the daemon's cancellation. A graceful shutdown
// cancels p.bg, and the HTTP request (or its body read) would then be aborted
// mid-flight: a rotation the issuer had already completed would be dropped with
// the old token spent upstream, and the next boot would get
// refresh_token_reused and tombstone a login that works. The call keeps a
// deadline of its own instead, so a hung issuer still cannot outlive the
// process.
//
// Once the drain has begun the deadline shrinks to half of it: a call started
// then must leave room inside the same drain for the write it produces, and a
// call that cannot finish in it would spend a single-use token this process
// will not live to store.
func (p *providerCreds) upstreamCtx(parent context.Context) (context.Context, context.CancelFunc) {
	limit := refreshTimeout
	if p.isDraining() {
		limit = p.drainBound() / 2
	}
	return context.WithTimeout(context.WithoutCancel(parent), limit)
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

	// sent is the refresh token the ROW holds as this refresh begins: what a
	// hand-off replaces and what the reuse re-read compares against.
	//
	// recorded is the sequence of the hand-off this refresh created for a
	// rotation the issuer has already completed; adopted is the sequence of the
	// one it continued. Both are captured from the callback for use after the
	// transaction: recorded identifies the save that must run if the write did
	// not land, and either identifies the record a successful commit clears.
	//
	// tombstonedHere records that THIS callback is what the store will tombstone
	// the row for. The store calls the callback only for a row that is live at
	// this transaction's first read, and it writes dead_at alone (leaving the
	// token columns holding `sent`), so a dead row this refresh meets and did not
	// tombstone itself predates the transaction.
	//
	// That is necessary but NOT sufficient: a retrying save and a concurrent
	// refresh are separate transactions, so this call's own tombstone predates
	// theirs and only dropping the record under the row lock stops them
	// (dropRejectedRotation). The guard below covers this call alone.
	var (
		sent           string
		recorded       uint64
		adopted        uint64
		tombstonedHere bool
	)

	next, err := p.store.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		// The callback's FIRST read of the row, taken before anything this
		// refresh can write: `sent` is the token a tombstone written from here
		// would leave behind, and the row is live (a dead one is answered before
		// the callback runs).
		sent = cur.RefreshToken
		// A rotation this daemon already completed whose write has not landed:
		// while the row still holds the spent token, adopt the rotation. Sending
		// `sent` again would come back refresh_token_reused and tombstone a login
		// that works. This is the ONLY place a hand-off is consulted, and it is
		// under the same row lock that records one.
		continuing := false
		if pend, ok := p.pendingFor(key); ok {
			if pend.sent != cur.RefreshToken {
				// The row has moved past this hand-off: whatever token it holds
				// now is newer, so the record can never be adopted again.
				p.clearRotation(key, pend.seq)
			} else {
				cur = pend.want
				adopted, continuing = pend.seq, true
			}
		}
		if !force {
			if continuing {
				// A routine caller is still holding the access token that worked:
				// keep it, and let this write land the rotation so the NEXT
				// refresh has a usable refresh token.
				return cur, nil
			}
			if usableAccess(cur.AccessToken, cur.AccessExpiry) {
				// Another holder (another daemon, another goroutine) refreshed
				// already: the row is the newer answer, and rewriting it would
				// hide a credential going stale.
				return cur, nil
			}
		}
		// A 401 recovery takes neither early return above: the access token it
		// holds is the one the server just refused, and the sender treats a
		// second 401 as terminal. It refreshes for real, using the rotation's
		// (valid) refresh token rather than the spent one.
		// The issuer call runs on its own context (upstreamCtx): a graceful
		// shutdown cancels the transaction's context, and aborting here would
		// lose a rotation the issuer has already completed.
		actx, acancel := p.upstreamCtx(ctx)
		tokens, rerr := p.auth.Refresh(actx, cur.RefreshToken, reason)
		acancel()
		switch {
		case rerr == nil:
			if tokens.AccountID != "" && cur.AccountID != "" && tokens.AccountID != cur.AccountID {
				// Never switch accounts silently: the owner's spend and their
				// conversation history belong to the account they logged in as.
				tombstonedHere = true
				p.dropRejectedRotation(key, adopted)
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
			if out.RefreshToken != sent {
				// Record the hand-off HERE, while the row lock is still held:
				// another refresh waiting on this row must find the rotation the
				// moment the lock is released, and a write that then fails must
				// not lose it. A successful commit clears it below.
				recorded = p.recordRotation(key, sent, out).seq
			}
			return out, nil
		case errors.Is(rerr, chatgptauth.ErrRefreshReused):
			// The issuer says this token was already spent. It may have been
			// another holder of this same row, so roll back and re-read once.
			//
			// Any tombstone this refresh call writes is accompanied by dropping
			// the record it was holding, in the same transaction and before the
			// tombstone commits. The re-read is the transaction that can tombstone
			// this row, and it runs only after this rollback, so the drop lands
			// strictly before that tombstone can exist: no later reviver (a
			// retrying save, or a concurrent refresh) may resurrect the row with
			// the token the issuer has just called spent.
			p.dropRejectedRotation(key, adopted)
			return cur, errRefreshReused
		case errors.Is(rerr, chatgptauth.ErrRefreshRejected):
			tombstonedHere = true
			p.dropRejectedRotation(key, adopted)
			return cur, fmt.Errorf("%w: %v", providercreds.ErrDead, rerr)
		}
		// Transient: the row is untouched and the turn's own retry handles it.
		// The issuer may still have rotated the refresh token on the way, and the
		// old one is already dead upstream, so the credential is recorded now,
		// under this row lock, for a save outside the transaction.
		if tokens.RefreshToken != "" && tokens.RefreshToken != cur.RefreshToken {
			out := cur
			out.RefreshToken = tokens.RefreshToken
			recorded = p.recordRotation(key, sent, out).seq
		}
		return cur, rerr
	})

	switch {
	case errors.Is(err, errRefreshReused):
		return p.rereadAfterReuse(ctx, key, gen, sent)
	case errors.Is(err, providercreds.ErrNotPersisted):
		// The upstream has already invalidated the old refresh token, so the new
		// one must not be dropped: turns keep working from memory while the write
		// is retried, and the hand-off stops any other refresh from spending the
		// token it replaced. Even a credential that cannot serve a request is
		// worth saving the rotation for.
		slog.Error("provider credential refresh not persisted",
			"owner", owner, "provider", provider, "error", err)
		p.resumeRotationSave(key, recorded, adopted)
		if stored, ok := usableCredential(next); ok {
			p.cachePut(key, accessFrom(stored), gen)
			return accessFrom(stored), nil
		}
		return cachedAccess{}, deadErr(provider)
	case errors.Is(err, providercreds.ErrDead):
		// A row can go dead underneath this daemon: another holder refreshed
		// with the token this one already spent, was told
		// refresh_token_reused, and tombstoned it. If the rotation in hand
		// replaces exactly that token, the tombstone is false and the rotation
		// is the only working login (see reviveRotation).
		//
		// ONLY a tombstone that was already there when this transaction began.
		// A tombstone this callback wrote is the issuer refusing the token it
		// sent, or refusing to stay on the same account, and the row still holds
		// the spent token that refusal replaced, so a revival here would write
		// the refused rotation back into a live row and every later turn would
		// fail with it instead of the owner being asked to log in.
		if !tombstonedHere {
			if pend, ok := p.pendingFor(key); ok {
				if stored, revived := p.reviveRotation(key, pend); revived {
					if live, usable := usableCredential(stored); usable {
						p.cachePut(key, accessFrom(live), gen)
						return accessFrom(live), nil
					}
				}
			}
		}
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
		p.resumeRotationSave(key, recorded, adopted)
		return cachedAccess{}, err
	}

	// The rotation is now in the row, so the hand-off this refresh continued or
	// created has done its job.
	switch {
	case recorded != 0:
		p.clearRotation(key, recorded)
	case adopted != 0:
		p.clearRotation(key, adopted)
	}
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

// startRotationSave starts the goroutine that will land an ALREADY RECORDED
// rotation. The record itself is written by recordRotation from inside the
// refresh callback, under the row lock; this only schedules the retry, so there
// is no window in which a rotation exists upstream and nowhere in memory.
func (p *providerCreds) startRotationSave(key credKey, rot pendingRotation) {
	p.addSave()
	go func() {
		defer p.wg.Done()
		p.persistRotation(key, rot)
	}()
}

// resumeRotationSave starts a save for the hand-off this refresh is still
// responsible for, if it is still current. A recorded hand-off wins over an
// adopted one: recording supersedes it.
func (p *providerCreds) resumeRotationSave(key credKey, recorded, adopted uint64) {
	for _, seq := range []uint64{recorded, adopted} {
		if seq == 0 {
			continue
		}
		if rot, ok := p.pendingFor(key); ok && rot.seq == seq {
			p.startRotationSave(key, rot)
			return
		}
	}
}

// persistRotation retries the write a refresh could not persist, 1s doubling
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
//
// Every attempt is guarded twice: this hand-off must still be the CURRENT one
// for the key (so a newer rotation is never undone), and the row must still hold
// the token this rotation replaced (so another daemon's rotation or a re-login
// is never undone either).
func (p *providerCreds) persistRotation(key credKey, rot pendingRotation) {
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
		var wrote, superseded bool
		_, err := p.store.Refresh(ctx, key.owner, key.provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
			if !p.rotationIsCurrent(key, rot.seq) {
				// A newer rotation took over while this save waited. Writing ours
				// now would undo it, so this save is finished.
				superseded = true
				return cur, nil
			}
			if cur.RefreshToken != rot.sent {
				// Somebody else moved the row on: their token is newer than the
				// one this save carries, so writing ours would undo a re-login
				// (or another daemon's rotation).
				return cur, nil
			}
			wrote = true
			return rot.want, nil
		})
		cancel()
		switch {
		case superseded:
			// The newer hand-off owns the row now; its own save is running.
			return
		case err == nil && wrote:
			p.clearRotation(key, rot.seq)
			slog.Info("provider credential refresh persisted after retry",
				"owner", key.owner, "provider", key.provider, "attempts", attempt)
			return
		case err == nil:
			p.clearRotation(key, rot.seq)
			slog.Info("provider credential already holds a newer token; nothing to save",
				"owner", key.owner, "provider", key.provider)
			return
		case errors.Is(err, providercreds.ErrNotFound):
			// A logout deleted the row while this retried. It stays deleted.
			p.clearRotation(key, rot.seq)
			slog.Info("provider credential row is gone; refreshed token not persisted",
				"owner", key.owner, "provider", key.provider)
			return
		case errors.Is(err, providercreds.ErrDead):
			// A row tombstoned by another holder's refresh_token_reused while this
			// save holds the rotation that replaces the token it died on is the one
			// dead row whose tombstone is known to be false.
			if _, revived := p.reviveRotation(key, rot); revived {
				return
			}
			p.clearRotation(key, rot.seq)
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
	// Giving up does NOT drop the hand-off: the refresh token it carries is the
	// only live one for this credential, and the next refresh must adopt it
	// rather than spend the token it replaced. A login or a logout clears it,
	// and an adopting refresh lands it.
	slog.Error("provider credential rotation not yet persisted; it stays in memory for the next refresh",
		"owner", key.owner, "provider", key.provider, "tries", persistTries)
}

// dropRejectedRotation drops the hand-off this refresh adopted because the
// issuer refused that rotation's own token (invalid_grant), called it already
// spent (refresh_token_reused), or would only honour it for another account.
// Two things follow from dropping it HERE, inside the callback and before the
// store commits the tombstone:
//
//   - it is never adopted again, not even by this daemon's next refresh; and
//   - no later reviver can resurrect the dead row with it. Both kinds are
//     separate transactions, so each reads this call's tombstone as one that
//     predates it: a save still retrying the same record, and a concurrent
//     refresh of the same key. Without the drop either could write the refused
//     or spent token back into a live row.
//
// A seq of 0 means no hand-off was adopted, so there is nothing to drop.
func (p *providerCreds) dropRejectedRotation(key credKey, seq uint64) {
	if seq != 0 {
		p.clearRotation(key, seq)
	}
}

// reviveRotation is the ONE sanctioned resurrection of a dead credential.
//
// The interleaving it exists for: this daemon rotated the refresh token but its
// write failed, so the rotation lives only in `pending`; meanwhile another
// holder of the same row refreshed with the spent token, was told
// refresh_token_reused, re-read the row once, found it unchanged, and
// tombstoned it. The tombstone is therefore a statement about a token THIS
// daemon has the successor to, not a refusal of the login, and the rotation it
// holds is the only working one: dropping it turns a recoverable credential
// into a forced re-login.
//
// Every other dead row stays dead. The comparison happens inside the store's row
// lock (Store.Revive writes only when the row is dead AND still holds sent), so
// a logout, a deleted user, another holder's newer rotation and an undecryptable
// row are all left alone.
//
// The caller owes the one judgment the store cannot make: the tombstone must
// have existed BEFORE its own transaction began. A tombstone this daemon's own
// refresh wrote sits on the very token the rotation replaced, so the store would
// read it as another holder's and write back a token the issuer has refused or
// called spent. `refresh` refuses that revival outright in its own call, and
// drops the record under the row lock so no later reviver (a retrying save or a
// concurrent refresh) has anything to revive with.
//
// It reports the now-live credential, and whether it revived anything. The log
// line names the owner and the provider, never a token.
func (p *providerCreds) reviveRotation(key credKey, rot pendingRotation) (providercreds.Credential, bool) {
	if p == nil || p.store == nil {
		return providercreds.Credential{}, false
	}
	if !p.rotationIsCurrent(key, rot.seq) {
		// A newer hand-off owns this key now; its own save is the one to run.
		return providercreds.Credential{}, false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.bgCtx()), refreshTimeout)
	defer cancel()
	wrote, err := p.store.Revive(ctx, key.owner, key.provider, rot.sent, rot.want)
	if err != nil {
		slog.Warn("provider credential revival failed; the refreshed token stays unsaved",
			"owner", key.owner, "provider", key.provider, "error", err)
		return providercreds.Credential{}, false
	}
	if !wrote {
		// The row is gone, or it is dead on some other token: not a tombstone this
		// rotation can contradict.
		return providercreds.Credential{}, false
	}
	p.clearRotation(key, rot.seq)
	slog.Warn("revived a provider credential another holder tombstoned with the token this daemon already rotated",
		"owner", key.owner, "provider", key.provider)
	return rot.want, true
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
	// The model directory belongs to the credential: a logout, a re-login to
	// another account and a dead row all invalidate it.
	delete(p.models, key)
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

// recordRotation replaces this key's hand-off with want and returns it. It is
// called from INSIDE a store callback, i.e. while that key's row lock is held:
// the record therefore exists before the lock is released, so a refresh that
// acquires the row next cannot spend the token this one replaced.
func (p *providerCreds) recordRotation(key credKey, sent string, want providercreds.Credential) pendingRotation {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[credKey]pendingRotation)
	}
	p.nextRotation++
	rot := pendingRotation{seq: p.nextRotation, sent: sent, want: want}
	p.pending[key] = rot
	return rot
}

// rotationIsCurrent reports whether seq is still this key's newest hand-off.
func (p *providerCreds) rotationIsCurrent(key credKey, seq uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pend, ok := p.pending[key]
	return ok && pend.seq == seq
}

// clearRotation drops the hand-off for one key, and only if it is still the
// rotation identified by seq: a newer hand-off must never be cleared by an older
// one finishing.
func (p *providerCreds) clearRotation(key credKey, seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pend, ok := p.pending[key]; ok && pend.seq == seq {
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

// ─── Model directory ─────────────────────────────────────────────────────────

// ownerModelsEntry is one owner's parsed model directory, whether anything has
// answered for it yet, and when a fetch was last attempted.
//
// dir is nil when the row holds no directory, or holds one this daemon could not
// parse: the two are the same answer to every reader, and a fetch replaces it in
// place. read is what separates "there is none" from "nobody has looked yet",
// which a fetch attempt alone must not change: stamping a fetch attempt must not
// make a directory that IS stored read as empty.
type ownerModelsEntry struct {
	dir     *ownerModels
	read    bool
	triedAt time.Time
}

// codexModel is one entry of the owner's /models directory.
//
// supported_in_api is deliberately NOT a field: it describes API-key access, so
// an account whose subscription can run a model frequently reports false for
// it, and filtering on it would hide exactly the models this kind exists for.
// The listing filters on visibility alone.
type codexModel struct {
	Slug            string   `json:"slug"`
	DisplayName     string   `json:"display_name"`
	Visibility      string   `json:"visibility"`
	ContextWindow   int      `json:"context_window"`
	InputModalities []string `json:"input_modalities"`

	SupportVerbosity         bool   `json:"support_verbosity"`
	DefaultVerbosity         string `json:"default_verbosity"`
	SupportsReasoningSummary bool   `json:"supports_reasoning_summary_parameter"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// ownerModels is one owner's /models document, parsed ONCE per fetch.
//
// Three readers share it: the sender's per-model facts (ModelFactsFor), a
// child's context window, and the model listing. Sharing the parsed value is
// what keeps them from disagreeing about the same document, and it is why
// nothing here re-reads or re-parses on a lookup.
type ownerModels struct {
	entries []codexModel // server order, duplicates and slugless entries included
	bySlug  map[string]int
}

// parseOwnerModels parses a /models document. A body that is not the expected
// shape is nil, never a partial directory: a malformed or empty answer must
// leave the previous cache alone rather than half-replace it, and an account
// whose directory is genuinely empty has nothing to list either way.
func parseOwnerModels(raw []byte) *ownerModels {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Models []codexModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if len(doc.Models) == 0 {
		return nil
	}
	m := &ownerModels{entries: doc.Models, bySlug: make(map[string]int, len(doc.Models))}
	for i, e := range doc.Models {
		if e.Slug == "" {
			continue
		}
		if _, dup := m.bySlug[e.Slug]; dup {
			continue // first entry wins, matching the server's own ordering
		}
		m.bySlug[e.Slug] = i
	}
	return m
}

// fact maps one entry onto the fields the Responses sender gates its request
// on. ok is false for a slug the account's directory does not carry, which is
// the sender's conservative case rather than an error.
func (m *ownerModels) fact(slug string) (llm.ModelFact, bool) {
	if m == nil {
		return llm.ModelFact{}, false
	}
	i, ok := m.bySlug[slug]
	if !ok {
		return llm.ModelFact{}, false
	}
	e := m.entries[i]
	f := llm.ModelFact{
		SupportsReasoningSummary: e.SupportsReasoningSummary,
		SupportsVerbosity:        e.SupportVerbosity,
		DefaultVerbosity:         e.DefaultVerbosity,
	}
	for _, lvl := range e.SupportedReasoningLevels {
		if lvl.Effort != "" {
			f.ReasoningLevels = append(f.ReasoningLevels, lvl.Effort)
		}
	}
	return f, true
}

// contextWindow is the entry's context_window, 0 when the directory does not
// carry the slug or does not report one. 0 is what fundi reads as "ask the
// catalog".
func (m *ownerModels) contextWindow(slug string) int {
	if m == nil {
		return 0
	}
	i, ok := m.bySlug[slug]
	if !ok {
		return 0
	}
	return m.entries[i].ContextWindow
}

// listable returns the entries the account offers for listing, in the server's
// order. Only visibility == "list" qualifies: the field also takes "hide" and
// "none", and both mean the account cannot address the model, so offering one
// would hand the picker a model whose every turn fails.
func (m *ownerModels) listable() []codexModel {
	if m == nil {
		return nil
	}
	out := make([]codexModel, 0, len(m.entries))
	for _, e := range m.entries {
		if e.Slug == "" || e.Visibility != "list" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// bgCtx is the daemon's lifetime context, with a background one as the fallback
// for a providerCreds a test built without one.
func (p *providerCreds) bgCtx() context.Context {
	if p == nil || p.bg == nil {
		return context.Background()
	}
	return p.bg
}

// ModelFactsFor returns the per-model facts one owner's ChatGPT sender gates
// its request on, read from that owner's cached model directory.
//
// It is deliberately a lookup over the directory this daemon already parsed,
// never a fetch: a model's facts arriving a turn late is nothing, while a store
// or network round trip inside request construction would be a stall on every
// send. A slug nothing is known about answers ok=false.
func (p *providerCreds) ModelFactsFor(owner, provider string) llm.ModelFacts {
	key := credKey{owner: owner, provider: provider}
	return func(model string) (llm.ModelFact, bool) {
		if p == nil || owner == "" || provider == "" {
			return llm.ModelFact{}, false
		}
		if dir, known := p.rememberedModels(key); known {
			return dir.fact(model)
		}
		// Nothing parsed yet (a sender built for a provider this spawn did not
		// warm). One bounded read fills the entry for every later call.
		ctx, cancel := context.WithTimeout(p.bgCtx(), modelsReadTimeout)
		defer cancel()
		dir, err := p.ownerModels(ctx, owner, provider)
		if err != nil {
			return llm.ModelFact{}, false
		}
		return dir.fact(model)
	}
}

// ownerModels returns the owner's cached model directory, reading and parsing
// the stored document at most once (a later fetch replaces the parse).
//
// A missing or dead credential answers (nil, nil): it is not an error to ask
// about an owner with no login, and it is never remembered, so a login fills
// the entry straight after.
func (p *providerCreds) ownerModels(ctx context.Context, owner, provider string) (*ownerModels, error) {
	if p == nil || p.store == nil || owner == "" || provider == "" {
		return nil, nil
	}
	key := credKey{owner: owner, provider: provider}
	if dir, known := p.rememberedModels(key); known {
		return dir, nil
	}
	cred, err := p.store.Get(ctx, owner, provider)
	switch {
	case errors.Is(err, providercreds.ErrNotFound), errors.Is(err, providercreds.ErrDead):
		return nil, nil
	case err != nil:
		return nil, err
	}
	dir := parseOwnerModels(cred.ModelsJSON)
	p.rememberReadModels(key, dir)
	return dir, nil
}

// rememberedModels returns the parsed directory and whether one has been read
// or fetched at all. known=true with a nil directory means "this daemon knows
// the account has none", which is an answer, not a miss.
func (p *providerCreds) rememberedModels(key credKey) (*ownerModels, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.models[key]
	if !ok || !e.read {
		return nil, false
	}
	return e.dir, true
}

// rememberReadModels installs the parse of what the ROW holds, and only when
// nothing has answered for this key yet.
//
// The guard is what keeps a slow read from overwriting a newer FETCH: a read at
// T1 and a fetch that started at T0 but lands at T2 both want to install, and
// the fetch's document is the newer one. A fetch therefore always installs
// (rememberFetchedModels) and a read never overwrites one.
func (p *providerCreds) rememberReadModels(key credKey, dir *ownerModels) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.modelsEntryLocked(key)
	if e.read {
		return
	}
	e.dir, e.read = dir, true
}

// rememberFetchedModels installs the parse of a document this daemon just
// fetched, unless the credential changed while the fetch was in flight.
//
// gen is the eviction generation captured when the fetch STARTED. A logout, a
// re-login or a dead row bumps it (forget), so a fetch that started as account A
// and finishes as account B is dropped rather than installed: the directory and
// the etag belong to the account, and adopting the old one would offer models
// the new account cannot address.
func (p *providerCreds) rememberFetchedModels(key credKey, dir *ownerModels, gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen[key] != gen {
		return
	}
	e := p.modelsEntryLocked(key)
	e.dir, e.read = dir, true
}

// modelsEntryLocked returns this key's entry, creating it. Callers hold p.mu.
func (p *providerCreds) modelsEntryLocked(key credKey) *ownerModelsEntry {
	e := p.models[key]
	if e == nil {
		if p.models == nil {
			p.models = make(map[credKey]*ownerModelsEntry)
		}
		e = &ownerModelsEntry{}
		p.models[key] = e
	}
	return e
}

// modelsFetchDue reports whether a fetch is due and stamps the attempt, so an
// upstream that is down is retried on the cadence rather than on every listing
// (and every listing does not log a WARN).
func (p *providerCreds) modelsFetchDue(key credKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.modelsEntryLocked(key)
	now := time.Now()
	if !e.triedAt.IsZero() && now.Sub(e.triedAt) < modelsRefreshInterval {
		return false
	}
	e.triedAt = now
	return true
}

// chatGPTModelsClientVersion is the client_version the directory request
// claims: the operator's override when set, else the constant.
func chatGPTModelsClientVersion() string {
	if v := strings.TrimSpace(paths.Get("RAFIKI_CHATGPT_CLIENT_VERSION")); v != "" {
		return v
	}
	return chatgptClientVersion
}

// refreshModelsInBackground starts a fetch when one is due and returns
// immediately.
//
// It is the ONLY way a listing or a login triggers one: neither may wait for
// the upstream (a listing is answered from the cache), and neither may be told
// the fetch failed. The fetch runs on the daemon's context and is counted, so
// shutdown drains it.
func (p *providerCreds) refreshModelsInBackground(owner, provider string) {
	if p == nil || owner == "" || provider == "" {
		return
	}
	if ok, _ := p.Available(); !ok {
		return
	}
	key := credKey{owner: owner, provider: provider}
	if !p.modelsFetchDue(key) {
		return
	}
	if !p.startWork() {
		return
	}
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(p.bgCtx(), refreshTimeout)
		defer cancel()
		p.refreshModels(ctx, owner, provider)
	}()
}

// refreshModels fetches one owner's model directory and replaces the cache.
//
// Best effort by contract: every failure is logged at WARN and leaves the
// previous cache in place, because a stale directory still names models that
// work while an empty one offers nothing. A 304 keeps the cache (the etag is
// the whole point of sending it), and a 200 lands only after the document
// PARSES: an unrecognised answer must never replace a usable directory.
//
// The whole fetch is bound to the credential's GENERATION, captured here before
// any work. A fetch in flight across a logout and a re-login to a different
// account must not write the old account's directory and etag onto the new
// account's row: the directory is per account, and the models in it would be
// ones the new login cannot address. Same discipline as the access-token cache
// (cachePut), for the same reason.
func (p *providerCreds) refreshModels(ctx context.Context, owner, provider string) {
	if ok, _ := p.Available(); !ok {
		return
	}
	if ctx == nil {
		ctx = p.bgCtx()
	}
	key := credKey{owner: owner, provider: provider}
	gen := p.generation(key)
	cred, err := p.store.Get(ctx, owner, provider)
	switch {
	case errors.Is(err, providercreds.ErrNotFound), errors.Is(err, providercreds.ErrDead):
		return
	case err != nil:
		slog.Warn("provider models: read the credential failed",
			"owner", owner, "provider", provider, "error", err)
		return
	}
	bearer, headers, err := p.TokenSourceFor(owner, provider).Token(ctx, false)
	if err != nil {
		slog.Warn("provider models: no usable credential",
			"owner", owner, "provider", provider, "error", err)
		return
	}

	endpoint := p.modelsURL
	if endpoint == "" {
		endpoint = chatGPTModelsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"?client_version="+url.QueryEscape(chatGPTModelsClientVersion()), nil)
	if err != nil {
		slog.Warn("provider models: build the request failed",
			"owner", owner, "provider", provider, "error", err)
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	// The credential's own headers (the account id) ride beside the bearer, the
	// same way the sender replays them.
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if cred.ModelsETag != "" {
		req.Header.Set("If-None-Match", cred.ModelsETag)
	}

	client := p.modelsClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("provider models: fetch failed",
			"owner", owner, "provider", provider, "error", err)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		// The cached directory is still current. Nothing to store, nothing to log.
		return
	case http.StatusOK:
	default:
		slog.Warn("provider models: unexpected status; keeping the cached directory",
			"owner", owner, "provider", provider, "status", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsResponseCap+1))
	if err != nil {
		slog.Warn("provider models: read the body failed",
			"owner", owner, "provider", provider, "error", err)
		return
	}
	if len(body) > modelsResponseCap {
		slog.Warn("provider models: model directory exceeds the size cap; keeping the cached one",
			"owner", owner, "provider", provider, "cap", modelsResponseCap)
		return
	}
	dir := parseOwnerModels(body)
	if dir == nil {
		slog.Warn("provider models: unrecognised model directory; keeping the cached one",
			"owner", owner, "provider", provider, "bytes", len(body))
		return
	}
	if p.generation(key) != gen {
		// The credential was logged out, re-logged-in or marked dead while this
		// fetch was in flight. The answer describes an account that is no longer
		// the owner's, so it is dropped unread rather than written to the row.
		slog.Info("provider models: credential changed during the fetch; discarding the directory",
			"owner", owner, "provider", provider)
		return
	}
	if err := p.store.SetModels(ctx, owner, provider, body, resp.Header.Get("ETag")); err != nil {
		slog.Warn("provider models: store the model directory failed",
			"owner", owner, "provider", provider, "error", err)
		return
	}
	// The parse of the document that was just stored: every reader of this
	// fetch's answer (facts, context window, listing) shares this one value. The
	// generation is re-checked here too, closing the window between the test
	// above and the write.
	p.rememberFetchedModels(key, dir, gen)
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
	// A fresh login has no model directory, and a login is the moment the
	// credential is known good. Fetch it in the background: a listing (and the
	// child's context window) would otherwise stay empty until something else
	// happened to trigger a fetch.
	p.refreshModelsInBackground(ownerUserID, provider)
	return provider, tokens.AccountID, nil
}

// DeleteProviderCredential forgets an owner's login and revokes it upstream.
// The revoke is best effort: the row is already gone, and a failure there must
// not look like a failed logout.
//
// It revokes BOTH live refresh tokens when there are two: the row's, and one a
// rotation this daemon completed but never persisted is holding. The row's is
// the older, already spent one in that case, so revoking only it would leave the
// grant usable upstream until it expired on its own, which is the opposite of
// what a logout means. `forget` drops the rotation, so it has to be read first.
func (p *providerCreds) DeleteProviderCredential(ctx context.Context, ownerUserID, provider string) error {
	if ok, why := p.Available(); !ok {
		return &connectapi.ControllerError{Code: protocol.ErrUnavailable, Message: why}
	}
	key := credKey{owner: ownerUserID, provider: provider}
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
	tokens := []string{deleted.RefreshToken}
	if pend, ok := p.pendingFor(key); ok {
		tokens = append(tokens, pend.want.RefreshToken)
	}
	p.forget(key)
	// The row existed but could not be decrypted with the current key: the logout
	// happened, and there is no token to revoke.
	p.revokeRefreshTokens(ownerUserID, provider, tokens)
	return nil
}

// DeleteOwnerCredentials forgets and revokes every credential one owner holds.
// It is what a user removal needs: conversations.users rows are tombstoned, not
// deleted, and the credential rows are separate, so nothing else would stop the
// idle sweep from refreshing a removed user's grant every eight days forever, or
// stop a resumed child of theirs from keeping the account billed.
//
// Best effort by design: the removal itself has already happened and is not the
// daemon's to undo, so a store failure is a WARN naming the owner and the error
// (never a token) and nothing more.
func (p *providerCreds) DeleteOwnerCredentials(ctx context.Context, ownerUserID string) {
	if p == nil || p.store == nil || ownerUserID == "" {
		return
	}
	// On a context the caller cannot cancel: deleting a removed user's logins is
	// a privacy action, and an admin's client disconnecting mid-call must not
	// leave half of them behind. Bounded, so a hung store cannot wedge it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	deleted, err := p.store.DeleteAll(ctx, ownerUserID)
	if err != nil {
		slog.Warn("provider credentials: deleting a removed user's logins failed",
			"owner", ownerUserID, "error", err)
		return
	}
	for _, c := range deleted {
		key := credKey{owner: ownerUserID, provider: c.Provider}
		tokens := []string{c.RefreshToken}
		if pend, ok := p.pendingFor(key); ok {
			tokens = append(tokens, pend.want.RefreshToken)
		}
		p.forget(key)
		if p.auth != nil {
			p.revokeRefreshTokens(ownerUserID, c.Provider, tokens)
		}
	}
	slog.Info("provider credentials deleted with the user",
		"owner", ownerUserID, "count", len(deleted))
}

// revokeRefreshTokens revokes each of these refresh tokens upstream, best
// effort: the rows are already gone, so a failure here logs by key and moves on
// (an unrevoked token is worth knowing about, never worth failing a logout for).
// Empty and repeated tokens are skipped, so an undecryptable row and a rotation
// that matches the row's token cost nothing.
func (p *providerCreds) revokeRefreshTokens(owner, provider string, tokens []string) {
	seen := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		// On the daemon's context with a deadline: a stuck issuer must not hang a
		// logout, and a client that disconnects mid-call must not cancel a revoke
		// the daemon already committed to.
		rctx, cancel := context.WithTimeout(p.bgCtx(), refreshTimeout)
		err := p.auth.Revoke(rctx, tok)
		cancel()
		if err != nil {
			slog.Warn("provider credential revoke failed after logout",
				"owner", owner, "provider", provider, "error", err)
		}
	}
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
