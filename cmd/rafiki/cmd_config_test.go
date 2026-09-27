// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/clientstate"

	"github.com/multigres/testkit/assert"
)

func TestParseConfigPairs(t *testing.T) {
	c := assert.NewAborting(t)
	pairs, err := parseConfigPairs([]string{"currency.code=CAD", "currency.rate=1.38"})
	c.NoError(err)
	c.Len(pairs, 2, "got %d pairs, want 2", len(pairs))
	if pairs[0].key.name != "currency.code" || pairs[0].val != "CAD" {
		t.Errorf("pair 0 = %+v", pairs[0])
	}
	if pairs[1].key.name != "currency.rate" || pairs[1].val != "1.38" {
		t.Errorf("pair 1 = %+v", pairs[1])
	}
}

func TestParseConfigPairs_MalformedArg(t *testing.T) {
	_, err := parseConfigPairs([]string{"currency.code"})
	assert.NewAborting(t).Error(err, "want an error for an arg with no '='")
}

func TestParseConfigPairs_UnknownKey(t *testing.T) {
	_, err := parseConfigPairs([]string{"bogus=5"})
	assert.NewAborting(t).Error(err, "want an error for an unregistered key")
}

// A value with its own '=' (unlikely for these keys, but the split must not
// assume there is exactly one) keeps everything after the first '='.
func TestParseConfigPairs_ValueContainsEquals(t *testing.T) {
	c := assert.NewCollecting(t)
	pairs, err := parseConfigPairs([]string{"currency.code=CA=D"})
	c.Require().NoError(err)
	c.Eq("CA=D", pairs[0].val, "val")
}

// The whole point of validating against a scratch state first: a later pair
// failing must not leave an earlier pair applied.
func TestRunConfigSet_BatchIsAtomic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	cmd := newConfigSetCmd()
	err := cmd.RunE(cmd, []string{"currency.code=CAD", "currency.rate=not-a-number"})
	c.Require().Error(err, "want an error from the invalid second pair")

	got := clientstate.LoadScoped(clientstate.Scope{})
	c.Nil(got.Currency, "Currency")
}

func TestRunConfigSet_AppliesValidBatch(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)

	cmd := newConfigSetCmd()
	c.Require().NoError(cmd.RunE(cmd, []string{"currency.code=cad", "currency.rate=1.38"}))

	got := clientstate.LoadScoped(clientstate.Scope{})
	c.False(got.Currency == nil || got.Currency.Code != "CAD" || got.Currency.Rate != 1.38, "Currency = %+v, want {CAD 1.38} (code uppercased)", got.Currency)
}

func TestRenderConfig_Table(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	s := clientstate.State{Currency: &clientstate.Currency{Code: "CAD", Rate: 1.38}}
	c.NoError(renderConfig(&buf, s, clientstate.State{}, outputTable, false))
	out := buf.String()
	for _, want := range []string{"currency.code", "CAD", "currency.rate", "1.38"} {
		c.StrContains(out, want, "output missing")
	}
}

// Unset settings show as "-" (table) rather than an empty cell, matching
// every other unset column in `rafiki list`.
func TestRenderConfig_TableUnset(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	c.NoError(renderConfig(&buf, clientstate.State{}, clientstate.State{}, outputTable, false))
	c.StrContains(buf.String(), "-", "unset value should render as \"-\":\n")
}

func TestRenderConfig_JSON(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	s := clientstate.State{Currency: &clientstate.Currency{Code: "CAD", Rate: 1.38}}
	c.NoError(renderConfig(&buf, s, clientstate.State{}, outputJSON, false))
	out := buf.String()
	c.False(!strings.Contains(out, `"currency.code": "CAD"`) || !strings.Contains(out, `"currency.rate": "1.38"`), "JSON output: %s", out)
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
		assert.NewCollecting(t).Eq(global[k.name], k.global, "configKey %q: global = %v, want %v — see the plan's Task 11 before changing this", k.name, k.global, global[k.name])
	}
}

func TestConfigShowReportsTheScope(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	var buf bytes.Buffer
	c.NoError(renderConfig(&buf, clientstate.State{}, clientstate.State{}, outputTable, false), "renderConfig")
	out := buf.String()
	c.StrContains(out, "SCOPE", "config show has no scope column:\n")
}
