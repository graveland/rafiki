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

	// failWriteOnce makes the next CHANGED write fail with ErrNotPersisted,
	// which is the "upstream already rotated, the database write did not land"
	// case.
	failWriteOnce bool

	// puts and refreshCalls count calls, so a test can prove a path never used
	// Put and how many transactions the store saw.
	puts         int
	refreshCalls int
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
	if f.failWriteOnce {
		f.failWriteOnce = false
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

// ─── Fake issuer ─────────────────────────────────────────────────────────────

// fakeIssuer is the ChatGPT issuer as an httptest server: the refresh endpoint,
// the code exchange (same endpoint, form encoded), and revoke.
type fakeIssuer struct {
	srv *httptest.Server

	mu         sync.Mutex
	bodies     []map[string]any
	tokenCalls int
	revokes    int

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

func (f *fakeIssuer) revoke(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.revokes++
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
	return fmt.Sprintf(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600,"id_token":%q}`,
		idToken(accountID))
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
	store.failWriteOnce = true
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

// TestTokenSourceForcedRefreshDuringRetryAdoptsPendingRotation is the fix round's
// A1: a rotation the database did not accept must not be lost when a 401
// recovery (or the sweep) refreshes during the retry window. The store still
// holds the SPENT refresh token, so sending it again comes back
// refresh_token_reused, the re-read finds the same spent token, and the row is
// tombstoned while the working token sits in the save goroutine. The pending
// hand-off makes the refresh adopt the rotation instead.
func TestTokenSourceForcedRefreshDuringRetryAdoptsPendingRotation(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failWriteOnce = true
	iss := newFakeIssuer(t)
	// The real issuer refuses a spent token. Answering reused on the second call
	// is what makes this test fail if the forced refresh spends `sent` again.
	iss.answer = func(call int) (int, string) {
		if call == 1 {
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
	c.Eq(1, iss.calls(), "the spent refresh token must not be sent upstream again")

	stored, ok = store.row(testOwner, testProvider)
	c.True(ok, "row still present")
	c.Eq("rotated-refresh", stored.RefreshToken, "the adopted rotation must be stored")
	c.False(stored.Dead, "adopting the rotation must not tombstone the credential")
	forced, _, err := src.Token(context.Background(), false)
	c.NoError(err, "a routine refresh after the adoption")
	c.Eq("new-access", forced, "token")
}

// TestTokenSourceRetryDoesNotClobberANewerLogin is the re-login-during-retry
// case: a save that started before another login must never undo it. The guard
// is the row's refresh token, not a timer.
func TestTokenSourceRetryDoesNotClobberANewerLogin(t *testing.T) {
	c := assert.NewAborting(t)
	store := newFakeCredStore()
	store.seed(staleCredential(time.Now().Add(time.Minute)))
	store.failWriteOnce = true
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
	store.failWriteOnce = true
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

	for _, rendered := range []string{fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), tok.String()} {
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
