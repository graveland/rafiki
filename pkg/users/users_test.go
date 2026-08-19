// SPDX-License-Identifier: Apache-2.0

package users

import (
	"errors"
	"strings"
	"testing"
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
			got, err := NormalizeUsername(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeUsername(%q) = %q, want an error", tt.in, got)
				}
				// Callers distinguish a bad name from an unreachable store.
				if !errors.Is(err, ErrInvalidUsername) {
					t.Fatalf("error = %v, want it to wrap ErrInvalidUsername", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeUsername(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeUsername(%q) = %q, want %q", tt.in, got, tt.want)
			}
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
	if _, err := NormalizeUsername(in); err == nil {
		t.Fatal("a 66-byte, 33-rune name was accepted; the cap is counting runes")
	}
}
