// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestActiveMarkerIsScopedToTheProfile(t *testing.T) {
	isolateProfiles(t)

	if err := setActive("work", "child-w"); err != nil {
		t.Fatalf("setActive(work): %v", err)
	}
	if err := setActive("personal", "child-p"); err != nil {
		t.Fatalf("setActive(personal): %v", err)
	}
	if got := getActive("work"); got != "child-w" {
		t.Fatalf("work active = %q, want child-w", got)
	}
	if got := getActive("personal"); got != "child-p" {
		t.Fatalf("personal active = %q, want child-p", got)
	}
}

func TestActiveMarkerIsEmptyForAProfileThatHasNone(t *testing.T) {
	isolateProfiles(t)
	if got := getActive("fresh"); got != "" {
		t.Fatalf("getActive(fresh) = %q, want empty", got)
	}
}
