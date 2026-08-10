// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A caller that puts rafiki's token in X-Rafiki-Token is declaring that its
// Authorization header holds its own upstream credential.
func TestStaticTokenAuth_XRafikiTokenMarksPassthrough(t *testing.T) {
	auth := NewStaticTokenAuth(map[string]string{"cli": "rafiki-token"})
	var gotIdentity, gotCred string
	h := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if id := IdentityFromContext(r.Context()); id != nil {
			gotIdentity = id.Username
		}
		gotCred = PassthroughCredential(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotIdentity != "cli" {
		t.Errorf("identity = %q, want %q", gotIdentity, "cli")
	}
	if gotCred != "Bearer sk-ant-oat01-client" {
		t.Errorf("passthrough credential = %q, want the client's Authorization verbatim", gotCred)
	}
}

// The ordinary path must be untouched: an Authorization-authenticated request
// has no passthrough credential, or every existing caller would start leaking
// its bearer upstream.
func TestStaticTokenAuth_BearerIsNotPassthrough(t *testing.T) {
	auth := NewStaticTokenAuth(map[string]string{"cli": "rafiki-token"})
	var gotCred string
	h := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCred = PassthroughCredential(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotCred != "" {
		t.Errorf("passthrough credential = %q, want empty", gotCred)
	}
}

// X-Rafiki-Token is a credential like any other: an unknown value is a 401,
// not an anonymous pass.
func TestStaticTokenAuth_XRafikiTokenUnknownRejected(t *testing.T) {
	auth := NewStaticTokenAuth(map[string]string{"cli": "rafiki-token"})
	h := auth.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached with an unknown token")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "not-a-real-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// X-Rafiki-Token without any Authorization is legal — it just means there is
// nothing to pass through, and the daemon key is used as usual.
func TestStaticTokenAuth_XRafikiTokenWithoutAuthorization(t *testing.T) {
	auth := NewStaticTokenAuth(map[string]string{"cli": "rafiki-token"})
	var gotCred string
	h := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCred = PassthroughCredential(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotCred != "" {
		t.Errorf("passthrough credential = %q, want empty", gotCred)
	}
}