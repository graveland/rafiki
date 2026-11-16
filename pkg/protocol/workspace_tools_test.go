// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestNormalizeWorkspaceTools pins the one place "" is read as "native": only
// the empty string and the literal "native" collapse to the stored default,
// "rafiki" survives, and everything else is refused so a spawn can answer
// InvalidArgument rather than persist a value nothing understands.
func TestNormalizeWorkspaceTools(t *testing.T) {
	c := assert.NewAborting(t)
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{WorkspaceToolsNative, "", true},
		{WorkspaceToolsRafiki, WorkspaceToolsRafiki, true},
		{"Rafiki", "", false},
		{"Native", "", false},
		{"none", "", false},
		{" rafiki", "", false},
		{"rafiki ", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeWorkspaceTools(tc.in)
		c.Eq(got, tc.want, "NormalizeWorkspaceTools(%q) value", tc.in)
		c.Eq(ok, tc.wantOK, "NormalizeWorkspaceTools(%q) ok", tc.in)
	}
}
