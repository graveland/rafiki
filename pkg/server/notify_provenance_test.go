// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// The notify provenance is the security boundary around a {notify} token: a
// narrowed credential that may call Control.Notify and nothing else. These
// tests pin the three shapes the token row can arrive in (nil capabilities =
// full user, exactly {notify} = notify-scoped, anything else = refused) and
// the confinement the middleware applies to the notify one.

// notifyToken is the fixture token value; its row lives in notifyStore.
const notifyToken = "rfk_notify_scoped"

// pathRequest builds an authenticated request for path, served by auth's
// middleware. Served through the paths a caller actually reaches, because the
// confinement is a property of the path.
func pathRequest(t *testing.T, a *UserTokenAuth, method, path, token string) (int, string, *Identity, bool) {
	t.Helper()
	var (
		id   *Identity
		next bool
	)
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		next = true
		id = IdentityFromContext(r.Context())
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), id, next
}

// notifyStore is a users.Store whose Authenticate answers exactly the rows a
// test seeded, keyed by token value.
func notifyStore(rows map[string]users.Identity) *stubStore { return &stubStore{tokens: rows} }

// A {notify} row resolves to ProvenanceNotify and keeps naming its owner; a
// NULL-capability row stays a plain user; anything else is an unknown token.
func TestNotifyProvenanceResolvedFromCapabilities(t *testing.T) {
	c := assert.NewAborting(t)
	st := notifyStore(map[string]users.Identity{
		notifyToken: {
			UserID: "u1", Username: "hooks", TokenID: "t_notify",
			Capabilities: []string{users.CapabilityNotify},
		},
		"rfk_full": {UserID: "u2", Username: "brent", TokenID: "t_full"},
		"rfk_other": {
			UserID: "u3", Username: "other", TokenID: "t_other",
			Capabilities: []string{"x"},
		},
	})
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	code, body, id, next := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
	c.Eq(http.StatusOK, code, "{notify} on Notify: status = %d, want 200: %s", code, body)
	c.True(next, "{notify} on Notify: next was never called")
	c.False(id == nil, "{notify} on Notify: no identity on the context")
	if id != nil {
		c.Eq(ProvenanceNotify, id.Via, "{notify} must resolve to ProvenanceNotify, got %v", id.Via)
		c.Eq("u1", id.UserID, "a notify-scoped identity still names its owner")
		c.Eq("hooks", id.Username, "username")
		c.Eq("t_notify", id.TokenID, "the credential must stay revocable by id")
		c.False(id.IsUserCredential(), "a notify-scoped token is NOT a user credential")
	}

	// nil capabilities: full user authority, unchanged — ProvenanceUser.
	code, body, id, _ = pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", "rfk_full")
	c.Eq(http.StatusOK, code, "NULL capabilities: status = %d: %s", code, body)
	c.False(id == nil, "NULL capabilities: no identity")
	if id != nil {
		c.Eq(ProvenanceUser, id.Via, "NULL capabilities must stay ProvenanceUser, got %v", id.Via)
	}

	// An unknown capability name is an unknown token — never a silent user.
	code, body, id, next = pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", "rfk_other")
	c.Eq(http.StatusUnauthorized, code, `{"x"} must be refused 401, got %d: %s`, code, body)
	c.False(next, `{"x"} reached next`)
	c.Nil(id, `{"x"} resolved an identity: %+v`, id)
}

// Every capability set that is not NULL and not exactly {notify} is refused as
// an invalid token, on both faces. Each set here is one an ordinary mistake
// produces: '{}' a token minted to do nothing, a duplicate, a narrowed token
// someone also marked operator, and a name a future release dropped.
func TestNotifyProvenanceRefusesEveryOtherCapabilitySet(t *testing.T) {
	c := assert.NewAborting(t)
	for name, caps := range map[string][]string{
		"empty":        {},
		"duplicate":    {users.CapabilityNotify, users.CapabilityNotify},
		"with-admin":   {users.CapabilityNotify, "admin"},
		"unknown":      {"x"},
		"admin-only":   {"admin"},
		"notify-first": {users.CapabilityNotify, "x"},
	} {
		t.Run(name, func(t *testing.T) {
			st := notifyStore(map[string]users.Identity{
				notifyToken: {UserID: "u1", Username: "hooks", TokenID: "t1", Capabilities: caps},
			})
			a := NewUserTokenAuth(st, "childsecret", time.Minute)

			code, body, id, next := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
			c.Eq(http.StatusUnauthorized, code, "%v must be refused 401, got %d: %s", caps, code, body)
			c.False(next, "%v reached next", caps)
			c.Nil(id, "%v resolved an identity: %+v", caps, id)

			// The Connect mounts resolve through IdentifyStrict, and must
			// answer the same refusal (Unauthenticated "invalid auth token")
			// rather than downgrade to anonymous.
			got, err := a.IdentifyStrict(context.Background(), http.Header{"Authorization": {"Bearer " + notifyToken}})
			c.Nil(got, "%v: IdentifyStrict resolved %+v", caps, got)
			c.True(errors.Is(err, users.ErrNotFound), "%v: IdentifyStrict err = %v, want users.ErrNotFound", caps, err)
		})
	}
}

// A notify-scoped row never inherits the operator bit, whatever the users row
// says: the token is narrowed by its capabilities, and IsAdmin is copied only
// for ProvenanceUser.
func TestNotifyProvenanceNeverAdmin(t *testing.T) {
	c := assert.NewAborting(t)
	st := notifyStore(map[string]users.Identity{
		notifyToken: {
			UserID: "u1", Username: "hooks", TokenID: "t1", IsAdmin: true,
			Capabilities: []string{users.CapabilityNotify},
		},
	})
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	code, body, id, _ := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
	c.Eq(http.StatusOK, code, "status = %d: %s", code, body)
	c.False(id == nil, "no identity on the context")
	if id == nil {
		return
	}
	c.Eq(ProvenanceNotify, id.Via, "provenance")
	c.False(id.IsAdmin, "a {notify} token resolved IsAdmin true: %+v", id)
}

// The confinement is applied at the middleware, before any handler: every face
// but Control.Notify answers 403 and next is never reached. A notify token is
// a write-only doorway into one RPC, not a user credential that happens to
// lack the operator bit.
func TestNotifyProvenanceMiddlewareConfinesPath(t *testing.T) {
	c := assert.NewAborting(t)
	a := NewUserTokenAuth(notifyStore(map[string]users.Identity{
		notifyToken: {
			UserID: "u1", Username: "hooks", TokenID: "t1",
			Capabilities: []string{users.CapabilityNotify},
		},
	}), "childsecret", time.Minute)

	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/chat/completions",
		"/mcp",
		"/rafiki.v1.Control/ListModels",
		"/rafiki.v1.Control/Spawn",
	} {
		// Every method, because the confinement is on the path alone.
		for _, method := range []string{"POST", "GET"} {
			code, body, _, next := pathRequest(t, a, method, path, notifyToken)
			c.Eq(http.StatusForbidden, code, "%s %s: status = %d, want 403: %s", method, path, code, body)
			c.False(next, "%s %s reached the handler", method, path)
			c.StrContains(body, "notify-scoped token may only call /rafiki.v1.Control/Notify",
				"%s %s: body = %q", method, path, body)
		}
	}

	code, body, _, next := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
	c.Eq(http.StatusOK, code, "Control.Notify must be admitted: %d %s", code, body)
	c.True(next, "Control.Notify did not reach the handler")
}

// The cached identity carries the provenance, so the SECOND request with the
// same token is confined identically — a cache hit must not fall back to
// "user token, allowed everywhere".
func TestNotifyProvenanceSurvivesAuthCache(t *testing.T) {
	c := assert.NewAborting(t)
	st := notifyStore(map[string]users.Identity{
		notifyToken: {
			UserID: "u1", Username: "hooks", TokenID: "t1",
			Capabilities: []string{users.CapabilityNotify},
		},
	})
	// Long TTL: the second request is guaranteed to be a cache hit.
	a := NewUserTokenAuth(st, "childsecret", time.Hour)

	code, body, _, _ := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
	c.Eq(http.StatusOK, code, "first request (store read): %d %s", code, body)

	code, body, id, next := pathRequest(t, a, "POST", "/v1/messages", notifyToken)
	c.Eq(http.StatusForbidden, code, "cached identity on /v1/messages: status = %d, want 403: %s", code, body)
	c.False(next, "cached identity reached /v1/messages")
	c.Nil(id, "the refused request must not carry an identity: %+v", id)
	c.Eq(int64(1), st.calls.Load(), "the second request must be served from cache, store calls")
}

// A {notify} row that names no user is refused: the notify path resolves its
// target against the caller's owner, and an identity with no owner is not the
// anonymous caller but an unattributable one.
func TestNotifyProvenanceRequiresAnOwner(t *testing.T) {
	c := assert.NewAborting(t)
	a := NewUserTokenAuth(notifyStore(map[string]users.Identity{
		notifyToken: {Username: "hooks", TokenID: "t1", Capabilities: []string{users.CapabilityNotify}},
	}), "childsecret", time.Minute)

	code, body, id, next := pathRequest(t, a, "POST", "/rafiki.v1.Control/Notify", notifyToken)
	c.Eq(http.StatusUnauthorized, code, "a {notify} row with no user must be refused, got %d: %s", code, body)
	c.False(next, "an ownerless {notify} identity reached the handler")
	c.Nil(id, "an ownerless {notify} identity resolved: %+v", id)
}
