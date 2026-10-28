// SPDX-License-Identifier: Apache-2.0

package main

// Tests for the ChatGPT-subscription provider at spawn: the three refusals, the
// owner's model directory feeding both the sender's facts and the child's
// context window from one parse, and the owner-scoped model listing.
//
// They use the real *providerCreds over provider_creds_test.go's in-memory store
// and fake issuer, because the property under test is what the daemon does with
// a stored credential, not what a stub says. Only the sender-building seam
// (providerSenderCreds) gets a fake, and only because the sender value a build
// returns cannot be read back out of pkg/llm.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providercreds"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

const (
	ownerA = "4d6f1e3a-0000-4000-8000-00000000000a"
	ownerB = "4d6f1e3a-0000-4000-8000-00000000000b"

	chatGPTTestProvider = "chatgpt"
)

// chatGPTModel is one model as the account's directory reports it. The two
// entries with visibility other than "list" and the one with
// supported_in_api=false exist for the listing rules: the first two must never
// be offered, and the third must be.
const chatGPTModelsJSON = `{"models":[
 {"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list",
  "context_window":500000,"input_modalities":["text","image"],
  "support_verbosity":true,"default_verbosity":"low",
  "supports_reasoning_summary_parameter":true,
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}],
  "supported_in_api":true},
 {"slug":"gpt-api-only","display_name":"GPT API Only","visibility":"list",
  "context_window":200000,"supported_in_api":false},
 {"slug":"gpt-hidden","display_name":"Hidden","visibility":"hide","context_window":1000},
 {"slug":"gpt-none","display_name":"None","visibility":"none","context_window":1000}
]}`

// ownerBModels is a DIFFERENT account's directory, with a slug owner A's does
// not carry: the listing must never show one owner another owner's models.
const ownerBModelsJSON = `{"models":[
 {"slug":"gpt-b-only","display_name":"B Only","visibility":"list","context_window":123}
]}`

// ─── fixtures ────────────────────────────────────────────────────────────────

// chatGPTProviderSet is a registry with exactly one ChatGPT-login provider, the
// shape providers.toml takes for a subscription-backed OpenAI provider.
func chatGPTProviderSet() *providers.Set {
	return &providers.Set{
		DefaultProvider: chatGPTTestProvider,
		Providers: map[string]providers.Provider{
			chatGPTTestProvider: {Name: chatGPTTestProvider, Kind: providers.KindOpenAIResponsesChatGPT},
		},
	}
}

// chatGPTTestCreds builds the daemon's real credential machinery over an
// in-memory store and a fake issuer, on the test's lifetime context so the idle
// sweeper drains with it.
//
// The model endpoint is a fixture answering 304, so a path that triggers a fetch
// keeps whatever directory the test seeded and no test in this file reaches
// chatgpt.com. A test that wants a fetch to land passes its own fixture.
func chatGPTTestCreds(t *testing.T, store providercreds.Store) *providerCreds {
	t.Helper()
	return chatGPTTestCredsWithModels(t, store, newModelsFixture(t, http.StatusNotModified, ""))
}

func chatGPTTestCredsWithModels(t *testing.T, store providercreds.Store, fx *modelsFixture) *providerCreds {
	t.Helper()
	// The registry is the login backend's provider lookup: CompleteProviderLogin
	// refuses a provider name it cannot resolve to a ChatGPT-kind entry.
	creds := newProviderCreds(t.Context(), store, newFakeIssuer(t).engine(), "", chatGPTProviderSet)
	creds.modelsURL = fx.srv.URL
	creds.modelsClient = fx.srv.Client()
	t.Cleanup(creds.Wait)
	return creds
}

// modelsFixture is the ChatGPT model endpoint as an httptest server, recording
// every request so a test can assert what the daemon asked for.
type modelsFixture struct {
	srv    *httptest.Server
	status int
	body   string
	etag   string
	// before, when set, runs after the request is recorded and before it is
	// answered. It is how a test changes the credential while a fetch is in
	// flight (a logout plus a re-login under the same key).
	before func()

	mu       sync.Mutex
	requests []modelsFixtureRequest
}

// modelsFixtureRequest is what one directory request carried.
type modelsFixtureRequest struct {
	Query         url.Values
	Authorization string
	AccountID     string
	IfNoneMatch   string
}

func newModelsFixture(t *testing.T, status int, body string) *modelsFixture {
	t.Helper()
	fx := &modelsFixture{status: status, body: body, etag: `"v1"`}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		fx.requests = append(fx.requests, modelsFixtureRequest{
			Query:         r.URL.Query(),
			Authorization: r.Header.Get("Authorization"),
			AccountID:     r.Header.Get(chatGPTAccountHeader),
			IfNoneMatch:   r.Header.Get("If-None-Match"),
		})
		before := fx.before
		fx.mu.Unlock()
		if before != nil {
			before()
		}
		if fx.status == http.StatusNotModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", fx.etag)
		w.WriteHeader(fx.status)
		_, _ = io.WriteString(w, fx.body)
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

// recorded returns the requests seen so far.
func (fx *modelsFixture) recorded() []modelsFixtureRequest {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]modelsFixtureRequest(nil), fx.requests...)
}

// liveCredential is one usable login: a live refresh token, an access token with
// an hour left, and (when modelsJSON is non-empty) a cached model directory.
func liveCredential(owner, modelsJSON string) providercreds.Credential {
	return providercreds.Credential{
		OwnerUserID:  owner,
		Provider:     chatGPTTestProvider,
		AccountID:    "acct_" + owner[len(owner)-1:],
		RefreshToken: "refresh-" + owner,
		AccessToken:  "access-" + owner,
		AccessExpiry: time.Now().Add(time.Hour),
		LastRefresh:  time.Now(),
		ModelsJSON:   []byte(modelsJSON),
	}
}

// recordingCredStore records what a path asked the store, and for WHICH owner.
//
// It answers three questions this file's tests could not otherwise ask: how many
// reads a path performs (one parse per fetch), whether a fetch ever WROTE a
// directory it should have dropped (SetModels), and whose credential a child was
// bound to (every owner a Get or Refresh named).
type recordingCredStore struct {
	*fakeCredStore

	mu          sync.Mutex
	gets        int
	getOwners   []string
	setModels   int
	refreshOwns []string
}

func (s *recordingCredStore) Get(ctx context.Context, owner, provider string) (providercreds.Credential, error) {
	s.mu.Lock()
	s.gets++
	s.getOwners = append(s.getOwners, owner)
	s.mu.Unlock()
	return s.fakeCredStore.Get(ctx, owner, provider)
}

func (s *recordingCredStore) SetModels(ctx context.Context, owner, provider string, modelsJSON []byte, etag string) error {
	s.mu.Lock()
	s.setModels++
	s.mu.Unlock()
	return s.fakeCredStore.SetModels(ctx, owner, provider, modelsJSON, etag)
}

func (s *recordingCredStore) Refresh(ctx context.Context, owner, provider string, fn func(providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	s.mu.Lock()
	s.refreshOwns = append(s.refreshOwns, owner)
	s.mu.Unlock()
	return s.fakeCredStore.Refresh(ctx, owner, provider, fn)
}

func (s *recordingCredStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// ownersRead is every owner a Get named.
func (s *recordingCredStore) ownersRead() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.getOwners...)
}

// writes is how many model directories were stored.
func (s *recordingCredStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setModels
}

// spawnTestChatGPTController is a Controller wired with the ChatGPT registry,
// the real credential machinery over store, and nothing else: no catalog, no
// pool, no executor plane.
func spawnTestChatGPTController(t *testing.T, creds *providerCreds) *Controller {
	t.Helper()
	c := newTestController(t)
	c.providers = chatGPTProviderSet()
	c.providerCreds = creds
	return c
}

// ownerCtx is a Connect request context carrying owner userID as the
// authenticated caller, which is what spawnOwner reads.
func ownerCtx(userID string) context.Context {
	return server.WithIdentity(context.Background(), &server.Identity{UserID: userID, Via: server.ProvenanceUser})
}

// ─── Spawn refusals ──────────────────────────────────────────────────────────

// A child with no owner cannot spend anyone's subscription, and this kind has no
// daemon-wide fallback credential by design, so the spawn is refused with the
// reason rather than started to fail its first turn.
func TestProviderSpawnRefusedWithoutOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: chatGPTTestProvider + "/gpt-6.1-sol"}
	err := c.checkLoginProviderUsable(context.Background(), req, "")
	ck.Require().Error(err, "checkLoginProviderUsable admitted an ownerless ChatGPT spawn")
	ce := controllerErr(t, err)
	// A connect-mappable code: InvalidArgs or FailedPrecondition, both of which
	// the caller can act on (spawn with a user credential).
	ck.True(ce.Code == protocol.ErrInvalidArgs || ce.Code == protocol.ErrFailedPrecondition,
		"code = %q, want a connect-mappable invalid-argument or failed-precondition", ce.Code)
	ck.StrContains(ce.Message, "bills a user's ChatGPT subscription", "message")
	ck.StrContains(ce.Message, "no owner", "message")
}

// An owner who has never logged in is refused with the one command that fixes
// it, before any child exists.
func TestProviderSpawnRefusedWithoutCredential(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, newFakeCredStore())) // no credential stored

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: chatGPTTestProvider + "/gpt-6.1-sol"}
	err := c.checkLoginProviderUsable(context.Background(), req, ownerA)
	ck.Require().Error(err, "checkLoginProviderUsable admitted a spawn with no stored login")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "code")
	ck.Eq("no ChatGPT login for chatgpt; run: rafiki providers login chatgpt", ce.Message, "message")
}

// A daemon with no credential store (no RAFIKI_SECRETS_KEY, or no database)
// cannot serve this kind at all. The refusal carries the daemon's reason, which
// names the missing setting and never a token.
func TestProviderSpawnRefusedWhenUnavailable(t *testing.T) {
	ck := assert.NewCollecting(t)
	const why = "provider credentials are unavailable: set RAFIKI_SECRETS_KEY"
	creds := newProviderCreds(t.Context(), nil, nil, why, nil)
	c := spawnTestChatGPTController(t, creds)

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: chatGPTTestProvider + "/gpt-6.1-sol"}
	err := c.checkLoginProviderUsable(context.Background(), req, ownerA)
	ck.Require().Error(err, "checkLoginProviderUsable admitted a spawn on a daemon with no credential store")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrUnavailable, ce.Code, "code")
	ck.StrContains(ce.Message, "RAFIKI_SECRETS_KEY", "message")
}

// A store that cannot answer is Unavailable, never "no login": sending a user to
// run a login command because the database is down has them fix the wrong thing.
func TestProviderSpawnRefusedWhenStoreFails(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := brokenCredStore{fakeCredStore: newFakeCredStore()}
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))

	err := c.checkLoginProviderUsable(context.Background(),
		protocol.SpawnRequest{Kind: protocol.KindFundi, Model: chatGPTTestProvider + "/gpt-6.1-sol"}, ownerA)
	ck.Require().Error(err, "a spawn was admitted over a failed credential read")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrUnavailable, ce.Code, "a store fault must be Unavailable, not a missing-login refusal")
	ck.StrContains(ce.Message, "connection refused", "the refusal must carry the store's fault")
	ck.False(strings.Contains(ce.Message, "rafiki providers login"),
		"a store fault told the user to log in again: %q", ce.Message)
}

// brokenCredStore answers every read with a store fault.
type brokenCredStore struct{ *fakeCredStore }

func (b brokenCredStore) Get(context.Context, string, string) (providercreds.Credential, error) {
	return providercreds.Credential{}, errors.New("connection refused")
}

// The admitted case, so the three refusals above are known to be refusals of
// something that otherwise works.
func TestProviderSpawnAdmittedWithCredential(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: chatGPTTestProvider + "/gpt-6.1-sol"}
	ck.NoError(c.checkLoginProviderUsable(context.Background(), req, ownerA), "checkLoginProviderUsable refused a spawn that has an owner and a login")

	// A provider of any other kind is not this check's business even for the same
	// owner: an anthropic model authenticates from the environment.
	other := &providers.Set{
		DefaultProvider: "anthropic",
		Providers:       map[string]providers.Provider{"anthropic": {Name: "anthropic", Kind: providers.KindAnthropic}},
	}
	c.providers = other
	ck.NoError(c.checkLoginProviderUsable(context.Background(),
		protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/claude-sonnet-4-5"}, ""),
		"a non-ChatGPT provider was checked as if it needed an owner")
}

// ─── Owner model directory ───────────────────────────────────────────────────

// The facts a sender gates its request on come from the owner's own directory,
// mapped field by field, and an unknown slug is ok=false (the sender's
// conservative case), never a zero-valued "known".
func TestOwnerModelFacts(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	creds := chatGPTTestCreds(t, store)

	facts := creds.ModelFactsFor(ownerA, chatGPTTestProvider)
	fact, ok := facts("gpt-6.1-sol")
	ck.Require().True(ok, "no facts for gpt-6.1-sol, which the owner's directory lists")
	ck.True(fact.SupportsReasoningSummary, "SupportsReasoningSummary (supports_reasoning_summary_parameter)")
	ck.True(fact.SupportsVerbosity, "SupportsVerbosity (support_verbosity)")
	ck.Eq("low", fact.DefaultVerbosity, "DefaultVerbosity")
	ck.EqDeep([]string{"low", "medium", "high", "xhigh"}, fact.ReasoningLevels, "ReasoningLevels (server order)")

	// A slug the directory does not carry is unknown, and so is another owner's
	// slug: the facts are per owner, not per daemon.
	if _, ok := facts("gpt-unknown"); ok {
		t.Error("facts reported a model the owner's directory does not carry")
	}
	if _, ok := creds.ModelFactsFor(ownerB, chatGPTTestProvider)("gpt-6.1-sol"); ok {
		t.Error("owner B's facts answered for owner A's slug")
	}
}

// The child's context window comes from the owner's directory entry, and the
// sender's facts come from the SAME parse: one store read for both.
//
// That second half is why this test goes through spawnAgentRuntimeOptions rather
// than the helpers: the spawn path is where the directory is read, and a second
// read there would be a second parse of the same document.
func TestContextWindowFromOwnerModels(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	creds := chatGPTTestCreds(t, store)
	c := spawnTestChatGPTController(t, creds)

	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: chatGPTTestProvider + "/gpt-6.1-sol",
	}
	ro, err := c.spawnAgentRuntimeOptions(req, "c_owner_window", false, "brent", ownerA)
	ck.Require().NoError(err, "spawnAgentRuntimeOptions")
	// RuntimeOptions.ContextWindow outranks the catalog; its plumbing down to the
	// engine is pinned by pkg/fundi's own end-to-end test, so the override
	// landing here is the assertion this side owes.
	ck.Eq(500000, ro.ContextWindow, "ContextWindow")
	ck.NotNil(ro.ProviderSenders[chatGPTTestProvider],
		"no sender for %s; a spawn with an owner and a login must get one", chatGPTTestProvider)

	reads := store.readCount()
	facts := creds.ModelFactsFor(ownerA, chatGPTTestProvider)
	_, ok := facts("gpt-6.1-sol")
	ck.Require().True(ok, "the sender's facts found nothing after the spawn already read the directory")
	ck.Eq(reads, store.readCount(),
		"the facts lookup read the credential again (%d reads before, %d after): the context window and the facts must share one parse", reads, store.gets)

	// A model the owner's directory does not carry leaves the override at 0, which
	// fundi reads as "ask the catalog".
	unknown := req
	unknown.Model = chatGPTTestProvider + "/gpt-unknown"
	ro, err = c.spawnAgentRuntimeOptions(unknown, "c_owner_unknown", false, "brent", ownerA)
	ck.Require().NoError(err, "spawnAgentRuntimeOptions(unknown model)")
	ck.Eq(0, ro.ContextWindow, "ContextWindow for a slug the directory does not carry")
}

// ─── Listing ─────────────────────────────────────────────────────────────────

// The listing offers only visibility == "list" entries of the CALLER's own
// directory. "hide" and "none" are excluded because the account cannot address
// those models; supported_in_api=false is included, because it describes
// API-key access and is false for subscription models that work.
func TestOwnerModelsListingHidesHiddenAndOtherOwners(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	store.seed(liveCredential(ownerB, ownerBModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))

	rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows")
	byID := rowsByID(rows)

	list, ok := byID[chatGPTTestProvider+"/gpt-6.1-sol"]
	ck.Require().True(ok, "the owner's listable model is missing from %d rows", len(rows))
	ck.Eq("login", list.Source, "Source")
	ck.Eq("gpt-6.1-sol", list.Model, "Model")
	ck.Eq("GPT-6.1-Sol", list.Name, "Name")
	ck.False(list.ContextWindow == nil || *list.ContextWindow != 500000,
		"ContextWindow = %v, want the entry's 500000", list.ContextWindow)
	ck.EqDeep([]string{"text", "image"}, list.InputModalities, "InputModalities")

	ck.HasKey(byID, chatGPTTestProvider+"/gpt-api-only",
		"a visibility=list entry with supported_in_api=false was hidden; that field describes API-key access, not what this account can run")
	ck.NotHasKey(byID, chatGPTTestProvider+"/gpt-hidden", `visibility="hide" was listed`)
	ck.NotHasKey(byID, chatGPTTestProvider+"/gpt-none", `visibility="none" was listed`)
	ck.NotHasKey(byID, chatGPTTestProvider+"/gpt-b-only", "another owner's model was listed for this caller")

	// The other owner sees their own directory and not this one's, which is the
	// property the whole per-owner store exists for.
	rowsB, err := c.ListModelRows(ownerCtx(ownerB), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows(owner B)")
	byIDB := rowsByID(rowsB)
	ck.HasKey(byIDB, chatGPTTestProvider+"/gpt-b-only", "owner B's own model is missing from their listing")
	ck.NotHasKey(byIDB, chatGPTTestProvider+"/gpt-6.1-sol", "owner A's model was listed for owner B")

	// A caller with no owner (the unix socket) gets no login rows at all, and no
	// error: a model listing is a picker, not an authorization decision.
	anon, err := c.ListModelRows(context.Background(), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows(no owner)")
	for id := range rowsByID(anon) {
		ck.False(strings.HasPrefix(id, chatGPTTestProvider+"/"), "an ownerless caller was offered %q", id)
	}
}

// Subscription models are priced from the catalog entry for the OPENAI id, not
// the row's own <provider>/<slug>: the catalog only ever hears OpenRouter's ids,
// so joining on the row id would find nothing and silently leave every
// subscription model unpriced.
func TestOwnerModelsListingPricesViaOpenAIID(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))
	// The catalog knows the model as openai/gpt-6.1-sol and knows nothing about
	// gpt-api-only, which must therefore stay unpriced rather than free.
	c.catalog = routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler)).
		WithCache(&byteSnapshotStore{data: []byte(`{"fetched":"2099-01-01T00:00:00Z","models":[
		 {"id":"openai/gpt-6.1-sol","name":"GPT-6.1-Sol","created":1,
		  "pricing":{"prompt":"0.000001","completion":"0.000002",
		             "input_cache_read":"0.0000001","input_cache_write":"0.0000002"}}
		]}`)})

	rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows")
	byID := rowsByID(rows)

	priced, ok := byID[chatGPTTestProvider+"/gpt-6.1-sol"]
	ck.Require().True(ok, "the priced model is missing from the listing")
	for _, f := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"PromptUSD", priced.PromptUSD, 0.000001},
		{"CompletionUSD", priced.CompletionUSD, 0.000002},
		{"CacheReadUSD", priced.CacheReadUSD, 0.0000001},
		{"CacheWriteUSD", priced.CacheWriteUSD, 0.0000002},
	} {
		ck.False(f.got == nil || *f.got != f.want, "%s = %v, want %v from the catalog's openai/gpt-6.1-sol entry", f.name, f.got, f.want)
	}

	unpriced, ok := byID[chatGPTTestProvider+"/gpt-api-only"]
	ck.Require().True(ok, "the unpriced model is missing from the listing")
	ck.True(unpriced.PromptUSD == nil && unpriced.CompletionUSD == nil &&
		unpriced.CacheReadUSD == nil && unpriced.CacheWriteUSD == nil,
		"a model the catalog does not know arrived with prices %v/%v/%v/%v, want all absent",
		unpriced.PromptUSD, unpriced.CompletionUSD, unpriced.CacheReadUSD, unpriced.CacheWriteUSD)
}

// ─── Model fetch ────────────────────────────────────────────────────────────

// A listing with no cached directory fetches one, in the background, with the
// owner's bearer, their account id and the codified client_version, and the
// document it lands is what the next listing shows.
//
// The client_version is the operator's override here, because the whole point of
// the constant is that the server gates entries by it.
func TestOwnerModelsListingFetchesWhenNoCache(t *testing.T) {
	ck := assert.NewCollecting(t)
	t.Setenv("RAFIKI_CHATGPT_CLIENT_VERSION", "9.9.9")

	fx := newModelsFixture(t, http.StatusOK, `{"models":[
	 {"slug":"gpt-fetched","display_name":"Fetched","visibility":"list","context_window":64000}
	]}`)
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, "")) // logged in, never fetched
	creds := chatGPTTestCredsWithModels(t, store, fx)
	c := spawnTestChatGPTController(t, creds)

	// The fetch runs on the daemon's context, so wait for it rather than sleep:
	// the listing that triggers it answers from whatever is cached at that moment,
	// which is the point (it never waits for the upstream).
	_, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows before the fetch")
	assert.Eventually(t, 10*time.Second, "the model directory fetch to land", func() error {
		rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
		if err != nil {
			return err
		}
		if _, ok := rowsByID(rows)[chatGPTTestProvider+"/gpt-fetched"]; !ok {
			return errors.New("gpt-fetched is not listed yet")
		}
		return nil
	})

	reqs := fx.recorded()
	ck.Require().NotEmpty(reqs, "the daemon never asked for the model directory")
	ck.Eq("9.9.9", reqs[0].Query.Get("client_version"), "client_version")
	ck.Eq("Bearer access-"+ownerA, reqs[0].Authorization, "Authorization")
	ck.Eq("acct_a", reqs[0].AccountID, chatGPTAccountHeader+" header")
	ck.Eq("", reqs[0].IfNoneMatch, "If-None-Match with no etag cached")

	// The directory and its etag are stored, so the next daemon does not re-ask.
	stored, ok := store.row(ownerA, chatGPTTestProvider)
	ck.Require().True(ok, "the credential row vanished")
	ck.Eq(fx.etag, stored.ModelsETag, "stored etag")
	ck.False(len(stored.ModelsJSON) == 0, "the fetched directory was not stored")
}

// A fetch that fails is a WARN and nothing else: the listing keeps serving the
// cached directory and never answers an error, and the failure does not make the
// next listing refetch immediately.
func TestOwnerModelsListingKeepsCacheWhenFetchFails(t *testing.T) {
	ck := assert.NewCollecting(t)
	fx := newModelsFixture(t, http.StatusInternalServerError, "")
	store := newFakeCredStore()
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCredsWithModels(t, store, fx))

	for i := 0; i < 3; i++ {
		rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
		ck.Require().NoError(err, "ListModelRows with a failing model endpoint")
		ck.HasKey(rowsByID(rows), chatGPTTestProvider+"/gpt-6.1-sol", "the cached directory stopped being listed")
	}

	// The attempt is stamped, so a failing upstream is retried on the cadence
	// rather than on every picker refresh (each of which would log a WARN).
	assert.Eventually(t, 10*time.Second, "the fetch attempt to be recorded", func() error {
		if len(fx.recorded()) == 0 {
			return errors.New("no request yet")
		}
		return nil
	})
	time.Sleep(50 * time.Millisecond) // let any (wrong) extra attempt arrive
	ck.Len(fx.recorded(), 1, "model directory fetches for three listings")
}

// A stored etag rides `If-None-Match`, and a 304 KEEPS the cache: the answer is
// "what you have is current", so nothing is written and the cached directory
// keeps serving the listing.
func TestOwnerModelsFetchSendsIfNoneMatchAndKeepsCacheOn304(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	cred := liveCredential(ownerA, chatGPTModelsJSON)
	cred.ModelsETag = `"v1"`
	store.seed(cred)
	fx := newModelsFixture(t, http.StatusNotModified, "")
	c := spawnTestChatGPTController(t, chatGPTTestCredsWithModels(t, store, fx))

	rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows")
	ck.HasKey(rowsByID(rows), chatGPTTestProvider+"/gpt-6.1-sol", "the cached directory is not listed")

	waitForModelsRequests(t, fx, 1)
	ck.Eq(`"v1"`, fx.recorded()[0].IfNoneMatch, "If-None-Match must carry the stored etag")

	ck.Eq(0, store.writes(), "a 304 stored a directory: there was nothing new to store")
	stored, ok := store.row(ownerA, chatGPTTestProvider)
	ck.Require().True(ok, "the credential row vanished")
	ck.Eq(`"v1"`, stored.ModelsETag, "a 304 must not clear the etag")
	ck.Eq(chatGPTModelsJSON, string(stored.ModelsJSON), "a 304 must not touch the stored directory")

	// And the cache still answers: a 304 means the parse we hold is current.
	ck.Eq(500000, dirContextWindow(t, c.providerCreds, ownerA, "gpt-6.1-sol"), "cached context window after a 304")
}

// A 200 whose body is not a model directory is NOT stored: a malformed answer
// must not replace a working cache (nor leave an unparsable row for the next
// daemon to read).
func TestOwnerModelsFetchDoesNotStoreUnparsableBody(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	cred := liveCredential(ownerA, chatGPTModelsJSON)
	cred.ModelsETag = `"v1"`
	store.seed(cred)
	fx := newModelsFixture(t, http.StatusOK, "<html>not a model directory</html>")
	c := spawnTestChatGPTController(t, chatGPTTestCredsWithModels(t, store, fx))

	rows, err := c.ListModelRows(ownerCtx(ownerA), "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows")
	ck.HasKey(rowsByID(rows), chatGPTTestProvider+"/gpt-6.1-sol", "the cached directory is not listed")

	waitForModelsRequests(t, fx, 1)
	ck.Eq(0, store.writes(), "an unparsable model directory was stored")
	stored, ok := store.row(ownerA, chatGPTTestProvider)
	ck.Require().True(ok, "the credential row vanished")
	ck.Eq(`"v1"`, stored.ModelsETag, "an unparsable answer must not replace the etag")
	ck.Eq(chatGPTModelsJSON, string(stored.ModelsJSON), "an unparsable answer must not replace the directory")
}

// A successful login TRIGGERS the fetch. Without it a user who just logged in
// sees an empty model list until something else happens to ask, and the child's
// context window stays at the catalog's answer.
func TestProviderLoginTriggersModelsFetch(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	fx := newModelsFixture(t, http.StatusOK, ownerBModelsJSON)
	creds := chatGPTTestCredsWithModels(t, store, fx)

	begin, err := creds.BeginProviderLogin(context.Background(), ownerA, chatGPTTestProvider)
	ck.Require().NoError(err, "BeginProviderLogin")
	authURL, err := url.Parse(begin.AuthorizeURL)
	ck.Require().NoError(err, "authorize URL must parse")
	_, _, err = creds.CompleteProviderLogin(context.Background(), ownerA, begin.LoginID,
		"code=auth-code&state="+url.QueryEscape(authURL.Query().Get("state")))
	ck.Require().NoError(err, "CompleteProviderLogin")

	waitForModelsRequests(t, fx, 1)
	ck.Eq(1, store.writes(), "the login did not store a model directory")
	stored, ok := store.row(ownerA, chatGPTTestProvider)
	ck.Require().True(ok, "the login stored no credential")
	ck.False(len(stored.ModelsJSON) == 0, "the login stored no directory")
}

// A fetch that outlives its credential must not write: a logout plus a re-login
// to ANOTHER account bumps the credential's generation, and the directory (and
// its etag) describe the account that is gone.
func TestOwnerModelsFetchDropsDirectoryWhenCredentialChanges(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	store.seed(liveCredential(ownerA, chatGPTModelsJSON)) // the old account's directory
	fx := newModelsFixture(t, http.StatusOK, ownerBModelsJSON)
	creds := chatGPTTestCredsWithModels(t, store, fx)
	key := credKey{owner: ownerA, provider: chatGPTTestProvider}
	fx.before = func() {
		// The user logs out and back in as a different account while the fetch is
		// in flight. Both go through forget, which bumps the generation.
		store.seed(liveCredential(ownerA, ""))
		creds.forget(key)
	}

	creds.refreshModels(t.Context(), ownerA, chatGPTTestProvider)

	ck.Eq(0, store.writes(), "a fetch across a re-login wrote the OLD account's model directory")
	stored, ok := store.row(ownerA, chatGPTTestProvider)
	ck.Require().True(ok, "the credential row vanished")
	ck.Eq("", stored.ModelsETag, "the old account's etag landed on the new account's row")
	ck.Empty(stored.ModelsJSON, "the old account's directory landed on the new account's row")
	if _, known := creds.rememberedModels(key); known {
		t.Error("the discarded fetch's parse was remembered")
	}
}

// A read that started BEFORE a fetch landed must not overwrite the fetch's
// parse: the fetched document is the newer one, and losing it would leave the
// directory stale for up to an hour.
func TestOwnerModelsReadDoesNotOverwriteFetchedParse(t *testing.T) {
	ck := assert.NewCollecting(t)
	creds := chatGPTTestCreds(t, newFakeCredStore())
	key := credKey{owner: ownerA, provider: chatGPTTestProvider}

	// The fetch lands first (it started earlier, and its document is newer than
	// anything a row read can see once it has been stored).
	creds.rememberFetchedModels(key, parseOwnerModels([]byte(ownerBModelsJSON)), creds.generation(key))
	// Then the slow read installs the parse of what the row held.
	creds.rememberReadModels(key, parseOwnerModels([]byte(chatGPTModelsJSON)))

	dir, known := creds.rememberedModels(key)
	ck.Require().True(known, "neither parse was remembered")
	ck.Eq(123, dir.contextWindow("gpt-b-only"), "the fetch's parse must win")
	ck.Eq(0, dir.contextWindow("gpt-6.1-sol"), "the older read's parse overwrote the fetched one")

	// A first read still installs: the guard is "never overwrite", not "never
	// remember".
	fresh := chatGPTTestCreds(t, newFakeCredStore())
	fresh.rememberReadModels(key, parseOwnerModels([]byte(chatGPTModelsJSON)))
	dir, known = fresh.rememberedModels(key)
	ck.Require().True(known, "a first read was not remembered")
	ck.Eq(500000, dir.contextWindow("gpt-6.1-sol"), "a first read must install its parse")

	// And a fetch still wins over a fetch: nothing else can replace a parse.
	creds.rememberFetchedModels(key, parseOwnerModels([]byte(chatGPTModelsJSON)), creds.generation(key))
	dir, _ = creds.rememberedModels(key)
	ck.Eq(500000, dir.contextWindow("gpt-6.1-sol"), "a later fetch must replace the earlier parse")
}

// chatGPTModelsClientVersion is the operator's override when set, the codex
// constant otherwise.
func TestChatGPTModelsClientVersion(t *testing.T) {
	ck := assert.NewCollecting(t)
	t.Setenv("RAFIKI_CHATGPT_CLIENT_VERSION", "")
	ck.Eq("0.165.0", chatGPTModelsClientVersion(), "default client_version")
	t.Setenv("RAFIKI_CHATGPT_CLIENT_VERSION", "1.2.3")
	ck.Eq("1.2.3", chatGPTModelsClientVersion(), "overridden client_version")
}

// A ChatGPT provider reached only through the primary provider's Fallback chain
// is exactly as billable as the primary itself, so it gets the same refusal. The
// chain is the model's own provider plus its one-level Fallback list, the same
// set providerSenders builds senders for (llm.Client never recurses into a
// fallback's own Fallback).
func TestProviderSpawnRefusedWhenChatGPTReachableOnlyViaFallback(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	c := newTestController(t)
	c.providers = &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic":         {Name: "anthropic", Kind: providers.KindAnthropic, Fallback: []string{chatGPTTestProvider}},
			chatGPTTestProvider: {Name: chatGPTTestProvider, Kind: providers.KindOpenAIResponsesChatGPT},
		},
	}
	c.providerCreds = chatGPTTestCreds(t, store)
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/claude-sonnet-4-5"}

	// No owner: the fallback would need one, so the spawn is refused by name.
	err := c.checkLoginProviderUsable(context.Background(), req, "")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "ownerless spawn whose FALLBACK is a ChatGPT provider")
	ck.StrContains(ce.Message, chatGPTTestProvider, "the refusal must name the fallback provider")

	// An owner with no login for that provider: refused with the fix.
	err = c.checkLoginProviderUsable(context.Background(), req, ownerA)
	ce = controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "spawn whose fallback has no login")
	ck.Eq("no ChatGPT login for chatgpt; run: rafiki providers login chatgpt", ce.Message, "message")

	// And admitted once the login exists, so the refusal above is a refusal of
	// something that otherwise works.
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	ck.NoError(c.checkLoginProviderUsable(context.Background(), req, ownerA),
		"a spawn whose fallback provider has a login was refused")
}

// Spawn itself must call the check. The unit tests above would all still pass
// with the call deleted, so this drives the real entry point and asserts both
// halves of the refusal: a coded error, and no child row.
func TestSpawnRefusesChatGPTModelWithoutLogin(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := newFakeCredStore()
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: chatGPTTestProvider + "/gpt-6.1-sol",
	}

	res, err := c.Spawn(context.Background(), req, users.Identity{UserID: ownerA})
	ck.Require().Error(err, "Spawn admitted a ChatGPT child with no login, child id %q", res.ChildID)
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "code")
	ck.Eq("no ChatGPT login for chatgpt; run: rafiki providers login chatgpt", ce.Message, "message")
	ck.Len(c.st.List(), 0, "a refused spawn wrote a child row: %+v", c.st.List())

	// The same spawn with the login in place gets past the check (and past the
	// refusal's position in the pipeline), so the assertion above is about the
	// login and not about some earlier refusal.
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	if _, err := c.Spawn(context.Background(), req, users.Identity{UserID: ownerA}); err != nil {
		ck.False(controllerErrCodeIs(err, protocol.ErrFailedPrecondition),
			"the control spawn was still refused for a missing login: %v", err)
	}
}

// controllerErrCodeIs reports whether err is a ControllerError carrying code.
func controllerErrCodeIs(err error, code string) bool {
	var ce *connectapi.ControllerError
	return errors.As(err, &ce) && ce.Code == code
}

// ─── Owner model directory ───────────────────────────────────────────────────

// The credential owner is the STORED owner, never the owner LABEL.
//
// On resume and recovery there is no connection, and a child row older than
// owner_user_id carries only Labels["owner"], which is an OS USERNAME. Spending
// whichever rafiki account shares that username is the owner-isolation hole this
// rule closes.
func TestCredentialOwnerIsNeverTheOwnerLabel(t *testing.T) {
	ck := assert.NewCollecting(t)

	// A fresh spawn has an authenticated identity and no snapshot.
	ck.Eq(ownerA, credentialOwnerUserID(ownerA, nil), "spawn: the authenticated owner is the credential owner")
	// Resume / recovery: the stored column only.
	ck.Eq(ownerA, credentialOwnerUserID("user-who-happens-to-share-the-username",
		&childstore.Snapshot{OwnerUserID: ownerA, Labels: map[string]string{"owner": "brent"}}), "resume: stored owner")
	// A legacy row with no stored owner gets NO credential owner at all, even
	// though its label resolves to somebody.
	ck.Eq("", credentialOwnerUserID("user-who-happens-to-share-the-username",
		&childstore.Snapshot{Labels: map[string]string{"owner": "brent"}}), "legacy row: no stored owner means nobody")

	// And the spawn builder never asks the store about the label-resolved user.
	store := &recordingCredStore{fakeCredStore: newFakeCredStore()}
	store.seed(liveCredential(ownerA, chatGPTModelsJSON))
	c := spawnTestChatGPTController(t, chatGPTTestCreds(t, store))
	const labelOwner = "user-who-happens-to-share-the-username"
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: chatGPTTestProvider + "/gpt-6.1-sol"}
	_, err := c.agentRunner(req, "c_resume_owner", false, "brent", labelOwner,
		&childstore.Snapshot{OwnerUserID: ownerA, Labels: map[string]string{"owner": "brent"}})
	ck.Require().NoError(err, "agentRunner(resume)")
	ck.EqDeep([]string{ownerA}, store.ownersRead(),
		"a resume read the credential of an owner the label named instead of the stored one")
}

// ─── helpers ────────────────────────────────────────────────────────────────

// waitForModelsRequests waits until the fixture has seen at least n requests.
func waitForModelsRequests(t *testing.T, fx *modelsFixture, n int) {
	t.Helper()
	assert.Eventually(t, 10*time.Second, "the model directory fetch to arrive", func() error {
		if len(fx.recorded()) < n {
			return errors.New("no request yet")
		}
		return nil
	})
}

// dirContextWindow reads the cached directory the way the spawn path does.
func dirContextWindow(t *testing.T, creds *providerCreds, owner, slug string) int {
	t.Helper()
	dir, err := creds.ownerModels(t.Context(), owner, chatGPTTestProvider)
	assert.NewAborting(t).NoError(err, "ownerModels")
	return dir.contextWindow(slug)
}

func rowsByID(rows []connectapi.ModelRow) map[string]connectapi.ModelRow {
	out := make(map[string]connectapi.ModelRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}
