// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
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

// recordingResolver stands in for the daemon's Controller-side resolver: it
// records every (session, model) it is asked about and answers with a fixed
// spec.
type recordingResolver struct {
	spec     routing.Spec
	sessions []string
	models   []string
}

func (r *recordingResolver) RoutingFor(sessionID, modelID string) routing.Spec {
	r.sessions = append(r.sessions, sessionID)
	r.models = append(r.models, modelID)
	return r.spec
}

// newRoutingTestProxy builds a capture-backed proxy whose OpenRouter upstream
// is a stub recording every request body. model has no provider pin, so the
// injected provider object is attributable to the spec alone.
func newRoutingTestProxy(t *testing.T, res RoutingResolver) (p *MessagesProxy, bodies *[][]byte) {
	t.Helper()
	bodies = &[][]byte{}
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":3}}`)
	}))
	t.Cleanup(orSrv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p = NewMessagesProxy(nil, nil, "real-key", "http://unused-primary", "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = &fakeProxyStore{}
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))
	if res != nil {
		p.SetRoutingResolver(res)
	}
	return p, bodies
}

// providerOf extracts the provider-routing object from a captured upstream
// body, failing the test when the body is not JSON.
func providerOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, body)
	}
	prov, _ := payload["provider"].(map[string]any)
	return prov
}

// listOf reads a decoded-JSON list out of a provider object, failing the test
// when the key is absent or not a list.
func listOf(t *testing.T, prov map[string]any, key string) []any {
	t.Helper()
	v, ok := prov[key].([]any)
	if !ok {
		t.Fatalf("provider.%s = %v (%T), want a list", key, prov[key], prov[key])
	}
	return v
}

// TestProxyRoutingFromSession proves the resolver's spec reaches the wire: a
// session resolved to sort=price,quant=fp8+ sends a provider object carrying
// the sort and the floor expanded to its quantization tier and above, and the
// resolver was asked with the request's X-Rafiki-Session value verbatim.
func TestProxyRoutingFromSession(t *testing.T) {
	c := assert.NewCollecting(t)
	spec, err := routing.ParseSpec("sort=price,quant=fp8+")
	c.Require().NoError(err, "parse spec")
	res := &recordingResolver{spec: spec}
	p, bodies := newRoutingTestProxy(t, res)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Rafiki-Session", "c_child7")
	p.ServeHTTP(rec, req)

	c.Require().Len(*bodies, 1, "OpenRouter requests")
	prov := providerOf(t, (*bodies)[0])

	c.EqDiff([]string{"c_child7"}, res.sessions, "session the resolver was asked with")
	c.EqDiff([]string{"openai/gpt-4o"}, res.models, "model the resolver was asked with")
	c.Eq("price", prov["sort"], "provider.sort")
	// quant=fp8+ is a floor: its tier (int8/fp8/mxfp8) and every tier above.
	c.EqDiff([]any{"int8", "fp8", "mxfp8", "fp16", "bf16", "fp32"}, listOf(t, prov, "quantizations"), "provider.quantizations")
	c.Nil(prov["only"], "spec names no only")
	c.Nil(prov["ignore"], "no guard bans to fold in")
	_, hasData := prov["data_collection"]
	c.False(hasData, "spec has no nodata flag, got %v", prov["data_collection"])
	_, hasZDR := prov["zdr"]
	c.False(hasZDR, "spec has no zdr flag")
}

// TestProxyRoutingCallerProviderWinsButKeepsData proves a caller-supplied
// provider object survives the resolver: its own sort is left alone (the spec
// does not override caller routing choices) while the spec's data policy —
// the one boundary — is forced on top of it.
func TestProxyRoutingCallerProviderWinsButKeepsData(t *testing.T) {
	c := assert.NewCollecting(t)
	spec, err := routing.ParseSpec("nodata")
	c.Require().NoError(err, "parse spec")
	res := &recordingResolver{spec: spec}
	p, bodies := newRoutingTestProxy(t, res)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"openai/gpt-4o","provider":{"sort":"latency"},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Rafiki-Session", "c_child7")
	p.ServeHTTP(rec, req)

	c.Require().Len(*bodies, 1, "OpenRouter requests")
	prov := providerOf(t, (*bodies)[0])
	c.Eq("latency", prov["sort"], "caller's sort must survive the resolver")
	c.Eq("deny", prov["data_collection"], "spec nodata forces data_collection over the caller's object")
	_, hasZDR := prov["zdr"]
	c.False(hasZDR, "spec has no zdr flag")
}

// TestProxyRoutingCallerOnlySkipsIgnore proves the guard's ignore list is NOT
// merged into a caller-supplied provider object that names an only (an
// explicit routing decision bypasses bans), while an object without an only
// still takes the merge — a budget guard a caller can switch off by sending a
// provider block is not a guard.
func TestProxyRoutingCallerOnlySkipsIgnore(t *testing.T) {
	c := assert.NewCollecting(t)
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler))
	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{
			Provider: "CoreWeave", Model: "deepseek/deepseek-v4-pro", Conversation: "c1",
			PrefixHash: "h1", InputTokens: 50000, CacheReadTokens: 0,
		})
	}
	res := &recordingResolver{} // zero spec: nothing to inject or force
	p, bodies := newRoutingTestProxy(t, res)
	p.SetProviderGuard(g)

	send := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Rafiki-Session", "c_child7")
		p.ServeHTTP(rec, req)
		got := providerOf(t, (*bodies)[len(*bodies)-1])
		return got
	}

	prov := send(`{"model":"deepseek/deepseek-v4-pro","provider":{"only":["novita"]},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.EqDiff([]any{"novita"}, listOf(t, prov, "only"), "caller's only survives")
	c.Nil(prov["ignore"], "caller's only must bypass the guard's bans, got %v", prov["ignore"])

	prov = send(`{"model":"deepseek/deepseek-v4-pro","provider":{"sort":"latency"},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.Eq("latency", prov["sort"], "caller's sort survives the merge")
	c.EqDiff([]any{"coreweave"}, listOf(t, prov, "ignore"), "no only on the caller object: the guard's ban is merged in")
}

// TestProxyRoutingCallerDataPolicyForced pins the OVERWRITE direction of the
// data-policy force and its composition with a caller only: a caller object
// that carries its own data_collection/zdr CANNOT clear them (a caller's
// "allow" is overwritten to "deny", a "false" zdr to "true"), and an only
// that bypasses bans does not bypass data policy — nodata still rides beside
// the caller's only. (Waves-1-2 checkpoint review finding 3.)
func TestProxyRoutingCallerDataPolicyForced(t *testing.T) {
	c := assert.NewCollecting(t)
	spec, err := routing.ParseSpec("nodata,zdr")
	c.Require().NoError(err, "parse spec")
	res := &recordingResolver{spec: spec}
	p, bodies := newRoutingTestProxy(t, res)

	send := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Rafiki-Session", "c_child7")
		p.ServeHTTP(rec, req)
		return providerOf(t, (*bodies)[len(*bodies)-1])
	}

	// The caller asks for the OPPOSITE policy: both keys are overwritten.
	prov := send(`{"model":"openai/gpt-4o","provider":{"data_collection":"allow","zdr":false},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.Eq("deny", prov["data_collection"], "a caller's allow cannot clear the spec's nodata")
	c.Eq(true, prov["zdr"], "a caller's zdr:false cannot clear the spec's zdr")

	// An only bypasses bans — it does NOT bypass data policy (decision 5):
	// the caller's only survives AND both forced keys ride beside it.
	prov = send(`{"model":"openai/gpt-4o","provider":{"only":["novita"]},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.EqDiff([]any{"novita"}, listOf(t, prov, "only"), "caller's only survives")
	c.Eq("deny", prov["data_collection"], "nodata rides beside a caller only")
	c.Eq(true, prov["zdr"], "zdr rides beside a caller only")
}

// TestProxyRoutingNoResolverUnchanged proves a proxy without a resolver keeps
// the pre-spec behaviour exactly: the static pin is still injected (and only
// the pin — no sort/quant/data fields appear), a caller object still wins for
// the pin, and the guard's ignore is still merged into a caller object even
// when it names an only.
func TestProxyRoutingNoResolverUnchanged(t *testing.T) {
	c := assert.NewCollecting(t)
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler))
	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{
			Provider: "CoreWeave", Model: "deepseek/deepseek-v4-pro", Conversation: "c1",
			PrefixHash: "h1", InputTokens: 50000, CacheReadTokens: 0,
		})
	}
	p, bodies := newRoutingTestProxy(t, nil)
	p.SetProviderGuard(g)

	send := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-token")
		p.ServeHTTP(rec, req)
		return providerOf(t, (*bodies)[len(*bodies)-1])
	}

	// Pinned model line: pin prefs injected, nothing spec-shaped added.
	prov := send(`{"model":"z-ai/glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.EqDiff([]any{"fireworks"}, listOf(t, prov, "only"), "pin still injected without a resolver")
	for _, key := range []string{"sort", "quantizations", "data_collection", "zdr", "ignore"} {
		c.Nil(prov[key], "pin-only object must not carry %q", key)
	}

	// Caller object with an only: wins for the pin, and — no resolver, so no
	// only-bypass rule — still takes the guard's ignore merge.
	prov = send(`{"model":"deepseek/deepseek-v4-pro","provider":{"only":["novita"]},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.EqDiff([]any{"novita"}, listOf(t, prov, "only"), "caller provider wins for the pin")
	c.EqDiff([]any{"coreweave"}, listOf(t, prov, "ignore"), "without a resolver the guard merges into a caller only as before")

	// Unpinned model, no caller object: no provider object at all.
	prov = send(`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	c.Nil(prov, "unpinned model must carry no provider object")
}
