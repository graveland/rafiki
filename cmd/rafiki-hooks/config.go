// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/notify"
)

const (
	// healthPath is the one path the mux answers itself. A route claiming it is
	// shadowed by the health handler and never receives a webhook.
	healthPath = "/healthz"
	// defaultTimeoutSeconds is daemon.timeout_seconds' default.
	defaultTimeoutSeconds = 10
	// writeTimeoutSeconds is the http.Server's WriteTimeout. A Notify call may
	// not outlast the response the sender is waiting for.
	writeTimeoutSeconds = 30
)

// Config is the gateway's whole configuration: one daemon to talk to, and one
// route per webhook path it accepts.
type Config struct {
	Daemon Daemon  `toml:"daemon"`
	Routes []Route `toml:"route"`

	// token is the daemon credential read from Daemon.TokenFile by Load. It is
	// deliberately not a config key: a file can be a mounted secret, a literal
	// in a config map cannot.
	token string
}

// Daemon is the one rafikid this gateway reports to.
type Daemon struct {
	// URL is the daemon's Connect endpoint. Required, and https unless the
	// host is loopback: the credential this gateway carries is a capability to
	// wake agents, so it never crosses the network in the clear.
	URL string `toml:"url"`
	// TokenFile holds a `{notify}`-capability token minted with
	// `rafikid user token mint --capability notify`. Required.
	TokenFile string `toml:"token_file"`
	// CAFile is an optional PEM pool trusted for the daemon's TLS in addition
	// to the system roots.
	CAFile string `toml:"ca_file"`
	// Listen is the address the webhook listener binds. Default ":8080".
	Listen string `toml:"listen"`
	// TLSCert and TLSKey are the gateway's own serving certificate. Both or
	// neither; unset means plain HTTP, for a TLS-terminating proxy in front.
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// TimeoutSeconds bounds one Notify call. Default 10.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// Route is one webhook path: how to authenticate a sender, which agent to wake,
// and under what conditions.
type Route struct {
	// Path is the exact URL path this route answers. Required, starts with
	// "/", and unique across the config.
	Path string `toml:"path"`
	// Source selects the signature scheme: "github", "gitea" or "generic".
	Source string `toml:"source"`
	// SecretFile holds the shared secret the sender signs with. Required.
	SecretFile string `toml:"secret_file"`
	// Header is the header carrying a generic route's secret. Required for
	// "generic", refused for the signature schemes (they name their own
	// header).
	Header string `toml:"header"`
	// Target is the NAME of the live top-level agent to wake. Required.
	Target string `toml:"target"`
	// Notify is the Notify.source value the daemon sees. Defaults to Source.
	Notify string `toml:"notify_source"`
	// MaxBody caps the request body in bytes. Default 1 MiB.
	MaxBody int64 `toml:"max_body"`
	// Rules are tried in order; the first match is delivered.
	Rules []Rule `toml:"rule"`

	// secret is SecretFile's trimmed contents, read by Load.
	secret []byte
}

// Rule decides whether one delivery becomes a notification, and what it says.
type Rule struct {
	// Event is the sender's event type; "" matches any. Must be "" for a
	// generic route, which carries no event type.
	Event string `toml:"event"`
	// Match is dotted path -> exact string value; every entry must hold.
	Match map[string]string `toml:"match"`
	// Key is the notification key, with {dotted.path} interpolated.
	Key string `toml:"key"`
	// Fields maps a notification field name to the dotted path it reads.
	Fields map[string]string `toml:"fields"`
}

// Load reads and validates a TOML config and reads the files it names, so a
// missing token or secret fails startup rather than the first delivery.
func Load(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// An unknown key is an error: a misspelled `max_body` silently falling back
	// to 1 MiB is how a route ends up accepting bodies nobody meant to accept.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.readSecrets(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaults fills the documented defaults before validation, so validation
// only ever sees the effective configuration.
func (c *Config) applyDefaults() {
	if c.Daemon.Listen == "" {
		c.Daemon.Listen = ":8080"
	}
	if c.Daemon.TimeoutSeconds == 0 {
		c.Daemon.TimeoutSeconds = defaultTimeoutSeconds
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Notify == "" {
			r.Notify = r.Source
		}
		if r.MaxBody == 0 {
			r.MaxBody = 1 << 20
		}
	}
}

// validate reports the first thing that would make the gateway misbehave.
func (c *Config) validate() error {
	if c.Daemon.URL == "" {
		return errors.New("daemon.url is required")
	}
	u, err := url.Parse(c.Daemon.URL)
	if err != nil {
		return fmt.Errorf("daemon.url %q: %w", c.Daemon.URL, err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		switch u.Hostname() {
		case "127.0.0.1", "localhost":
		default:
			return fmt.Errorf("daemon.url %q: must be https unless the host is 127.0.0.1 or localhost", c.Daemon.URL)
		}
	case "":
		return fmt.Errorf("daemon.url %q: must be an absolute URL", c.Daemon.URL)
	default:
		return fmt.Errorf("daemon.url %q: unsupported scheme %q", c.Daemon.URL, u.Scheme)
	}
	if c.Daemon.TokenFile == "" {
		return errors.New("daemon.token_file is required")
	}
	if (c.Daemon.TLSCert == "") != (c.Daemon.TLSKey == "") {
		return errors.New("daemon.tls_cert and daemon.tls_key must be set together")
	}
	if c.Daemon.TimeoutSeconds <= 0 {
		return fmt.Errorf("daemon.timeout_seconds %d: must be positive (0 means the default, %ds)", c.Daemon.TimeoutSeconds, defaultTimeoutSeconds)
	}
	// A timeout at or past the server's WriteTimeout outlives the response: the
	// sender sees a dropped connection after the daemon has accepted the
	// notification, and retries it into a duplicate. Refuse it at startup.
	if c.Daemon.TimeoutSeconds >= writeTimeoutSeconds {
		return fmt.Errorf("daemon.timeout_seconds %d: must be under the %ds write timeout, or the sender's connection "+
			"drops after the notification was accepted", c.Daemon.TimeoutSeconds, writeTimeoutSeconds)
	}
	if c.Daemon.Listen == "" {
		return errors.New("daemon.listen must not be empty")
	}
	if len(c.Routes) == 0 {
		return errors.New("at least one route is required")
	}
	seen := make(map[string]bool, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]
		if err := r.validate(); err != nil {
			return err
		}
		if seen[r.Path] {
			return fmt.Errorf("route %q: path is used more than once", r.Path)
		}
		seen[r.Path] = true
	}
	return nil
}

// validate checks one route. The error messages name the path rather than an
// index because the config is hand-written.
func (r *Route) validate() error {
	if r.Path == "" {
		return errors.New("route path is required")
	}
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("route %q: path must start with %q", r.Path, "/")
	}
	if r.Path == healthPath {
		return fmt.Errorf("route %q: the health endpoint owns this path; a webhook would never reach it", r.Path)
	}
	// ServeMux cleans and redirects a non-clean path before the gateway sees the
	// request, so a sender would get a 301 and treat the delivery as failed
	// while the route looks configured. Refuse it instead.
	if cleaned := path.Clean(r.Path); cleaned != r.Path {
		return fmt.Errorf("route %q: path is not clean (want %q); ServeMux would redirect it", r.Path, cleaned)
	}
	if r.Source == "" {
		return fmt.Errorf("route %q: source is required", r.Path)
	}
	src, ok := sources[r.Source]
	if !ok {
		return fmt.Errorf("route %q: unknown source %q; want github, gitea or generic", r.Path, r.Source)
	}
	if _, generic := src.(genericSource); generic {
		if r.Header == "" {
			return fmt.Errorf("route %q: header is required for source generic", r.Path)
		}
	} else if r.Header != "" {
		return fmt.Errorf("route %q: header is only valid for source generic", r.Path)
	}
	if r.SecretFile == "" {
		return fmt.Errorf("route %q: secret_file is required", r.Path)
	}
	if r.Target == "" {
		return fmt.Errorf("route %q: target is required", r.Path)
	}
	// notify.ValidSource is the daemon's own Notify.source rule, so a typo is a
	// startup failure here rather than a 422 on every delivery.
	if err := notify.ValidSource(r.Notify); err != nil {
		return fmt.Errorf("route %q: notify_source: %w", r.Path, err)
	}
	if r.MaxBody <= 0 {
		return fmt.Errorf("route %q: max_body must be positive", r.Path)
	}
	if len(r.Rules) == 0 {
		return fmt.Errorf("route %q: at least one rule is required", r.Path)
	}
	_, generic := src.(genericSource)
	for i := range r.Rules {
		if generic && r.Rules[i].Event != "" {
			return fmt.Errorf("route %q: rule %d: event must be empty for source generic, which carries no event type", r.Path, i)
		}
	}
	return nil
}

// readSecrets reads the token and every route secret. Each is trimmed of
// surrounding whitespace, the way a mounted secret file conventionally ends in
// a newline.
func (c *Config) readSecrets() error {
	token, err := readSecretFile(c.Daemon.TokenFile)
	if err != nil {
		return fmt.Errorf("daemon.token_file: %w", err)
	}
	c.token = string(token)
	for i := range c.Routes {
		r := &c.Routes[i]
		secret, err := readSecretFile(r.SecretFile)
		if err != nil {
			return fmt.Errorf("route %q: secret_file: %w", r.Path, err)
		}
		r.secret = secret
	}
	return nil
}

func readSecretFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return []byte(trimmed), nil
}

// timeout is how long one Notify call may take.
func (c *Config) timeout() time.Duration {
	return time.Duration(c.Daemon.TimeoutSeconds) * time.Second
}
