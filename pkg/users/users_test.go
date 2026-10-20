// SPDX-License-Identifier: Apache-2.0

package users

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestNormalizeUsername(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "brent", "brent", false},
		{"trims surrounding space", "  brent\t\n", "brent", false},
		// No charset rule by design — these must all survive.
		{"dotted", "brent.graveland", "brent.graveland", false},
		{"email", "brent@graveland.net", "brent@graveland.net", false},
		{"dashed", "ci-runner-01", "ci-runner-01", false},
		{"unicode", "bréntß", "bréntß", false},
		{"empty", "", "", true},
		{"whitespace only", "   \t ", "", true},
		{"at the length cap", strings.Repeat("a", MaxUsernameLen), strings.Repeat("a", MaxUsernameLen), false},
		{"one past the cap", strings.Repeat("a", MaxUsernameLen+1), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			got, err := NormalizeUsername(tt.in)
			if tt.wantErr {
				c.Require().Error(err, "NormalizeUsername(%q) = %q, want an error", tt.in, got)
				// Callers distinguish a bad name from an unreachable store.
				c.Require().ErrorIs(err, ErrInvalidUsername, "error")
				return
			}
			c.Require().NoError(err, "NormalizeUsername(%q)", tt.in)
			c.Eq(tt.want, got, "NormalizeUsername(%q) = %q, want", tt.in, got)
		})
	}
}

// The cap is a byte bound, not a rune count — worth pinning, because a rune
// bound would let a multi-byte name past a TEXT column limit chosen in bytes.
func TestNormalizeUsernameCapCountsBytesNotRunes(t *testing.T) {
	// 33 two-byte runes = 66 bytes, over a 64-byte cap but only 33 runes.
	in := strings.Repeat("é", 33)
	if len(in) <= MaxUsernameLen {
		t.Skipf("fixture is %d bytes, not over the %d cap", len(in), MaxUsernameLen)
	}
	_, err := NormalizeUsername(in)
	assert.NewAborting(t).Error(err, "a 66-byte, 33-rune name was accepted; the cap is counting runes")
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Lowercase is the stored form: an address is an identity key, and
		// Brent@ and brent@ must resolve to one user, not two.
		{"lowercases", "Brent@Graveland.NET", "brent@graveland.net", false},
		{"trims surrounding space", "  brent@graveland.net\t\n", "brent@graveland.net", false},
		{"empty is valid and stays empty", "", "", false},
		{"whitespace only is empty", "   \t ", "", false},
		{"two @", "a@b@c", "", true},
		{"missing @", "brent.graveland.net", "", true},
		{"empty local part", "@graveland.net", "", true},
		{"empty domain part", "brent@", "", true},
		{"embedded space", "br ent@graveland.net", "", true},
		{"embedded tab", "brent@graveland. net", "", true},
		{"exactly 255 bytes, one over the cap", strings.Repeat("a", 250) + "@" + strings.Repeat("b", 4), "", true},
		{"exactly at the byte cap", strings.Repeat("a", 245) + "@" + strings.Repeat("b", 4) + ".net", strings.Repeat("a", 245) + "@" + strings.Repeat("b", 4) + ".net", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			got, err := NormalizeEmail(tt.in)
			if tt.wantErr {
				c.Require().Error(err, "NormalizeEmail(%q) = %q, want an error", tt.in, got)
				// Callers distinguish a bad address from an unreachable store.
				c.Require().ErrorIs(err, ErrInvalidEmail, "error")
				// Same reason shape as NormalizeUsername: text after ": ".
				c.Require().NotEq("users: invalid email", err.Error(), "error must carry a reason after the sentinel text")
				return
			}
			c.Require().NoError(err, "NormalizeEmail(%q)", tt.in)
			c.Eq(tt.want, got, "NormalizeEmail(%q) = %q, want", tt.in, got)
		})
	}
}

// TestTokenCapabilitiesValidation pins ValidCapabilities without a database:
// nil is full user authority and always legal, and a non-nil set is legal only
// when it is exactly the known capabilities with no repeats. The refusal names
// the offending value, so a mint can say what was wrong.
func TestTokenCapabilitiesValidation(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		wantErr bool
		names   string
	}{
		{"nil is full authority", nil, false, ""},
		{"the one capability", []string{CapabilityNotify}, false, ""},
		{"empty is none", []string{}, false, ""},
		{"unknown", []string{"admin"}, true, `"admin"`},
		{"unknown among known", []string{CapabilityNotify, "admin"}, true, `"admin"`},
		{"duplicate", []string{CapabilityNotify, CapabilityNotify}, true, `"notify"`},
		{"case matters", []string{"Notify"}, true, `"Notify"`},
		{"empty element", []string{""}, true, `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := ValidCapabilities(tt.in)
			if !tt.wantErr {
				c.Require().NoError(err, "ValidCapabilities(%v)", tt.in)
				return
			}
			c.Require().Error(err, "ValidCapabilities(%v) was accepted", tt.in)
			c.Require().ErrorIs(err, ErrInvalidCapability, "error")
			c.StrContains(err.Error(), tt.names, "err = %v, want the offending value named", err)
		})
	}
}
