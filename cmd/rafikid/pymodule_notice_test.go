// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// All-ready results (and the absence of any results) render nothing: the
// notice must stay empty so the tool result is the plain saved form.
func TestFormatVenvFailuresEmptyWhenAllReady(t *testing.T) {
	for name, results := range map[string][]*executorpb.PyModuleVenvResult{
		"nil slice":   nil,
		"empty slice": {},
		"all ready": {
			{Name: "alice_chart", Ready: true},
			{Name: "bob_util", Ready: true, Error: "should be ignored when Ready"},
		},
	} {
		if got := formatVenvFailures(results); got != "" {
			t.Errorf("%s: formatVenvFailures = %q, want \"\"", name, got)
		}
	}
}

// Each failing entry gets its own line naming its module and error. The
// aggregation's order is nondeterministic across executors and duplicates are
// kept, so the test only asserts containment -- and a duplicated failure
// renders once per occurrence, not deduplicated away.
func TestFormatVenvFailuresListsEachFailure(t *testing.T) {
	results := []*executorpb.PyModuleVenvResult{
		{Name: "zebra_plot", Ready: false, Error: "uv: not found"},
		{Name: "apple_util", Ready: false, Error: "no solution for requests==9.9"},
	}
	got := formatVenvFailures(results)
	for _, want := range []string{
		"dependency install failed on zebra_plot: uv: not found",
		"dependency install failed on apple_util: no solution for requests==9.9",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatVenvFailures = %q, want it to contain %q", got, want)
		}
	}
	if strings.Count(got, "dependency install failed on") != 2 {
		t.Errorf("formatVenvFailures = %q, want exactly one line per failing module", got)
	}
}
