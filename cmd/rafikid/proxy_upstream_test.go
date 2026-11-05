// SPDX-License-Identifier: Apache-2.0

package main

// The proxy face's /v1/messages upstream comes from the same provider config
// the senders read. Two branches, both pinned here:
//
//   - a configured [providers.anthropic] base_url WINS, so an operator pointing
//     rafiki at an Anthropic-compatible endpoint gets that endpoint for the face
//     every claude child talks through — not just for fundi children, whose
//     sender already honoured it;
//   - no declared base_url falls back to the literal public API, so an operator
//     who declares none sees byte-identical behaviour to before this existed.
//
// The integration half lives in test/integration (TestClaudeDeliveryCapture*),
// where a daemon booted with a providers.toml naming a local fake actually
// reaches that fake — which is the only way to observe the wiring, since the
// proxy keeps no upstream getter.

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/providers"
)

func TestMessagesUpstreamURL(t *testing.T) {
	c := assert.NewAborting(t)

	withBase := func(url string) *providers.Set {
		set := providers.Default()
		p := set.Providers["anthropic"]
		p.BaseURL = url
		set.Providers["anthropic"] = p
		return set
	}

	for name, tc := range map[string]struct {
		set  *providers.Set
		want string
	}{
		"configured base_url wins": {
			set:  withBase("http://127.0.0.1:1234"),
			want: "http://127.0.0.1:1234",
		},
		"no base_url falls back to the public API": {
			set:  providers.Default(),
			want: "https://api.anthropic.com",
		},
		"nil registry uses the built-in defaults": {
			set:  nil,
			want: "https://api.anthropic.com",
		},
		"a registry with no anthropic entry falls back": {
			set:  &providers.Set{Providers: map[string]providers.Provider{"other": {Name: "other"}}},
			want: "https://api.anthropic.com",
		},
		// An empty string is "not declared", not "the empty URL": forwarding to
		// "" would be a request with no scheme, which every caller would see as a
		// dead upstream rather than the default.
		"an empty base_url is not a declaration": {
			set:  withBase(""),
			want: "https://api.anthropic.com",
		},
		// The face appends "/v1/messages" to what this returns, and every sender
		// trims the trailing slash. Without the same trim, one declaration gives a
		// sender ".../anthropic/v1/messages" and this face ".../anthropic//v1/messages".
		"a trailing slash is trimmed, as the senders trim it": {
			set:  withBase("https://gw.example.test/anthropic/"),
			want: "https://gw.example.test/anthropic",
		},
	} {
		c.Eq(tc.want, messagesUpstreamURL(tc.set), "%s: upstream URL", name)
	}
}
