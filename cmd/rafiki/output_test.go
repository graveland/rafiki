package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestRenderList_Table(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: 1716636789},
	}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"c_01HXABC", "afk-impl", "streaming", "claude-sonnet-4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderList_KindColumn(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "c_claude1", Name: "claude-worker", Kind: "claude", Status: "idle"},
		{ChildID: "c_fundi1", Name: "fundi-worker", Kind: "", Status: "idle"},
	}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "KIND") {
		t.Fatalf("output missing KIND header:\n%s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Fatalf("output missing the claude child's kind:\n%s", out)
	}
	if !strings.Contains(out, "fundi") {
		t.Fatalf("output missing the default \"fundi\" kind for an empty Kind field:\n%s", out)
	}
}

func costPtr(v float64) *float64 { return &v }

// A leaf's COST and TOTAL are the same number; a parent's TOTAL also carries
// its child's spend.
func TestRenderList_CostColumns(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "parent", Name: "coordinator", CostUSD: costPtr(1.5)},
		{ChildID: "kid", Name: "worker", Labels: map[string]string{"rafiki/parent": "parent"}, CostUSD: costPtr(0.25)},
	}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "COST") || !strings.Contains(out, "TOTAL") {
		t.Fatalf("output missing COST/TOTAL headers:\n%s", out)
	}
	if !strings.Contains(out, "$1.75") {
		t.Fatalf("parent's TOTAL should roll up its child's spend ($1.75):\n%s", out)
	}
	if !strings.Contains(out, "$0.25") {
		t.Fatalf("child's own cost missing:\n%s", out)
	}
}

// A configured currency converts COST and TOTAL alike -- `rafiki list` reads
// the same clientstate.Currency section `rafiki config set` writes.
func TestRenderList_CostColumnsConvertCurrency(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})

	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x", CostUSD: costPtr(1.0)}}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "$1.38 CAD") {
		t.Fatalf("cost was not converted through the configured currency:\n%s", out)
	}
}

// No cost known anywhere (no agent database) renders as "-", not "$0.00".
func TestRenderList_CostColumnsUnknown(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "-") {
		t.Fatalf("unknown cost should render as \"-\":\n%s", out)
	}
}

func TestSubtreeCosts(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "a", CostUSD: costPtr(1)},
		{ChildID: "b", Labels: map[string]string{"rafiki/parent": "a"}, CostUSD: costPtr(2)},
		{ChildID: "c", Labels: map[string]string{"rafiki/parent": "b"}, CostUSD: costPtr(4)},
		{ChildID: "z"}, // no cost known at all
	}
	got := subtreeCosts(in)
	if v := got["a"]; v == nil || *v != 7 {
		t.Errorf("a's subtree total = %v, want 7 (1+2+4)", v)
	}
	if v := got["b"]; v == nil || *v != 6 {
		t.Errorf("b's subtree total = %v, want 6 (2+4)", v)
	}
	if v := got["c"]; v == nil || *v != 4 {
		t.Errorf("c's subtree total = %v, want 4", v)
	}
	if v := got["z"]; v != nil {
		t.Errorf("z's subtree total = %v, want nil (no cost known)", v)
	}
}

// A cyclic parent chain must not hang the rollup, matching
// sortChildrenAsTree's own cycle guard.
func TestSubtreeCostsCycleTerminates(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "x", Labels: map[string]string{"rafiki/parent": "y"}, CostUSD: costPtr(1)},
		{ChildID: "y", Labels: map[string]string{"rafiki/parent": "x"}, CostUSD: costPtr(2)},
	}
	done := make(chan map[string]*float64, 1)
	go func() { done <- subtreeCosts(in) }()
	select {
	case got := <-done:
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subtreeCosts did not terminate on a cyclic parent chain")
	}
}

func TestRenderList_JSON(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputJSON, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Pretty-printed JSON has a space after the colon.
	if !strings.Contains(out, `"childId": "c_1"`) {
		t.Fatalf("JSON output: %s", out)
	}
}

func TestProfileIndicatorOnlyAppearsWhenThereIsAChoice(t *testing.T) {
	isolateProfiles(t)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"only": {Name: "only", Socket: "/s"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := profileIndicator("only"); got != "" {
		t.Fatalf("indicator with one profile = %q, want empty", got)
	}

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work":     {Name: "work", Socket: "/s"},
		"personal": {Name: "personal", URL: "https://h"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := profileIndicator("work")
	if !strings.Contains(got, "work") {
		t.Fatalf("indicator with two profiles = %q, want it to name the profile", got)
	}
}

func TestProfileIndicatorIsSilentWithNoManifest(t *testing.T) {
	isolateProfiles(t)
	if got := profileIndicator("anything"); got != "" {
		t.Fatalf("indicator with no manifest = %q, want empty", got)
	}
}

func TestColorEnabled_AlwaysFlag(t *testing.T) {
	if !colorEnabled("always", false) {
		t.Fatal("always should be true")
	}
	if colorEnabled("never", true) {
		t.Fatal("never should be false")
	}
}

func TestSortChildrenAsTree(t *testing.T) {
	mk := func(id, parent, root string) protocol.ChildSummary {
		labels := map[string]string{}
		if parent != "" {
			labels["rafiki/parent"] = parent
			labels["rafiki/root"] = root
		}
		return protocol.ChildSummary{ChildID: id, Labels: labels}
	}
	// Deliberately out of order, and a second root, to prove ordering is
	// derived rather than incidental.
	in := []protocol.ChildSummary{
		mk("c", "b", "a"),
		mk("z", "", ""),
		mk("a", "", ""),
		mk("b", "a", "a"),
	}
	rows := sortChildrenAsTree(in)

	var gotIDs []string
	depth := map[string]int{}
	for _, r := range rows {
		gotIDs = append(gotIDs, r.Child.ChildID)
		depth[r.Child.ChildID] = r.Depth
	}

	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 — every child must appear exactly once", len(rows))
	}
	// a's subtree must be contiguous and in depth order.
	want := []string{"a", "b", "c", "z"}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("order = %v; want %v", gotIDs, want)
		}
	}
	if depth["a"] != 0 || depth["b"] != 1 || depth["c"] != 2 || depth["z"] != 0 {
		t.Fatalf("depths wrong: %v", depth)
	}
}

func TestSortChildrenAsTreeOrphanedParent(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "kid", Labels: map[string]string{"rafiki/parent": "gone", "rafiki/root": "gone"}},
	}
	rows := sortChildrenAsTree(in)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Depth != 0 {
		t.Fatalf("depth = %d; want 0 when the parent is absent from the set", rows[0].Depth)
	}
}

func TestSortChildrenAsTreeCycleTerminates(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "x", Labels: map[string]string{"rafiki/parent": "y"}},
		{ChildID: "y", Labels: map[string]string{"rafiki/parent": "x"}},
	}
	done := make(chan int, 1)
	go func() { done <- len(sortChildrenAsTree(in)) }()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("got %d rows, want 2 — a cycle must not drop children", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sortChildrenAsTree did not terminate on a cyclic parent chain")
	}
}

// driveOutputFlags runs a `list` subcommand of the real root command with the
// given args and captures outputOpts from inside its RunE — the flags must be
// parsed by cobra the way a real invocation parses them, not read by hand.
func driveOutputFlags(t *testing.T, args ...string) (outputMode, error) {
	t.Helper()
	root := newRootCmd()
	list, _, err := root.Find([]string{"list"})
	if err != nil {
		t.Fatalf("locate list: %v", err)
	}
	var (
		gotMode outputMode
		gotErr  error
	)
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		m, _, e := outputOpts(cmd)
		gotMode, gotErr = m, e
		return nil
	}
	root.SetArgs(append([]string{"list"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
	return gotMode, gotErr
}

func TestResolveOutputModeTableByDefault(t *testing.T) {
	// The flip, pinned: "auto" and the legacy "table" both resolve to table
	// with no TTY probe involved. The old pipe→JSON rule is dead — table is
	// the default on TTY and pipe alike.
	for _, flag := range []string{"auto", "table", ""} {
		if got := resolveOutputMode(flag); got != outputTable {
			t.Errorf("resolveOutputMode(%q) = %v, want table", flag, got)
		}
	}
}

func TestResolveOutputModeJSONOnlyOnRequest(t *testing.T) {
	if got := resolveOutputMode("json"); got != outputJSON {
		t.Errorf("resolveOutputMode(json) = %v, want json", got)
	}
	if got := resolveOutputMode("jsonl"); got != outputJSONL {
		t.Errorf("resolveOutputMode(jsonl) = %v, want jsonl", got)
	}
}

func TestOutputOptsJSONLShorthand(t *testing.T) {
	mode, err := driveOutputFlags(t, "-J")
	if err != nil {
		t.Fatal(err)
	}
	if mode != outputJSONL {
		t.Errorf("-J: got mode %v, want jsonl", mode)
	}
	mode, err = driveOutputFlags(t, "-j")
	if err != nil {
		t.Fatal(err)
	}
	if mode != outputJSON {
		t.Errorf("-j: got mode %v, want json", mode)
	}
	// The long spellings ride the same registration.
	mode, err = driveOutputFlags(t, "--jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if mode != outputJSONL {
		t.Errorf("--jsonl: got mode %v, want jsonl", mode)
	}
}

func TestOutputOptsRejectsJAndBigJ(t *testing.T) {
	_, err := driveOutputFlags(t, "-j", "-J")
	if err == nil {
		t.Fatal("want an error when -j and -J are combined, got nil")
	}
	if err.Error() != "cannot combine -j and -J" {
		t.Errorf("error text = %q, want exactly %q", err.Error(), "cannot combine -j and -J")
	}
}

func TestWriteJSONLOneCompactObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	rows := []any{
		map[string]any{"id": "c_01", "cost": 1.5},
		map[string]any{"id": "c_02", "labels": []string{"a", "b"}},
	}
	if err := writeJSONL(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("missing trailing newline: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Errorf("row %d is not a bare object: %q", i+1, line)
		}
	}
	if strings.Contains(out, ": ") || strings.Contains(out, ", ") {
		t.Errorf("output is not compact: %q", out)
	}
	if strings.Contains(out, "\"rows\"") || strings.Contains(out, "\"children\"") {
		t.Errorf("output carries an envelope: %q", out)
	}
}
