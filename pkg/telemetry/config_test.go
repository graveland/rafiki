// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/telemetry"
)

func TestValidateOrder(t *testing.T) {
	providers := []string{"openrouter", "anthropic"}

	tests := []struct {
		name string
		cfg  telemetry.Config
		want string // empty means nil
	}{
		{
			name: "protocol unknown",
			cfg:  telemetry.Config{Protocol: "jaeger"},
			want: `telemetry: protocol "jaeger": want "http/protobuf" or "grpc"`,
		},
		{
			name: "protocol reported before content",
			cfg:  telemetry.Config{Protocol: "zipkin", Content: "everything"},
			want: `telemetry: protocol "zipkin": want "http/protobuf" or "grpc"`,
		},
		{
			name: "content unknown",
			cfg:  telemetry.Config{Content: "everything"},
			want: `telemetry: content "everything": want "off", "all" or "rules"`,
		},
		{
			name: "rules mode with no rules",
			cfg:  telemetry.Config{Content: "rules"},
			want: `telemetry: content = "rules" needs at least one [[telemetry.content_rule]]`,
		},
		{
			name: "empty rule",
			cfg: telemetry.Config{
				Content: "rules",
				Rules:   []telemetry.Rule{{}},
			},
			want: `telemetry: content_rule 0 sets no field; use content = "all" to match everything`,
		},
		{
			name: "bad model glob",
			cfg: telemetry.Config{
				Content: "rules",
				Rules:   []telemetry.Rule{{Model: "deepseek/["}},
			},
			want: `telemetry: content_rule 0: bad model glob "deepseek/["`,
		},
		{
			name: "unknown provider",
			cfg: telemetry.Config{
				Content: "rules",
				Rules:   []telemetry.Rule{{Provider: "openrouetr"}},
			},
			want: `telemetry: content_rule 0: provider "openrouetr" is not in providers.toml`,
		},
		{
			// The brief's numbered order decides: check 5 (glob) before
			// check 6 (provider), for the same rule.
			name: "bad glob wins over unknown provider in one rule",
			cfg: telemetry.Config{
				Content: "rules",
				Rules:   []telemetry.Rule{{Provider: "openrouetr", Model: "deepseek/["}},
			},
			want: `telemetry: content_rule 0: bad model glob "deepseek/["`,
		},
		{
			name: "later rule failures are reported after earlier clean rules",
			cfg: telemetry.Config{
				Content: "rules",
				Rules: []telemetry.Rule{
					{Provider: "openrouter"},
					{Provider: "nope"},
				},
			},
			want: `telemetry: content_rule 1: provider "nope" is not in providers.toml`,
		},
		{
			name: "valid rules",
			cfg: telemetry.Config{
				Endpoint: "http://127.0.0.1:4318",
				Protocol: "http/protobuf",
				Content:  "rules",
				Rules: []telemetry.Rule{
					{User: "alice"},
					{Provider: "openrouter", Model: "deepseek/*"},
				},
			},
		},
		{
			name: "valid empty",
			cfg:  telemetry.Config{},
		},
		{
			// The mode does not exempt a rule from validation: an unused
			// stale rule is still a config bug.
			name: "unknown provider under content=all",
			cfg: telemetry.Config{
				Content: "all",
				Rules:   []telemetry.Rule{{Provider: "not-in-providers.toml"}},
			},
			want: `telemetry: content_rule 0: provider "not-in-providers.toml" is not in providers.toml`,
		},
		{
			name: "bad model glob under content=all",
			cfg: telemetry.Config{
				Content: "all",
				Rules:   []telemetry.Rule{{Model: "deepseek/["}},
			},
			want: `telemetry: content_rule 0: bad model glob "deepseek/["`,
		},
		{
			name: "empty rule under content=all",
			cfg: telemetry.Config{
				Content: "all",
				Rules:   []telemetry.Rule{{}},
			},
			want: `telemetry: content_rule 0 sets no field; use content = "all" to match everything`,
		},
		{
			// Rules in non-rules mode are not themselves an error; they are
			// carried and unused.
			name: "valid unused rules while content is all",
			cfg: telemetry.Config{
				Content: "all",
				Rules: []telemetry.Rule{
					{User: "alice"},
					{Provider: "openrouter", Model: "deepseek/*"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := tc.cfg.Validate(providers)
			if tc.want == "" {
				c.NoError(err)
				return
			}
			c.EqualError(err, tc.want)
		})
	}
}

func TestPolicyAllows(t *testing.T) {
	const (
		user     = "alice"
		provider = "openrouter"
		model    = "deepseek/deepseek-v4"
	)

	tests := []struct {
		name string
		cfg  telemetry.Config
		want bool
	}{
		{name: "zero policy denies", cfg: telemetry.Config{}, want: false},
		{name: "off denies", cfg: telemetry.Config{Content: "off"}, want: false},
		{name: "all allows", cfg: telemetry.Config{Content: "all"}, want: true},
		{
			name: "user-only rule matches",
			cfg:  telemetry.Config{Content: "rules", Rules: []telemetry.Rule{{User: user}}},
			want: true,
		},
		{
			name: "user-only rule does not match another user",
			cfg:  telemetry.Config{Content: "rules", Rules: []telemetry.Rule{{User: "bob"}}},
			want: false,
		},
		{
			name: "provider+model rule matches",
			cfg: telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
				{Provider: provider, Model: "deepseek/*"},
			}},
			want: true,
		},
		{
			name: "AND semantics: provider matches but model does not",
			cfg: telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
				{Provider: provider, Model: "openai/*"},
			}},
			want: false,
		},
		{
			name: "OR across rules",
			cfg: telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
				{User: "bob"},
				{Model: "openai/*"},
				{User: user},
			}},
			want: true,
		},
		{
			// A Config built without Validate (a fixture, a future caller)
			// must still fail closed: the zero Rule matches everything.
			name: "a rule with no fields denies",
			cfg:  telemetry.Config{Content: "rules", Rules: []telemetry.Rule{{}}},
			want: false,
		},
		{
			name: "an empty rule does not disable its siblings",
			cfg: telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
				{},
				{User: user},
			}},
			want: true,
		},
		{
			name: "rules that all miss deny",
			cfg: telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
				{User: "bob"},
				{Provider: "anthropic"},
			}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			c.Eq(tc.want, tc.cfg.Policy().Allows(user, provider, model))
		})
	}

	t.Run("glob matches and does not", func(t *testing.T) {
		c := assert.NewCollecting(t)
		p := telemetry.Config{Content: "rules", Rules: []telemetry.Rule{
			{Provider: provider, Model: "deepseek/*"},
		}}.Policy()
		c.True(p.Allows(user, provider, "deepseek/deepseek-v4"))
		c.False(p.Allows(user, provider, "openai/gpt-5"))
	})
}
