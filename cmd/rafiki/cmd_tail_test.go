// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestTailFlagsDefaults(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTailCmd()
	n, err := cmd.Flags().GetInt("tail")
	c.Require().False(err != nil || n != 20, "tail default = %d (err %v), want 20", n, err)
	if raw, err := cmd.Flags().GetBool("raw"); err != nil || raw {
		t.Fatalf("raw default = %v (err %v), want false", raw, err)
	}
	c.Require().NotNil(cmd.Flags().ShorthandLookup("n"), "missing -n shorthand for --tail")
	for _, flag := range []string{"label", "has-label", "types", "all-types"} {
		c.NotNil(cmd.Flags().Lookup(flag), "tail is missing the %q flag", flag)
	}
	if !containsAlias(cmd.Aliases, "stream") {
		t.Errorf("tail lost its 'stream' alias: %v", cmd.Aliases)
	}
}

// tail with no id streams from everything the caller is entitled to: subject
// all, no selector, lifecycle types only, durable tier, and — there being no
// history to backfill — no cursor.
func TestTailWithNoIDBuildsSubjectAll(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &stubControl{}
	serveStubControl(t, stub)

	cmd := newTailCmd()
	_, _, err := runCmd(t, cmd)
	c.Require().NoError(err, "rafiki tail (no id)")
	c.Require().Eq(1, stub.streamCalls, "StreamEvents called")
	req := stub.streamReqs[0]
	if _, ok := req.GetSubject().GetScope().(*rafikiv1.EventSubject_All); !ok {
		t.Errorf("subject scope = %T, want all", req.GetSubject().GetScope())
	}
	c.Eq("", req.GetSubject().GetLabelSelector(), "label selector")
	if got := strings.Join(req.GetTypes(), ","); !strings.Contains(got, "agent_status") || !strings.Contains(got, "turn_end") {
		t.Errorf("multi-child default types = %v, want the lifecycle set", req.GetTypes())
	}
	for _, banned := range []string{"user_message", "content_block_delta"} {
		for _, got := range req.GetTypes() {
			c.NotEq(banned, got, "multi-child default types include")
		}
	}
	c.Eq(rafikiv1.EventTier_EVENT_TIER_DURABLE, req.GetTier(), "tier")
	c.Nil(req.GetCursor(), "multi-child tail must not backfill a cursor, got")
	c.Eq(0, stub.historyCalls, "no-id tail must not load history (called")
}

// --label a=b --has-label c builds the comma-joined label selector, bare k
// meaning presence.
func TestTailLabelsBuildTheSelector(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &stubControl{}
	serveStubControl(t, stub)

	cmd := newTailCmd()
	_, _, err := runCmd(t, cmd, "--label", "a=b", "--has-label", "c")
	c.Require().NoError(err, "rafiki tail --label a=b --has-label c")
	req := stub.streamReqs[0]
	c.Eq("a=b,c", req.GetSubject().GetLabelSelector(), "label selector")
	if _, ok := req.GetSubject().GetScope().(*rafikiv1.EventSubject_All); !ok {
		t.Errorf("label-filtered tail must keep the all-subject, got %T", req.GetSubject().GetScope())
	}
}

// A child argument and a label selector are mutually exclusive.
func TestTailIDAndLabelsAreMutuallyExclusive(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	cmd := newTailCmd()
	_, _, err := runCmd(t, cmd, "--label", "a=b", "c_1")
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "not both"), "tail c_1 --label a=b = %v, want the mutual-exclusion error", err)
}

// buildLabelSelector: order preserved, validated, grammar poisoning refused.
func TestBuildLabelSelector(t *testing.T) {
	c := assert.NewAborting(t)
	got, err := buildLabelSelector([]string{"a=b"}, []string{"c"})
	c.False(err != nil || got != "a=b,c", "buildLabelSelector(a=b, c) = %q, %v; want a=b,c", got, err)
	got, err = buildLabelSelector([]string{"x=1", "y=2"}, []string{"k"})
	c.False(err != nil || got != "x=1,y=2,k", "buildLabelSelector order = %q, %v; want x=1,y=2,k", got, err)
	// A bare key alone means presence.
	got, err = buildLabelSelector(nil, []string{"only"})
	c.False(err != nil || got != "only", "buildLabelSelector(only) = %q, %v; want only", got, err)
	// Comma or whitespace in a term would read as two selector terms — refuse
	// rather than silently narrow.
	if _, err := buildLabelSelector([]string{"a=b,c=d"}, nil); err == nil {
		t.Fatal("a comma inside a term must be refused")
	}
	if _, err := buildLabelSelector([]string{"a=b c"}, nil); err == nil {
		t.Fatal("whitespace inside a term must be refused")
	}
	if _, err := buildLabelSelector([]string{"nope"}, nil); err == nil {
		t.Fatal("a --label term without = must be refused")
	}
	if got, err := buildLabelSelector(nil, nil); err != nil || got != "" {
		t.Fatalf("no labels = %q, %v; want empty selector", got, err)
	}
}
