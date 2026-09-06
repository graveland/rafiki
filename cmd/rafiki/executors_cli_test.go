// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestCompleteExecutorFiltersByPrefix(t *testing.T) {
	ids := []string{"greyshift", "silvershift", "sess-01ABC"}
	got := filterByPrefix(ids, "s")
	want := []string{"sess-01ABC", "silvershift"} // sorted
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
