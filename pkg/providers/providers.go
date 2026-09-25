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
	"net/url"
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
	// KindOpenAI is a generic OpenAI-compatible Chat Completions provider,
	// translated to/from the Anthropic shape at the sender boundary
	// (pkg/llm/openai_sender.go). base_url is required — there is no
	// canonical default, so SenderForKey rejects an empty one.
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
	Name        string                `toml:"-"`
	Kind        Kind                  `toml:"kind"`
	BaseURL     string                `toml:"base_url"`
	APIKeyEnv   string                `toml:"api_key_env"`
	Fallback    []string              `toml:"fallback"`
	Extras      map[string]any        `toml:"extras"`
	ViaExecutor *ViaExecutor          `toml:"via_executor"`
	Models      map[string]ModelAlias `toml:"models"`
	// SessionHeader, when non-empty, is the HTTP header name a stable
	// per-conversation identifier is sent on for backend-sticky routing (see
	// llm.WithSessionID) — the header name is provider-specific (OpenRouter's
	// is "x-session-id"; Fireworks' Anthropic-compatible endpoint wants
	// "x-session-affinity"). Empty defers to Kind's own default where one
	// exists: KindAnthropicOpenRouter always sends "x-session-id" even when
	// this is empty, preserving pre-session_header behavior for any Provider
	// value that doesn't set it — including one built as a struct literal
	// rather than through Default() or providers.toml. KindAnthropic and
	// KindOpenAI have no implicit default and stay silent unless this is set.
	// "x-session-affinity"), so this is opt-in per entry, not tied to Kind.
	SessionHeader string `toml:"session_header"`
}

// ModelAlias names a short local id for a provider's real model id, and
// optionally declares what the catalog can't know about a model this
// provider isn't in — its context window, how much of the prompt budget
// context files may occupy, and which skills/MCP servers are even relevant
// for it. Split substitutes ID for the alias key before a request is sent,
// so "vmlx/qwen" and "vmlx/models/Qwen3.8-27B-Abliterated-MLX-4bit" address
// the same model; only the alias key carries the declared metadata, since
// that's what makes the alias worth using rather than typing the real id.
//
// Skills and MCPServers are *string, not string, because "key absent" (nil:
// no override, today's unrestricted default applies) must be distinguishable
// from "key present but empty" ("": none) — a plain string can't represent
// that. Both share the same tri-state convention: nil=no override, ""=none,
// "*"=explicitly all, "a,b,c"=allowlist.
type ModelAlias struct {
	ID                  string  `toml:"id"`
	ContextWindow       int     `toml:"context_window"`
	MaxCompletionTokens int     `toml:"max_completion_tokens"`
	ContextFilesTokens  int     `toml:"context_files_tokens"`
	Skills              *string `toml:"skills"`
	MCPServers          *string `toml:"mcp_servers"`
	// Only lists the OpenRouter provider slugs allowed to serve requests made
	// through this alias, sent as the request body's "provider".only. It is
	// what lets two aliases name the SAME real id while routing it to
	// DIFFERENT providers (an A/B eval: "glm-flash@together" vs
	// "glm-flash@fireworks"), so the pin is per-alias and travels with the
	// alias through Set.Resolve — never looked up by id. A non-empty value
	// REPLACES the static pin's only for that request (the alias is the
	// explicit, more specific declaration); the ProviderGuard's ignore list
	// still merges in. nil/empty = no alias pin: static pins and the guard
	// still apply as before. Only meaningful for kind "anthropic-openrouter";
	// Validate refuses it on any other kind.
	Only []string `toml:"only"`
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

// EmbeddingsConfig is the [embeddings] table: a self-contained,
// OpenAI-shaped /embeddings endpoint. Absent = BM25-only recall.
type EmbeddingsConfig struct {
	URL        string `toml:"url"`
	APIKeyEnv  string `toml:"api_key_env"`
	Model      string `toml:"model"`
	Dimensions int    `toml:"dimensions"`
}

// APIKey resolves the embeddings credential from the environment, mirroring
// Provider.APIKey: empty for a keyless endpoint, and empty (not an error) when
// the named variable is unset — an unset key surfaces as an upstream 401.
func (e EmbeddingsConfig) APIKey() string {
	if e.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(e.APIKeyEnv)
}

func (e EmbeddingsConfig) validate() error {
	u, err := url.Parse(e.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("providers: [embeddings] url must be an absolute http(s) URL, got %q", e.URL)
	}
	if e.Model == "" {
		return errors.New("providers: [embeddings] model is required")
	}
	if e.Dimensions < 1 || e.Dimensions > 16000 {
		return fmt.Errorf("providers: [embeddings] dimensions must be 1..16000, got %d", e.Dimensions)
	}
	return nil
}

// SummariesConfig is the [summaries] table. Model uses the same
// provider/model addressing as everything else. Absent = no summaries.
type SummariesConfig struct {
	Model            string `toml:"model"`
	MaxSegmentTokens int    `toml:"max_segment_tokens"`
}

func (s SummariesConfig) validate() error {
	if s.Model == "" {
		return errors.New("providers: [summaries] model is required")
	}
	if s.MaxSegmentTokens < 0 {
		return fmt.Errorf("providers: [summaries] max_segment_tokens must be >= 0, got %d", s.MaxSegmentTokens)
	}
	return nil
}

// Set is the whole registry: the contents of providers.toml, which is by
// definition the contents of the future rafikid.toml [llm] section.
type Set struct {
	DefaultProvider string              `toml:"default_provider"`
	Providers       map[string]Provider `toml:"providers"`
	// Embeddings is the [embeddings] table; nil means BM25-only recall.
	Embeddings *EmbeddingsConfig `toml:"embeddings"`
	// Summaries is the [summaries] table; nil means no summaries.
	Summaries *SummariesConfig `toml:"summaries"`
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
		aliases := make([]string, 0, len(p.Models))
		for alias := range p.Models {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			m := p.Models[alias]
			if m.ID == "" {
				return fmt.Errorf("providers: provider %q: models.%s: id is required", name, alias)
			}
			// A provider pin is an OpenRouter routing instruction (the request
			// body's "provider".only); on any other kind it would be silently
			// meaningless, so refuse it rather than let a config look like it
			// constrains routing when nothing reads it.
			if len(m.Only) > 0 && p.Kind != KindAnthropicOpenRouter {
				return fmt.Errorf("providers: provider %q: models.%s: only requires kind %q, not %q", name, alias, KindAnthropicOpenRouter, p.Kind)
			}
			for _, slug := range m.Only {
				if strings.TrimSpace(slug) == "" {
					return fmt.Errorf("providers: provider %q: models.%s: only must not contain an empty provider slug", name, alias)
				}
			}
		}
	}
	if _, ok := s.Providers[s.DefaultProvider]; !ok {
		return fmt.Errorf("providers: default_provider %q is not a defined provider (have: %s)", s.DefaultProvider, strings.Join(s.Names(), ", "))
	}
	if s.Embeddings != nil {
		if err := s.Embeddings.validate(); err != nil {
			return err
		}
	}
	if s.Summaries != nil {
		if err := s.Summaries.validate(); err != nil {
			return err
		}
	}
	return nil
}
