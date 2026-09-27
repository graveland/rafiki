// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestPresetValidName(t *testing.T) {
	c := assert.NewCollecting(t)
	tests := []struct {
		name  string
		valid bool
	}{
		{"default:reviewer", true},
		{"r1", true},
		{"a:b:c", false},
		{":x", false},
		{"x:", false},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"has space", false},
	}
	for _, tt := range tests {
		err := ValidName(tt.name)
		c.False(tt.valid && err != nil, "ValidName(%q) = %v, want nil", tt.name, err)
		c.False(!tt.valid && err == nil, "ValidName(%q) = nil, want an error", tt.name)
	}
	// 64 chars is the inclusive upper bound the 65-char case implies.
	c.NoError(ValidName(strings.Repeat("a", 64)), "ValidName(64 chars)")
}

func TestPresetGroup(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("local:", Group("local:reviewer"), "Group(local:reviewer)")
	c.Eq("", Group("plain"), "Group(plain)")
}

func TestPresetValidate(t *testing.T) {
	c := assert.NewCollecting(t)
	fundi := func() Record {
		return Record{Name: "ok", Kind: KindFundi}
	}
	claude := func() Record {
		return Record{Name: "ok", Kind: KindClaude}
	}
	boolPtr := func(b bool) *bool { return &b }
	floatPtr := func(f float64) *float64 { return &f }
	intPtr := func(i int) *int { return &i }

	tests := []struct {
		subtest string
		r       Record
		field   string // the error must mention this name
	}{
		{"valid fundi record", fundi(), ""},
		{"empty name", func() Record { r := fundi(); r.Name = ""; return r }(), "name"},
		{"65 char name", func() Record { r := fundi(); r.Name = strings.Repeat("a", 65); return r }(), "name"},
		{"name with space", func() Record { r := fundi(); r.Name = "has space"; return r }(), "name"},
		{"unknown kind", func() Record { r := fundi(); r.Kind = "agent"; return r }(), "kind"},
		{"empty kind", func() Record { r := fundi(); r.Kind = ""; return r }(), "kind"},
		{"unknown thinking", func() Record { r := fundi(); r.Thinking = "maximum"; return r }(), "thinking"},
		{"claude kind with thinking", func() Record { r := claude(); r.Thinking = "low"; return r }(), "thinking"},
		// An EMPTY non-nil slice must still be rejected for claude -- this
		// fails if Validate checks len(r.Tools) > 0 instead of r.Tools != nil.
		{"claude kind with tools=[]", func() Record { r := claude(); r.Tools = []string{}; return r }(), "tools"},
		{"claude kind with tools list", func() Record { r := claude(); r.Tools = []string{"bash"}; return r }(), "tools"},
		{"claude kind with skills", func() Record { r := claude(); r.Skills = []string{}; return r }(), "skills"},
		{"claude kind with mcp_servers", func() Record { r := claude(); r.MCPServers = []string{}; return r }(), "mcp_servers"},
		{"claude kind with context_files", func() Record { r := claude(); r.ContextFiles = boolPtr(true); return r }(), "context_files"},
		{"claude kind with system_prompt", func() Record { r := claude(); r.SystemPrompt = "be terse"; return r }(), "system_prompt"},
		{"negative max_cost", func() Record { r := fundi(); r.MaxCost = floatPtr(-0.01); return r }(), "max_cost"},
		{"negative max_depth", func() Record { r := fundi(); r.MaxDepth = intPtr(-1); return r }(), "max_depth"},
		{"negative max_children", func() Record { r := fundi(); r.MaxChildren = intPtr(-1); return r }(), "max_children"},
		{"empty labels key", func() Record { r := fundi(); r.Labels = map[string]string{"": "v"}; return r }(), "labels"},
		{"labels key in reserved rafiki/ namespace", func() Record { r := fundi(); r.Labels = map[string]string{"rafiki/x": "v"}; return r }(), "labels"},
		// The spawn path reserves these too (validateUserLabelKeys,
		// cmd/rafikid/labels.go); a preset saved with one would be unspawnable.
		{"labels key reserved: owner", func() Record { r := fundi(); r.Labels = map[string]string{"owner": "x"}; return r }(), "labels"},
		{"labels key in reserved fundi/ namespace", func() Record { r := fundi(); r.Labels = map[string]string{"fundi/x": "v"}; return r }(), "labels"},
		{"labels key with a space", func() Record { r := fundi(); r.Labels = map[string]string{"has space": "v"}; return r }(), "labels"},
		// thinking must be one of the levels pkg/fundi's thinkingBudgets can
		// actually honour; "minimal" is not one of them.
		{"minimal thinking", func() Record { r := fundi(); r.Thinking = "minimal"; return r }(), "thinking"},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			c := assert.NewAborting(t)
			err := Validate(tt.r)
			if tt.field == "" {
				c.NoError(err, "Validate(%+v) = %v, want nil", tt.r, err)
				return
			}
			c.Error(err, "Validate(%+v) = nil, want an error mentioning %q", tt.r, tt.field)
			c.StrContains(err.Error(), tt.field, "Validate(%+v) = %v, want the error to mention", tt.r, err)
		})
	}

	// A script preset honours only executor, labels, budgets, description
	// and append_system_prompt; every LLM-shaping knob is refused, and the
	// empty-[]string tri-state must be rejected too (nil-check, not len).
	script := func() Record {
		return Record{Name: "ok", Kind: KindScript}
	}
	for _, bad := range []struct {
		subtest string
		r       Record
		field   string
	}{
		{"script kind with thinking", func() Record { r := script(); r.Thinking = "low"; return r }(), "thinking"},
		{"script kind with model", func() Record { r := script(); r.Model = "anthropic/x"; return r }(), "model"},
		{"script kind with provider", func() Record { r := script(); r.Provider = "anthropic"; return r }(), "provider"},
		{"script kind with tools=[]", func() Record { r := script(); r.Tools = []string{}; return r }(), "tools"},
		{"script kind with skills", func() Record { r := script(); r.Skills = []string{}; return r }(), "skills"},
		{"script kind with mcp_servers", func() Record { r := script(); r.MCPServers = []string{}; return r }(), "mcp_servers"},
		{"script kind with context_files", func() Record { r := script(); r.ContextFiles = boolPtr(true); return r }(), "context_files"},
		{"script kind with system_prompt", func() Record { r := script(); r.SystemPrompt = "be terse"; return r }(), "system_prompt"},
	} {
		t.Run(bad.subtest, func(t *testing.T) {
			err := Validate(bad.r)
			assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), bad.field), "Validate(%+v) = %v, want an error mentioning %q", bad.r, err, bad.field)
		})
	}
	// A clean script preset — executor/labels/budgets only — validates.
	okScript := script()
	okScript.Executor = "home-lab"
	okScript.MaxCost = floatPtr(0)
	okScript.MaxDepth = intPtr(1)
	okScript.AppendSystemPrompt = "extra"
	c.NoError(Validate(okScript), "Validate(clean script preset)")

	// A claude preset may carry the knobs claude actually honours.
	ok := claude()
	ok.Model = "claude-x"
	ok.AppendSystemPrompt = "extra"
	ok.MaxCost = floatPtr(0)
	c.NoError(Validate(ok), "Validate(claude with model/append_system_prompt/max_cost=0)")

	// The five thinking levels Validate accepts are exactly the runtime's
	// (pkg/fundi's thinkingBudgets); a label key may use the full allowed
	// charset. Both are the saveable-but-unspawnable guards.
	for _, level := range []string{"off", "low", "medium", "high", "xhigh"} {
		r := fundi()
		r.Thinking = level
		err := Validate(r)
		c.NoError(err, "Validate(thinking=%q) = %v, want nil", level, err)
	}
	good := fundi()
	good.Labels = map[string]string{"env.prod/x_1-y": "v"}
	c.NoError(Validate(good), "Validate(labels with full allowed charset)")
}

func TestPresetSpecRoundTrip(t *testing.T) {
	tests := []struct {
		subtest  string
		json     string
		wantNil  bool // Record().Tools == nil
		wantJSON string
	}{
		{"empty array means none", `{"name":"a","tools":[]}`, false, `"tools":[]`},
		{"absent means unset", `{"name":"a"}`, true, ""},
		{"null means unset", `{"name":"a","tools":null}`, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			c := assert.NewAborting(t)
			spec, err := ParseSpec([]byte(tt.json))
			c.NoError(err, "ParseSpec(%s)", tt.json)
			r := spec.Record()
			c.False(tt.wantNil && r.Tools != nil, "Tools = %#v, want nil (the kind default)", r.Tools)
			if !tt.wantNil {
				c.NotNil(r.Tools, "Tools = nil, want non-nil")
				c.Empty(r.Tools, "Tools")
			}
			out, err := json.Marshal(SpecOf(r))
			c.NoError(err, "marshal SpecOf")
			if tt.wantJSON == "" {
				c.NotStrContains(string(out), `"tools"`, "SpecOf re-marshal = %s, want no tools key at all", out)
				return
			}
			c.StrContains(string(out), tt.wantJSON, "SpecOf re-marshal = %s, want it to contain", out)
		})
	}
}

func TestPresetParseSpecRejectsUnknownField(t *testing.T) {
	_, err := ParseSpec([]byte(`{"name":"a","tool":["x"]}`))
	assert.NewAborting(t).Error(err, "ParseSpec with unknown field \"tool\" = nil error, want DisallowUnknownFields rejection")
}
