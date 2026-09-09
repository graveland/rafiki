package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/modelquery"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestModelCompletionServesFromCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("models-fundi", completionEndpointKey(nil),
		[]string{"anthropic/claude-opus-5", "openai/gpt-4o"})

	got := completeModel(nil, "fundi", "anthropic/")
	if len(got) != 1 || got[0] != "anthropic/claude-opus-5" {
		t.Errorf("got %v, want the one anthropic id", got)
	}
}

// The two kinds have different model universes, so their caches must not share
// a file — a claude completion served from the fundi cache offers OpenRouter
// ids that Claude Code cannot resolve.
func TestModelCompletionCacheIsKeyedByKind(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("models-fundi", completionEndpointKey(nil), []string{"openai/gpt-4o"})

	if got := completeModel(nil, "claude", ""); len(got) != 0 {
		t.Errorf("claude completion read the fundi cache: %v", got)
	}
}

func TestModelCompletionOnAnUnreachableDaemonIsEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://127.0.0.1:1")
	t.Setenv("RAFIKI_TOKEN", "t")

	if got := completeModel(nil, "fundi", ""); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// ─── fixtures ──────────────────────────────────────────────────────────────

func fp64(v float64) *float64 { return &v }
func ip32(v int32) *int32     { return &v }
func ip64(v int64) *int64     { return &v }

func modelTestRows() []*rafikiv1.ModelRow {
	zero := int32(0)
	return []*rafikiv1.ModelRow{
		{Id: "anthropic/claude-opus-5", Provider: "anthropic", Source: "builtin", ContextWindow: &zero},
		// Model set: the table's MODEL cell shows the bare model part, not
		// the triple-prefixed id.
		{Id: "openrouter/openai/gpt-4o", Model: "openai/gpt-4o", Provider: "openrouter", Source: "openrouter"},
		{Id: "vmlx/qwen", Provider: "vmlx", Source: "local"},
	}
}

// ─── renderModelRows ───────────────────────────────────────────────────────

// renderedHeaders returns the header names of a pkg/table render in column
// order: line 0 is the top border, line 1 the header row, cells separated by
// the border rune.
func renderedHeaders(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("table too short to carry a header row:\n%s", out)
	}
	var names []string
	for i, c := range strings.Split(lines[1], "│") {
		c = strings.TrimSpace(c)
		if c == "" && (i == 0 || i == strings.Count(lines[1], "│")) {
			continue // the empty cells the outer edges split off
		}
		names = append(names, c)
	}
	return names
}

// The JSON arm is what `rafiki models | jq` reads. Optional fields that the
// daemon did not report must be ABSENT, not zero: a zero context window would
// sort every local model to the top of a cheapest-first filter, and a zero
// price reads as free.
func TestRenderModelRowsJSONOmitsUnreportedOptionals(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), modelsQuery{}, outputJSON, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}

	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON output: %v\nraw:\n%s", err, buf.String())
	}
	if len(out.Models) != 3 {
		t.Fatalf("got %d rows, want 3; output:\n%s", len(out.Models), buf.String())
	}

	byID := make(map[string]map[string]any, len(out.Models))
	for _, m := range out.Models {
		byID[m["id"].(string)] = m
	}

	// A set zero is a REAL value and must print as 0.
	if v, ok := byID["anthropic/claude-opus-5"]["context_window"]; !ok || v != float64(0) {
		t.Errorf("claude context_window = %v (%v), want an explicit 0", v, byID["anthropic/claude-opus-5"])
	}
	// Unreported optionals must be absent, not zero.
	for _, key := range []string{"context_window", "prompt_usd", "completion_usd", "input_modalities"} {
		if _, present := byID["vmlx/qwen"][key]; present {
			t.Errorf("local model vmlx/qwen carries %s; an unreported optional must be absent, not zero", key)
		}
	}
}

// The --source filter is a display filter and must apply in the JSON arm too —
// a jq pipeline filtering on source gets no help from the table renderer.
func TestRenderModelRowsJSONAppliesSourceFilter(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), modelsQuery{source: "builtin"}, outputJSON, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	if strings.Contains(buf.String(), "openrouter/openai/gpt-4o") {
		t.Errorf("--source builtin leaked a non-builtin row; output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "anthropic/claude-opus-5") {
		t.Errorf("--source builtin dropped the builtin row; output:\n%s", buf.String())
	}
}

func TestRenderModelRowsTable(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), modelsQuery{}, outputTable, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	out := buf.String()

	// Every default column, in order.
	want := []string{"MODEL", "SOURCE", "CTX", "IN $", "OUT $", "CODE", "AGENTIC", "V"}
	if got := renderedHeaders(t, out); !slices.Equal(got, want) {
		t.Errorf("headers = %v, want %v; output:\n%s", got, want, out)
	}
	// MODEL falls back to the full id when the row carries no display model,
	// and shows the bare model part when it does — killing the
	// triple-openrouter label.
	if !strings.Contains(out, "anthropic/claude-opus-5") {
		t.Errorf("table missing the fallback id; output:\n%s", out)
	}
	if !strings.Contains(out, "openai/gpt-4o") {
		t.Errorf("table missing the bare model cell; output:\n%s", out)
	}
	if strings.Contains(out, "openrouter/openai/gpt-4o") {
		t.Errorf("MODEL cell leaked the full id although Model is set; output:\n%s", out)
	}
}

// TestModelsJSONLUnwrapped: -J is one compact ModelRow per line, no envelope.
func TestModelsJSONLUnwrapped(t *testing.T) {
	var buf bytes.Buffer
	rows := modelTestRows()
	if err := renderModelRows(&buf, rows, modelsQuery{}, outputJSONL, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	if strings.Contains(buf.String(), `"models"`) {
		t.Errorf("JSONL must be unwrapped, no envelope; output:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), ": ") {
		t.Errorf("JSONL must be compact (one object per line); output:\n%s", buf.String())
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("got %d lines for %d rows:\n%s", len(lines), len(rows), buf.String())
	}
	var first rafikiv1.ModelRow
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first line: %v\nline:\n%s", err, lines[0])
	}
	if first.GetId() != rows[0].GetId() {
		t.Errorf("first line id = %q, want %q", first.GetId(), rows[0].GetId())
	}
}

// ─── filters: flags → modelquery ───────────────────────────────────────────

// TestModelsFlagsMapToModelquery maps each filter flag onto a fixture row and
// asserts admit/reject. The absence rule is the spine of the table: the
// uncataloged row — every locally-served model — passes EVERY bound but fails
// --scored, and only a definitive catalog claim filters it under
// --tools/--vision.
func TestModelsFlagsMapToModelquery(t *testing.T) {
	cataloged := &rafikiv1.ModelRow{
		Id: "openrouter/anthropic/claude-opus-5", Model: "anthropic/claude-opus-5",
		Name: "Claude Opus 5", Source: "openrouter",
		ContextWindow: ip32(200_000), MaxCompletionTokens: ip32(64_000),
		PromptUsd: fp64(0.000003), CompletionUsd: fp64(0.000015), CacheReadUsd: fp64(0.0000003),
		Created:             ip64(1735689600),
		SupportedParameters: []string{"tools", "reasoning"},
		InputModalities:     []string{"text", "image"},
		IntelligenceIndex:   fp64(59.5), CodingIndex: fp64(40.2), AgenticIndex: fp64(55.1),
	}
	free := &rafikiv1.ModelRow{ // priced, AT zero: free is a KNOWN value, not an absent one
		Id: "openrouter/free/model", Source: "openrouter",
		PromptUsd: fp64(0), CompletionUsd: fp64(0),
	}
	textOnly := &rafikiv1.ModelRow{ // catalog entry that definitively lacks both capabilities
		Id: "openrouter/text/only", Source: "openrouter",
		SupportedParameters: []string{"reasoning"}, InputModalities: []string{"text"},
	}
	uncataloged := &rafikiv1.ModelRow{ // no catalog entry: every locally-served model
		Id: "vmlx/qwen", Source: "local",
	}
	all := []*rafikiv1.ModelRow{cataloged, free, textOnly, uncataloged}

	// admit lists the rows that must SURVIVE filterModelRows, in `all` order —
	// the table drives the real pipeline rather than AdmitsAll directly, so
	// the capability flags (which live outside q.bounds) are covered too.
	cases := []struct {
		name  string
		flags []string // flag, value pairs
		admit []*rafikiv1.ModelRow
	}{
		{"min-ctx k suffix", []string{"min-ctx", "100k"}, all},
		{"min-ctx exact tokens", []string{"min-ctx", "200000"}, all},
		{"min-ctx m suffix rejects known", []string{"min-ctx", "1m"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		{"max-ctx", []string{"max-ctx", "100k"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		{"min-in", []string{"min-in", "2"},
			[]*rafikiv1.ModelRow{cataloged, textOnly, uncataloged}},
		{"max-in", []string{"max-in", "2"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		{"min-out", []string{"min-out", "10"},
			[]*rafikiv1.ModelRow{cataloged, textOnly, uncataloged}},
		{"max-out", []string{"max-out", "10"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		// Unscored rows pass numeric score bounds — unknown is not a low score.
		{"min-intel", []string{"min-intel", "55"}, all},
		{"max-intel", []string{"max-intel", "55"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		{"min-code", []string{"min-code", "40"}, all},
		{"max-code", []string{"max-code", "40"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		{"min-agentic", []string{"min-agentic", "55"}, all},
		{"max-agentic", []string{"max-agentic", "55"},
			[]*rafikiv1.ModelRow{free, textOnly, uncataloged}},
		// PaidOnly rejects a KNOWN zero; unknown stays (unknown is not free).
		{"paid-only", []string{"paid-only", "true"},
			[]*rafikiv1.ModelRow{cataloged, textOnly, uncataloged}},
		// --scored is the ONE exception to admit-unknowns: the uncataloged row
		// that passed every bound above fails here.
		{"scored", []string{"scored", "true"},
			[]*rafikiv1.ModelRow{cataloged}},
		// Only a definitive catalog claim filters; unknown is KEPT.
		{"tools", []string{"tools", "true"},
			[]*rafikiv1.ModelRow{cataloged, free, uncataloged}},
		{"vision", []string{"vision", "true"},
			[]*rafikiv1.ModelRow{cataloged, free, uncataloged}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newModelsCmd()
			for i := 0; i+1 < len(tc.flags); i += 2 {
				if err := cmd.Flags().Set(tc.flags[i], tc.flags[i+1]); err != nil {
					t.Fatalf("set --%s=%s: %v", tc.flags[i], tc.flags[i+1], err)
				}
			}
			q, err := modelsQueryFromFlags(cmd)
			if err != nil {
				t.Fatalf("modelsQueryFromFlags: %v", err)
			}
			got := filterModelRows(all, q)
			var ids, want []string
			for _, r := range got {
				ids = append(ids, r.GetId())
			}
			for _, r := range tc.admit {
				want = append(want, r.GetId())
			}
			if !slices.Equal(ids, want) {
				t.Errorf("survivors = %v, want %v", ids, want)
			}
		})
	}
}

func TestParseModelSortKey(t *testing.T) {
	cases := []struct {
		in   string
		want modelquery.SortKey
	}{
		{"agentic:desc", modelquery.SortKey{Field: modelquery.FieldAgentic, Desc: true}},
		{"ctx", modelquery.SortKey{Field: modelquery.FieldContext}},
		{"ctx:asc", modelquery.SortKey{Field: modelquery.FieldContext}},
		{"in$", modelquery.SortKey{Field: modelquery.FieldPromptUSD}},
		{"in:desc", modelquery.SortKey{Field: modelquery.FieldPromptUSD, Desc: true}},
		{"out:desc", modelquery.SortKey{Field: modelquery.FieldCompletionUSD, Desc: true}},
		{"cache:asc", modelquery.SortKey{Field: modelquery.FieldCacheReadUSD}},
		{"max out:desc", modelquery.SortKey{Field: modelquery.FieldMaxCompletion, Desc: true}},
		{"age:desc", modelquery.SortKey{Field: modelquery.FieldAge, Desc: true}},
		{"intel:desc", modelquery.SortKey{Field: modelquery.FieldIntelligence, Desc: true}},
		{"code", modelquery.SortKey{Field: modelquery.FieldCoding}},
	}
	for _, tc := range cases {
		got, err := parseModelSortKey(tc.in)
		if err != nil {
			t.Errorf("parseModelSortKey(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseModelSortKey(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	_, err := parseModelSortKey("bogus")
	if err == nil || !strings.Contains(err.Error(), `unknown sort field "bogus"`) ||
		!strings.Contains(err.Error(), "try ctx, in, out, cache, max out, age, intel, code, agentic") {
		t.Errorf("unknown field error = %v, want the named-fields diagnostic", err)
	}
	if _, err := parseModelSortKey("ctx:sideways"); err == nil ||
		!strings.Contains(err.Error(), "unknown sort direction") {
		t.Errorf("unknown direction error = %v", err)
	}
}

// TestModelsQFiltersCaseInsensitive: --q matches the id and the display name,
// case-insensitively.
func TestModelsQFiltersCaseInsensitive(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{Id: "openrouter/anthropic/claude-opus-5", Name: "Claude Opus 5"},
		{Id: "vmlx/qwen3-8b", Name: "Qwen3 8B"},
	}
	cases := []struct {
		q    string
		want []string
	}{
		{"opus", []string{"openrouter/anthropic/claude-opus-5"}},   // id
		{"OPUS 5", []string{"openrouter/anthropic/claude-opus-5"}}, // name, uppercase
		{"QWEN3", []string{"vmlx/qwen3-8b"}},                       // id, uppercase
		{"qwen3 8b", []string{"vmlx/qwen3-8b"}},                    // name
		{"", []string{"openrouter/anthropic/claude-opus-5", "vmlx/qwen3-8b"}},
		{"no-such-model", nil},
	}
	for _, tc := range cases {
		got := filterModelRows(rows, modelsQuery{q: tc.q})
		var ids []string
		for _, r := range got {
			ids = append(ids, r.GetId())
		}
		if !slices.Equal(ids, tc.want) {
			t.Errorf("q=%q: got %v, want %v", tc.q, ids, tc.want)
		}
	}
}

// TestModelsDefaultSortAgenticThenIntel: the default keys are agentic:desc,
// intel:desc, and the unscored rows land LAST despite the descending keys —
// the presence rule (an absent value is no answer, never "the largest").
func TestModelsDefaultSortAgenticThenIntel(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{Id: "a/agentic-10-intel-90", AgenticIndex: fp64(10), IntelligenceIndex: fp64(90)},
		{Id: "c/agentic-50-intel-10", AgenticIndex: fp64(50), IntelligenceIndex: fp64(10)},
		{Id: "z/unscored"},
		{Id: "b/unscored"},
	}
	var buf bytes.Buffer
	if err := renderModelRows(&buf, rows, modelsQuery{}, outputTable, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	out := buf.String()
	want := []string{"c/agentic-50-intel-10", "a/agentic-10-intel-90", "b/unscored", "z/unscored"}
	last := -1
	for _, id := range want {
		i := strings.Index(out, id)
		if i < 0 {
			t.Fatalf("row %q missing from output:\n%s", id, out)
		}
		if i < last {
			t.Errorf("row %q rendered out of order (agentic desc, unscored last):\n%s", id, out)
		}
		last = i
	}
}

// ─── table shape ───────────────────────────────────────────────────────────

// TestModelsColumnsDropInDeclaredOrder scans widths downward and asserts the
// columns disappear exactly in the declared Drop order — CODE, then AGENTIC,
// then SOURCE, then V, CTX and the prices — with MODEL never dropping.
func TestModelsColumnsDropInDeclaredOrder(t *testing.T) {
	rows := modelTestRows()
	dropOrder := []string{"CODE", "AGENTIC", "SOURCE", "V", "CTX", "IN $", "OUT $"}

	var full bytes.Buffer
	if err := renderModelRowsWidth(&full, rows, modelsQuery{}, false, 0); err != nil {
		t.Fatalf("uncapped render: %v", err)
	}
	// The uncapped render's top border bounds the natural width. Byte length
	// over-measures the box runes (3 bytes each), which only widens the scan.
	natural := len(strings.Split(strings.TrimRight(full.String(), "\n"), "\n")[0])

	present := map[string]bool{}
	for _, h := range renderedHeaders(t, full.String()) {
		present[h] = true
	}
	if len(present) != len(dropOrder)+1 {
		t.Fatalf("uncapped render lost columns: got %v", present)
	}

	next := 0
	for w := natural; w >= 40; w-- {
		var buf bytes.Buffer
		if err := renderModelRowsWidth(&buf, rows, modelsQuery{}, false, w); err != nil {
			t.Fatalf("render at width %d: %v", w, err)
		}
		now := map[string]bool{}
		for _, h := range renderedHeaders(t, buf.String()) {
			now[h] = true
		}
		if !now["MODEL"] {
			t.Fatalf("MODEL vanished at width %d; it must never drop:\n%s", w, buf.String())
		}
		var disappeared []string
		for _, h := range dropOrder {
			if present[h] && !now[h] {
				disappeared = append(disappeared, h)
			}
		}
		for _, h := range disappeared {
			if next >= len(dropOrder) || dropOrder[next] != h {
				t.Fatalf("column %q dropped at width %d, want %q — drop order violated",
					h, w, dropOrder[next])
			}
			next++
		}
		present = now
	}
	if next < 3 {
		t.Fatalf("expected at least CODE, AGENTIC and SOURCE to drop across the scan, got %d", next)
	}
}

// TestModelsVerboseColumns: --verbose adds the eight sparse columns, the ID
// cell keeps the full id while MODEL shows the bare model part, and the
// tri-state TOOLS column says ? when the catalog has no entry.
func TestModelsVerboseColumns(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{
			Id: "openrouter/anthropic/claude-opus-5", Model: "anthropic/claude-opus-5",
			Name: "Claude Opus 5", Source: "openrouter",
			ContextWindow: ip32(200_000), MaxCompletionTokens: ip32(64_000),
			PromptUsd: fp64(0.000003), CompletionUsd: fp64(0.000015), CacheReadUsd: fp64(0.0000007),
			Created:         ip64(1735689600),
			KnowledgeCutoff: "2025-03-01", ExpiresAt: "2098-12-31",
			SupportedParameters: []string{"tools"}, InputModalities: []string{"text", "image"},
			IntelligenceIndex: fp64(59.5), CodingIndex: fp64(40.2), AgenticIndex: fp64(55.1),
		},
		{Id: "vmlx/qwen", Source: "local"}, // no catalog entry: every sparse cell is absent
	}

	var buf bytes.Buffer
	if err := renderModelRows(&buf, rows, modelsQuery{verbose: true}, outputTable, false); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	out := buf.String()
	for _, h := range []string{"MODEL", "ID", "CACHE", "MAX OUT", "INTEL", "TOOLS", "CREATED", "CUTOFF", "EXPIRES"} {
		if !strings.Contains(out, h) {
			t.Errorf("verbose table missing column %q; output:\n%s", h, out)
		}
	}

	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "openrouter/anthropic/claude-opus-5") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("cataloged row missing from output:\n%s", out)
	}
	for _, want := range []string{"0.70", "64000", "59.5", "yes", "2025-01-01", "2025-03-01", "2098-12-31"} {
		if !strings.Contains(row, want) {
			t.Errorf("verbose row missing %q; row:\n%s", want, row)
		}
	}
	if strings.Contains(row, "unknown") {
		t.Errorf("TOOLS cell must render the tri-state as yes/no/?; row:\n%s", row)
	}

	// The uncataloged row's sparse cells are all absent markers, and its
	// TOOLS cell says ? — never "no".
	var bare bool
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "vmlx/qwen") {
			bare = strings.Contains(line, "?") &&
				strings.Contains(line, "—") &&
				!strings.Contains(line, "yes") && !strings.Contains(line, "no")
		}
	}
	if !bare {
		t.Errorf("uncataloged row's sparse cells must all read absent (? and —):\n%s", out)
	}
}
