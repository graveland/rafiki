package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"
)

func TestRenderList_Table(t *testing.T) {
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: 1716636789},
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
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_claude1", Name: "claude-worker", Kind: "claude", Status: "idle"},
		{ChildId: "c_fundi1", Name: "fundi-worker", Status: "idle"},
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
	children := []*rafikiv1.ChildSummary{
		{ChildId: "parent", Name: "coordinator", CostUsd: costPtr(1.5)},
		{ChildId: "kid", Name: "worker", Labels: map[string]string{"rafiki/parent": "parent"}, CostUsd: costPtr(0.25)},
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
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x", CostUsd: costPtr(1.0)}}
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
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "-") {
		t.Fatalf("unknown cost should render as \"-\":\n%s", out)
	}
}

func TestSubtreeCosts(t *testing.T) {
	in := []*rafikiv1.ChildSummary{
		{ChildId: "a", CostUsd: costPtr(1)},
		{ChildId: "b", Labels: map[string]string{"rafiki/parent": "a"}, CostUsd: costPtr(2)},
		{ChildId: "c", Labels: map[string]string{"rafiki/parent": "b"}, CostUsd: costPtr(4)},
		{ChildId: "z"}, // no cost known at all
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
	in := []*rafikiv1.ChildSummary{
		{ChildId: "x", Labels: map[string]string{"rafiki/parent": "y"}, CostUsd: costPtr(1)},
		{ChildId: "y", Labels: map[string]string{"rafiki/parent": "x"}, CostUsd: costPtr(2)},
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
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputJSON, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Pretty-printed protojson has a space after the colon, and int64 fields
	// render as strings.
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
	mk := func(id, parent, root string) *rafikiv1.ChildSummary {
		labels := map[string]string{}
		if parent != "" {
			labels["rafiki/parent"] = parent
			labels["rafiki/root"] = root
		}
		return &rafikiv1.ChildSummary{ChildId: id, Labels: labels}
	}
	// Deliberately out of order, and a second root, to prove ordering is
	// derived rather than incidental.
	in := []*rafikiv1.ChildSummary{
		mk("c", "b", "a"),
		mk("z", "", ""),
		mk("a", "", ""),
		mk("b", "a", "a"),
	}
	rows := sortChildrenAsTree(in)

	var gotIDs []string
	depth := map[string]int{}
	for _, r := range rows {
		gotIDs = append(gotIDs, r.Child.GetChildId())
		depth[r.Child.GetChildId()] = r.Depth
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
	in := []*rafikiv1.ChildSummary{
		{ChildId: "kid", Labels: map[string]string{"rafiki/parent": "gone", "rafiki/root": "gone"}},
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
	in := []*rafikiv1.ChildSummary{
		{ChildId: "x", Labels: map[string]string{"rafiki/parent": "y"}},
		{ChildId: "y", Labels: map[string]string{"rafiki/parent": "x"}},
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

// ─── Task 4.1: list-shaped verbs under the three-way contract ───────────────

// JSONL from renderList is the ChildSummary objects themselves — one per
// line, unwrapped, compact, in canonical protojson.
func TestRenderListJSONLOnePerLine(t *testing.T) {
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_01", Name: "alpha", Status: "idle"},
		{ChildId: "c_02", Name: "beta", Status: "exited", ExitCode: int32Ptr(0)},
		{ChildId: "c_03", Name: "gamma", Status: "streaming"},
	}
	if err := renderList(&buf, children, outputJSONL, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("missing trailing newline: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(children) {
		t.Fatalf("got %d lines for %d children: %q", len(lines), len(children), out)
	}
	var ids []string
	for i, line := range lines {
		var ch rafikiv1.ChildSummary
		if err := protojson.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("line %d is not a bare ChildSummary protojson object: %v (%q)", i+1, err, line)
		}
		ids = append(ids, ch.ChildId)
		if strings.Contains(line, "\"children\"") {
			t.Fatalf("line %d carries an envelope: %q", i+1, line)
		}
		// The int64-rendered fields must be strings (protojson), not numbers.
		if strings.Contains(line, `"startedAt":0`) || strings.Contains(line, `"startedAt":1`) {
			t.Fatalf("line %d encodes an int64 field as a number, not a protojson string: %q", i+1, line)
		}
	}
	want := []string{"c_01", "c_02", "c_03"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("line order = %v, want %v", ids, want)
		}
	}
}

// get's default (table) mode renders the same table `list` renders, flat.
func TestGetTextRendersListTable(t *testing.T) {
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_get1", Name: "worker", Status: "streaming", Model: "anthropic/claude-sonnet-4"},
		{ChildId: "c_get2", Name: "reviewer", Status: "idle"},
	}
	var buf bytes.Buffer
	if err := emitGet(&buf, []string{"worker", "reviewer"}, children, 0, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "NAME", "STATUS", "c_get1", "worker", "c_get2", "reviewer", "claude-sonnet-4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("table mode with color off leaked ANSI:\n%s", out)
	}
}

// get's JSON shapes are the backward-compatibility contract: single target,
// no failures → bare object; multiple targets or any failures → wrapped. The
// encoding is now the canonical protojson (camelCase, int64 as string).
func TestGetJSONShapesUnchanged(t *testing.T) {
	one := &rafikiv1.ChildSummary{ChildId: "c_1", Name: "solo", Status: "idle", StartedAt: 1700000000000}
	two := []*rafikiv1.ChildSummary{
		one,
		{ChildId: "c_2", Name: "duo", Status: "exited", ExitCode: int32Ptr(3)},
	}

	var bare bytes.Buffer
	if err := emitGet(&bare, []string{"c_1"}, []*rafikiv1.ChildSummary{one}, 0, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(bare.Bytes(), &obj); err != nil {
		t.Fatalf("bare shape is not a JSON object: %v", err)
	}
	if _, ok := obj["childId"]; !ok {
		t.Fatalf("single-target shape must be a bare ChildSummary, got:\n%s", bare.String())
	}
	if _, ok := obj["children"]; ok {
		t.Fatalf("single-target shape must not be wrapped, got:\n%s", bare.String())
	}
	if !strings.Contains(bare.String(), `"childId": "c_1"`) {
		t.Fatalf("JSON mode must stay pretty-printed:\n%s", bare.String())
	}
	// protojson: int64 fields are strings.
	if !strings.Contains(bare.String(), `"startedAt": "1700000000000"`) {
		t.Fatalf("startedAt must render as a protojson string:\n%s", bare.String())
	}

	var wrapped bytes.Buffer
	if err := emitGet(&wrapped, []string{"a", "b"}, two, 0, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	obj = nil
	if err := json.Unmarshal(wrapped.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	kids, ok := obj["children"].([]any)
	if !ok || len(kids) != 2 {
		t.Fatalf("multi-target shape must be {\"children\":[...]}, got:\n%s", wrapped.String())
	}
	if _, ok := obj["childId"]; ok {
		t.Fatalf("multi-target shape must not carry a bare childId:\n%s", wrapped.String())
	}

	// A single target with a failed sibling falls to the wrapped shape.
	var failed bytes.Buffer
	if err := emitGet(&failed, []string{"gone"}, []*rafikiv1.ChildSummary{one}, 1, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	obj = nil
	if err := json.Unmarshal(failed.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["children"]; !ok {
		t.Fatalf("a failed sibling must switch to the wrapped shape:\n%s", failed.String())
	}
}

// JSONL from get is the successes only, one per line — failures have already
// gone to stderr in runGet, before emitGet runs.
func TestGetJSONLSuccessesOnly(t *testing.T) {
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_ok1", Name: "kept", Status: "idle"},
		{ChildId: "c_ok2", Name: "also-kept", Status: "idle"},
	}
	var buf bytes.Buffer
	// Three targets requested, one failed: only the two successes arrive here.
	if err := emitGet(&buf, []string{"kept", "also-kept", "missing"}, children, 1, outputJSONL, false); err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	if out == "" {
		t.Fatal("no output")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines for 2 successes: %q", len(lines), buf.String())
	}
	if strings.Contains(buf.String(), "\"children\"") {
		t.Fatalf("JSONL rows must be unwrapped:\n%s", buf.String())
	}
	for i, line := range lines {
		var ch rafikiv1.ChildSummary
		if err := protojson.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("line %d not a bare ChildSummary protojson object: %v", i+1, err)
		}
	}
}

// status's text mode is a key/value block, one line per populated field.
func TestStatusTextKeyValue(t *testing.T) {
	started := time.UnixMilli(1757000000000)
	daemon := &rafikiv1.StatusResponse{
		Version:     "1.2.3",
		StartedAt:   1757000000000,
		Children:    &rafikiv1.StatusResponse_ChildCounts{Live: 2, Exited: 1},
		MemoryBytes: 16 << 20,
		Socket:      "/tmp/d.sock",
		LogsDir:     "/tmp/logs",
	}
	var buf bytes.Buffer
	if err := emitStatus(&buf, daemon, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"version: 1.2.3",
		"started: " + started.Format("2006-01-02 15:04"),
		"children: 2 live, 1 exited",
		"memory: 16.0 MiB",
		"socket: /tmp/d.sock",
		"logs: /tmp/logs",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	// A child summary renders the same block with the child's fields.
	child := &rafikiv1.ChildSummary{
		ChildId:   "c_9",
		Name:      "worker",
		Kind:      "claude",
		Status:    "exited",
		ExitCode:  int32Ptr(2),
		Model:     "openrouter/x/y",
		CostUsd:   costPtr(0.5),
		Cwd:       "/repo",
		StartedAt: 1757000000000,
		Labels:    map[string]string{"env": "prod"},
	}
	buf.Reset()
	if err := emitStatus(&buf, child, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	for _, want := range []string{
		"id: c_9", "name: worker", "kind: claude", "status: exited (2)",
		"model: openrouter/x/y", "cost: $0.50", "cwd: /repo", "labels: env=prod",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// status's JSONL mode is the whole payload as one compact line — the
// canonical protojson of the Status response (int64 as string).
func TestStatusJSONLCompactLine(t *testing.T) {
	daemon := &rafikiv1.StatusResponse{
		Version:   "1.2.3",
		StartedAt: 1757000000000,
		Children:  &rafikiv1.StatusResponse_ChildCounts{Live: 1},
	}
	var buf bytes.Buffer
	if err := emitStatus(&buf, daemon, outputJSONL, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("want exactly one line, got %q", out)
	}
	line := strings.TrimSuffix(out, "\n")
	if strings.Contains(line, ": ") || strings.Contains(line, ", ") {
		t.Fatalf("JSONL line is not compact: %q", line)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(line), &back); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if back["version"] != "1.2.3" {
		t.Fatalf("payload changed: %q", line)
	}
	if back["startedAt"] != "1757000000000" {
		t.Fatalf("int64 startedAt must render as a protojson string: %q", line)
	}
}

// tasks' text mode is a table with every column the wire row carries, always:
// handle (the addressable id), the owning conversation (CHILD), status (with
// the drop reason), subject, assignee. UPDATED is the one framed column that
// stays gone — it was always a client-side dash, never carried on the wire.
func TestTasksTextTableColumns(t *testing.T) {
	resp := &rafikiv1.ListTasksResponse{Tasks: []*rafikiv1.TaskRow{
		{Handle: "2.1", ConversationId: "conv_9", Content: "implement the parser", Status: "in_progress", Assignee: "c_worker"},
		{Handle: "3", Content: "exploratory probe", Status: "dropped", DropReason: "turned out unnecessary"},
	}}
	var buf bytes.Buffer
	if err := emitTasks(&buf, resp, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "CHILD", "STATUS", "SUBJECT", "ASSIGNEE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q header:\n%s", want, out)
		}
	}
	for _, want := range []string{"2.1", "conv_9", "implement the parser", "c_worker", "in_progress"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "dropped (turned out unnecessary)") {
		t.Fatalf("drop reason should ride the STATUS cell:\n%s", out)
	}
	// Row 2 carries no conversation id or assignee: those cells still render,
	// as dashes — a column's presence never depends on the rows beneath it.
	if !strings.Contains(out, " - ") && !strings.Contains(out, "- ") {
		t.Fatalf("CHILD/ASSIGNEE columns should render even without data:\n%s", out)
	}
}

// tasks' JSONL mode unwraps the response to one canonical TaskRow protojson
// object per line.
func TestTasksJSONLUnwrapped(t *testing.T) {
	resp := &rafikiv1.ListTasksResponse{Tasks: []*rafikiv1.TaskRow{
		{Handle: "1", ConversationId: "conv_9", Content: "a", Status: "pending"},
		{Handle: "2", Content: "b", Status: "completed", Assignee: "c_1"},
	}}
	var buf bytes.Buffer
	if err := emitTasks(&buf, resp, outputJSONL, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("missing trailing newline: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}
	for i, line := range lines {
		if strings.Contains(line, "\"tasks\"") || strings.Contains(line, "\"rows\"") {
			t.Fatalf("line %d carries an envelope: %q", i+1, line)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d is not a bare object: %v", i+1, err)
		}
		if _, ok := row["handle"]; !ok {
			t.Fatalf("line %d lost the wire row's field names (protojson camelCase): %q", i+1, line)
		}
	}
	// The restored CHILD field rides every row's protojson under its camelCase
	// name, present only when the row carries one.
	if !strings.Contains(lines[0], "\"conversationId\":\"conv_9\"") {
		t.Fatalf("row's owning conversation must render as protojson conversationId: %q", lines[0])
	}
	if strings.Contains(lines[1], "conversationId") {
		t.Fatalf("a row without a conversation must omit the field (protojson zero semantics): %q", lines[1])
	}
}

// int32Ptr is a shared test helper (the watch tests' definition died with
// cmd_watch_test.go; conversations/stop/output tests use it).
func int32Ptr(v int32) *int32 { return &v }
