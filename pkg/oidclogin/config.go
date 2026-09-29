// SPDX-License-Identifier: Apache-2.0

// Package oidclogin is the daemon's OIDC login engine: it walks a caller
// through an authorization-code + PKCE flow against the configured identity
// provider and mints a rafiki user token when the login resolves to a user.
//
// Daemon-only: cmd/rafiki (the socket client) must never import this package
// — the client drives the flow over the Connect Login service and links no
// OIDC library, the same split TestClientDoesNotLinkPostgres pins for pgx.
// The engine is wired post-construction through connectapi.Server's
// SetLoginBackend, and its routes mount OUTSIDE authentication on every face
// (see cmd/rafikid/proxy.go and cmd/rafikid/connect_uds.go): Login is how a
// caller without a valid credential gets one.
package oidclogin

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that decodes from a TOML string — oidc.toml
// writes session_ttl = "12h". TOML has no native duration type, and
// BurntSushi/toml routes string values through encoding.TextUnmarshaler.
type Duration time.Duration

// UnmarshalText parses "300ms", "1h30m" and friends.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("invalid duration %q", string(b))
	}
	*d = Duration(v)
	return nil
}

// MarshalText renders the duration the way it is written in oidc.toml.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Config is the oidc.toml schema. LoadConfig applies the defaults: session_ttl
// 12h and scopes ["openid","email","profile"] when unset.
type Config struct {
	Issuer          string   `toml:"issuer"`
	ClientID        string   `toml:"client_id"`
	ClientSecretEnv string   `toml:"client_secret_env"`
	EmailDomains    []string `toml:"email_domains"`
	SessionTTL      Duration `toml:"session_ttl"`
	// RedirectPort pins the loopback port the IdP redirects back to. 0 means
	// the client's own ephemeral port: each Begin rebinds to whatever the
	// caller reports. A pinned port is how a daemon behind a tight firewall
	// admits callbacks without opening a range — the client must then listen
	// on the pinned port instead of its own.
	RedirectPort uint16   `toml:"redirect_port"`
	Scopes       []string `toml:"scopes"`
}

// LoadConfig reads path. A missing file is (nil, nil) — login simply is not
// configured, which is a daemon's ordinary state. A present-but-invalid file
// is an error: the caller logs it and runs with login unconfigured, because a
// mis-edited oidc.toml must never take the whole daemon down.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults fills the unset knobs and normalizes email_domains to the
// form the engine compares against: emails are lowercased by
// users.NormalizeEmail before the domain check, so an uppercase entry in
// oidc.toml could never match — normalize here rather than lock the operator
// out of their own daemon.
func (c *Config) applyDefaults() {
	if c.SessionTTL == 0 {
		c.SessionTTL = Duration(12 * time.Hour)
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "email", "profile"}
	}
	for i, d := range c.EmailDomains {
		c.EmailDomains[i] = strings.ToLower(strings.TrimSpace(d))
	}
}

// validate enforces the non-negotiables at load. Every failure is a config
// error the operator can fix; none of them is worth crashing a daemon over.
func (c *Config) validate() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Host == "" || !httpsOrLoopback(u) {
		return fmt.Errorf("issuer %q must be an https URL (http://127.0.0.1 and http://localhost are also accepted)", c.Issuer)
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("client_id is required")
	}
	if strings.TrimSpace(c.ClientSecretEnv) == "" {
		return errors.New("client_secret_env is required (the env var holding the provider's client secret)")
	}
	if os.Getenv(c.ClientSecretEnv) == "" {
		return fmt.Errorf("client_secret_env names %q, but that environment variable is empty", c.ClientSecretEnv)
	}
	if len(c.EmailDomains) == 0 {
		return errors.New("email_domains must name at least one domain")
	}
	for _, d := range c.EmailDomains {
		switch {
		case d == "":
			return errors.New("email_domains holds an empty entry")
		case strings.Contains(d, "@"):
			return fmt.Errorf("email_domains entry %q must be a bare domain with no @", d)
		case strings.ContainsAny(d, " \t"):
			return fmt.Errorf("email_domains entry %q must not contain whitespace", d)
		}
	}
	if c.SessionTTL <= 0 {
		return errors.New("session_ttl must be positive")
	}
	return nil
}

// httpsOrLoopback reports whether u is an https URL, or plain http against
// loopback — the allowance tests need for an in-process fake issuer.
func httpsOrLoopback(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	}
	return false
}
