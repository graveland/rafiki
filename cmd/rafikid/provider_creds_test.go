// SPDX-License-Identifier: Apache-2.0

package main

// Tests for the per-owner provider credential machinery: the token cache, the
// refresh path (including the two ways a rotated token can be lost), the idle
// sweep, and the login backend.
//
// The store is a hand-written in-memory providercreds.Store rather than the
// Postgres one, because the interesting failures are about what the CALLER does
// with a store answer (ErrDead, ErrNotPersisted, ErrNotFound), and a fake
// states them directly instead of having to provoke them through SQL. The fake
// mirrors the real store's two load-bearing semantics: fn runs under the row's
// lock, and an unchanged credential writes nothing.
//
// The issuer is an httptest server through chatgptauth.NewWithEndpoints, so the
// reason this daemon sends is read out of the body the engine actually built.

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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/chatgptauth"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providercreds"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

const (
	testOwner    = "alice"
	testProvider = "chatgpt"
)

var testKey = credKey{owner: testOwner, provider: testProvider}

// ─── Fake credential store ───────────────────────────────────────────────────

// fakeCredStore is an in-memory providercreds.Store.
type fakeCredStore struct {
	mu   sync.Mutex
	rows map[credKey]providercreds.Credential

	// stale is what ListStale reports, and staleAge is the age it was asked
	// for: the sweep's cadence is part of its contract.
	stale    []providercreds.Key
	staleAge time.Duration

	// rotateOnRefresh, keyed by credential, is a row "another daemon" commits
	// while our first Refresh is failing: it is what makes refresh_token_reused
	// recoverable. Applied once, on the first Refresh after it is set.
	rotateOnRefresh map[credKey]providercreds.Credential

	// failWrites makes the next N CHANGED writes fail with ErrNotPersisted,
	// which is the "upstream already rotated, the database write did not land"
	// case. A count, not a flag: one credential can lose more than one write in
	// a row when a rotation is adopted and rotated again.
	failWrites int

	// puts and refreshCalls count calls, so a test can prove a path never used
	// Put and how many transactions the store saw.
	puts         int
	refreshCalls int

	// revives counts Revive calls that actually wrote, and reviveErr /
	// deleteAllErr make those two methods fail on demand.
	revives      int
	reviveErr    error
	deleteAllErr error

	// onDead, when set, runs ONCE at the moment a refresh callback's ErrDead is
	// committed, with this store's lock still held and before the refresh that
	// caused it sees the error. Nothing else can call the store in that window,
	// which is what lets a test observe the state the tombstone was written in
	// (see afterOwnDeadStore for the hook that runs once the lock is free).
	onDead func()
}

var _ providercreds.Store = (*fakeCredStore)(nil)

func newFakeCredStore() *fakeCredStore {
	return &fakeCredStore{rows: make(map[credKey]providercreds.Credential)}
}

func (f *fakeCredStore) seed(c providercreds.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[credKey{owner: c.OwnerUserID, provider: c.Provider}] = c
}

// row returns the stored credential as it sits, dead ones included.
func (f *fakeCredStore) row(owner, provider string) (providercreds.Credential, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[credKey{owner: owner, provider: provider}]
	return c, ok
}

func (f *fakeCredStore) refreshCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

// setOnDead installs the hook run when a refresh callback's ErrDead is
// committed, once.
func (f *fakeCredStore) setOnDead(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onDead = hook
}

// failNextWrite makes the next n changed writes fail with ErrNotPersisted.
func (f *fakeCredStore) failNextWrite(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWrites += n
}

func (f *fakeCredStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func (f *fakeCredStore) Put(_ context.Context, c providercreds.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	c.Dead = false
	f.rows[credKey{owner: c.OwnerUserID, provider: c.Provider}] = c
	return nil
}

func (f *fakeCredStore) Get(_ context.Context, owner, provider string) (providercreds.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[credKey{owner: owner, provider: provider}]
	if !ok {
		return providercreds.Credential{}, providercreds.ErrNotFound
	}
	if c.Dead {
		return c, providercreds.ErrDead
	}
	return c, nil
}

func (f *fakeCredStore) Delete(_ context.Context, owner, provider string) (providercreds.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := credKey{owner: owner, provider: provider}
	c, ok := f.rows[key]
	if !ok {
		return providercreds.Credential{}, providercreds.ErrNotFound
	}
	delete(f.rows, key)
	return c, nil
}

func (f *fakeCredStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := credKey{owner: owner, provider: provider}
	cur, ok := f.rows[key]
	if !ok {
		return providercreds.Credential{}, providercreds.ErrNotFound
	}
	if cur.Dead {
		return cur, providercreds.ErrDead
	}
	f.refreshCalls++
	first := f.refreshCalls == 1

	next, err := fn(cur)
	if err != nil {
		if errors.Is(err, providercreds.ErrDead) {
			cur.Dead = true
			f.rows[key] = cur
			if hook := f.onDead; hook != nil {
				f.onDead = nil
				hook()
			}
			return cur, err
		}
		// Rolled back. The other daemon's commit still lands: it is a
		// different transaction, not ours.
		if first {
			if rot, ok := f.rotateOnRefresh[key]; ok {
				f.rows[key] = rot
				delete(f.rotateOnRefresh, key)
			}
		}
		return cur, err
	}
	if sameCredential(next, cur) {
		return cur, nil
	}
	next.OwnerUserID, next.Provider = cur.OwnerUserID, cur.Provider
	if next.AccountID == "" {
		next.AccountID = cur.AccountID
	}
	next.ModelsJSON, next.ModelsETag = cur.ModelsJSON, cur.ModelsETag
	if f.failWrites > 0 {
		f.failWrites--
		return next, fmt.Errorf("%w: fake write failure", providercreds.ErrNotPersisted)
	}
	// The real store's write is ctx-bound: a cancelled context leaves the row
	// untouched and reports ErrNotPersisted with the rotated credential. That is
	// what makes a shutdown-time save observable.
	if err := ctx.Err(); err != nil {
		return next, fmt.Errorf("%w: %v", providercreds.ErrNotPersisted, err)
	}
	f.rows[key] = next
	return next, nil
}

// sameCredential is the real store's "an unchanged credential writes nothing"
// rule, which is why ErrNotPersisted can only describe a real change.
func sameCredential(a, b providercreds.Credential) bool {
	return a.RefreshToken == b.RefreshToken &&
		a.AccessToken == b.AccessToken &&
		a.AccessExpiry.Equal(b.AccessExpiry) &&
		a.AccountID == b.AccountID &&
		a.Dead == b.Dead
}

func (f *fakeCredStore) SetModels(_ context.Context, owner, provider string, modelsJSON []byte, etag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := credKey{owner: owner, provider: provider}
	c, ok := f.rows[key]
	if !ok {
		return providercreds.ErrNotFound
	}
	c.ModelsJSON, c.ModelsETag = modelsJSON, etag
	f.rows[key] = c
	return nil
}

func (f *fakeCredStore) ListStale(_ context.Context, age time.Duration) ([]providercreds.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staleAge = age
	return append([]providercreds.Key(nil), f.stale...), nil
}

// Revive mirrors the real store: it writes only a row that is DEAD on exactly
// sent, and clears the tombstone.
func (f *fakeCredStore) Revive(_ context.Context, owner, provider, sent string, want providercreds.Credential) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reviveErr != nil {
		return false, f.reviveErr
	}
	key := credKey{owner: owner, provider: provider}
	cur, ok := f.rows[key]
	if !ok {
		return false, providercreds.ErrNotFound
	}
	if !cur.Dead || cur.RefreshToken != sent {
		return false, nil
	}
	next := cur
	next.RefreshToken, next.AccessToken, next.AccessExpiry = want.RefreshToken, want.AccessToken, want.AccessExpiry
	if want.AccountID != "" {
		next.AccountID = want.AccountID
	}
	next.Dead = false
	f.rows[key] = next
	f.revives++
	return true, nil
}

// DeleteAll removes one owner's rows and hands them back, like the real store.
func (f *fakeCredStore) DeleteAll(_ context.Context, owner string) ([]providercreds.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteAllErr != nil {
		return nil, f.deleteAllErr
	}
	var out []providercreds.Credential
	for key, c := range f.rows {
		if key.owner == owner {
			out = append(out, c)
			delete(f.rows, key)
		}
	}
	return out, nil
}

// reviveCount is how many rows this store actually revived.
func (f *fakeCredStore) reviveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revives
}

// ownerRows counts the rows stored for one owner, for a removal test.
func (f *fakeCredStore) ownerRows(owner string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for key := range f.rows {
		if key.owner == owner {
			n++
		}
	}
	return n
}

// ─── Fake issuer ─────────────────────────────────────────────────────────────

// fakeIssuer is the ChatGPT issuer as an httptest server: the refresh endpoint,
// the code exchange (same endpoint, form encoded), and revoke.
type fakeIssuer struct {
	srv *httptest.Server

	mu         sync.Mutex
	bodies     []map[string]any
	tokenCalls int
	revokes    int
	// revokedTokens is every token the revoke endpoint was handed, in order.
	revokedTokens []string

	// answer, when set, overrides the token endpoint's response.
	answer func(call int) (status int, body string)
	// block, when non-nil, is received from after the request is recorded and
	// before the answer is written: a stalled upstream.
	block chan struct{}
	// revokeStatus is the revoke endpoint's status; 0 means 200.
	revokeStatus int
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", f.token)
	mux.HandleFunc("/oauth/revoke", f.revoke)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) engine() *chatgptauth.Engine {
	return chatgptauth.NewWithEndpoints(f.srv.Client(),
		f.srv.URL+"/oauth/authorize", f.srv.URL+"/oauth/token", f.srv.URL+"/oauth/revoke")
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.tokenCalls++
	call := f.tokenCalls
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) == nil {
		f.bodies = append(f.bodies, decoded)
	}
	block, answer := f.block, f.answer
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	status, body := http.StatusOK, defaultTokenBody("acct_1")
	if answer != nil {
		status, body = answer(call)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeIssuer) revoke(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.revokes++
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) == nil {
		if tok, ok := decoded["token"].(string); ok {
			f.revokedTokens = append(f.revokedTokens, tok)
		}
	}
	status := f.revokeStatus
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeIssuer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func (f *fakeIssuer) revokeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokes
}

// revoked is the tokens the revoke endpoint was handed, in order.
func (f *fakeIssuer) revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revokedTokens...)
}

func (f *fakeIssuer) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		return nil
	}
	return f.bodies[i]
}

// defaultTokenBody is a healthy refresh response: a rotated refresh token, a new
// access token, and an id_token naming the account.
func defaultTokenBody(accountID string) string {
	return tokenBody("new-access", "rotated-refresh", accountID)
}

// tokenBody is one refresh answer naming both tokens, so a test can tell which
// rotation reached the row and which token a later call spent upstream.
func tokenBody(access, refresh, accountID string) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600,"id_token":%q}`,
		access, refresh, idToken(accountID))
}

// idToken builds a JWT-shaped string whose payload carries the ChatGPT account
// claim. chatgptauth reads the payload without verifying the signature, so the
// header and signature segments are inert.
func idToken(accountID string) string {
	payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, accountID)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// ─── Fixtures ────────────────────────────────────────────────────────────────

// testProviderSet is a registry holding one login-authenticated provider and one
// api-key provider, so a test can see the kind check decide.
func testProviderSet(t *testing.T) func() *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(`
default_provider = "chatgpt"
[providers.chatgpt]
kind = "openai-responses-chatgpt"
[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"
`))
	if err != nil {
		t.Fatalf("providers.Parse: %v", err)
	}
	return func() *providers.Set { return set }
}

// testCreds builds the machinery under test with a cancellable daemon context
// (so the idle sweep exits) and a millisecond retry delay (so the
// ErrNotPersisted retry is not a one-second sleep).
func testCreds(t *testing.T, store providercreds.Store, iss *fakeIssuer, why string) *providerCreds {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var auth *chatgptauth.Engine
	if iss != nil {
		auth = iss.engine()
	}
	p := newProviderCreds(ctx, store, auth, why, testProviderSet(t))
	p.retryDelay = time.Millisecond
	return p
}

// staleCredential is a stored credential whose access token is already inside
// the refresh window, so a non-forced Token call refreshes.
func staleCredential(expiry time.Time) providercreds.Credential {
	return providercreds.Credential{
		OwnerUserID:  testOwner,
		Provider:     testProvider,
		AccountID:    "acct_1",
		RefreshToken: "r0",
		AccessToken:  "old-access",
		AccessExpiry: expiry,
		LastRefresh:  time.Now(),
	}
}

// ─── Token source ────────────────────────────────────────────────────────────

// TestTokenSourceUsesCacheUntil5m pins codex's five-minute window from both
// sides: a token with 5m1s left is used as it stands, one with 4m59s left is
// refreshed before it is handed out.
func TestTokenSourceUsesCacheUntil5m(t *testing.T) {
	t.Run("5m1s left is used as-is", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(5*time.Minute + time.Second)))
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")

		tok, headers, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		c.NoError(err, "Token")
		c.Eq("old-access", tok, "token")
		c.Eq("acct_1", headers.Get(chatGPTAccountHeader), "account header")
		c.Eq(0, iss.calls(), "upstream refresh calls")
	})

	t.Run("4m59s left is refreshed", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(5*time.Minute - time.Second)))
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")

		src := p.TokenSourceFor(testOwner, testProvider)
		tok, _, err := src.Token(context.Background(), false)
		c.NoError(err, "Token")
		c.Eq("new-access", tok, "token")
		c.Eq(1, iss.calls(), "upstream refresh calls")

		// The refreshed token is cached: a second call spends nothing upstream.
		tok, _, err = src.Token(context.Background(), false)
		c.NoError(err, "second Token")
		c.Eq("new-access", tok, "second token")
		c.Eq(1, iss.calls(), "upstream refresh calls after the cached read")
	})
}

// TestTokenSourceRefreshReasons pins what the issuer is told: a forced refresh
// says unauthorized_recovery, an expiry-driven one proactive_expiry, and the
// idle sweep scheduled.
func TestTokenSourceRefreshReasons(t *testing.T) {
	t.Run("forced", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(time.Hour))) // healthy: only force refreshes it
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")

		_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
		c.NoError(err, "Token")
		c.Eq(1, iss.calls(), "upstream refresh calls")
		c.Eq("unauthorized_recovery", iss.body(0)["refresh_reason"], "refresh_reason")
	})

	t.Run("proactive expiry", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(time.Minute)))
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")

		_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		c.NoError(err, "Token")
		c.Eq("proactive_expiry", iss.body(0)["refresh_reason"], "refresh_reason")
	})

	t.Run("scheduled", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(time.Hour)))
		store.stale = []providercreds.Key{{Owner: testOwner, Provider: testProvider}}
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")

		p.sweepOnce(context.Background())
		c.Eq(1, iss.calls(), "upstream refresh calls")
		c.Eq("scheduled", iss.body(0)["refresh_reason"], "refresh_reason")
	})
}

// TestTokenSourceReusedRereadsOnce covers refresh_token_reused, which is not a
// dead login until the row has been re-read: the token may have been rotated by
// another holder between our read and our call. Either way the issuer is asked
// ONCE: the recovery is a re-read, never a second upstream attempt.
func TestTokenSourceReusedRereadsOnce(t *testing.T) {
	reused := func(int) (int, string) {
		return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
	}

	t.Run("another holder rotated it", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(time.Minute)))
		store.rotateOnRefresh = map[credKey]providercreds.Credential{
			testKey: {
				OwnerUserID:  testOwner,
				Provider:     testProvider,
				AccountID:    "acct_1",
				RefreshToken: "other-daemon-refresh",
				AccessToken:  "other-daemon-access",
				AccessExpiry: time.Now().Add(time.Hour),
			},
		}
		iss := newFakeIssuer(t)
		iss.answer = reused
		p := testCreds(t, store, iss, "")

		tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		c.NoError(err, "Token must adopt the other holder's rotation")
		c.Eq("other-daemon-access", tok, "token")
		c.Eq(1, iss.calls(), "the upstream must be asked exactly once")
		stored, ok := store.row(testOwner, testProvider)
		c.True(ok, "row still present")
		c.Eq("other-daemon-refresh", stored.RefreshToken, "stored refresh token")
	})

	t.Run("nobody rotated it", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		store.seed(staleCredential(time.Now().Add(time.Minute)))
		iss := newFakeIssuer(t)
		iss.answer = reused
		p := testCreds(t, store, iss, "")

		_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		c.Require().Error(err, "Token")
		c.StrContains(err.Error(), "rafiki providers login", "error names the fix")
		c.Eq(1, iss.calls(), "the upstream must be asked exactly once")
		stored, ok := store.row(testOwner, testProvider)
		c.True(ok, "row still present")
		c.True(stored.Dead, "a reused token nobody rotated is dead")
	})
}

// TestTokenSourceAccountMismatchIsDead pins the refusal to switch accounts
// silently: the refreshed login belongs to a different ChatGPT account than the
// stored one, so the credential is dead rather than quietly repointed.
func TestTokenSourceAccountMismatchIsDead(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute))) // account acct_1
	iss := newFakeIssuer(t)
	iss.answer = func(int) (int, string) {
		return http.StatusOK, defaultTokenBody("acct_2")
	}
	p := testCreds(t, store, iss, "")

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.Require().Error(err, "Token")
	c.StrContains(err.Error(), "rafiki providers login", "error names the fix")

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "a different account must tombstone the credential")
	c.Eq("r0", stored.RefreshToken, "the stored refresh token must not change")

	has, herr := p.HasCredential(context.Background(), testOwner, testProvider)
	c.NoError(herr, "HasCredential")
	c.False(has, "a dead credential counts as no credential")
}

// TestTokenSourceNotPersistedKeepsToken is the rotated-token rule at its
// sharpest: the issuer has already invalidated the old refresh token when the
// database write fails, so the new token must be kept in memory AND retried into
// the store, and the retry must never go through Put (which would clear dead_at
// and resurrect a logged-out credential).
func TestTokenSourceNotPersistedKeepsToken(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1)
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "Token must answer with the new token despite the failed write")
	c.Eq("new-access", tok, "token")

	c.EventuallyTrue(2*time.Second, 2*time.Millisecond, func() bool {
		stored, ok := store.row(testOwner, testProvider)
		return ok && stored.RefreshToken == "rotated-refresh"
	}, "the rotated refresh token must reach the store")
	c.Eq(0, store.putCount(), "the retry must never call Put")
}

// TestTokenSourceRotatedTokenPersistedOnTransientError is chatgptauth's
// contract made real: a 2xx that carried a NEW refresh token but no access token
// has already invalidated the old one, so the new token reaches the store even
// though the request failed. Dropping it would kill a credential that works.
func TestTokenSourceRotatedTokenPersistedOnTransientError(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	iss.answer = func(int) (int, string) {
		return http.StatusOK, `{"refresh_token":"rotated-refresh"}`
	}
	p := testCreds(t, store, iss, "")

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.Require().Error(err, "a response with no access token is a failure")

	c.EventuallyTrue(2*time.Second, 2*time.Millisecond, func() bool {
		stored, ok := store.row(testOwner, testProvider)
		return ok && stored.RefreshToken == "rotated-refresh"
	}, "the rotated refresh token must be stored")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.False(stored.Dead, "a transient failure must not tombstone the credential")
}

// TestProviderCredSweepRefreshesStale pins the idle sweep itself: one stale key
// from ListStale means one scheduled refresh, and the row ends up rotated.
func TestProviderCredSweepRefreshesStale(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Hour)))
	store.stale = []providercreds.Key{{Owner: testOwner, Provider: testProvider}}
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	p.sweepOnce(context.Background())

	c.Eq(1, iss.calls(), "upstream refresh calls")
	c.Eq(8*24*time.Hour, store.staleAge, "the sweep refreshes what has idled 8 days")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("rotated-refresh", stored.RefreshToken, "stored refresh token")
}

// TestTokenSourceSingleflight is the point of the singleflight: twenty children
// of one owner asking at once spend ONE single-use refresh token, not twenty.
func TestTokenSourceSingleflight(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	release := make(chan struct{})
	iss.block = release
	p := testCreds(t, store, iss, "")
	src := p.TokenSourceFor(testOwner, testProvider)

	const callers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		tokens  []string
		errs    []error
		arrived = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, _, err := src.Token(context.Background(), false)
			mu.Lock()
			defer mu.Unlock()
			tokens = append(tokens, tok)
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	// Let every caller reach the singleflight before the upstream answers.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(arrived)
		close(release)
	}()
	<-arrived
	wg.Wait()

	c.Len(errs, 0, "Token errors: %v", errs)
	c.Len(tokens, callers, "Token results")
	for _, tok := range tokens {
		c.Eq("new-access", tok, "every caller sees the refreshed token")
	}
	c.Eq(1, iss.calls(), "20 concurrent callers must spend exactly one upstream refresh")
	// The count that makes this a singleflight assertion rather than an
	// accident of the store's own no-op guard: without coalescing every caller
	// enters a transaction of its own, even the ones that then find the row
	// already refreshed.
	c.Eq(1, store.refreshCallCount(), "20 concurrent callers must share one store transaction")
}

// TestTokenSourceRefreshSurvivesCallerCancel is the rule that a cancelled turn
// must not abandon a rotated token: the caller returns promptly, and the
// refresh keeps running to the store on the daemon's context.
func TestTokenSourceRefreshSurvivesCallerCancel(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	release := make(chan struct{})
	iss.block = release
	p := testCreds(t, store, iss, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(ctx, false)
		done <- err
	}()

	// Wait until the refresh is actually in flight upstream, then abandon it.
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool { return iss.calls() == 1 },
		"the refresh must reach the issuer")
	cancel()
	c.ErrorIs(<-done, context.Canceled, "the caller must return its own cancellation")

	// The refresh was not abandoned: releasing the upstream lands the rotated
	// token in the store.
	close(release)
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		stored, ok := store.row(testOwner, testProvider)
		return ok && stored.RefreshToken == "rotated-refresh"
	}, "the rotated refresh token must reach the store after the caller gave up")
}

// TestTokenSourceInvalidGrantIsDead pins the permanent refusal: the message
// names the command that fixes it, and the credential counts as missing
// afterwards.
func TestTokenSourceInvalidGrantIsDead(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	iss.answer = func(int) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}
	p := testCreds(t, store, iss, "")

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.Require().Error(err, "Token")
	c.StrContains(err.Error(), "rafiki providers login", "error names the fix")

	has, herr := p.HasCredential(context.Background(), testOwner, testProvider)
	c.NoError(herr, "HasCredential")
	c.False(has, "an invalid_grant credential has to be logged in again")
}

// TestTokenSourceForceRefreshes pins the 401 recovery path: force refreshes even
// when the cached token still looks healthy.
func TestTokenSourceForceRefreshes(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Hour)))
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")
	src := p.TokenSourceFor(testOwner, testProvider)

	tok, _, err := src.Token(context.Background(), false)
	c.NoError(err, "healthy Token")
	c.Eq("old-access", tok, "token")
	c.Eq(0, iss.calls(), "a healthy credential must not refresh")

	tok, _, err = src.Token(context.Background(), true)
	c.NoError(err, "forced Token")
	c.Eq("new-access", tok, "token")
	c.Eq(1, iss.calls(), "force must refresh")
}

// ─── Login backend ───────────────────────────────────────────────────────────

// TestProviderLoginRejectsNonLoginKind pins the classification: a provider that
// does not exist and one that authenticates by an env key get the SAME
// InvalidArgument answer, because a client cannot act on the difference.
func TestProviderLoginRejectsNonLoginKind(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	for _, name := range []string{"anthropic", "nosuchprovider"} {
		_, err := p.BeginProviderLogin(context.Background(), testOwner, name)
		c.Require().Error(err, "BeginProviderLogin(%q)", name)
		ce := c.ErrorAs[*connectapi.ControllerError](err)
		c.Eq(protocol.ErrInvalidArgs, ce.Code, "code for %q", name)
		c.Eq(fmt.Sprintf("provider %q does not use login authentication", name), ce.Message, "message for %q", name)
	}
	c.Eq(0, iss.calls(), "no login may start")
}

// TestProviderLoginUnavailableWithoutKey pins the degradation: with no usable
// key the daemon still starts, and every login verb answers Unavailable with the
// reason a user can act on.
func TestProviderLoginUnavailableWithoutKey(t *testing.T) {
	c := assert.NewAborting(t)
	const why = "RAFIKI_SECRETS_KEY is not set; run rafiki setup or set it in service.env"
	p := testCreds(t, nil, nil, why)

	ok, got := p.Available()
	c.False(ok, "Available")
	c.Eq(why, got, "reason")

	_, err := p.BeginProviderLogin(context.Background(), testOwner, testProvider)
	ce := c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrUnavailable, ce.Code, "BeginProviderLogin code")
	c.Eq(why, ce.Message, "BeginProviderLogin message")

	_, _, err = p.CompleteProviderLogin(context.Background(), testOwner, "login_1", "code=c&state=s")
	ce = c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrUnavailable, ce.Code, "CompleteProviderLogin code")

	err = p.DeleteProviderCredential(context.Background(), testOwner, testProvider)
	ce = c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrUnavailable, ce.Code, "DeleteProviderCredential code")
}

// TestProviderLoginBindsLoginToOwnerAndStoresCredential covers the whole login
// flow against a fake issuer, with the binding this wave had to add: a login id
// begun by one owner answers the SAME NotFound to any other owner, and the
// owner's own completion stores the credential.
func TestProviderLoginBindsLoginToOwnerAndStoresCredential(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	begin, err := p.BeginProviderLogin(context.Background(), testOwner, testProvider)
	c.NoError(err, "BeginProviderLogin")
	c.NotEmpty(begin.LoginID, "login id")
	c.Eq(chatgptauth.CallbackPath, begin.CallbackPath, "callback path")
	c.Eq(uint32(chatgptauth.CallbackPort), begin.RedirectPort, "redirect port")
	c.EqDeep([]string{chatgptauth.CallbackHost}, begin.RedirectHosts, "redirect hosts")

	authURL, err := url.Parse(begin.AuthorizeURL)
	c.NoError(err, "authorize URL must parse")
	query := "code=auth-code&state=" + url.QueryEscape(authURL.Query().Get("state"))

	// Another owner presenting this login id learns nothing: the answer is the
	// same NotFound as an unknown id, and the login stays completable by its
	// own owner.
	_, _, err = p.CompleteProviderLogin(context.Background(), "mallory", begin.LoginID, query)
	ce := c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrNotFound, ce.Code, "a foreign owner must be refused as NotFound")

	provider, accountID, err := p.CompleteProviderLogin(context.Background(), testOwner, begin.LoginID, query)
	c.NoError(err, "CompleteProviderLogin for the owning user")
	c.Eq(testProvider, provider, "provider")
	c.Eq("acct_1", accountID, "account id")

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "the credential must be stored against the owner who logged in")
	c.Eq("acct_1", stored.AccountID, "stored account id")
	c.Eq("rotated-refresh", stored.RefreshToken, "stored refresh token")
	c.Eq(1, store.putCount(), "a completed login is the one path that uses Put")

	// A login is single use: replaying the callback answers NotFound too.
	_, _, err = p.CompleteProviderLogin(context.Background(), testOwner, begin.LoginID, query)
	ce = c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrNotFound, ce.Code, "a replayed login must be NotFound")
}

// TestProviderCredDeleteRevokesBestEffort pins logout: the row goes even when the
// upstream revoke fails, because a local credential the daemon can still use is
// the worse outcome.
func TestProviderCredDeleteRevokesBestEffort(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Hour)))
	iss := newFakeIssuer(t)
	iss.revokeStatus = http.StatusInternalServerError
	p := testCreds(t, store, iss, "")
	p.cachePut(testKey, cachedAccess{token: "cached", accountID: "acct_1", expiry: time.Now().Add(time.Hour)},
		p.generation(testKey))

	err := p.DeleteProviderCredential(context.Background(), testOwner, testProvider)
	c.NoError(err, "a failed revoke must not fail the logout")
	c.Eq(1, iss.revokeCount(), "the revoke must be attempted")

	_, ok := store.row(testOwner, testProvider)
	c.False(ok, "the credential must be deleted")
	_, cached := p.cached(testKey)
	c.False(cached, "the cached access token must be evicted")

	// Deleting again answers NotFound, not an internal error.
	err = p.DeleteProviderCredential(context.Background(), testOwner, testProvider)
	ce := c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrNotFound, ce.Code, "deleting a missing credential")
}

// TestProviderCredDeadOrMissingIsNotACredential pins HasCredential's two
// negative answers, which the spawn path refuses on.
func TestProviderCredDeadOrMissingIsNotACredential(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	p := testCreds(t, store, newFakeIssuer(t), "")

	has, err := p.HasCredential(context.Background(), testOwner, testProvider)
	c.NoError(err, "HasCredential with no row")
	c.False(has, "no row is no credential")

	dead := staleCredential(time.Now().Add(time.Hour))
	dead.Dead = true
	store.seed(dead)
	has, err = p.HasCredential(context.Background(), testOwner, testProvider)
	c.NoError(err, "HasCredential with a dead row")
	c.False(has, "a dead credential is no credential")

	store.seed(staleCredential(time.Now().Add(time.Hour)))
	has, err = p.HasCredential(context.Background(), testOwner, testProvider)
	c.NoError(err, "HasCredential with a live row")
	c.True(has, "a live credential is a credential")
}

// TestProviderCredsAvailable pins the three ways the capability can be off, and
// that each one carries a reason rather than a silent false.
func TestProviderCredsAvailable(t *testing.T) {
	c := assert.NewAborting(t)

	var absent *providerCreds
	ok, why := absent.Available()
	c.False(ok, "a nil engine")
	c.NotEmpty(why, "reason for a nil engine")

	ok, why = testCreds(t, nil, nil, "").Available()
	c.False(ok, "no store")
	c.NotEmpty(why, "reason for a missing store")

	ok, _ = testCreds(t, newFakeCredStore(), newFakeIssuer(t), "").Available()
	c.True(ok, "a wired engine")
}

// TestTokenSourceForcedRefreshDuringRetryAdoptsPendingRotation is fix round 1's
// A1: a rotation the database did not accept must not be lost when a 401
// recovery refreshes during the retry window. The store still holds the SPENT
// refresh token, so sending it again comes back refresh_token_reused, the re-read
// finds the same spent token, and the row is tombstoned while the working token
// sits in the save goroutine.
//
// Since fix round 2 the forced caller adopts the rotation AND refreshes with it,
// because a 401 recovery cannot be answered with an access token the server just
// refused. What must never happen is the SPENT token going upstream again, which
// is what the recorded request bodies prove.
func TestTokenSourceForcedRefreshDuringRetryAdoptsPendingRotation(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1)
	iss := newFakeIssuer(t)
	// The real issuer refuses a spent token, so a second use of `r0` would answer
	// refresh_token_reused and tombstone the row.
	iss.answer = func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, defaultTokenBody("acct_1")
		}
		if call == 2 {
			return http.StatusOK, defaultTokenBody("acct_1")
		}
		return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
	}
	p := testCreds(t, store, iss, "")
	p.retryDelay = time.Hour // hold the save back: the forced refresh must land it

	src := p.TokenSourceFor(testOwner, testProvider)
	tok, _, err := src.Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory despite the failed write")
	c.Eq("new-access", tok, "token")

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("r0", stored.RefreshToken, "the row still holds the spent token")
	if _, ok := p.pendingFor(testKey); !ok {
		c.Fail("a failed write must leave the rotation pending")
	}

	// The 401 recovery arrives before the save has run.
	tok, _, err = src.Token(context.Background(), true)
	c.NoError(err, "the forced refresh must adopt the pending rotation, not die on it")
	c.Eq("new-access", tok, "token")
	c.Eq(2, iss.calls(), "the recovery refreshes once, with the rotation")
	c.Eq("rotated-refresh", iss.body(1)["refresh_token"], "the recovery must refresh with the rotation, never the spent token")

	stored, ok = store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("rotated-refresh", stored.RefreshToken, "the adopted rotation must be stored")
	c.False(stored.Dead, "adopting the rotation must not tombstone the credential")
	if _, ok := p.pendingFor(testKey); ok {
		c.Fail("the adopted rotation is in the row, so nothing may stay pending")
	}
	forced, _, err := src.Token(context.Background(), false)
	c.NoError(err, "a routine refresh after the adoption")
	c.Eq("new-access", forced, "token")
}

// TestTokenSourceForcedAdoptionRefreshesForReal is fix round 2's ruling: when a
// pending rotation supplies the refresh token, a FORCED caller must not be handed
// the old access token (the server just refused it, and the sender treats a
// second 401 as terminal), while a routine caller keeps it.
//
// The pending entry here is seeded directly, so the only upstream call in either
// subtest is the one the forced caller makes.
func TestTokenSourceForcedAdoptionRefreshesForReal(t *testing.T) {
	// The rotation as the TRANSIENT path leaves it: the issuer rotated the
	// refresh token but issued no access token, so the pending credential still
	// carries the old one.
	seedPending := func(p *providerCreds, store *fakeCredStore) {
		cur := staleCredential(time.Now().Add(time.Minute))
		store.seed(cur)
		rotated := cur
		rotated.RefreshToken = "rotated-refresh"
		p.recordRotation(testKey, cur.RefreshToken, rotated)
	}

	t.Run("forced refreshes with the rotation", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")
		seedPending(p, store)

		tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
		c.NoError(err, "the forced refresh must succeed")
		c.Eq("new-access", tok, "a 401 recovery must get the freshly issued access token")
		c.Eq(1, iss.calls(), "exactly one upstream refresh")
		c.Eq("rotated-refresh", iss.body(0)["refresh_token"], "it must refresh with the rotation, not the spent token")

		stored, ok := store.row(testOwner, testProvider)
		c.True(ok, "row still present")
		c.Eq("rotated-refresh", stored.RefreshToken, "the row is rotated")
		c.False(stored.Dead, "a recovery must not tombstone the credential")
		if _, ok := p.pendingFor(testKey); ok {
			c.Fail("the rotation is in the row, so nothing may stay pending")
		}
	})

	t.Run("routine keeps the working token", func(t *testing.T) {
		c := assert.NewAborting(t)
		store := newFakeCredStore()
		iss := newFakeIssuer(t)
		p := testCreds(t, store, iss, "")
		seedPending(p, store)

		tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		c.NoError(err, "the routine call must succeed")
		c.Eq("old-access", tok, "a routine caller keeps the access token that was working")
		c.Eq(0, iss.calls(), "and spends nothing upstream")

		stored, ok := store.row(testOwner, testProvider)
		c.True(ok, "row still present")
		c.Eq("rotated-refresh", stored.RefreshToken, "the rotation still lands, so the next refresh has a usable token")
	})
}

// TestTokenSourceRetryDoesNotClobberANewerLogin is the re-login-during-retry
// case: a save that started before another login must never undo it. The guard
// is the row's refresh token, not a timer.
func TestTokenSourceRetryDoesNotClobberANewerLogin(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1)
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")
	p.retryDelay = 100 * time.Millisecond

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")

	// The owner logs in again while the save is still waiting: a fresh row, and
	// the daemon forgetting everything it remembered about the old one.
	login := providercreds.Credential{
		OwnerUserID:  testOwner,
		Provider:     testProvider,
		AccountID:    "acct_1",
		RefreshToken: "login-refresh",
		AccessToken:  "login-access",
		AccessExpiry: time.Now().Add(time.Hour),
		LastRefresh:  time.Now(),
	}
	c.NoError(store.Put(context.Background(), login), "Put")
	p.forget(testKey)

	// Wait for the save to have attempted its write, then check it left the row
	// alone. Without the row guard it would write the OLD rotation over the
	// login's tokens.
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		return store.refreshCallCount() >= 2
	}, "the save must attempt its write")

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("login-refresh", stored.RefreshToken, "a save must never overwrite a newer login")
	c.Eq("login-access", stored.AccessToken, "a save must never overwrite a newer login")
}

// TestTokenSourceForcedFlightIsNotShared pins the 401 recovery owning its
// refresh: a forced caller that joined an ordinary flight would be handed the
// very token the server just refused.
func TestTokenSourceForcedFlightIsNotShared(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	release := make(chan struct{})
	iss.block = release
	p := testCreds(t, store, iss, "")
	src := p.TokenSourceFor(testOwner, testProvider)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	call := func(force bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := src.Token(context.Background(), force); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	call(false)
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool { return iss.calls() == 1 },
		"the ordinary refresh must reach the issuer")
	call(true) // must NOT join the ordinary flight
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	c.Len(errs, 0, "Token errors: %v", errs)
	c.Eq(2, iss.calls(), "a forced refresh must make its own upstream call")
}

// TestProviderCredsWaitDrainsInFlightSave is the shutdown rule: cancel the
// daemon context and Wait, and the rotation still reaches the row before the
// caller closes the pool. The write survives the cancelled context
// (WithoutCancel), and Wait is what gives it the time.
func TestProviderCredsWaitDrainsInFlightSave(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1)
	iss := newFakeIssuer(t)
	p := newProviderCreds(ctx, store, iss.engine(), "", testProviderSet(t))
	// Long enough that the save cannot land before the cancel below: it fires on
	// bg.Done instead, which is the shutdown path.
	p.retryDelay = 2 * time.Second

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")

	cancel()
	p.Wait()

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("rotated-refresh", stored.RefreshToken, "the save must land before Wait returns")
}

// TestProviderLoginReplacesCachedToken kills the "no eviction after a login"
// mutation: an access token cached for the key must not survive the login that
// replaced the account behind it.
func TestProviderLoginReplacesCachedToken(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	// A previous account's token, cached and healthy.
	p.cachePut(testKey, cachedAccess{
		token:     "previous-account-access",
		accountID: "acct_previous",
		expiry:    time.Now().Add(time.Hour),
	}, p.generation(testKey))
	if _, ok := p.cached(testKey); !ok {
		c.Fail("fixture: the previous token must be cached")
	}

	begin, err := p.BeginProviderLogin(context.Background(), testOwner, testProvider)
	c.NoError(err, "BeginProviderLogin")
	authURL, err := url.Parse(begin.AuthorizeURL)
	c.NoError(err, "authorize URL must parse")
	query := "code=auth-code&state=" + url.QueryEscape(authURL.Query().Get("state"))
	_, accountID, err := p.CompleteProviderLogin(context.Background(), testOwner, begin.LoginID, query)
	c.NoError(err, "CompleteProviderLogin")
	c.Eq("acct_1", accountID, "account id")

	if _, ok := p.cached(testKey); ok {
		c.Fail("a login must evict the previous account's cached token")
	}
	tok, headers, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "Token after the login")
	c.Eq("new-access", tok, "the login's own access token")
	c.Eq("acct_1", headers.Get(chatGPTAccountHeader), "the login's own account")
}

// TestProviderLoginRefusesLoginWithoutAccountID pins the account pin at login
// time: a credential that cannot be pinned to an account is refused, rather than
// stored and discovered on the first turn.
func TestProviderLoginRefusesLoginWithoutAccountID(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	iss := newFakeIssuer(t)
	iss.answer = func(int) (int, string) {
		// A healthy token response carrying an id_token with no account claim.
		return http.StatusOK, fmt.Sprintf(
			`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"id_token":%q}`,
			idTokenWithoutAccount())
	}
	p := testCreds(t, store, iss, "")

	begin, err := p.BeginProviderLogin(context.Background(), testOwner, testProvider)
	c.NoError(err, "BeginProviderLogin")
	authURL, err := url.Parse(begin.AuthorizeURL)
	c.NoError(err, "authorize URL must parse")
	query := "code=auth-code&state=" + url.QueryEscape(authURL.Query().Get("state"))

	_, _, err = p.CompleteProviderLogin(context.Background(), testOwner, begin.LoginID, query)
	ce := c.ErrorAs[*connectapi.ControllerError](err)
	c.Eq(protocol.ErrFailedPrecondition, ce.Code, "code")
	c.StrContains(ce.Message, "pinned", "the error must say why an account id is required")

	if _, ok := store.row(testOwner, testProvider); ok {
		c.Fail("a login with no account id must not be stored")
	}
	c.Eq(0, store.putCount(), "a login with no account id must not be stored")
}

// idTokenWithoutAccount is an id_token whose payload carries no ChatGPT account
// claim, which is the shape that leaves Tokens.AccountID empty.
func idTokenWithoutAccount() string {
	payload := `{"sub":"user-1","email":"alice@example.test"}`
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestCachedAccessRedactsToken is the same leak-closing rule
// providercreds.Credential follows: %v and slog must never render a live token.
func TestCachedAccessRedactsToken(t *testing.T) {
	c := assert.NewAborting(t)
	tok := cachedAccess{token: "sekrit-access-token", accountID: "acct_1", expiry: time.Now()}

	for _, rendered := range []string{
		fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), tok.String(),
	} {
		c.NotStrContains(rendered, "sekrit-access-token", "rendering: %s", rendered)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cache", "access", tok)
	c.NotStrContains(buf.String(), "sekrit-access-token", "slog output: %s", buf.String())
	c.StrContains(buf.String(), redactedToken, "slog output names the redaction")
}

// TestCachePutLosesToAForget is the D1 race at its narrowest: a refresh reads the
// generation before it starts, a logout forgets the key while it runs, and its
// late cachePut must then be discarded. Without that test, a logged-out (or
// previous-account) access token stays served from cache until it expires, and
// the design's "the next turn fails with the re-login error" is a promise the
// daemon does not keep.
func TestCachePutLosesToAForget(t *testing.T) {
	c := assert.NewAborting(t)
	p := testCreds(t, newFakeCredStore(), newFakeIssuer(t), "")
	tok := cachedAccess{token: "late-access", accountID: "acct_1", expiry: time.Now().Add(time.Hour)}

	// A flight that captured the generation, then lost the race to a logout.
	gen := p.generation(testKey)
	p.forget(testKey)
	p.cachePut(testKey, tok, gen)
	if _, ok := p.cached(testKey); ok {
		c.Fail("a cache write from before the forget must be discarded")
	}

	// The same write on the CURRENT generation must store, otherwise this test
	// would pass on an implementation that never caches anything.
	p.cachePut(testKey, tok, p.generation(testKey))
	if _, ok := p.cached(testKey); !ok {
		c.Fail("a cache write on the current generation must be stored")
	}
}

// ─── Rotations that must never be lost ───────────────────────────────────────

// blockStore hangs chosen Refresh calls until release, so a test can hold a
// background save inside the store the way a hung database would. Calls are
// numbered from one across every method call on this wrapper.
type blockStore struct {
	*fakeCredStore
	from, to int // 1-based call numbers to hang: [from, to)

	mu      sync.Mutex
	calls   int
	block   chan struct{}
	reached chan struct{}
	once    sync.Once
}

func newBlockStore(inner *fakeCredStore, from, to int) *blockStore {
	return &blockStore{
		fakeCredStore: inner,
		from:          from,
		to:            to,
		block:         make(chan struct{}),
		reached:       make(chan struct{}),
	}
}

func (s *blockStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n >= s.from && n < s.to {
		s.once.Do(func() { close(s.reached) })
		<-s.block
	}
	return s.fakeCredStore.Refresh(ctx, owner, provider, fn)
}

// waitHeld fails unless the store call under test has reached its hang.
func (s *blockStore) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the store call under test never hung")
	}
}

func (s *blockStore) release() { close(s.block) }

var _ providercreds.Store = (*blockStore)(nil)

// gateStore holds chosen Refresh calls until the test releases them, and
// reports when each one reaches its gate and when it finishes. Two saves can
// then be ordered exactly, rather than by racing timers.
type gateStore struct {
	*fakeCredStore
	mu     sync.Mutex
	calls  int
	gates  map[int]chan struct{}
	opened map[int]chan struct{}
	done   map[int]chan struct{}
}

func newGateStore(inner *fakeCredStore, calls ...int) *gateStore {
	s := &gateStore{
		fakeCredStore: inner,
		gates:         map[int]chan struct{}{},
		opened:        map[int]chan struct{}{},
		done:          map[int]chan struct{}{},
	}
	for _, n := range calls {
		s.gates[n] = make(chan struct{})
		s.opened[n] = make(chan struct{})
		s.done[n] = make(chan struct{})
	}
	return s
}

func (s *gateStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	gate, opened, done := s.gates[n], s.opened[n], s.done[n]
	s.mu.Unlock()
	if opened != nil {
		close(opened)
		<-gate
	}
	next, err := s.fakeCredStore.Refresh(ctx, owner, provider, fn)
	if done != nil {
		close(done)
	}
	return next, err
}

// waitOpened fails unless call n has reached its gate.
func (s *gateStore) waitOpened(t *testing.T, n int) {
	t.Helper()
	select {
	case <-s.opened[n]:
	case <-time.After(5 * time.Second):
		t.Fatalf("store call %d never reached its gate", n)
	}
}

// release opens call n's gate and waits for that call to return.
func (s *gateStore) release(t *testing.T, n int) {
	t.Helper()
	s.waitOpened(t, n)
	close(s.gates[n])
	select {
	case <-s.done[n]:
	case <-time.After(5 * time.Second):
		t.Fatalf("store call %d never finished", n)
	}
}

var _ providercreds.Store = (*gateStore)(nil)

// notPersistedHookStore runs a hook the moment the wrapped store reports
// ErrNotPersisted: after the row lock has been released and before the caller
// sees the error. That is the A1-b gap made observable.
type notPersistedHookStore struct {
	*fakeCredStore
	once sync.Once
	hook func()
}

func (s *notPersistedHookStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	next, err := s.fakeCredStore.Refresh(ctx, owner, provider, fn)
	if errors.Is(err, providercreds.ErrNotPersisted) {
		s.once.Do(s.hook)
	}
	return next, err
}

var _ providercreds.Store = (*notPersistedHookStore)(nil)

// TestTokenSourceTwoHandOffsNeverLoseTheNewestRotation is A1-a. A routine
// refresh rotates the token and cannot write it; a 401 recovery adopts that
// rotation, rotates again, and cannot write that either. Two hand-offs now
// describe the same spent row token, and the OLDER one must neither write (it
// would put a spent token in the row) nor clear the newer one (which would drop
// the only live token).
//
// Both saves are gated, so the interleaving is exact: each hand-off is recorded
// while the other save is held in the store, and only then is either let write.
func TestTokenSourceTwoHandOffsNeverLoseTheNewestRotation(t *testing.T) {
	c := assert.NewAborting(t)
	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		switch call {
		case 1:
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		case 2:
			return http.StatusOK, tokenBody("access-b", "refresh-b", "acct_1")
		case 3:
			return http.StatusOK, tokenBody("access-c", "refresh-c", "acct_1")
		}
		// Any later call is spending a token the issuer has already retired.
		return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
	}
	// Call 1 is the routine transaction, 2 the first save, 3 the recovery's
	// transaction, 4 the second save.
	store := newGateStore(inner, 2, 4)
	p := testCreds(t, inner, iss, "")
	p.store = store
	p.retryDelay = time.Millisecond
	src := p.TokenSourceFor(testOwner, testProvider)

	tok, _, err := src.Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	c.Eq("access-a", tok, "token")
	store.waitOpened(t, 2) // the first save holds; the row still has r0

	inner.failNextWrite(1) // the recovery's write fails too
	tok, _, err = src.Token(context.Background(), true)
	c.NoError(err, "the recovery adopts the rotation and refreshes with it")
	c.Eq("access-b", tok, "a 401 recovery gets the freshly issued token")
	c.Eq("refresh-a", iss.body(1)["refresh_token"], "the recovery must spend the rotation, never the spent r0")

	// The second save holds too, so BOTH hand-offs now exist. Let the older one
	// write first: it must refuse, because its rotation has been superseded.
	store.waitOpened(t, 4)
	store.release(t, 2)
	store.release(t, 4)

	c.EventuallyTrue(time.Second, time.Millisecond, func() bool {
		stored, ok := inner.row(testOwner, testProvider)
		return ok && stored.RefreshToken == "refresh-b"
	}, "the newest rotation must reach the row")
	stored, ok := inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.False(stored.Dead, "a second hand-off must not kill the credential")
	c.EventuallyTrue(time.Second, time.Millisecond, func() bool {
		_, ok := p.pendingFor(testKey)
		return !ok
	}, "a landed rotation must leave nothing pending")

	// And the row is usable: the next refresh spends refresh-b.
	tok, _, err = src.Token(context.Background(), true)
	c.NoError(err, "a refresh after both hand-offs must succeed")
	c.Eq("access-c", tok, "token")
	c.Eq("refresh-b", iss.body(2)["refresh_token"], "the next refresh spends the newest token")
}

// TestTokenSourceHandOffExistsWhenTheRowLockIsReleased is A1-b. The store
// releases the row lock before Refresh reports ErrNotPersisted, so a 401
// recovery waiting on that lock enters the gap. A hand-off recorded only after
// the store returned would be invisible there, and the recovery would spend the
// token the issuer has already retired.
func TestTokenSourceHandOffExistsWhenTheRowLockIsReleased(t *testing.T) {
	c := assert.NewAborting(t)
	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		switch call {
		case 1:
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		case 2:
			return http.StatusOK, tokenBody("access-b", "refresh-b", "acct_1")
		}
		return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
	}
	p := testCreds(t, inner, iss, "")
	p.retryDelay = time.Hour // this test is about the gap, not the retry

	var (
		recoveryTok string
		recoveryErr error
	)
	// The hook runs at the exact point the real store has released the row lock
	// and reported ErrNotPersisted: before the caller could record anything.
	p.store = &notPersistedHookStore{fakeCredStore: inner, hook: func() {
		recoveryTok, _, recoveryErr = p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	}}

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	c.NoError(recoveryErr, "the 401 recovery must find the rotation that is already in memory")
	c.Eq("access-b", recoveryTok, "the recovery gets its own freshly issued token")
	c.Eq("refresh-a", iss.body(1)["refresh_token"], "the recovery must spend the rotation, never the spent r0")

	stored, ok := inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.False(stored.Dead, "a credential that works must not be tombstoned")
	c.Eq("refresh-b", stored.RefreshToken, "the recovery's rotation reaches the row")
}

// TestTokenSourceGiveUpKeepsTheRotation is A1-c. A save that exhausts its
// retries must KEEP the hand-off: it holds the only live refresh token this
// credential has. Dropping it hands the next refresh a token the issuer already
// retired, which tombstones a login that works.
func TestTokenSourceGiveUpKeepsTheRotation(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1 + persistTries)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusOK, tokenBody("access-b", "refresh-b", "acct_1")
	}
	p := testCreds(t, store, iss, "")
	p.retryDelay = time.Microsecond // the backoff is not what this test is about

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")

	// One transaction plus persistTries save attempts, all refused.
	c.EventuallyTrue(5*time.Second, time.Millisecond, func() bool {
		return store.refreshCallCount() >= 1+persistTries
	}, "the save must exhaust its retries")
	time.Sleep(50 * time.Millisecond) // let the give-up complete

	if _, ok := p.pendingFor(testKey); !ok {
		c.Fail("a save that gives up must keep the rotation in memory")
	}

	tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.NoError(err, "a recovery after the give-up must adopt the kept rotation")
	c.Eq("access-b", tok, "token")
	c.Eq("refresh-a", iss.body(1)["refresh_token"], "the recovery must spend the kept rotation, never r0")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.False(stored.Dead, "the credential must survive")
	c.Eq("refresh-b", stored.RefreshToken, "the adopted rotation lands")
}

// TestTokenSourceForcedRecoveryNeverServesTheRefusedToken is I5. A 401 recovery
// must come back with a token the server has not just refused. After a save has
// given up, the rotation in memory is the only live refresh token, so the
// recovery adopts it and refreshes for real: never the row's access token, never
// the cached one.
func TestTokenSourceForcedRecoveryNeverServesTheRefusedToken(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	// Inside the refresh window, so the routine call refreshes and its write
	// fails: old-access is already spent by the server, cached-access cannot be
	// written, and the rotation is the only live refresh token.
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1 + persistTries)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("cached-access", "refresh-a", "acct_1")
		}
		return http.StatusOK, tokenBody("recovered-access", "refresh-b", "acct_1")
	}
	p := testCreds(t, store, iss, "")
	p.retryDelay = time.Microsecond
	src := p.TokenSourceFor(testOwner, testProvider)

	tok, _, err := src.Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	c.Eq("cached-access", tok, "the failed write still serves turns")
	if _, ok := p.cached(testKey); !ok {
		c.Fail("fixture: the refreshed access token must be cached")
	}

	c.EventuallyTrue(5*time.Second, time.Millisecond, func() bool {
		return store.refreshCallCount() >= 1+persistTries
	}, "the save must exhaust its retries")
	time.Sleep(50 * time.Millisecond)

	// The server refused the cached token, so this is a 401 recovery.
	tok, _, err = src.Token(context.Background(), true)
	c.NoError(err, "the recovery must succeed from the kept rotation")
	c.Eq("recovered-access", tok, "a forced caller gets a token the server has not refused")
	c.NotEq("cached-access", tok, "never the refused cached token")
	c.NotEq("old-access", tok, "never the stored token")
	c.Eq("refresh-a", iss.body(1)["refresh_token"], "the recovery refreshes with the kept rotation")
}

// TestTokenSourceSaveDoesNotClobberAnotherDaemonsRotation pins the save's second
// guard. The hand-off is still this daemon's current one, but the row moved on:
// another daemon rotated the credential while this save waited. Writing our want
// over it would undo their rotation.
func TestTokenSourceSaveDoesNotClobberAnotherDaemonsRotation(t *testing.T) {
	c := assert.NewAborting(t)
	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	store := newGateStore(inner, 2)
	p := testCreds(t, inner, iss, "")
	p.store = store
	p.retryDelay = time.Millisecond

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	store.waitOpened(t, 2) // the save holds, and its hand-off is still current

	// Another daemon commits its own rotation: only the row moves, so the
	// hand-off is still current and only the row guard can stop the write.
	other := staleCredential(time.Now().Add(time.Hour))
	other.RefreshToken = "other-daemon-refresh"
	other.AccessToken = "other-daemon-access"
	inner.seed(other)

	store.release(t, 2)
	c.EventuallyTrue(time.Second, time.Millisecond, func() bool {
		_, ok := p.pendingFor(testKey)
		return !ok
	}, "a hand-off the row has moved past must be cleared")

	stored, ok := inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("other-daemon-refresh", stored.RefreshToken, "a save must never undo another daemon's rotation")
	c.Eq("other-daemon-access", stored.AccessToken, "a save must never undo another daemon's rotation")
}

// TestProviderCredsWaitIsBounded pins the drain's upper bound. A save makes at
// most persistTries attempts, each bounded only by refreshTimeout, so a database
// that HANGS rather than refusing must not hold shutdown for hours per key. What
// the drain gave up on is logged by key, never by token.
func TestProviderCredsWaitIsBounded(t *testing.T) {
	c := assert.NewAborting(t)

	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	ctx, cancel := context.WithCancel(context.Background())
	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	p := newProviderCreds(ctx, inner, iss.engine(), "", testProviderSet(t))
	p.retryDelay = time.Millisecond
	p.drainWait = 100 * time.Millisecond
	// The save's first attempt hangs inside the store: a database that stopped
	// answering, not one that refuses.
	store := newBlockStore(inner, 2, 3)
	p.store = store

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	store.waitHeld(t)

	cancel()
	start := time.Now()
	p.Wait()
	elapsed := time.Since(start)
	c.True(elapsed < 2*time.Second, "Wait must be bounded; it took %s", elapsed)

	if _, ok := p.pendingFor(testKey); !ok {
		c.Fail("the unsaved rotation must still be identifiable after the timeout")
	}
	c.StrContains(logs.String(), "shutdown drain timed out", "the timeout must be logged: %s", logs.String())
	c.StrContains(logs.String(), testOwner+"/"+testProvider, "the log names the key it gave up on: %s", logs.String())
	c.NotStrContains(logs.String(), "access", "the log must not name a token: %s", logs.String())

	// Unblock the save so nothing outlives the test.
	store.release()
	p.wg.Wait()
}

// TestProviderCredsWaitRefusesNewFlights pins the draining guard: once Wait has
// begun no new flight may be added to the WaitGroup, because a WaitGroup must
// never Add from zero while Wait is blocked.
func TestProviderCredsWaitRefusesNewFlights(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	p := newProviderCreds(ctx, inner, iss.engine(), "", testProviderSet(t))
	p.retryDelay = time.Millisecond
	// Only the save's first attempt hangs; anything else the store handles.
	store := newBlockStore(inner, 2, 3)
	p.store = store

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	store.waitHeld(t)

	waited := make(chan struct{})
	go func() {
		p.Wait()
		close(waited)
	}()
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		p.workMu.Lock()
		defer p.workMu.Unlock()
		return p.draining
	}, "Wait must begin the drain")

	_, _, err = p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.ErrorIs(err, errProviderCredsDraining, "a flight must be refused once the drain has begun")

	store.release()
	cancel()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		c.Fail("Wait never returned after the drain finished")
	}
}

// TestProviderCredsShutdownDoesNotAbortAnInFlightRefresh is the shutdown half
// of "a rotated token is never lost". A graceful shutdown cancels the daemon
// context, and the issuer call must NOT die with it: the issuer has already
// rotated, so an aborted request drops the only working refresh token and the
// next boot gets refresh_token_reused and tombstones a login that works. The
// call keeps its own deadline, and the drain waits for the rotation to land.
func TestProviderCredsShutdownDoesNotAbortAnInFlightRefresh(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	iss := newFakeIssuer(t)
	release := make(chan struct{})
	iss.block = release
	p := newProviderCreds(ctx, store, iss.engine(), "", testProviderSet(t))
	p.retryDelay = time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
		done <- err
	}()
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool { return iss.calls() == 1 },
		"the refresh must reach the issuer")

	// The daemon starts going down while the issuer has not answered yet.
	cancel()
	waited := make(chan struct{})
	go func() {
		p.Wait()
		close(waited)
	}()
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		p.workMu.Lock()
		defer p.workMu.Unlock()
		return p.draining
	}, "Wait must begin the drain before the issuer answers")
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		c.Fail("the in-flight refresh never returned")
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		c.Fail("Wait never returned after the refresh landed")
	}

	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("rotated-refresh", stored.RefreshToken, "the rotation the issuer completed must reach the row")
	c.Eq("new-access", stored.AccessToken, "and its access token with it")
	c.False(stored.Dead, "a shutdown must not tombstone the credential")
	if _, ok := p.pendingFor(testKey); ok {
		c.Fail("a landed rotation must leave nothing pending")
	}
}

// TestTokenSourceSaveRevivesARowTombstonedByAnotherDaemon is the two-daemon
// interleaving I1 does not cover. Daemon A rotates r0 to refresh-a and cannot
// write; daemon B (separate in-memory state, same row) then spends r0, is told
// refresh_token_reused, re-reads the unchanged row and tombstones it. A's save
// must recognise the tombstone as false: the row died on exactly the token A
// holds the successor to, so the pending rotation is written and dead_at
// cleared, rather than dropped with the only working login in it.
func TestTokenSourceSaveRevivesARowTombstonedByAnotherDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		switch call {
		case 1:
			// A's rotation: r0 is spent from here on.
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		case 2:
			// B spends the token A already spent.
			return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
		}
		return http.StatusOK, tokenBody("access-c", "refresh-c", "acct_1")
	}

	// A's first save is held in the store, so B's refresh happens while A still
	// holds refresh-a.
	gate := newGateStore(inner, 2)
	a := testCreds(t, inner, iss, "")
	a.store = gate
	a.retryDelay = time.Millisecond
	b := testCreds(t, inner, iss, "")

	_, _, err := a.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "A's first refresh answers from memory")
	gate.waitOpened(t, 2)
	if _, ok := a.pendingFor(testKey); !ok {
		c.Fail("fixture: A must hold the unpersisted rotation")
	}

	_, _, err = b.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.Error(err, "B has no valid login left to offer")
	stored, ok := inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "fixture: B tombstoned the row with the spent token")

	gate.release(t, 2)
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		row, ok := inner.row(testOwner, testProvider)
		return ok && !row.Dead
	}, "A's save must revive the row it holds the successor for")
	c.Eq(1, inner.reviveCount(), "exactly one revival")

	stored, ok = inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("refresh-a", stored.RefreshToken, "the row now holds A's rotation")
	c.Eq("access-a", stored.AccessToken, "and its access token")
	if _, ok := a.pendingFor(testKey); ok {
		c.Fail("a revived rotation must leave nothing pending")
	}
	// The row is written before the log line, so wait for the line itself.
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		return strings.Contains(logs.String(), "revived a provider credential")
	}, "the revival must be logged: %s", logs.String())
	c.StrContains(logs.String(), testOwner, "the log names the owner: %s", logs.String())
	c.StrContains(logs.String(), testProvider, "the log names the provider: %s", logs.String())
	c.NotStrContains(logs.String(), "refresh-a", "the log must never name a token: %s", logs.String())

	// The resurrected row is usable: the next refresh spends the held rotation,
	// which is the whole point of keeping it.
	tok, _, err := a.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.NoError(err, "the revived credential must serve a recovery")
	c.Eq("access-c", tok, "token")
	c.Eq("refresh-a", iss.body(2)["refresh_token"], "the recovery spends the token A held, not the spent one")
}

// TestTokenSourceSaveLeavesADeadRowThatIsNotOurs is the other side of the
// revival rule: a dead row the rotation in hand cannot contradict stays dead.
// Another daemon's rotation moved the row on before it died, so the tombstone
// is not about A's token, and A's save must drop its record rather than
// overwrite a login someone else has since replaced.
func TestTokenSourceSaveLeavesADeadRowThatIsNotOurs(t *testing.T) {
	c := assert.NewAborting(t)
	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	inner := newFakeCredStore()
	inner.seed(staleCredential(time.Now().Add(time.Minute)))
	inner.failNextWrite(1)
	iss := newFakeIssuer(t)
	iss.answer = func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusOK, tokenBody("access-b", "refresh-b", "acct_1")
	}
	gate := newGateStore(inner, 2)
	p := testCreds(t, inner, iss, "")
	p.store = gate
	p.retryDelay = time.Millisecond

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	gate.waitOpened(t, 2)

	// Another daemon rotated the row AND it is now dead: not on the token this
	// hand-off replaced, so this save has nothing to resurrect.
	other := staleCredential(time.Now().Add(time.Hour))
	other.RefreshToken = "other-daemon-refresh"
	other.AccessToken = "other-daemon-access"
	other.Dead = true
	inner.seed(other)

	gate.release(t, 2)
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		_, ok := p.pendingFor(testKey)
		return !ok
	}, "a hand-off that cannot be written must be cleared")

	c.Eq(0, inner.reviveCount(), "a tombstone on another token is never revived")
	stored, ok := inner.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "the row stays dead")
	c.Eq("other-daemon-refresh", stored.RefreshToken, "the row keeps the other daemon's token")
	c.EventuallyTrue(2*time.Second, time.Millisecond, func() bool {
		return strings.Contains(logs.String(), "the refreshed token was lost")
	}, "the loss must be logged: %s", logs.String())
	c.NotStrContains(logs.String(), "refresh-a", "the log must never name a token: %s", logs.String())
}

// heldRotationCreds is the pose every revival order starts from: a rotation the
// issuer completed whose write never landed, so refresh-a exists only in the
// hand-off while the row still holds the spent r0.
func heldRotationCreds(t *testing.T, answer func(call int) (int, string)) (*fakeCredStore, *fakeIssuer, *providerCreds) {
	return heldRotation(t, nil, answer)
}

// heldRotation is heldRotationCreds with an optional store wrapper in front of
// the fake, for the orders that need a hook in the tombstone's window.
func heldRotation(t *testing.T, wrap func(*fakeCredStore) providercreds.Store, answer func(call int) (int, string)) (*fakeCredStore, *fakeIssuer, *providerCreds) {
	t.Helper()
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failNextWrite(1 + persistTries)
	iss := newFakeIssuer(t)
	iss.answer = answer
	var wrapped providercreds.Store = store
	if wrap != nil {
		wrapped = wrap(store)
	}
	p := testCreds(t, wrapped, iss, "")
	p.retryDelay = time.Microsecond

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), false)
	c.NoError(err, "the first refresh answers from memory")
	c.EventuallyTrue(5*time.Second, time.Millisecond, func() bool {
		return store.refreshCallCount() >= 1+persistTries
	}, "the save must exhaust its retries")
	time.Sleep(50 * time.Millisecond)
	if _, ok := p.pendingFor(testKey); !ok {
		c.Fail("fixture: the give-up must keep the rotation")
	}
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.False(stored.Dead, "fixture: the row is still live on the spent token")
	c.Eq("r0", stored.RefreshToken, "fixture: the row still holds the token the rotation replaced")
	return store, iss, p
}

// TestTokenSourceRevivesOnlyATombstoneThatPredatesTheRefresh pins the judgment
// the store's token comparison cannot make: whether a daemon looking at a dead
// row is seeing a tombstone ANOTHER holder wrote or one its OWN refresh just
// wrote.
//
// The starting pose is a save that gave up holding refresh-a, so the row still
// holds the spent r0 and refresh-a survives nowhere but the hand-off. A forced
// refresh then ADOPTS refresh-a and spends it. From there the two orders
// diverge:
//
//   - the row was already dead when the transaction began (another daemon's
//     refresh_token_reused): that tombstone is about r0, the token this daemon
//     holds the successor to, so it is contradicted and the row is revived with
//     refresh-a;
//   - the issuer REFUSES the adopted refresh-a, or answers for a different
//     account: THIS callback tombstoned the row, and the store's markDead writes
//     dead_at only, leaving r0 in the token column. A revival judged on the row
//     alone would read that tombstone as another holder's and write the refused
//     refresh-a back into a live credential, which is a login that fails on
//     every later turn instead of a login that asks for one.
func TestTokenSourceRevivesOnlyATombstoneThatPredatesTheRefresh(t *testing.T) {
	t.Run("another holder's tombstone before the transaction revives", func(t *testing.T) {
		c := assert.NewAborting(t)
		store, iss, p := heldRotationCreds(t, func(call int) (int, string) {
			if call == 1 {
				return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
			}
			return http.StatusOK, tokenBody("access-b", "refresh-b", "acct_1")
		})

		// The tombstone is already in the row when this transaction begins: the
		// store answers ErrDead without calling the refresh callback at all, so
		// nothing this daemon did in this transaction wrote it.
		dead := staleCredential(time.Now().Add(time.Minute))
		dead.Dead = true
		store.seed(dead)

		tok, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
		c.NoError(err, "a held rotation makes the tombstone false")
		c.Eq("access-a", tok, "the rotation's own access token")
		c.Eq(1, store.reviveCount(), "the row is revived, not forgotten")

		stored, ok := store.row(testOwner, testProvider)
		c.True(ok, "row still present")
		c.False(stored.Dead, "the row is live again")
		c.Eq("refresh-a", stored.RefreshToken, "and holds the held rotation")
		if _, ok := p.pendingFor(testKey); ok {
			c.Fail("a revived rotation must leave nothing pending")
		}

		// The resurrected row is usable: the next recovery spends the rotation
		// whose tombstone was false, which is the whole point of keeping it.
		tok, _, err = p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
		c.NoError(err, "the revived credential must serve a recovery")
		c.Eq("access-b", tok, "token")
		c.Eq("refresh-a", iss.body(1)["refresh_token"], "the recovery spends the token this daemon held")
	})

	for _, tc := range []struct {
		name   string
		answer func(call int) (int, string)
	}{
		{
			// The user revoked the grant: the adopted token is genuinely dead.
			name: "the issuer refuses the adopted rotation",
			answer: func(call int) (int, string) {
				if call == 1 {
					return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
				}
				return http.StatusBadRequest, `{"error":"invalid_grant"}`
			},
		},
		{
			// The grant now belongs to another account: a deliberate tombstone.
			name: "the adopted rotation names another account",
			answer: func(call int) (int, string) {
				if call == 1 {
					return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
				}
				return http.StatusOK, tokenBody("access-x", "refresh-x", "acct_2")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			logs := &syncBuffer{}
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })

			store, iss, p := heldRotationCreds(t, tc.answer)
			// The refused rotation must already be gone when the tombstone becomes
			// visible to any other store call: a save still retrying it is a
			// separate transaction, reads that tombstone as another holder's, and
			// would otherwise revive the row with the token the issuer just refused.
			var (
				sawTombstone   bool
				pendingAtDeath bool
			)
			store.setOnDead(func() {
				sawTombstone = true
				_, pendingAtDeath = p.pendingFor(testKey)
			})

			_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
			c.Error(err, "a refused rotation leaves nothing usable")

			c.Eq(0, store.reviveCount(), "a row this daemon's own refresh tombstoned must not be revived")
			c.True(sawTombstone, "fixture: this refresh must be the one that tombstoned the row")
			c.False(pendingAtDeath, "the refused rotation is dropped before the tombstone is visible")
			stored, ok := store.row(testOwner, testProvider)
			c.True(ok, "row still present")
			c.True(stored.Dead, "the row stays dead: the owner has to log in again")
			c.Eq("r0", stored.RefreshToken, "the row keeps the token the refused rotation replaced")
			c.NotStrContains(logs.String(), "revived a provider credential",
				"no revival may be logged: %s", logs.String())
			if _, ok := p.pendingFor(testKey); ok {
				c.Fail("the refused rotation must be dropped, never adopted again")
			}

			// And a later recovery does not resurrect it either: nothing is left
			// to adopt, and the dead row stays dead.
			_, _, err = p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
			c.Error(err, "a dead credential keeps asking for a login")
			c.Eq(0, store.reviveCount(), "a second recovery revives nothing")
			c.Eq(2, iss.calls(), "neither recovery spends a refused token upstream")
			stored, ok = store.row(testOwner, testProvider)
			c.True(ok, "row still present")
			c.True(stored.Dead, "still dead")
		})
	}
}

// TestTokenSourceOwnTombstoneIsNeverRevivedWithARecordInHand pins the rule on
// its own, with the record the rejection drops put back: even when a hand-off
// replacing the row's spent token is in hand, a tombstone THIS callback wrote is
// not revived. The rejection path drops that hand-off under the row lock, so
// only a rejection site that forgot to drop it reaches this state; the guard is
// what keeps that mistake from resurrecting a refused login.
func TestTokenSourceOwnTombstoneIsNeverRevivedWithARecordInHand(t *testing.T) {
	c := assert.NewAborting(t)
	store, _, p := heldRotationCreds(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	})
	// The rotation the rejection drops, reinstated as this refresh's own
	// tombstone becomes visible: it replaces r0, the token the row died on, so
	// the store alone would read that tombstone as another holder's.
	refused := staleCredential(time.Now().Add(time.Hour))
	refused.RefreshToken, refused.AccessToken = "refresh-a", "access-a"
	store.setOnDead(func() {
		p.recordRotation(testKey, "r0", refused)
	})

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.Error(err, "the refused rotation leaves nothing usable")
	c.Eq(0, store.reviveCount(), "a tombstone this callback wrote is never revived, record or no record")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "the row stays dead")
	c.Eq("r0", stored.RefreshToken, "the refused rotation is not written back into the row")
}

// afterOwnDeadStore runs a hook ONCE, synchronously, after a Refresh whose
// callback returned ErrDead has COMMITTED the tombstone and released the row
// lock, and before that error reaches the caller. That is the window a second
// transaction runs in: a save still retrying the record, or a concurrent
// refresh of the same key. It sits OUTSIDE fakeCredStore because the hook
// itself calls the store, and the fake's own onDead hook runs under its lock.
//
// A Refresh whose callback did not run is never hooked, so a nested call made
// from the hook (a dead row is answered before the callback) cannot recurse.
type afterOwnDeadStore struct {
	*fakeCredStore
	once sync.Once
	hook func()
}

func (s *afterOwnDeadStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	ran := false
	out, err := s.fakeCredStore.Refresh(ctx, owner, provider, func(c providercreds.Credential) (providercreds.Credential, error) {
		ran = true
		return fn(c)
	})
	if ran && errors.Is(err, providercreds.ErrDead) && s.hook != nil {
		s.once.Do(s.hook)
	}
	return out, err
}

var _ providercreds.Store = (*afterOwnDeadStore)(nil)

// heldRotationAfterOwnDead is the held-rotation pose behind the tombstone-window
// hook: the row is still live on the spent r0, refresh-a exists only in the
// hand-off, and `answer` decides what the forced refresh that ADOPTS it is told.
func heldRotationAfterOwnDead(t *testing.T, answer func(call int) (int, string)) (*afterOwnDeadStore, *providerCreds, pendingRotation) {
	t.Helper()
	var store *afterOwnDeadStore
	_, _, p := heldRotation(t, func(f *fakeCredStore) providercreds.Store {
		store = &afterOwnDeadStore{fakeCredStore: f}
		return store
	}, answer)
	c := assert.NewAborting(t)
	rot, ok := p.pendingFor(testKey)
	c.True(ok, "fixture: the give-up must keep the rotation")
	return store, p, rot
}

// TestTokenSourceRetryingSaveNeverRevivesThisRefreshesTombstone is the order
// the guard cannot see at all: a save is a SEPARATE transaction, so the
// tombstone this refresh wrote predates it, and only the drop (in the same
// transaction, before the tombstone commits) keeps the save from reviving the
// row with the token the issuer just refused.
func TestTokenSourceRetryingSaveNeverRevivesThisRefreshesTombstone(t *testing.T) {
	c := assert.NewAborting(t)
	store, p, rot := heldRotationAfterOwnDead(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	})
	// The save wakes when this refresh's tombstone is committed.
	store.hook = func() { p.persistRotation(testKey, rot) }

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.Error(err, "the refused rotation leaves nothing usable")
	c.Eq(0, store.reviveCount(), "a retrying save must not revive this refresh's own tombstone")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "the row stays dead")
	c.Eq("r0", stored.RefreshToken, "and keeps the token the rotation replaced")
}

// TestTokenSourceConcurrentRefreshNeverRevivesASiblingsTombstone is the other
// reviver outside the guard's view: a second refresh of the same key meets the
// tombstone as one that predates its own transaction. Only the drop stops it.
func TestTokenSourceConcurrentRefreshNeverRevivesASiblingsTombstone(t *testing.T) {
	c := assert.NewAborting(t)
	store, p, _ := heldRotationAfterOwnDead(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	})
	var siblingErr error
	store.hook = func() {
		// refresh directly, not through the token source: DoChan would coalesce
		// this into the flight already under test, and the point is a SECOND
		// transaction on the same row.
		_, siblingErr = p.refresh(testOwner, testProvider, true, chatgptauth.ReasonUnauthorizedRecovery)
	}

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.Error(err, "the refused rotation leaves nothing usable")
	c.Error(siblingErr, "a concurrent refresh must not be handed a usable token")
	c.Eq(0, store.reviveCount(), "a concurrent refresh must not revive a sibling's own tombstone")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "the row stays dead")
}

// TestTokenSourceRetryingSaveNeverRevivesTheRereadTombstone is the same rule on
// the refresh_token_reused path, where the tombstone comes from the re-read's
// transaction rather than the callback that decided it. The adopted refresh-a
// was spent upstream by an answer the daemon never saw, so the re-read finds the
// row still on r0 and tombstones it; a save still retrying the record wakes in
// that window and must not revive the row with the spent token.
func TestTokenSourceRetryingSaveNeverRevivesTheRereadTombstone(t *testing.T) {
	c := assert.NewAborting(t)
	store, p, rot := heldRotationAfterOwnDead(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusOK, tokenBody("access-a", "refresh-a", "acct_1")
		}
		return http.StatusBadRequest, `{"error":"refresh_token_reused"}`
	})
	store.hook = func() { p.persistRotation(testKey, rot) }

	_, _, err := p.TokenSourceFor(testOwner, testProvider).Token(context.Background(), true)
	c.Error(err, "a spent rotation leaves nothing usable")
	c.Eq(0, store.reviveCount(), "a retrying save must not revive the re-read's own tombstone with the spent token")
	stored, ok := store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.True(stored.Dead, "the row stays dead")
	c.Eq("r0", stored.RefreshToken, "and keeps the token the re-read died on")
}

// TestProviderCredDeleteRevokesTheUnpersistedRotation pins logout's revoke of
// BOTH live tokens. While a rotation is unpersisted the ROW holds the token
// that was already spent, so revoking only the row's token leaves the working
// one usable upstream until it expires.
func TestProviderCredDeleteRevokesTheUnpersistedRotation(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	cur := staleCredential(time.Now().Add(time.Minute))
	store.seed(cur)
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	rotated := cur
	rotated.RefreshToken = "rotated-refresh"
	rotated.AccessToken = "rotated-access"
	p.recordRotation(testKey, cur.RefreshToken, rotated)

	c.NoError(p.DeleteProviderCredential(context.Background(), testOwner, testProvider), "logout")

	c.Eq(2, iss.revokeCount(), "both the stored and the pending refresh token are revoked")
	c.EqDeep([]string{"r0", "rotated-refresh"}, iss.revoked(), "revoked tokens")
	if _, ok := p.pendingFor(testKey); ok {
		c.Fail("logout must forget the rotation it just revoked")
	}
	if _, ok := store.row(testOwner, testProvider); ok {
		c.Fail("logout must delete the row")
	}
}

// TestUserRmDeletesProviderCredentials is the privacy rule for a removed user:
// the user row is tombstoned, not deleted, and the provider credentials are
// separate rows, so removing the user must delete them (and revoke them) or the
// idle sweep keeps refreshing a removed user's grant forever.
func TestUserRmDeletesProviderCredentials(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	iss := newFakeIssuer(t)
	p := testCreds(t, store, iss, "")

	// The user store resolves the username to "id_alice", so the credentials are
	// stored against that id.
	const owner = "id_alice"
	for _, provider := range []string{testProvider, "other"} {
		cred := staleCredential(time.Now().Add(time.Hour))
		cred.OwnerUserID, cred.Provider = owner, provider
		cred.RefreshToken = "refresh-" + provider
		store.seed(cred)
	}
	// Another user's credential, which the removal must not touch.
	other := staleCredential(time.Now().Add(time.Hour))
	other.OwnerUserID = "id_bob"
	store.seed(other)

	ctrl := &Controller{users: &userAdminFakeStore{}, providerCreds: p}
	c.NoError(ctrl.UserRm(context.Background(), "alice"), "UserRm")

	c.Eq(0, store.ownerRows(owner), "every credential of the removed user is deleted")
	c.Eq(1, store.ownerRows("id_bob"), "another user's credential is untouched")
	c.Eq(2, iss.revokeCount(), "each deleted credential is revoked upstream")
}

// TestUserRmSurvivesACredentialDeleteFailure pins the best-effort contract: the
// removal has already happened, so a store failure deleting the credentials is
// logged and does not fail the call.
func TestUserRmSurvivesACredentialDeleteFailure(t *testing.T) {
	c := assert.NewAborting(t)
	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	store := newFakeCredStore()
	store.deleteAllErr = errors.New("store is down")
	p := testCreds(t, store, newFakeIssuer(t), "")
	ctrl := &Controller{users: &userAdminFakeStore{}, providerCreds: p}

	c.NoError(ctrl.UserRm(context.Background(), "alice"), "a credential failure must not fail the removal")
	c.StrContains(logs.String(), "deleting a removed user's logins failed", "the failure must be logged: %s", logs.String())
}

// TestPendingRotationRedactsTokens closes the leak fmt has through an
// UNEXPORTED field: a whole pendingRotation printed with %v or %+v renders its
// embedded Credential's own fields, because fmt does not call the inner type's
// String there.
func TestPendingRotationRedactsTokens(t *testing.T) {
	c := assert.NewAborting(t)
	rot := pendingRotation{
		seq:  7,
		sent: "refresh-token-must-not-print",
		want: providercreds.Credential{
			OwnerUserID:  testOwner,
			Provider:     testProvider,
			AccountID:    "acct_1",
			RefreshToken: "wanted-refresh-token-must-not-print",
			AccessToken:  "wanted-access-token-must-not-print",
			AccessExpiry: time.Now().Add(time.Hour),
		},
	}

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, rot)
		c.NotStrContains(out, rot.sent, "%s leaked the spent token: %s", format, out)
		c.NotStrContains(out, rot.want.RefreshToken, "%s leaked the rotation's refresh token: %s", format, out)
		c.NotStrContains(out, rot.want.AccessToken, "%s leaked the rotation's access token: %s", format, out)
		c.StrContains(out, redactedToken, "%s does not mark the tokens redacted: %s", format, out)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("rotation", "pending", rot)
	c.NotStrContains(buf.String(), rot.sent, "slog leaked the spent token: %s", buf.String())
	c.NotStrContains(buf.String(), rot.want.RefreshToken, "slog leaked the refresh token: %s", buf.String())
	c.NotStrContains(buf.String(), rot.want.AccessToken, "slog leaked the access token: %s", buf.String())
	c.StrContains(buf.String(), testProvider, "slog keeps the non-secret fields: %s", buf.String())
}
