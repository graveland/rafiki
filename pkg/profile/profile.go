// SPDX-License-Identifier: Apache-2.0

// Package profile binds a rafiki daemon's endpoint to the credential that
// endpoint needs, under a name.
//
// Before profiles the two were independent globals — RAFIKI_URL (or --socket)
// for the endpoint, RAFIKI_TOKEN/~/.config/rafiki/token for the credential —
// so aiming the client at a second daemon meant coordinating them by hand, and
// `rafiki user create` clobbered whichever token file the other daemon's
// client was using. A profile is the pair, named.
//
// Client-side only. Nothing here may import a pgx package: cmd/rafiki links
// this and TestClientDoesNotLinkPostgres enforces that the client opens no
// database connection. pkg/paths must never import this package — the
// dependency runs one way.
package profile

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// Profile is one named daemon: where it is, and how this client should behave
// against it. Exactly one of Socket or URL is set.
type Profile struct {
	// Name is the key from the [profile.<name>] table. Never decoded from the
	// file body, so it carries no toml tag.
	Name string `toml:"-"`

	// Socket is a local daemon's framed control socket. The Connect socket is
	// NOT configured — it is always a sibling of this one, which is how
	// pkg/paths pins them (SocketPath and ConnectSocketPath both live in
	// RuntimeDir).
	Socket string `toml:"socket,omitempty"`

	// URL is a remote daemon's control plane, "https://host[:port]". Only
	// https: the shared TLS listener is the only control listener, and a
	// plaintext control port does not exist.
	URL string `toml:"url,omitempty"`

	// Proxy is the LLM proxy face `rafiki claude` points at. Optional.
	Proxy string `toml:"proxy,omitempty"`

	// Spawn defaults. All optional; see the plan's Task 9 for precedence.
	Kind   string            `toml:"kind,omitempty"`
	Model  string            `toml:"model,omitempty"`
	Preset string            `toml:"preset,omitempty"`
	Labels map[string]string `toml:"labels,omitempty"`
}

// Set is the whole parsed manifest.
type Set struct {
	Profiles map[string]Profile
}

// tomlFile is the on-disk shape: a single [profile.<name>] table map.
type tomlFile struct {
	Profile map[string]Profile `toml:"profile"`
}

// Parse decodes and validates profiles.toml content. A nil or empty input is
// a valid, empty Set — that is what a machine with no profiles looks like
// before bootstrap.
func Parse(b []byte) (Set, error) {
	var f tomlFile
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return Set{}, fmt.Errorf("profiles: parse: %w", err)
	}

	// A typo'd key is silently ignored by every TOML decoder, which here means
	// a profile that quietly behaves as its zero value — and this file decides
	// which machine a destructive verb reaches. Same guard, and same nesting
	// filter, as pkg/providers.Parse: "profile.work.socket" is 2 dots and is a
	// struct key, while "profile.work.labels.env" is 3 and is correctly
	// consumed by the map.
	if und := md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			if strings.Count(k.String(), ".") > 2 {
				continue
			}
			keys = append(keys, k.String())
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			return Set{}, fmt.Errorf("profiles: unknown key(s): %s", strings.Join(keys, ", "))
		}
	}

	out := Set{Profiles: make(map[string]Profile, len(f.Profile))}
	for name, p := range f.Profile {
		if strings.TrimSpace(name) == "" {
			return Set{}, fmt.Errorf("profiles: a profile name may not be empty")
		}
		p.Name = name
		if err := validate(p); err != nil {
			return Set{}, err
		}
		out.Profiles[name] = p
	}
	return out, nil
}

// validate enforces the rules a malformed profile would otherwise fail on much
// later, against a real daemon, with a worse message.
func validate(p Profile) error {
	switch {
	case p.Socket != "" && p.URL != "":
		return fmt.Errorf("profiles: %q sets both socket and url; exactly one of them is required", p.Name)
	case p.Socket == "" && p.URL == "":
		return fmt.Errorf("profiles: %q sets neither socket nor url; exactly one of them is required", p.Name)
	}

	if p.URL != "" {
		u, err := url.Parse(p.URL)
		if err != nil {
			return fmt.Errorf("profiles: %q has an unparseable url %q: %w", p.Name, p.URL, err)
		}
		if u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("profiles: %q url must be https://host[:port] (got %q)", p.Name, p.URL)
		}
	}

	switch p.Kind {
	case "", protocol.KindFundi, protocol.KindClaude:
	default:
		return fmt.Errorf("profiles: %q has unknown kind %q (want %q or %q)",
			p.Name, p.Kind, protocol.KindFundi, protocol.KindClaude)
	}
	return nil
}

// Names returns every profile name, sorted, for stable output and errors.
func (s Set) Names() []string {
	out := make([]string, 0, len(s.Profiles))
	for name := range s.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Get returns one profile by name.
func (s Set) Get(name string) (Profile, bool) {
	p, ok := s.Profiles[name]
	return p, ok
}
