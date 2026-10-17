// SPDX-License-Identifier: Apache-2.0

package relsign_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/relsign"
)

func TestCompareTags(t *testing.T) {
	c := assert.NewCollecting(t)
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"minor numeric order", "v0.10.0", "v0.9.0", 1},
		{"equal", "v1.0.0", "v1.0.0", 0},
		{"below", "v0.0.1", "v0.1.0", -1},
		{"major over patch", "v2.0.0", "v1.99.99", 1},
	}
	for _, tc := range cases {
		got, err := relsign.CompareTags(tc.a, tc.b)
		c.Require().NoError(err, "CompareTags(%q, %q)", tc.a, tc.b)
		c.Eq(tc.want, got, "CompareTags(%q, %q)", tc.a, tc.b)
	}
}

func TestParseTagRejects(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tag := range []string{"v01.0.0", "v1.0.0-rc1", "v99999999999999999999.0.0", "V1.0.0", "v1.0"} {
		_, err := relsign.ParseTag(tag)
		c.ErrorIs(err, relsign.ErrBadTag, "ParseTag(%q)", tag)
	}
}

func TestCompareTagsBadArgumentWrapsErrBadTag(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct{ a, b string }{
		{"nope", "v1.0.0"},
		{"v1.0.0", "nope"},
	} {
		_, err := relsign.CompareTags(tc.a, tc.b)
		c.ErrorIs(err, relsign.ErrBadTag, "CompareTags(%q, %q)", tc.a, tc.b)
	}
}
