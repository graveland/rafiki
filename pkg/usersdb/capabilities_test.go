// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// assertCapabilities pins the three states a capability set can be in. nil and
// a non-nil empty slice are DIFFERENT answers — NULL is full user authority and
// '{}' is nothing — so this fails loudly if they collapse into one another.
func assertCapabilities(c *assert.C, got, want []string, what string) {
	if want == nil {
		c.Nil(got, "%s: capabilities = %v (%T), want nil (full user authority)", what, got, got)
		return
	}
	c.NotNil(got, "%s: capabilities = nil, want a non-nil %v", what, want)
	c.EqDiff(want, got, "%s: capabilities", what)
	c.Len(got, len(want), "%s: capabilities", what)
}

// TestTokenCapabilitiesRoundTrip mints a nil, an empty and a {notify} set and
// reads each back through BOTH Authenticate and ListTokens. A store that let
// NULL and '{}' become the same value fails here whichever way it collapsed
// them: an empty set read back as nil loses "this token may do nothing", and a
// nil set read back as empty strips a pre-existing token of full authority.
func TestTokenCapabilitiesRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "hook"})
	c.Require().NoError(err, "create user")

	cases := []struct {
		name string
		caps []string
	}{
		{"full", nil},
		{"none", []string{}},
		{"notify", []string{users.CapabilityNotify}},
	}
	plain := make(map[string]string, len(cases))
	for _, tc := range cases {
		tok, secret, err := s.MintToken(ctx, u.ID, users.NewToken{
			Name: tc.name, Origin: users.OriginService, Capabilities: tc.caps,
		})
		c.Require().NoError(err, "mint %s", tc.name)
		c.Eq(u.ID, tok.UserID, "minted user id for %s", tc.name)
		// MintToken's returned Token carries the set it stored.
		assertCapabilities(c, tok.Capabilities, tc.caps, "mint "+tc.name)

		// The column holds what it was given: a nil set is NULL, never '{}'.
		var isNull bool
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT capabilities IS NULL FROM conversations.user_token WHERE id = $1::uuid`,
			tok.ID).Scan(&isNull), "read capabilities of %s", tc.name)
		c.Eq(tc.caps == nil, isNull, "%s: capabilities IS NULL = %v, want want-nil = %v",
			tc.name, isNull, tc.caps == nil)

		plain[tc.name] = secret
	}

	// ListTokens carries the set of every row.
	listed, err := s.ListTokens(ctx, u.ID, false)
	c.Require().NoError(err, "list tokens")
	c.Len(listed, len(cases), "listed tokens")
	byName := make(map[string]users.Token, len(listed))
	for _, tok := range listed {
		byName[tok.Name] = tok
	}
	for _, tc := range cases {
		tok, ok := byName[tc.name]
		c.Require().True(ok, "ListTokens did not return %q", tc.name)
		assertCapabilities(c, tok.Capabilities, tc.caps, "list "+tc.name)
	}

	// And so does the identity Authenticate builds.
	for _, tc := range cases {
		id, err := s.Authenticate(ctx, plain[tc.name])
		c.Require().NoError(err, "authenticate %s", tc.name)
		c.Eq(u.ID, id.UserID, "authenticated user for %s", tc.name)
		assertCapabilities(c, id.Capabilities, tc.caps, "authenticate "+tc.name)
	}
}

// TestTokenCapabilitiesRejectsUnknown pins the refusals MintToken owes: an
// unknown capability, a repeated one, and a set on an OIDC-origin mint (a
// federated session has full user authority, and nothing may narrow it). The
// two capability refusals are users.ErrInvalidCapability — an ANSWER — and
// every refusal must leave the table untouched.
func TestTokenCapabilitiesRejectsUnknown(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "hook"})
	c.Require().NoError(err, "create user")

	cases := []struct {
		name string
		tok  users.NewToken
		// names is a substring the refusal must contain: the offending value,
		// so an operator can see what was rejected.
		names string
		// answer is true when the refusal is users.ErrInvalidCapability rather
		// than a plain error.
		answer bool
	}{
		{
			name:   "unknown capability",
			tok:    users.NewToken{Name: "bad", Origin: users.OriginService, Capabilities: []string{"admin"}},
			names:  `"admin"`,
			answer: true,
		},
		{
			name:   "duplicate capability",
			tok:    users.NewToken{Name: "dup", Origin: users.OriginService, Capabilities: []string{users.CapabilityNotify, users.CapabilityNotify}},
			names:  `"notify"`,
			answer: true,
		},
		{
			name:  "oidc origin with capabilities",
			tok:   users.NewToken{Name: "oidc", Origin: users.OriginOIDC, Capabilities: []string{users.CapabilityNotify}},
			names: "oidc",
		},
	}
	for _, tc := range cases {
		_, _, err := s.MintToken(ctx, u.ID, tc.tok)
		c.Require().Error(err, "%s was minted", tc.name)
		c.StrContains(err.Error(), tc.names, "%s: err = %v, want the offending value named", tc.name, err)
		if tc.answer {
			c.ErrorIs(err, users.ErrInvalidCapability, "%s is an answer, not an outage", tc.name)
		}
	}

	tokens, err := s.ListTokens(ctx, u.ID, true)
	c.Require().NoError(err, "list tokens")
	c.Len(tokens, 0, "a refused mint inserted %d token row(s)", len(tokens))
}
