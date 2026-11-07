// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestNormalizeNotify(t *testing.T) {
	c := assert.NewAborting(t)
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", NotifyAll, true},
		{"all", NotifyAll, true},
		{"result", NotifyResult, true},
		{"Result", "", false},
		{"none", "", false},
		{" result", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeNotify(tc.in)
		c.Eq(got, tc.want)
		c.Eq(ok, tc.wantOK)
	}
}
