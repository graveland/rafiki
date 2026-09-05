// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

func TestParseConfigPairs(t *testing.T) {
	pairs, err := parseConfigPairs([]string{"currency.code=CAD", "currency.rate=1.38"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want 2", len(pairs))
	}
	if pairs[0].key.name != "currency.code" || pairs[0].val != "CAD" {
		t.Errorf("pair 0 = %+v", pairs[0])
	}
	if pairs[1].key.name != "currency.rate" || pairs[1].val != "1.38" {
		t.Errorf("pair 1 = %+v", pairs[1])
	}
}

func TestParseConfigPairs_MalformedArg(t *testing.T) {
	if _, err := parseConfigPairs([]string{"currency.code"}); err == nil {
		t.Fatal("want an error for an arg with no '='")
	}
}

func TestParseConfigPairs_UnknownKey(t *testing.T) {
	if _, err := parseConfigPairs([]string{"bogus=5"}); err == nil {
		t.Fatal("want an error for an unregistered key")
	}
}

// A value with its own '=' (unlikely for these keys, but the split must not
// assume there is exactly one) keeps everything after the first '='.
func TestParseConfigPairs_ValueContainsEquals(t *testing.T) {
	pairs, err := parseConfigPairs([]string{"currency.code=CA=D"})
	if err != nil {
		t.Fatal(err)
	}
	if pairs[0].val != "CA=D" {
		t.Errorf("val = %q, want %q", pairs[0].val, "CA=D")
	}
}

// The whole point of validating against a scratch state first: a later pair
// failing must not leave an earlier pair applied.
func TestRunConfigSet_BatchIsAtomic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	cmd := newConfigSetCmd()
	err := cmd.RunE(cmd, []string{"currency.code=CAD", "currency.rate=not-a-number"})
	if err == nil {
		t.Fatal("want an error from the invalid second pair")
	}

	got := clientstate.LoadScoped(clientstate.Scope{})
	if got.Currency != nil {
		t.Errorf("Currency = %+v, want nil -- the valid first pair must not have applied", got.Currency)
	}
}

func TestRunConfigSet_AppliesValidBatch(t *testing.T) {
	isolateProfiles(t)

	cmd := newConfigSetCmd()
	if err := cmd.RunE(cmd, []string{"currency.code=cad", "currency.rate=1.38"}); err != nil {
		t.Fatal(err)
	}

	got := clientstate.LoadScoped(clientstate.Scope{})
	if got.Currency == nil || got.Currency.Code != "CAD" || got.Currency.Rate != 1.38 {
		t.Errorf("Currency = %+v, want {CAD 1.38} (code uppercased)", got.Currency)
	}
}

func TestRenderConfig_Table(t *testing.T) {
	var buf bytes.Buffer
	s := clientstate.State{Currency: &clientstate.Currency{Code: "CAD", Rate: 1.38}}
	if err := renderConfig(&buf, s, clientstate.State{}, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"currency.code", "CAD", "currency.rate", "1.38"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// Unset settings show as "-" (table) rather than an empty cell, matching
// every other unset column in `rafiki list`.
func TestRenderConfig_TableUnset(t *testing.T) {
	var buf bytes.Buffer
	if err := renderConfig(&buf, clientstate.State{}, clientstate.State{}, outputTable, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "-") {
		t.Fatalf("unset value should render as \"-\":\n%s", buf.String())
	}
}

func TestRenderConfig_JSON(t *testing.T) {
	var buf bytes.Buffer
	s := clientstate.State{Currency: &clientstate.Currency{Code: "CAD", Rate: 1.38}}
	if err := renderConfig(&buf, s, clientstate.State{}, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"currency.code": "CAD"`) || !strings.Contains(out, `"currency.rate": "1.38"`) {
		t.Fatalf("JSON output: %s", out)
	}
}

// Per-profile is the default; a key is global only when it is a property
// of the person that no daemon can influence. If you are adding a key and
// this test makes you think, that is the point.
func TestEveryConfigKeyDeclaresItsScopeAndDefaultsToProfile(t *testing.T) {
	global := map[string]bool{
		"currency.code": true,
		"currency.rate": true,
	}
	for _, k := range configKeys {
		if k.global != global[k.name] {
			t.Errorf("configKey %q: global = %v, want %v — see the plan's Task 11 before changing this",
				k.name, k.global, global[k.name])
		}
	}
}

func TestConfigShowReportsTheScope(t *testing.T) {
	isolateProfiles(t)
	var buf bytes.Buffer
	if err := renderConfig(&buf, clientstate.State{}, clientstate.State{}, outputTable, false); err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "SCOPE") {
		t.Fatalf("config show has no scope column:\n%s", out)
	}
}
