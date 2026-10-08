// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"reflect"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestParseModelSplitsBase pins the one syntax the whole routing-spec feature
// hangs on: base id outside, spec inside the brackets.
func TestParseModelSplitsBase(t *testing.T) {
	base, spec, err := ParseModel("openrouter/z-ai/glm-5.3-flash[sort=price,nodata]")
	if err != nil {
		t.Fatalf("ParseModel: %v", err)
	}
	if base != "openrouter/z-ai/glm-5.3-flash" {
		t.Errorf("base = %q, want the id without its bracketed spec", base)
	}
	want := Spec{Sort: SortPrice, NoData: true}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("spec = %+v, want %+v", spec, want)
	}
}

// TestParseModelNoBrackets pins that a plain model id passes through with the
// zero Spec — the overwhelmingly common case must parse untouched.
func TestParseModelNoBrackets(t *testing.T) {
	const id = "openrouter/z-ai/glm-5.3-flash"
	base, spec, err := ParseModel(id)
	if err != nil {
		t.Fatalf("ParseModel(%q): %v", id, err)
	}
	if base != id {
		t.Errorf("base = %q, want unchanged %q", base, id)
	}
	if !spec.IsZero() {
		t.Errorf("spec = %+v, want the zero Spec", spec)
	}
}

// TestParseModelMalformed pins every bracket-syntax guard: unclosed, trailing
// text, empty brackets, a second bracket, an empty base. Deleting any guard
// from ParseModel fails its row.
func TestParseModelMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"unclosed", "openrouter/z-ai/glm-5.3-flash[sort=price"},
		{"trailing text", "openrouter/z-ai/glm-5.3-flash[sort=price]x"},
		{"empty brackets", "openrouter/z-ai/glm-5.3-flash[]"},
		{"double bracket", "openrouter/z-ai/glm-5.3-flash[only=a[b]"},
		{"bracket after close", "openrouter/z-ai/glm-5.3-flash[only=a]b]"},
		{"empty base", "[sort=price]"},
		{"empty model", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, spec, err := ParseModel(tc.in)
			if err == nil {
				t.Fatalf("ParseModel(%q) = (%q, %+v), want an error", tc.in, base, spec)
			}
			if base != "" || !spec.IsZero() {
				t.Errorf("on error ParseModel must return zero values, got (%q, %+v)", base, spec)
			}
		})
	}
}

// TestParseSpecRejects pins the item grammar guards: unknown key, unknown sort
// value, unknown quantization, a flag given a value, a valued key without one,
// a repeated key, an empty item, and a floor mixed into a list. Every error
// names the offending item — a spec is never half-ignored.
func TestParseSpecRejects(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantItem string // substring the error must name; "" = don't check
	}{
		{"unknown key", "foo=bar", "foo=bar"},
		{"unknown sort", "sort=fast", "sort=fast"},
		{"unknown quant", "quant=fp99", "quant=fp99"},
		{"flag given a value", "nodata=1", "nodata=1"},
		{"valued key without a value", "sort", "sort"},
		{"valued key with empty value", "sort=", "sort="},
		{"repeated key", "sort=price,sort=latency", "sort=latency"},
		{"repeated flag", "nodata,nodata", ""},
		{"empty item", "sort=price,,nodata", ""},
		{"floor combined in a list", "quant=fp8+|bf16", "quant=fp8+|bf16"},
		{"empty slug", "only=gmicloud|", "only=gmicloud|"},
		{"empty quant element", "quant=fp8||bf16", "quant=fp8||bf16"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseSpec(tc.in)
			if err == nil {
				t.Fatalf("ParseSpec(%q) = %+v, want an error", tc.in, spec)
			}
			if tc.wantItem != "" && !strings.Contains(err.Error(), tc.wantItem) {
				t.Errorf("error %q does not name the offending item %q", err, tc.wantItem)
			}
		})
	}
}

// TestSpecParseRejects carries the pinned TestParseSpecRejects table under the
// -run patterns used by verify commands, which match only TestSpec*/TestParse
// Model*/TestQuant* prefixes — the pinned name itself contains neither
// ("TestParse" + "SpecRejects"), so without this shim the verify command would
// silently skip it (see CLAUDE.md's -run note).
func TestSpecParseRejects(t *testing.T) {
	t.Run("TestParseSpecRejects", TestParseSpecRejects)
}

// TestSpecParseEmpty pins the policy-store contract: ParseSpec("") is the zero
// Spec, not an error — a policy row stores ” to mean "no spec".
func TestSpecParseEmpty(t *testing.T) {
	spec, err := ParseSpec("")
	if err != nil {
		t.Fatalf("ParseSpec(\"\") = error %v, want the zero Spec", err)
	}
	if !spec.IsZero() {
		t.Errorf("ParseSpec(\"\") = %+v, want the zero Spec", spec)
	}
}

// TestSpecParseClearedKeys pins that quant=, prefer=, only= parse to non-nil
// empty slices — the "explicitly cleared" signal that Merge keeps.
func TestSpecParseClearedKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		got  func(Spec) []string
	}{
		{"quant cleared", "quant=", func(s Spec) []string { return s.Quant }},
		{"prefer cleared", "prefer=", func(s Spec) []string { return s.Prefer }},
		{"only cleared", "only=", func(s Spec) []string { return s.Only }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseSpec(tc.in)
			if err != nil {
				t.Fatalf("ParseSpec(%q) = %v, want success", tc.in, err)
			}
			got := tc.got(spec)
			if got == nil {
				t.Errorf("%s: slice is nil, want non-nil empty", tc.in)
			}
			if len(got) != 0 {
				t.Errorf("%s: slice = %v, want empty", tc.in, got)
			}
		})
	}
	// sort= remains an error.
	if _, err := ParseSpec("sort="); err == nil {
		t.Error("ParseSpec(\"sort=\") must error: sort needs a value")
	}
}

// TestQuantFloorTiers pins the quantization ladder: a floor admits its whole
// tier and everything above, in tier order — fp8+ = int8 fp8 mxfp8 fp16 bf16
// fp32 exactly, fp4+ starts with the T1 names, fp6+ excludes T1.
func TestQuantFloorTiers(t *testing.T) {
	t.Run("fp8+", func(t *testing.T) {
		spec := mustParseSpec(t, "quant=fp8+")
		want := []string{"int8", "fp8", "mxfp8", "fp16", "bf16", "fp32"}
		got := spec.Quantizations()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("fp8+ expands to %v, want %v", got, want)
		}
	})
	t.Run("fp4+ starts at T1", func(t *testing.T) {
		spec := mustParseSpec(t, "quant=fp4+")
		got := spec.Quantizations()
		t1 := []string{"int4", "fp4", "mxfp4", "nvfp4"}
		if len(got) < len(t1) || !reflect.DeepEqual(got[:len(t1)], t1) {
			t.Errorf("fp4+ starts with %v, got %v", t1, got)
		}
		if !reflect.DeepEqual(got[len(got)-1:], []string{"fp32"}) {
			t.Errorf("fp4+ must end at the top of the ladder (fp32), got %v", got)
		}
	})
	t.Run("fp6+ excludes T1", func(t *testing.T) {
		spec := mustParseSpec(t, "quant=fp6+")
		got := spec.Quantizations()
		if len(got) < 1 || got[0] != "fp6" {
			t.Errorf("fp6+ must start with fp6 itself, got %v", got)
		}
		for _, name := range []string{"int4", "fp4", "mxfp4", "nvfp4"} {
			for _, g := range got {
				if g == name {
					t.Errorf("fp6+ must exclude the T1 quantization %q, got %v", name, got)
				}
			}
		}
	})
}

// TestQuantFloorExcludesUnknown pins that no floor admits an unlabelled host:
// no tier's expansion contains "unknown", and unknown+ is a parse error — while
// a list may still name unknown explicitly.
func TestQuantFloorExcludesUnknown(t *testing.T) {
	for _, tier := range quantTiers {
		for _, name := range tier {
			spec := mustParseSpec(t, "quant="+name+"+")
			for _, got := range spec.Quantizations() {
				if got == "unknown" {
					t.Errorf("floor %q+ expands to %v, which contains \"unknown\"", name, spec.Quantizations())
				}
			}
		}
	}
	if _, err := ParseSpec("quant=unknown+"); err == nil {
		t.Error("ParseSpec(\"quant=unknown+\") must error: unknown is on no tier")
	}
	if spec, err := ParseSpec("quant=fp8|unknown"); err != nil {
		t.Errorf("ParseSpec(\"quant=fp8|unknown\") = %v, want a list may name unknown explicitly", err)
	} else if got := spec.Quantizations(); !reflect.DeepEqual(got, []string{"fp8", "unknown"}) {
		t.Errorf("list kept as written, got %v", got)
	}
}

// TestSpecMergePerKey pins per-key resolution: the receiver (more specific)
// wins each key it sets, the lower level fills the gaps, and an explicit
// balanced beats a lower price.
func TestSpecMergePerKey(t *testing.T) {
	receiver := Spec{Sort: SortBalanced, Quant: []string{"fp8"}}
	lower := Spec{Sort: SortPrice, Quant: []string{"fp4"}, Only: []string{"gmicloud"}, NoData: true}
	merged := receiver.Merge(lower)
	want := Spec{Sort: SortBalanced, Quant: []string{"fp8"}, Only: []string{"gmicloud"}, NoData: true}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("merged = %+v, want %+v", merged, want)
	}
	// The zero receiver inherits the lower level wholesale.
	inherited := Spec{}.Merge(lower)
	if !reflect.DeepEqual(inherited, lower) {
		t.Errorf("zero receiver merged with %+v = %+v, want the lower spec", lower, inherited)
	}
	// Receiver and lower each hold a key the other lacks.
	gap := Spec{Only: []string{"novita"}}.Merge(Spec{Quant: []string{"fp16"}})
	wantGap := Spec{Only: []string{"novita"}, Quant: []string{"fp16"}}
	if !reflect.DeepEqual(gap, wantGap) {
		t.Errorf("merged = %+v, want %+v", gap, wantGap)
	}
}

// TestSpecMergeDataFlagsMonotone pins that NoData and ZDR are monotone: set at
// the lower level they survive a receiver without them — no more specific level
// can clear them.
func TestSpecMergeDataFlagsMonotone(t *testing.T) {
	lower := Spec{NoData: true, ZDR: true}
	merged := Spec{Sort: SortPrice}.Merge(lower)
	if !merged.NoData || !merged.ZDR {
		t.Errorf("merged = %+v, want lower NoData/ZDR to survive a receiver without them", merged)
	}
	if merged.String() != "sort=price,nodata,zdr" {
		t.Errorf("String() = %q, want \"sort=price,nodata,zdr\"", merged.String())
	}
}

// TestSpecMergeClearedQuant pins that a receiver with an explicitly cleared
// quant (non-nil empty slice) does not inherit the lower level's quant — the
// nil-vs-empty distinction Merge already reads.
func TestSpecMergeClearedQuant(t *testing.T) {
	receiver := Spec{Quant: []string{}}
	lower := Spec{Quant: []string{"fp8+"}}
	merged := receiver.Merge(lower)
	if merged.Quant == nil {
		t.Error("merged.Quant is nil, want non-nil empty (the cleared signal)")
	}
	if len(merged.Quant) != 0 {
		t.Errorf("merged.Quant = %v, want empty (cleared)", merged.Quant)
	}
	// And the same for Prefer and Only.
	prefMerged := Spec{Prefer: []string{}}.Merge(Spec{Prefer: []string{"fireworks"}})
	if prefMerged.Prefer == nil || len(prefMerged.Prefer) != 0 {
		t.Errorf("cleared Prefer after Merge = %v, want non-nil empty", prefMerged.Prefer)
	}
	onlyMerged := Spec{Only: []string{}}.Merge(Spec{Only: []string{"gmicloud"}})
	if onlyMerged.Only == nil || len(onlyMerged.Only) != 0 {
		t.Errorf("cleared Only after Merge = %v, want non-nil empty", onlyMerged.Only)
	}
	// A nil (unset) receiver still inherits as before.
	inherited := Spec{}.Merge(lower)
	if inherited.Quant == nil || len(inherited.Quant) != 1 || inherited.Quant[0] != "fp8+" {
		t.Errorf("nil receiver should inherit; got %v", inherited.Quant)
	}
}

// TestSpecStringRoundTrip pins the canonical form: fixed key order
// (sort, quant, only, nodata, zdr), "|" lists, a floor kept as written,
// sort=balanced emitted, unset keys omitted — and ParseSpec(String()) equal.
func TestSpecStringRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"zero", Spec{}, ""},
		{"sort price", Spec{Sort: SortPrice}, "sort=price"},
		{"sort balanced is a decision", Spec{Sort: SortBalanced}, "sort=balanced"},
		{"floor kept as written", Spec{Quant: []string{"fp8+"}}, "quant=fp8+"},
		{"quant list", Spec{Quant: []string{"fp8", "bf16"}}, "quant=fp8|bf16"},
		{"quant cleared", Spec{Quant: []string{}}, "quant="},
		{"prefer list", Spec{Prefer: []string{"fireworks", "together"}}, "prefer=fireworks|together"},
		{"prefer cleared", Spec{Prefer: []string{}}, "prefer="},
		{"only list", Spec{Only: []string{"gmicloud", "novita"}}, "only=gmicloud|novita"},
		{"only cleared", Spec{Only: []string{}}, "only="},
		{"nodata", Spec{NoData: true}, "nodata"},
		{"zdr", Spec{ZDR: true}, "zdr"},
		{"everything", Spec{Sort: SortLatency, Quant: []string{"fp8+"}, Only: []string{"gmicloud"}, NoData: true, ZDR: true},
			"sort=latency,quant=fp8+,only=gmicloud,nodata,zdr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			back, err := ParseSpec(tc.spec.String())
			if err != nil {
				t.Fatalf("ParseSpec(%q) = %v, want round-trip", tc.spec.String(), err)
			}
			if !reflect.DeepEqual(back, tc.spec) {
				t.Errorf("round trip = %+v, want %+v", back, tc.spec)
			}
		})
	}
}

// TestSpecPrefsOnlyBypassesIgnore pins decision 5: a spec only is an explicit
// routing decision that bypasses bans and ejections — the guard's ignore is
// dropped — while without a spec only the guard's ignore passes through and
// the pin fills Only.
func TestSpecPrefsOnlyBypassesIgnore(t *testing.T) {
	t.Run("spec only drops ignore", func(t *testing.T) {
		prefs, ok := Spec{Only: []string{"gmicloud"}}.Prefs([]string{"pinned"}, []string{"banned"})
		if !ok {
			t.Fatal("prefs not sent")
		}
		if !reflect.DeepEqual(prefs.Only, []string{"gmicloud"}) {
			t.Errorf("Only = %v, want the spec's only", prefs.Only)
		}
		if prefs.Ignore != nil {
			t.Errorf("Ignore = %v, want nil: a spec only bypasses bans and ejections", prefs.Ignore)
		}
	})
	t.Run("no spec only passes ignore through", func(t *testing.T) {
		prefs, ok := Spec{}.Prefs(nil, []string{"banned"})
		if !ok {
			t.Fatal("prefs not sent")
		}
		if !reflect.DeepEqual(prefs.Ignore, []string{"banned"}) {
			t.Errorf("Ignore = %v, want the guard's ignore passed through", prefs.Ignore)
		}
		if prefs.Only != nil {
			t.Errorf("Only = %v, want nil with no pin", prefs.Only)
		}
	})
	t.Run("no spec only with a pin", func(t *testing.T) {
		prefs, ok := Spec{}.Prefs([]string{"pinned"}, nil)
		if !ok {
			t.Fatal("prefs not sent")
		}
		if !reflect.DeepEqual(prefs.Only, []string{"pinned"}) {
			t.Errorf("Only = %v, want the pin", prefs.Only)
		}
	})
}

// TestSpecPrefsKeepsDataPolicyWithOnly pins that data policy is the one
// boundary an only never bypasses: Only + zdr + nodata puts all three on the
// wire prefs (OpenRouter applies them jointly).
func TestSpecPrefsKeepsDataPolicyWithOnly(t *testing.T) {
	spec := Spec{Only: []string{"gmicloud"}, NoData: true, ZDR: true}
	prefs, ok := spec.Prefs(nil, []string{"banned"})
	if !ok {
		t.Fatal("prefs not sent")
	}
	if prefs.DataCollection != "deny" {
		t.Errorf("DataCollection = %q, want \"deny\" alongside Only", prefs.DataCollection)
	}
	if !prefs.ZDR {
		t.Error("ZDR = false, want true alongside Only")
	}
	if prefs.Ignore != nil {
		t.Errorf("Ignore = %v, want nil (spec only)", prefs.Ignore)
	}
}

// TestSpecPrefsBalancedSendsNoSort pins that sort=balanced sends no sort: it
// is a decision to omit the field, not a wire value.
func TestSpecPrefsBalancedSendsNoSort(t *testing.T) {
	prefs, ok := Spec{Sort: SortBalanced}.Prefs(nil, nil)
	if ok {
		t.Errorf("(prefs, %v), want false: a balanced-only spec sends nothing", ok)
	}
	if prefs.Sort != "" {
		t.Errorf("Sort = %q, want omitted", prefs.Sort)
	}
}

// TestSpecPrefsZeroSendsNothing pins the no-provider-object case: a zero spec
// with no pin and no guard ignore sends nothing (second result false) — the
// caller must skip the provider field entirely.
func TestSpecPrefsZeroSendsNothing(t *testing.T) {
	prefs, ok := Spec{}.Prefs(nil, nil)
	if ok {
		t.Errorf("(prefs, %v), want false for the zero spec, nil pin, nil ignore", ok)
	}
	if !reflect.DeepEqual(prefs, ProviderPrefs{}) {
		t.Errorf("prefs = %+v, want the zero ProviderPrefs", prefs)
	}
	if prefs.Quantizations != nil {
		t.Errorf("Quantizations = %v, want nil for an unset Quant", prefs.Quantizations)
	}
}

// mustParseSpec parses s, failing the test on any error.
func mustParseSpec(t *testing.T, s string) Spec {
	t.Helper()
	spec, err := ParseSpec(s)
	if err != nil {
		t.Fatalf("ParseSpec(%q) = %v, want success", s, err)
	}
	return spec
}

// TestPreferParsesAndRoundTrips pins that prefer joins the valued keys: parsed
// into Spec.Prefer, rendered in its canonical slot (after quant, before only),
// and equal after a String/Parse round trip.
func TestPreferParsesAndRoundTrips(t *testing.T) {
	ck := assert.NewAborting(t)
	spec, err := ParseSpec("sort=price,quant=fp8+,prefer=fireworks|together")
	ck.NoError(err)
	ck.EqDeep([]string{"fireworks", "together"}, spec.Prefer, "Prefer")
	ck.Eq("sort=price,quant=fp8+,prefer=fireworks|together", spec.String(), "String()")
	back, err := ParseSpec(spec.String())
	ck.NoError(err)
	ck.EqDeep(spec, back, "ParseSpec(String()) round trip")
}

// TestPreferRejectsEmptySlugAndRepeat pins prefer's item grammar: an empty slug
// (prefer=a||b, prefer=|a) and a missing value are errors naming the item, and a
// repeated key is refused like every other valued key.
func TestPreferRejectsEmptySlugAndRepeat(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, in := range []string{"prefer=a||b", "prefer", "prefer=a,prefer=b"} {
		_, err := ParseSpec(in)
		ck.Error(err, "ParseSpec(%q) must error", in)
	}
	_, err := ParseSpec("prefer=a||b")
	ck.True(strings.Contains(err.Error(), "empty provider slug"),
		"error %q must name the empty provider slug", err)
}

// TestPreferMergeReceiverWins pins prefer's per-key merge: the receiver's
// non-nil Prefer wins, an unset receiver inherits the lower level, and a
// non-nil EMPTY receiver stays empty (copyList's nil-vs-empty rule).
func TestPreferMergeReceiverWins(t *testing.T) {
	ck := assert.NewAborting(t)
	ck.EqDeep([]string{"a"}, Spec{Prefer: []string{"a"}}.Merge(Spec{Prefer: []string{"b"}}).Prefer,
		"receiver prefer wins")
	ck.EqDeep([]string{"b"}, Spec{}.Merge(Spec{Prefer: []string{"b"}}).Prefer,
		"unset receiver inherits the lower prefer")
	empty := Spec{Prefer: []string{}}.Merge(Spec{Prefer: []string{"b"}}).Prefer
	ck.True(empty != nil, "non-nil empty receiver prefer must stay non-nil")
	ck.Eq(0, len(empty), "non-nil empty receiver prefer must stay empty")
}

// TestPreferIsNotZero pins that a spec carrying only Prefer is not the zero
// Spec — IsZero must account for it.
func TestPreferIsNotZero(t *testing.T) {
	assert.NewAborting(t).False(Spec{Prefer: []string{"a"}}.IsZero(),
		"Spec{Prefer: [a]}.IsZero() must be false")
}

// TestPreferPrefsEmitsOrderAndKeepsIgnore pins that prefer becomes the wire
// Order ALWAYS and never drops bans — only a spec Only bypasses ignore. With a
// spec Only, Ignore is dropped but Order still rides.
func TestPreferPrefsEmitsOrderAndKeepsIgnore(t *testing.T) {
	ck := assert.NewAborting(t)
	prefs, ok := Spec{Prefer: []string{"fireworks"}}.Prefs(nil, []string{"openinference"})
	ck.True(ok, "a prefer-only spec must send")
	ck.EqDeep([]string{"fireworks"}, prefs.Order, "Order")
	ck.EqDeep([]string{"openinference"}, prefs.Ignore, "prefer must NOT drop bans")

	contrast, ok := Spec{Only: []string{"x"}, Prefer: []string{"fireworks"}}.Prefs(nil, []string{"openinference"})
	ck.True(ok, "only+prefer must send")
	ck.EqDeep([]string{"x"}, contrast.Only, "Only")
	ck.Eq(0, len(contrast.Ignore), "a spec only drops ignore")
	ck.EqDeep([]string{"fireworks"}, contrast.Order, "Order rides even under an only")
}
