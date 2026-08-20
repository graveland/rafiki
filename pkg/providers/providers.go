// SPDX-License-Identifier: Apache-2.0

// Package providers is the provider registry: the providers.toml config type,
// its validation, and provider/model addressing.
//
// It is deliberately free of any database dependency. cmd/rafiki (the socket
// client) must resolve a provider/model id to validate --model and to drive
// completion, and it links zero pgx packages — an invariant pinned by
// TestClientDoesNotLinkPostgres. Sender construction, which needs the Anthropic
// SDK and the model catalog, lives in pkg/llm instead.
package providers

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind selects the code path that constructs a sender, mutates the request and
// translates model ids. It is <protocol>[-<variant>]: the variant exists
// because OpenRouter's behaviour (headers, routing preferences, the cache
// guard, catalog translation) differs from plain Anthropic while the protocol
// does not.
type Kind string

const (
	KindAnthropic           Kind = "anthropic"
	KindAnthropicOpenRouter Kind = "anthropic-openrouter"
	// KindOpenAI is reserved and NOT implemented. It is accepted by the parser
	// so a config naming it loads, and rejected at sender construction.
	KindOpenAI Kind = "openai"
)

// nameRE is the legal shape of a provider name. Slash-free is load-bearing:
// model addressing splits on the FIRST slash, so a name containing one would
// make the split ambiguous.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ViaExecutor routes a provider's HTTP traffic through an executor rather than
// dialing base_url from the daemon. Phase B implements the transport; Phase A
// parses and validates the table.
type ViaExecutor struct {
	Selector string `toml:"selector"`
	Proxy    string `toml:"proxy"`
}

// Provider is one named endpoint.
type Provider struct {
	// Name is the map key, stamped in by Parse. It is not read from the file.
	Name        string         `toml:"-"`
	Kind        Kind           `toml:"kind"`
	BaseURL     string         `toml:"base_url"`
	APIKeyEnv   string         `toml:"api_key_env"`
	Fallback    []string       `toml:"fallback"`
	Extras      map[string]any `toml:"extras"`
	ViaExecutor *ViaExecutor   `toml:"via_executor"`
}

// Keyless reports whether this provider sends no credential at all.
func (p Provider) Keyless() bool { return p.APIKeyEnv == "" }

// APIKey resolves the provider's credential from the environment. Empty for a
// keyless provider, and empty (not an error) when the named variable is unset —
// an unset key surfaces as an upstream 401, which is a clearer diagnosis than a
// startup error for a provider the process may never use.
func (p Provider) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

// Set is the whole registry: the contents of providers.toml, which is by
// definition the contents of the future rafikid.toml [llm] section.
type Set struct {
	DefaultProvider string              `toml:"default_provider"`
	Providers       map[string]Provider `toml:"providers"`
}

// Get returns a provider by name.
func (s *Set) Get(name string) (Provider, bool) {
	p, ok := s.Providers[name]
	return p, ok
}

// Names returns every provider name, sorted, so error messages and completion
// output are stable.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Parse decodes and validates providers.toml content.
func Parse(b []byte) (*Set, error) {
	var set Set
	md, err := toml.Decode(string(b), &set)
	if err != nil {
		return nil, fmt.Errorf("providers: parse: %w", err)
	}
	// A typo'd key is silently ignored by every TOML decoder, which for a
	// routing config means a provider that quietly behaves as its zero value.
	// Only struct-level keys are errors: keys nested inside an Extras map value
	// (e.g. extras.<name>.<knob>) are correctly consumed by map[string]any but
	// still surface in Undecoded, so deeper paths are filtered out here and
	// checked by Validate instead.
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			if strings.Count(k.String(), ".") > 2 {
				continue
			}
			keys = append(keys, k.String())
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			return nil, fmt.Errorf("providers: unknown key(s): %s", strings.Join(keys, ", "))
		}
	}
	for name, p := range set.Providers {
		p.Name = name
		set.Providers[name] = p
	}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	return &set, nil
}

// Load reads providers.toml. A missing file is not an error — it returns the
// shipped default, so a fresh install with no config behaves as it always did.
func Load(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("providers: read %s: %w", path, err)
	}
	set, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("providers: %s: %w", path, err)
	}
	return set, nil
}

// Default is the shipped configuration used when no providers.toml exists. The
// two names match the values already stored in conversation_turn.upstream, so
// historical rows stay meaningful.
func Default() *Set {
	return &Set{
		DefaultProvider: "anthropic",
		Providers: map[string]Provider{
			"anthropic": {
				Name:      "anthropic",
				Kind:      KindAnthropic,
				APIKeyEnv: "ANTHROPIC_API_KEY",
				Fallback:  []string{"openrouter"},
			},
			"openrouter": {
				Name:      "openrouter",
				Kind:      KindAnthropicOpenRouter,
				APIKeyEnv: "OPENROUTER_API_KEY",
			},
		},
	}
}

// Validate enforces every rule that must hold before any request is routed.
func (s *Set) Validate() error {
	if len(s.Providers) == 0 {
		return errors.New("providers: no providers defined")
	}
	if s.DefaultProvider == "" {
		return errors.New("providers: default_provider is required")
	}
	for _, name := range s.Names() {
		p := s.Providers[name]
		if !nameRE.MatchString(name) {
			return fmt.Errorf("providers: invalid provider name %q (want [a-z0-9][a-z0-9_-]*, no slashes)", name)
		}
		switch p.Kind {
		case KindAnthropic, KindAnthropicOpenRouter, KindOpenAI:
		case "":
			return fmt.Errorf("providers: provider %q: kind is required", name)
		default:
			return fmt.Errorf("providers: provider %q: unknown kind %q", name, p.Kind)
		}
		// SetExtraFields replaces the whole map, so a user-supplied "provider"
		// key would delete the cache guard's provider.ignore silently. Refusing
		// it here is the only version of this that fails loudly.
		if _, bad := p.Extras["provider"]; bad {
			return fmt.Errorf("providers: provider %q: extras.provider is reserved (it would overwrite routing preferences and the cache guard's ejections)", name)
		}
		for _, fb := range p.Fallback {
			if _, ok := s.Providers[fb]; !ok {
				return fmt.Errorf("providers: provider %q: fallback target %q is not a defined provider", name, fb)
			}
		}
		if p.ViaExecutor != nil {
			if p.ViaExecutor.Proxy == "" {
				return fmt.Errorf("providers: provider %q: via_executor.proxy is required", name)
			}
			if p.ViaExecutor.Selector == "" {
				return fmt.Errorf("providers: provider %q: via_executor.selector is required", name)
			}
			if p.BaseURL == "" {
				return fmt.Errorf("providers: provider %q: via_executor requires base_url (it is the request's target URL either way)", name)
			}
		}
	}
	if _, ok := s.Providers[s.DefaultProvider]; !ok {
		return fmt.Errorf("providers: default_provider %q is not a defined provider (have: %s)", s.DefaultProvider, strings.Join(s.Names(), ", "))
	}
	return nil
}
