// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Source is one webhook sender's signature scheme.
//
// Verify decides whether a body really came from the sender holding secret;
// Event reports the sender's event type and delivery id, either of which may be
// empty for a scheme that has none.
type Source interface {
	Verify(r *http.Request, body, secret []byte) error
	Event(r *http.Request) (eventType, deliveryID string)
}

var (
	// errMissingSignature is a request that carries no credential at all. It is
	// distinct from a wrong one only in the log line.
	errMissingSignature = errors.New("signature header is missing")
	// errBadSignature is a credential that did not verify.
	errBadSignature = errors.New("signature does not verify")
)

// sources is the scheme table. The generic source carries the route's header
// name, so this entry is a template: Route.verifier returns a copy with the
// header filled in.
var sources = map[string]Source{
	"github":  githubSource{},
	"gitea":   giteaSource{},
	"generic": genericSource{},
}

// verifier is the Source this route authenticates with.
func (r *Route) verifier() Source {
	if g, ok := sources[r.Source].(genericSource); ok {
		g.header = r.Header
		return g
	}
	return sources[r.Source]
}

// githubSource is GitHub's webhook signature: X-Hub-Signature-256 carries
// "sha256=" followed by the hex HMAC-SHA256 of the raw body.
//
// GitHub also sends the legacy SHA-1 signature in X-Hub-Signature; that header
// is ignored rather than accepted, because SHA-1 over a shared secret is the
// weaker of the two and a sender that offers both is not asking for it.
type githubSource struct{}

const githubSignatureHeader = "X-Hub-Signature-256"

func (githubSource) Verify(r *http.Request, body, secret []byte) error {
	value := r.Header.Get(githubSignatureHeader)
	if value == "" {
		return errMissingSignature
	}
	const prefix = "sha256="
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s: want the %q prefix: %w", githubSignatureHeader, prefix, errBadSignature)
	}
	// hex.DecodeString accepts either case, so an uppercase-hex signature is
	// accepted: the encoding is not the security property.
	signature, err := hex.DecodeString(value[len(prefix):])
	if err != nil {
		return fmt.Errorf("%s: %w", githubSignatureHeader, errBadSignature)
	}
	if !hmac.Equal(signature, hmacSHA256(secret, body)) {
		return errBadSignature
	}
	return nil
}

func (githubSource) Event(r *http.Request) (eventType, deliveryID string) {
	return r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
}

// giteaSource is Gitea's webhook signature: X-Gitea-Signature carries the bare
// hex HMAC-SHA256 of the raw body, with no algorithm prefix.
type giteaSource struct{}

const giteaSignatureHeader = "X-Gitea-Signature"

func (giteaSource) Verify(r *http.Request, body, secret []byte) error {
	value := r.Header.Get(giteaSignatureHeader)
	if value == "" {
		return errMissingSignature
	}
	signature, err := hex.DecodeString(value)
	if err != nil {
		return fmt.Errorf("%s: %w", giteaSignatureHeader, err)
	}
	if !hmac.Equal(signature, hmacSHA256(secret, body)) {
		return errBadSignature
	}
	return nil
}

func (giteaSource) Event(r *http.Request) (eventType, deliveryID string) {
	return r.Header.Get("X-Gitea-Event"), r.Header.Get("X-Gitea-Delivery")
}

// genericSource is the scheme for everything that has no signature standard of
// its own: the route names a header, and that header's value is the secret,
// compared in constant time.
//
// It cannot sign a body, so a generic route has no replay protection beyond the
// delivery id its sender chooses to send (usually none). It is for a device on
// the inside of a network that posts its own JSON, not for a public endpoint.
type genericSource struct {
	header string
}

func (g genericSource) Verify(r *http.Request, body, secret []byte) error {
	if g.header == "" {
		return errors.New("generic source is configured without a header")
	}
	value := r.Header.Get(g.header)
	if value == "" {
		return errMissingSignature
	}
	if subtle.ConstantTimeCompare([]byte(value), secret) != 1 {
		return errBadSignature
	}
	return nil
}

func (genericSource) Event(*http.Request) (eventType, deliveryID string) {
	return "", ""
}

func hmacSHA256(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return mac.Sum(nil)
}
