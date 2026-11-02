// SPDX-License-Identifier: Apache-2.0

// Package mcpoauth holds the daemon's OAuth 2.1 credentials for remote MCP
// servers, together with the two things every leg of that flow goes through:
// the canonical spelling of a URL (this file) and the operator's destination
// policy plus the guarded HTTP clients built from it (network.go).
//
// The package is deliberately pgx-free. The daemon's durable store is the
// separate pkg/mcpauthdb; this half is imported by code that must never open a
// database connection, and nothing here touches the network except through a
// Policy.
//
// Nothing here injects a configured MCP header either. The endpoint-only header
// wrapper belongs to the runtime and sits OUTSIDE these checks; the redirect
// rule in network.go is what keeps it from re-applying a credential to a
// redirected URL.
package mcpoauth

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrInvalidURL reports a string that is not an absolute HTTP(S) URL in the
// shape this package keys on: no userinfo, no fragment, a host, a real http or
// https scheme.
var ErrInvalidURL = errors.New("mcpoauth: invalid URL")

// Canonicalize returns the canonical, storage-key spelling of an absolute
// HTTP(S) URL.
//
// Canonical means: lowercase scheme and host, the scheme's default port
// dropped, an empty path written as "/", a trailing dot on a DNS name dropped,
// and the path, its escaping and the query kept exactly as given. It never
// reduces a URL to its origin and never rewrites a trailing "/mcp" — those are
// different resources, which is the whole point of keying a grant on this
// string.
//
// This is a KEY, not a validation. The SDK's issuer and resource checks run on
// the ORIGINAL spelling the server advertised: canonicalizing an issuer first
// would let a canonical-equivalence shortcut stand in for "the advertised
// issuer string matches what I stored". A resource key, by contrast, is
// canonicalized *after* the SDK has accepted the resource it names.
func Canonicalize(raw string) (string, error) {
	u, err := parseTarget(raw)
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = canonicalHost(u)
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// SameOrigin reports whether a and b are the same origin: same scheme, same
// host, same effective port (a default port and an explicit one are the same
// origin). Redirect handling refuses a hop that fails this, and an
// authenticated request's extra headers are scoped to the origin that was
// configured.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return originKey(a) == originKey(b)
}

// parseTarget parses raw and applies every rule that does not need a network.
func parseTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrInvalidURL, raw, err)
	}
	if err := checkTarget(u); err != nil {
		return nil, fmt.Errorf("%w: %q", err, raw)
	}
	// url.Parse drops an empty fragment, so "https://h/x#" would pass the
	// Fragment test below while still arriving with a '#' in it.
	if strings.ContainsRune(raw, '#') {
		return nil, fmt.Errorf("%w: %q: fragment", ErrInvalidURL, raw)
	}
	return u, nil
}

// checkTarget applies the URL-shape rules to an already-parsed URL and does not
// mutate it.
func checkTarget(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: no URL", ErrInvalidURL)
	}
	if u.Opaque != "" {
		return fmt.Errorf("%w: %q: not a hierarchical URL", ErrInvalidURL, u.String())
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: %q: scheme must be http or https", ErrInvalidURL, u.String())
	}
	if u.User != nil {
		return fmt.Errorf("%w: %q: userinfo", ErrInvalidURL, u.Redacted())
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("%w: %q: fragment", ErrInvalidURL, u.Redacted())
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: %q: no host", ErrInvalidURL, u.Redacted())
	}
	return nil
}

func canonicalHost(u *url.URL) string {
	host := hostKeyForCompare(u.Hostname())
	port := u.Port()
	if port == "" || port == defaultPortFor(u.Scheme) {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

func originKey(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = defaultPortFor(scheme)
	}
	return scheme + "://" + net.JoinHostPort(hostKeyForCompare(u.Hostname()), port)
}

func defaultPortFor(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// hostKeyForCompare normalizes a host for exact, case-insensitive comparison.
//
// IP literals go through netip and are unmapped, so ::ffff:10.0.0.1 and
// 10.0.0.1 are one key — an allow_hosts entry written in the IPv4 spelling must
// not be bypassed by the mapped one. A trailing dot on a DNS name is dropped
// (example.com and example.com. resolve the same), and a bracketed IPv6 literal
// is accepted.
func hostKeyForCompare(host string) string {
	h := strings.ToLower(host)
	h = strings.TrimSuffix(h, ".")
	if len(h) > 2 && strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if addr, err := parseAddr(h); err == nil {
		return addr.String()
	}
	return h
}
