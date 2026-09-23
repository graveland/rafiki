// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPresetValidName(t *testing.T) {
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
		if tt.valid && err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", tt.name, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", tt.name)
		}
	}
	// 64 chars is the inclusive upper bound the 65-char case implies.
	if err := ValidName(strings.Repeat("a", 64)); err != nil {
		t.Errorf("ValidName(64 chars) = %v, want nil", err)
	}
}

func TestPresetGroup(t *testing.T) {
	if got := Group("local:reviewer"); got != "local:" {
		t.Errorf("Group(local:reviewer) = %q, want %q", got, "local:")
	}
	if got := Group("plain"); got != "" {
		t.Errorf("Group(plain) = %q, want %q", got, "")
	}
}

func TestPresetValidate(t *testing.T) {
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
			err := Validate(tt.r)
			if tt.field == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tt.r, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error mentioning %q", tt.r, tt.field)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Validate(%+v) = %v, want the error to mention %q", tt.r, err, tt.field)
			}
		})
	}

	// A claude preset may carry the knobs claude actually honours.
	ok := claude()
	ok.Model = "claude-x"
	ok.AppendSystemPrompt = "extra"
	ok.MaxCost = floatPtr(0)
	if err := Validate(ok); err != nil {
		t.Errorf("Validate(claude with model/append_system_prompt/max_cost=0) = %v, want nil", err)
	}

	// The five thinking levels Validate accepts are exactly the runtime's
	// (pkg/fundi's thinkingBudgets); a label key may use the full allowed
	// charset. Both are the saveable-but-unspawnable guards.
	for _, level := range []string{"off", "low", "medium", "high", "xhigh"} {
		r := fundi()
		r.Thinking = level
		if err := Validate(r); err != nil {
			t.Errorf("Validate(thinking=%q) = %v, want nil", level, err)
		}
	}
	good := fundi()
	good.Labels = map[string]string{"env.prod/x_1-y": "v"}
	if err := Validate(good); err != nil {
		t.Errorf("Validate(labels with full allowed charset) = %v, want nil", err)
	}
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
			spec, err := ParseSpec([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseSpec(%s): %v", tt.json, err)
			}
			r := spec.Record()
			if tt.wantNil && r.Tools != nil {
				t.Fatalf("Tools = %#v, want nil (the kind default)", r.Tools)
			}
			if !tt.wantNil {
				if r.Tools == nil {
					t.Fatal("Tools = nil, want non-nil")
				}
				if len(r.Tools) != 0 {
					t.Fatalf("Tools = %#v, want non-nil and empty", r.Tools)
				}
			}
			out, err := json.Marshal(SpecOf(r))
			if err != nil {
				t.Fatalf("marshal SpecOf: %v", err)
			}
			if tt.wantJSON == "" {
				if strings.Contains(string(out), `"tools"`) {
					t.Fatalf("SpecOf re-marshal = %s, want no tools key at all", out)
				}
				return
			}
			if !strings.Contains(string(out), tt.wantJSON) {
				t.Fatalf("SpecOf re-marshal = %s, want it to contain %s", out, tt.wantJSON)
			}
		})
	}
}

func TestPresetParseSpecRejectsUnknownField(t *testing.T) {
	if _, err := ParseSpec([]byte(`{"name":"a","tool":["x"]}`)); err == nil {
		t.Fatal("ParseSpec with unknown field \"tool\" = nil error, want DisallowUnknownFields rejection")
	}
}
