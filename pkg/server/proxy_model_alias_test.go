// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// fakeAliasResolver stands in for the daemon's Controller-side alias resolver:
// it answers from a fixed target map, records what it was asked, and can be
// made to fail the way a store outage does.
type fakeAliasResolver struct {
	targets map[string]string
	err     error
	owners  []string
	models  []string
}

func (f *fakeAliasResolver) LookupModelAlias(_ context.Context, ownerUserID, model string) (string, bool, error) {
	f.owners = append(f.owners, ownerUserID)
	f.models = append(f.models, model)
	if f.err != nil {
		return "", false, f.err
	}
	target, ok := f.targets[model]
	if !ok {
		return "", false, nil
	}
	return target, true, nil
}

// newAliasTestProxy builds a capture-backed proxy with BOTH a fake Anthropic
// primary and a fake OpenRouter upstream, each recording the request bodies it
// receives, so a test can prove which one a request reached and with which
// model.
func newAliasTestProxy(t *testing.T, alias ModelAliasResolver, res RoutingResolver, auth Authenticator) (p *MessagesProxy, anthropic, or *[][]byte) {
	t.Helper()
	anthropicBodies := &[][]byte{}
	orBodies := &[][]byte{}
	record := func(into *[][]byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			*into = append(*into, b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":3}}`)
		}
	}
	anthropicSrv := httptest.NewServer(record(anthropicBodies))
	t.Cleanup(anthropicSrv.Close)
	orSrv := httptest.NewServer(record(orBodies))
	t.Cleanup(orSrv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p = NewMessagesProxy(nil, auth, "real-key", anthropicSrv.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = &fakeProxyStore{}
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))
	if res != nil {
		p.SetRoutingResolver(res)
	}
	if alias != nil {
		p.SetModelAliasResolver(alias)
	}
	return p, anthropicBodies, orBodies
}

// modelOfAliasBody reads the model an upstream received.
func modelOfAliasBody(t *testing.T, body []byte) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, body)
	}
	m, _ := payload["model"].(string)
	return m
}

func aliasRequest(t *testing.T, p *MessagesProxy, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	p.ServeHTTP(rec, req)
	return rec
}

// TestProxyModelAliasRewritesModel proves a slash-free name is resolved through
// the owner's aliases before it leaves the proxy: the OpenRouter upstream sees
// the alias's target, with the face's own "openrouter/" prefix stripped.
func TestProxyModelAliasRewritesModel(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "openrouter/~deepseek/deepseek-flash-latest"}}
	p, anthropic, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Len(*anthropic, 0, "Anthropic requests (an alias target with a slash is an OpenRouter id)")
	c.Require().Len(*or, 1, "OpenRouter requests")
	c.Eq("~deepseek/deepseek-flash-latest", modelOfAliasBody(t, (*or)[0]), "model reaching OpenRouter")
	c.EqDiff([]string{"ds"}, alias.models, "model the resolver was asked with")
}

// TestProxyModelAliasAnthropicTarget proves an alias may target Anthropic: the
// "anthropic/" prefix is stripped and the request stays on the primary.
func TestProxyModelAliasAnthropicTarget(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "anthropic/claude-sonnet-5-5"}}
	p, anthropic, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Len(*or, 0, "OpenRouter requests (an anthropic/ target is not a slash id)")
	c.Require().Len(*anthropic, 1, "Anthropic requests")
	c.Eq("claude-sonnet-5-5", modelOfAliasBody(t, (*anthropic)[0]), "model reaching Anthropic")
}

// TestProxyModelAliasSpecReachesProvider proves a target's [spec] reaches the
// wire: the provider object carries the alias's sort.
func TestProxyModelAliasSpecReachesProvider(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "openrouter/~deepseek/deepseek-flash-latest[sort=price]"}}
	p, _, or := newAliasTestProxy(t, alias, &recordingResolver{}, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Require().Len(*or, 1, "OpenRouter requests")
	c.Eq("~deepseek/deepseek-flash-latest", modelOfAliasBody(t, (*or)[0]), "model reaching OpenRouter (the spec is not part of the id)")
	c.Eq("price", providerOf(t, (*or)[0])["sort"], "provider.sort")
}

// TestProxyModelAliasSpecCannotClearPolicyDataFlags proves the alias spec merges
// OVER the session/policy spec without clearing its data boundary: the alias's
// sort wins, and the policy's nodata/zdr still hold.
func TestProxyModelAliasSpecCannotClearPolicyDataFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	policy, err := routing.ParseSpec("sort=latency,nodata,zdr")
	c.Require().NoError(err, "parse policy spec")
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "openrouter/~deepseek/deepseek-flash-latest[sort=price]"}}
	p, _, or := newAliasTestProxy(t, alias, &recordingResolver{spec: policy}, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Require().Len(*or, 1, "OpenRouter requests")
	prov := providerOf(t, (*or)[0])
	c.Eq("price", prov["sort"], "provider.sort (the alias's own key wins over policy)")
	c.Eq("deny", prov["data_collection"], "provider.data_collection (policy nodata survives)")
	c.Eq(true, prov["zdr"], "provider.zdr (policy zdr survives)")
}

// TestProxyModelAliasMissFallsBack proves a miss is not an error: a bare
// claude-* id (what Claude Code's background calls and /model menu send) is
// forwarded exactly as it was before this face knew about aliases.
func TestProxyModelAliasMissFallsBack(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{}}
	p, anthropic, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "claude-haiku-4-5-20251001")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Len(*or, 0, "OpenRouter requests")
	c.Require().Len(*anthropic, 1, "Anthropic requests")
	c.Eq("claude-haiku-4-5-20251001", modelOfAliasBody(t, (*anthropic)[0]), "model reaching Anthropic")
	c.EqDiff([]string{"claude-haiku-4-5-20251001"}, alias.models, "resolver was consulted for the bare id")
}

// TestProxyModelAliasStoreError503 proves a lookup failure is surfaced, never
// swallowed: forwarding the alias NAME upstream would silently send traffic to
// a model the caller never asked for.
func TestProxyModelAliasStoreError503(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{err: errors.New("db down")}
	p, anthropic, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusServiceUnavailable, rec.Code, "status")
	c.StrContains(rec.Body.String(), "model alias lookup failed", "error body")
	c.Len(*anthropic, 0, "Anthropic requests")
	c.Len(*or, 0, "OpenRouter requests")
}

// TestProxyModelAliasForeignProvider400 proves an alias naming a provider this
// face cannot route is refused loudly rather than misrouted.
func TestProxyModelAliasForeignProvider400(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "vmlx/qwen"}}
	p, anthropic, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "ds")

	c.Eq(http.StatusBadRequest, rec.Code, "status")
	c.StrContains(rec.Body.String(), `model alias "ds" targets provider "vmlx", which this proxy cannot route`, "error body")
	c.Len(*anthropic, 0, "Anthropic requests")
	c.Len(*or, 0, "OpenRouter requests")
}

// TestProxyModelAliasSlashedSkipsResolver proves a slashed model id is a model
// id and never a lookup: the resolver must not even be consulted.
func TestProxyModelAliasSlashedSkipsResolver(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"deepseek/deepseek-v4-flash": "openrouter/x/y"}}
	p, _, or := newAliasTestProxy(t, alias, nil, nil)

	rec := aliasRequest(t, p, "deepseek/deepseek-v4-flash")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Require().Len(*or, 1, "OpenRouter requests")
	c.Eq("deepseek/deepseek-v4-flash", modelOfAliasBody(t, (*or)[0]), "model reaching OpenRouter")
	c.Eq(0, len(alias.models), "resolver calls for a slashed id")
}

// TestProxyModelAliasOwnerFromCredential proves the lookup is scoped to the
// authenticated identity's user id — the credential, never a header.
func TestProxyModelAliasOwnerFromCredential(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "openrouter/~deepseek/deepseek-flash-latest"}}
	auth := staticAuthenticator{id: &Identity{UserID: "9f1c7b2e-0000-4000-8000-000000000001", Username: "brent", Via: ProvenanceUser}}
	p, _, or := newAliasTestProxy(t, alias, nil, auth)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"ds","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	// A caller-supplied header must not be able to pick another owner.
	req.Header.Set("X-Rafiki-User", "someone-else")
	p.ServeHTTP(rec, req)

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Require().Len(*or, 1, "OpenRouter requests")
	c.EqDiff([]string{"9f1c7b2e-0000-4000-8000-000000000001"}, alias.owners, "owner the resolver was asked with")
}

// TestProxyModelAliasCountTokensAgreesWithMessages proves the token-counting
// endpoint resolves aliases the same way /v1/messages does — same rewrite, same
// 503 on a lookup failure — so a client that counts tokens before a turn sees
// the model the turn will actually use.
func TestProxyModelAliasCountTokensAgreesWithMessages(t *testing.T) {
	c := assert.NewCollecting(t)
	alias := &fakeAliasResolver{targets: map[string]string{"ds": "anthropic/claude-sonnet-5-5"}}
	p, anthropic, _ := newAliasTestProxy(t, alias, nil, nil)

	rec := httptest.NewRecorder()
	p.ServeCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"ds","messages":[{"role":"user","content":"hi"}]}`)))
	c.Eq(http.StatusOK, rec.Code, "count_tokens status")
	c.Require().Len(*anthropic, 1, "Anthropic count_tokens requests")
	c.Eq("claude-sonnet-5-5", modelOfAliasBody(t, (*anthropic)[0]), "model reaching Anthropic for a token count")

	failing := &fakeAliasResolver{err: errors.New("db down")}
	p2, anthropic2, _ := newAliasTestProxy(t, failing, nil, nil)
	rec = httptest.NewRecorder()
	p2.ServeCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"ds","messages":[{"role":"user","content":"hi"}]}`)))
	c.Eq(http.StatusServiceUnavailable, rec.Code, "count_tokens status on a lookup failure")
	c.StrContains(rec.Body.String(), "model alias lookup failed", "count_tokens error body")
	c.Len(*anthropic2, 0, "upstream requests after a lookup failure")
}
