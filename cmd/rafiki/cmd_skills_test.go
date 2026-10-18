// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestSplitQualified(t *testing.T) {
	for _, tc := range []struct{ in, ns, name string }{
		{"rafiki:coordinating", "rafiki", "coordinating"},
		{"pg:design-postgres-tables", "pg", "design-postgres-tables"},
		{"bare", "rafiki", "bare"},
	} {
		ns, name := splitQualified(tc.in)
		assert.NewCollecting(t).False(ns != tc.ns || name != tc.name, "%q: got (%q,%q), want (%q,%q)", tc.in, ns, name, tc.ns, tc.name)
	}
}
