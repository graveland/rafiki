// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

const testSecret = "hook-secret"

// signHex is the vector the senders produce: HMAC-SHA256 over the raw body,
// hex-encoded. Computed here with crypto/hmac rather than pasted, so the test
// states the scheme rather than a constant that could only be reproduced by
// running the code under test.
func signHex(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func request(t *testing.T, headers map[string]string, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSourceGitHub(t *testing.T) {
	const body = `{"action":"failure"}`
	secret := []byte(testSecret)
	src := githubSource{}

	t.Run("valid signature", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha256=" + signHex(testSecret, body),
			"X-GitHub-Event":      "workflow_run",
			"X-GitHub-Delivery":   "delivery-1",
		}, body)
		c.NoError(src.Verify(r, []byte(body), secret))
		eventType, deliveryID := src.Event(r)
		c.Eq("workflow_run", eventType)
		c.Eq("delivery-1", deliveryID)
	})

	t.Run("wrong secret", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha256=" + signHex("another-secret", body),
		}, body)
		c.ErrorIs(src.Verify(r, []byte(body), secret), errBadSignature)
	})

	t.Run("tampered body", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha256=" + signHex(testSecret, body),
		}, body)
		c.ErrorIs(src.Verify(r, []byte(body+" "), secret), errBadSignature)
	})

	t.Run("missing header", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, nil, body)
		c.ErrorIs(src.Verify(r, []byte(body), secret), errMissingSignature)
	})

	t.Run("sha1 prefix is refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha1=" + signHex(testSecret, body),
		}, body)
		c.Error(src.Verify(r, []byte(body), secret))
	})

	t.Run("uppercase hex is accepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha256=" + strings.ToUpper(signHex(testSecret, body)),
		}, body)
		c.NoError(src.Verify(r, []byte(body), secret))
	})

	t.Run("non-hex signature is refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{
			githubSignatureHeader: "sha256=zzzz",
		}, body)
		c.Error(src.Verify(r, []byte(body), secret))
	})
}

func TestSourceGitea(t *testing.T) {
	c := assert.NewAborting(t)
	const body = `{"ref":"refs/heads/main"}`
	secret := []byte(testSecret)
	src := giteaSource{}

	r := request(t, map[string]string{
		giteaSignatureHeader: signHex(testSecret, body),
		"X-Gitea-Event":      "push",
		"X-Gitea-Delivery":   "gitea-1",
	}, body)
	c.NoError(src.Verify(r, []byte(body), secret))
	eventType, deliveryID := src.Event(r)
	c.Eq("push", eventType)
	c.Eq("gitea-1", deliveryID)

	t.Run("wrong secret", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{giteaSignatureHeader: signHex("another-secret", body)}, body)
		c.ErrorIs(src.Verify(r, []byte(body), secret), errBadSignature)
	})

	t.Run("tampered body", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{giteaSignatureHeader: signHex(testSecret, body)}, body)
		c.ErrorIs(src.Verify(r, []byte(body+"x"), secret), errBadSignature)
	})

	t.Run("sha256 prefix is refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{giteaSignatureHeader: "sha256=" + signHex(testSecret, body)}, body)
		c.Error(src.Verify(r, []byte(body), secret))
	})

	t.Run("uppercase hex is accepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := request(t, map[string]string{giteaSignatureHeader: strings.ToUpper(signHex(testSecret, body))}, body)
		c.NoError(src.Verify(r, []byte(body), secret))
	})

	t.Run("missing header", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.ErrorIs(src.Verify(request(t, nil, body), []byte(body), secret), errMissingSignature)
	})
}

func TestSourceGeneric(t *testing.T) {
	c := assert.NewAborting(t)
	src := genericSource{header: "X-Omada-Secret"}
	secret := []byte(testSecret)

	r := request(t, map[string]string{"X-Omada-Secret": testSecret}, "anything")
	c.NoError(src.Verify(r, []byte("anything"), secret))
	eventType, deliveryID := src.Event(r)
	c.Eq("", eventType)
	c.Eq("", deliveryID)

	t.Run("wrong value", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.ErrorIs(src.Verify(request(t, map[string]string{"X-Omada-Secret": "nope"}, ""), nil, secret), errBadSignature)
	})

	t.Run("prefix of the secret is refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.ErrorIs(src.Verify(request(t, map[string]string{"X-Omada-Secret": testSecret[:4]}, ""), nil, secret), errBadSignature)
	})

	t.Run("missing header", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.ErrorIs(src.Verify(request(t, nil, ""), nil, secret), errMissingSignature)
	})

	t.Run("header configured as another name", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.ErrorIs(src.Verify(request(t, map[string]string{"X-Other": testSecret}, ""), nil, secret), errMissingSignature)
	})

	t.Run("no header configured", func(t *testing.T) {
		c := assert.NewAborting(t)
		c.Error((genericSource{}).Verify(request(t, map[string]string{"X-Omada-Secret": testSecret}, ""), nil, secret))
	})
}

// TestSourceVerifierForRoute pins that a generic route's verifier carries the
// route's header name while the signature schemes come from the shared table.
func TestSourceVerifierForRoute(t *testing.T) {
	c := assert.NewAborting(t)
	genericVerifier := (&Route{Source: "generic", Header: "X-Omada-Secret"}).verifier()
	c.NoError(genericVerifier.Verify(request(t, map[string]string{"X-Omada-Secret": testSecret}, ""), nil, []byte(testSecret)))
	c.Error(genericVerifier.Verify(request(t, map[string]string{"X-Omada-Secret": "no"}, ""), nil, []byte(testSecret)))

	github := (&Route{Source: "github"}).verifier()
	c.ErrorIs(github.Verify(request(t, nil, ""), nil, nil), errMissingSignature)
}
