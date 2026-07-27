package main

import (
	"strings"
	"testing"
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
		if err := validateCLILabelKey(k); err != nil {
			t.Errorf("validateCLILabelKey(%q): unexpected error: %v", k, err)
		}
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
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("validateCLILabelKey(%q): error %q does not contain %q", tc.key, err.Error(), tc.wantSub)
		}
	}
}

func TestValidateCLILabelKey_ReservedPrefix(t *testing.T) {
	keys := []string{"fundi/model", "fundi/provider", "fundi/cwd", "fundi/", "fundi/x"}
	for _, k := range keys {
		err := validateCLILabelKey(k)
		if err == nil {
			t.Errorf("validateCLILabelKey(%q): expected reserved-prefix error, got nil", k)
			continue
		}
		if !strings.Contains(err.Error(), "fundi/") {
			t.Errorf("validateCLILabelKey(%q): error %q should mention fundi/", k, err.Error())
		}
	}
}

// ─── parseLabelPairs ──────────────────────────────────────────────────────────

func TestParseLabelPairs_Basic(t *testing.T) {
	pairs := []string{"env=prod", "tier=fast", "owner=brent"}
	got, err := parseLabelPairs(pairs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["env"] != "prod" || got["tier"] != "fast" || got["owner"] != "brent" {
		t.Errorf("got %v", got)
	}
}

func TestParseLabelPairs_ValueWithEquals(t *testing.T) {
	// Value itself contains '=' — only first = splits.
	got, err := parseLabelPairs([]string{"url=http://x?a=b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["url"] != "http://x?a=b" {
		t.Errorf("got %q, want %q", got["url"], "http://x?a=b")
	}
}

func TestParseLabelPairs_Empty(t *testing.T) {
	got, err := parseLabelPairs(nil)
	if err != nil || got != nil {
		t.Errorf("nil input: got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestParseLabelPairs_MissingEquals(t *testing.T) {
	_, err := parseLabelPairs([]string{"noequals"})
	if err == nil {
		t.Fatal("expected error for missing =")
	}
}

func TestParseLabelPairs_BadKey(t *testing.T) {
	_, err := parseLabelPairs([]string{"bad key=v"})
	if err == nil {
		t.Fatal("expected error for key with space")
	}
}

func TestParseLabelPairs_ReservedKey(t *testing.T) {
	_, err := parseLabelPairs([]string{"fundi/model=evil"})
	if err == nil {
		t.Fatal("expected error for fundi/ prefix")
	}
}

// ─── parseEnvLabels ───────────────────────────────────────────────────────────

func TestParseEnvLabels_Basic(t *testing.T) {
	got, err := parseEnvLabels("context=work,env=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["context"] != "work" || got["env"] != "prod" {
		t.Errorf("got %v", got)
	}
}

func TestParseEnvLabels_WithSpaces(t *testing.T) {
	got, err := parseEnvLabels("  key=val , key2=val2  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["key"] != "val" || got["key2"] != "val2" {
		t.Errorf("got %v", got)
	}
}

func TestParseEnvLabels_Empty(t *testing.T) {
	got, err := parseEnvLabels("")
	if err != nil || got != nil {
		t.Errorf("empty: got (%v, %v), want (nil, nil)", got, err)
	}
}

// ─── mergeLabels ─────────────────────────────────────────────────────────────

func TestMergeLabels_LaterWins(t *testing.T) {
	a := map[string]string{"k": "a", "only-in-a": "yes"}
	b := map[string]string{"k": "b", "only-in-b": "yes"}
	got := mergeLabels(a, b)
	if got["k"] != "b" {
		t.Errorf("expected b to win, got %q", got["k"])
	}
	if got["only-in-a"] != "yes" || got["only-in-b"] != "yes" {
		t.Errorf("missing keys: %v", got)
	}
}

func TestMergeLabels_AllNil(t *testing.T) {
	if got := mergeLabels(nil, nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// ─── formatLabels ────────────────────────────────────────────────────────────

func TestFormatLabels_Empty(t *testing.T) {
	if got := formatLabels(nil, 0, false); got != "-" {
		t.Errorf("got %q, want -", got)
	}
}

func TestFormatLabels_Sorted(t *testing.T) {
	labels := map[string]string{"z": "last", "a": "first", "m": "mid"}
	got := formatLabels(labels, 0, false)
	if !strings.HasPrefix(got, "a=first,m=mid,z=last") {
		t.Errorf("got %q, want sorted a,m,z", got)
	}
}

func TestFormatLabels_Truncation(t *testing.T) {
	labels := map[string]string{"longkey": "longvalue"}
	got := formatLabels(labels, 10, false)
	if !strings.HasSuffix(got, "\u2026") {
		t.Errorf("expected truncation marker, got %q", got)
	}
	if len(got) >= len("longkey=longvalue") {
		t.Errorf("got %q, should be shorter than untruncated", got)
	}
}

func TestFormatLabels_HidesAutoLabelPrefixByDefault(t *testing.T) {
	labels := map[string]string{
		"fundi/cwd":   "/home/foo",
		"fundi/model": "claude-opus-4",
		"context":     "work",
	}
	got := formatLabels(labels, 0, false)
	if got != "context=work" {
		t.Errorf("got %q, want only user labels (fundi/* hidden)", got)
	}
}

func TestFormatLabels_IncludesAutoLabelPrefixWhenRequested(t *testing.T) {
	labels := map[string]string{
		"fundi/cwd": "/home/foo",
		"context":   "work",
	}
	got := formatLabels(labels, 0, true)
	if !strings.Contains(got, "fundi/cwd=/home/foo") {
		t.Errorf("got %q, expected fundi/cwd to be included", got)
	}
	if !strings.Contains(got, "context=work") {
		t.Errorf("got %q, expected context=work to be included", got)
	}
}

func TestFormatLabels_AllAutoLabelsHiddenReturnsDash(t *testing.T) {
	labels := map[string]string{
		"fundi/cwd":   "/home/foo",
		"fundi/model": "claude",
	}
	if got := formatLabels(labels, 0, false); got != "-" {
		t.Errorf("got %q, want - when only fundi/* labels present", got)
	}
}
