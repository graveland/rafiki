// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEffortCacheClamp(t *testing.T) {
	c := NewEffortCache()
	c.Learn("codex", []string{"medium"})
	c.Learn("onlylow", []string{"low"})
	c.Learn("lowmed", []string{"low", "medium"})
	c.Learn("medhigh", []string{"medium", "high"})
	c.Learn("rejectsall", []string{})

	cases := []struct {
		model, req, wantEffort, wantAction string
	}{
		{"absent", "high", "high", "keep"},    // not learned -> passthrough
		{"codex", "medium", "medium", "keep"}, // already allowed
		{"codex", "high", "medium", "clamp"},  // down: only allowed (medium)
		{"codex", "low", "medium", "clamp"},   // up: only allowed (medium)
		{"codex", "max", "medium", "clamp"},   // down: only allowed (medium)
		{"lowmed", "high", "medium", "clamp"}, // down: highest allowed <= high
		{"medhigh", "low", "medium", "clamp"}, // up: below all allowed -> lowest (medium)
		{"medhigh", "high", "high", "keep"},   // already allowed
		{"medhigh", "xhigh", "high", "clamp"}, // down: highest allowed <= xhigh
		{"medhigh", "max", "high", "clamp"},   // down: highest allowed <= max
		{"onlylow", "high", "low", "clamp"},   // down: nearest (low)
		{"rejectsall", "high", "", "strip"},   // empty allowed -> strip
	}
	for _, c2 := range cases {
		gotE, gotA := c.Clamp(c2.model, c2.req)
		if gotE != c2.wantEffort || gotA != c2.wantAction {
			t.Errorf("Clamp(%q,%q) = (%q,%q), want (%q,%q)", c2.model, c2.req, gotE, gotA, c2.wantEffort, c2.wantAction)
		}
	}
}

func TestEffortCacheClampUnknownRequested(t *testing.T) {
	c := NewEffortCache()
	c.Learn("lowmed", []string{"low", "medium"})
	// An unrecognized requested effort is treated as the highest, then clamped to
	// the highest allowed value.
	if eff, act := c.Clamp("lowmed", "bogus"); eff != "medium" || act != "clamp" {
		t.Errorf("Clamp(lowmed,bogus) = (%q,%q), want (medium,clamp)", eff, act)
	}
}

func TestEffortCacheLearnOverwrites(t *testing.T) {
	c := NewEffortCache()
	c.Learn("m", []string{"low"})
	if eff, _ := c.Clamp("m", "high"); eff != "low" {
		t.Fatalf("first learn: got %q, want low", eff)
	}
	c.Learn("m", []string{"medium", "high"})
	if _, act := c.Clamp("m", "high"); act != "keep" {
		t.Errorf("after re-learn high is allowed -> want keep, got %q", act)
	}
}

func TestParseSupportedEfforts(t *testing.T) {
	codexRaw := `{"error":{"message":"Unsupported value: 'high' is not supported with the 'gpt-5-codex' model. Supported values are: 'medium'.","type":"invalid_request_error","param":"text.verbosity","code":"unsupported_value"}}`
	codexEnvelope, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":  "Provider returned error",
		"metadata": map[string]any{"raw": codexRaw, "provider_name": "OpenAI"},
	}})
	multi := []byte(`{"error":{"message":"Supported values are: 'low', 'medium', 'high'."}}`)

	cases := []struct {
		name   string
		body   []byte
		want   []string
		wantOK bool
	}{
		{"codex nested", codexEnvelope, []string{"medium"}, true},
		{"multi values, sorted low->high", multi, []string{"low", "medium", "high"}, true},
		{"non-effort enum ignored", []byte(`{"error":{"message":"Supported values are: 'json', 'text'."}}`), nil, false},
		{"no enumeration", []byte(`{"error":{"message":"bad request"}}`), nil, false},
		{"not json", []byte(`not json`), nil, false},
	}
	for _, c := range cases {
		got, ok := ParseSupportedEfforts(c.body)
		if ok != c.wantOK || (ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s: ParseSupportedEfforts = (%v,%v), want (%v,%v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
