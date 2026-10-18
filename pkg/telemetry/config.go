// SPDX-License-Identifier: Apache-2.0

// Package telemetry decides what content may leave the daemon on a span, and
// caps it. It is the only place prompt, reply and tool-argument text is shaped
// for export; errors are the deliberate exception and are always exported,
// capped only for size.
package telemetry

import (
	"errors"
	"fmt"
	"path"
	"slices"
)

// Config is the [telemetry] section of rafikid.toml.
type Config struct {
	Endpoint string `toml:"endpoint"`
	Protocol string `toml:"protocol"` // "", "http/protobuf", "grpc"
	Content  string `toml:"content"`  // "", "off", "all", "rules"
	Rules    []Rule `toml:"content_rule"`
}

// Rule is one [[telemetry.content_rule]]: every non-empty field must match for
// the rule to select a request.
type Rule struct {
	User     string `toml:"user"`
	Provider string `toml:"provider"`
	Model    string `toml:"model"` // path.Match glob
}

// Validate reports the first thing wrong with the [telemetry] section, or nil.
// It reads providers.toml's names so a rule naming a typo'd provider is caught
// at boot rather than silently never matching.
func (c Config) Validate(providerNames []string) error {
	switch c.Protocol {
	case "", "http/protobuf", "grpc":
	default:
		return fmt.Errorf("telemetry: protocol %q: want \"http/protobuf\" or \"grpc\"", c.Protocol)
	}
	switch c.Content {
	case "", "off", "all", "rules":
	default:
		return fmt.Errorf("telemetry: content %q: want \"off\", \"all\" or \"rules\"", c.Content)
	}
	if c.Content == "rules" && len(c.Rules) == 0 {
		return errors.New(`telemetry: content = "rules" needs at least one [[telemetry.content_rule]]`)
	}
	// Every rule is validated whatever the mode: an unused typo'd glob or an
	// unused unknown provider is a config bug, not a silent never-matching
	// rule.
	for i, r := range c.Rules {
		if r.User == "" && r.Provider == "" && r.Model == "" {
			return fmt.Errorf(`telemetry: content_rule %d sets no field; use content = "all" to match everything`, i)
		}
		if r.Model != "" {
			if _, err := path.Match(r.Model, ""); errors.Is(err, path.ErrBadPattern) {
				return fmt.Errorf("telemetry: content_rule %d: bad model glob %q", i, r.Model)
			}
		}
		if r.Provider != "" && !slices.Contains(providerNames, r.Provider) {
			return fmt.Errorf("telemetry: content_rule %d: provider %q is not in providers.toml", i, r.Provider)
		}
	}
	return nil
}

// Policy is the compiled form of Config.Content and Config.Rules. Its zero
// value allows nothing.
type Policy struct {
	content string
	rules   []Rule
}

// Policy compiles the content decision.
//
// A rule that sets no field is dropped here rather than in Rule.matches: it
// would otherwise match every request, turning content = "rules" into
// content = "all". Validate rejects such a rule, but the fail-closed guarantee
// must not depend on Validate having run — a Config built by a test fixture or
// a future caller compiles through this same function.
func (c Config) Policy() Policy {
	rules := make([]Rule, 0, len(c.Rules))
	for _, r := range c.Rules {
		if r.User == "" && r.Provider == "" && r.Model == "" {
			continue
		}
		rules = append(rules, r)
	}
	return Policy{content: c.Content, rules: rules}
}

// Allows reports whether the given request's content may be exported.
func (p Policy) Allows(user, provider, model string) bool {
	switch p.content {
	case "all":
		return true
	case "rules":
		for _, r := range p.rules {
			if r.matches(user, provider, model) {
				return true
			}
		}
		return false
	default: // "", "off", and any value Validate rejected
		return false
	}
}

// matches reports whether every non-empty field of the rule selects the
// request.
func (r Rule) matches(user, provider, model string) bool {
	if r.User != "" && r.User != user {
		return false
	}
	if r.Provider != "" && r.Provider != provider {
		return false
	}
	if r.Model != "" {
		ok, err := path.Match(r.Model, model)
		if err != nil || !ok {
			return false
		}
	}
	return true
}
