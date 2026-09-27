package main

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// ─── validateCLILabelKey ──────────────────────────────────────────────────────

func TestValidateCLILabelKey_Valid(t *testing.T) {
	valid := []string{
		"env", "tier", "owner", "k", "K",
		"a.b.c", "a-b-c", "a_b_c",
		"a/b", "x/y/z",
		"0", "123abc",
	}
	for _, k := range valid {
		assert.NewCollecting(t).NoError(validateCLILabelKey(k), "validateCLILabelKey(%q): unexpected error", k)
	}
}

func TestValidateCLILabelKey_Invalid(t *testing.T) {
	cases := []struct {
		key     string
		wantSub string
	}{
		{"", "empty"},
		{"has space", "invalid characters"},
		{"has\ttab", "invalid characters"},
		{"has@at", "invalid characters"},
		{"has!bang", "invalid characters"},
	}
	for _, tc := range cases {
		err := validateCLILabelKey(tc.key)
		if err == nil {
			t.Errorf("validateCLILabelKey(%q): expected error, got nil", tc.key)
			continue
		}
		assert.NewCollecting(t).StrContains(err.Error(), tc.wantSub, "validateCLILabelKey(%q): error %q does not contain", tc.key, err.Error())
	}
}

func TestValidateCLILabelKey_ReservedPrefix(t *testing.T) {
	keys := []string{"rafiki/model", "rafiki/provider", "rafiki/cwd", "rafiki/", "rafiki/x"}
	for _, k := range keys {
		err := validateCLILabelKey(k)
		if err == nil {
			t.Errorf("validateCLILabelKey(%q): expected reserved-prefix error, got nil", k)
			continue
		}
		assert.NewCollecting(t).StrContains(err.Error(), "rafiki/", "validateCLILabelKey(%q): error %q should mention rafiki/", k, err.Error())
	}
}

// ─── parseLabelPairs ──────────────────────────────────────────────────────────

func TestParseLabelPairs_Basic(t *testing.T) {
	c := assert.NewCollecting(t)
	pairs := []string{"env=prod", "tier=fast", "owner=brent"}
	got, err := parseLabelPairs(pairs)
	c.Require().NoError(err, "unexpected error")
	c.False(got["env"] != "prod" || got["tier"] != "fast" || got["owner"] != "brent", "got %v", got)
}

func TestParseLabelPairs_ValueWithEquals(t *testing.T) {
	c := assert.NewCollecting(t)
	// Value itself contains '=' — only first = splits.
	got, err := parseLabelPairs([]string{"url=http://x?a=b"})
	c.Require().NoError(err, "unexpected error")
	c.Eq("http://x?a=b", got["url"], "got")
}

func TestParseLabelPairs_Empty(t *testing.T) {
	got, err := parseLabelPairs(nil)
	assert.NewCollecting(t).False(err != nil || got != nil, "nil input: got (%v, %v), want (nil, nil)", got, err)
}

func TestParseLabelPairs_MissingEquals(t *testing.T) {
	_, err := parseLabelPairs([]string{"noequals"})
	assert.NewAborting(t).Error(err, "expected error for missing =")
}

func TestParseLabelPairs_BadKey(t *testing.T) {
	_, err := parseLabelPairs([]string{"bad key=v"})
	assert.NewAborting(t).Error(err, "expected error for key with space")
}

func TestParseLabelPairs_ReservedKey(t *testing.T) {
	_, err := parseLabelPairs([]string{"rafiki/model=evil"})
	assert.NewAborting(t).Error(err, "expected error for rafiki/ prefix")
}

// ─── mergeLabels ─────────────────────────────────────────────────────────────

func TestMergeLabels_LaterWins(t *testing.T) {
	c := assert.NewCollecting(t)
	a := map[string]string{"k": "a", "only-in-a": "yes"}
	b := map[string]string{"k": "b", "only-in-b": "yes"}
	got := mergeLabels(a, b)
	c.Eq("b", got["k"], "expected b to win, got")
	c.False(got["only-in-a"] != "yes" || got["only-in-b"] != "yes", "missing keys: %v", got)
}

func TestMergeLabels_AllNil(t *testing.T) {
	assert.NewCollecting(t).Nil(mergeLabels(nil, nil), "expected nil, got")
}

// ─── formatLabels ────────────────────────────────────────────────────────────

func TestFormatLabels_Empty(t *testing.T) {
	assert.NewCollecting(t).Eq("-", formatLabels(nil, 0, false), "got")
}

func TestFormatLabels_Sorted(t *testing.T) {
	labels := map[string]string{"z": "last", "a": "first", "m": "mid"}
	got := formatLabels(labels, 0, false)
	if !strings.HasPrefix(got, "a=first,m=mid,z=last") {
		t.Errorf("got %q, want sorted a,m,z", got)
	}
}

func TestFormatLabels_Truncation(t *testing.T) {
	c := assert.NewCollecting(t)
	labels := map[string]string{"longkey": "longvalue"}
	got := formatLabels(labels, 10, false)
	c.True(strings.HasSuffix(got, "\u2026"), "expected truncation marker, got %q", got)
	c.Less(len("longkey=longvalue"), len(got), "got %q, should be shorter than untruncated", got)
}

func TestFormatLabels_HidesAutoLabelPrefixByDefault(t *testing.T) {
	labels := map[string]string{
		"rafiki/cwd":   "/home/foo",
		"rafiki/model": "claude-opus-4",
		"context":      "work",
	}
	got := formatLabels(labels, 0, false)
	assert.NewCollecting(t).Eq("context=work", got, "got")
}

func TestFormatLabels_IncludesAutoLabelPrefixWhenRequested(t *testing.T) {
	c := assert.NewCollecting(t)
	labels := map[string]string{
		"rafiki/cwd": "/home/foo",
		"context":    "work",
	}
	got := formatLabels(labels, 0, true)
	c.StrContains(got, "rafiki/cwd=/home/foo", "got")
	c.StrContains(got, "context=work", "got")
}

func TestFormatLabels_AllAutoLabelsHiddenReturnsDash(t *testing.T) {
	labels := map[string]string{
		"rafiki/cwd":   "/home/foo",
		"rafiki/model": "claude",
	}
	assert.NewCollecting(t).Eq("-", formatLabels(labels, 0, false), "got")
}
