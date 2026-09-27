// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestCompleteExecutorFiltersByPrefix(t *testing.T) {
	c := assert.NewAborting(t)
	ids := []string{"greyshift", "silvershift", "sess-01ABC"}
	got := filterByPrefix(ids, "s")
	want := []string{"sess-01ABC", "silvershift"} // sorted
	c.Len(got, len(want), "got %v, want %v", got, want)
	for i := range want {
		c.Eq(want[i], got[i], "got %v, want %v", got, want)
	}
}
