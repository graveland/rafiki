package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

func TestRenderList_Table(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: timestamppb.New(time.UnixMilli(1716636789))},
	}
	c.NoError(renderList(&buf, children, outputTable, false, false))
	out := buf.String()
	for _, want := range []string{"c_01HXABC", "afk-impl", "streaming", "claude-sonnet-4"} {
		c.StrContains(out, want, "output missing")
	}
}

func TestRenderList_KindColumn(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_claude1", Name: "claude-worker", Kind: "claude", Status: "idle"},
		{ChildId: "c_fundi1", Name: "fundi-worker", Status: "idle"},
	}
	c.NoError(renderList(&buf, children, outputTable, false, false))
	out := buf.String()
	c.StrContains(out, "KIND", "output missing KIND header:\n")
	c.StrContains(out, "claude", "output missing the claude child's kind:\n")
	c.StrContains(out, "fundi", "output missing the default \"fundi\" kind for an empty Kind field:\n")
}

func costPtr(v float64) *float64 { return &v }

// A leaf's COST and TOTAL are the same number; a parent's TOTAL also carries
// its child's spend.
func TestRenderList_CostColumns(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "parent", Name: "coordinator", CostUsd: costPtr(1.5)},
		{ChildId: "kid", Name: "worker", Labels: map[string]string{"rafiki/parent": "parent"}, CostUsd: costPtr(0.25)},
	}
	c.NoError(renderList(&buf, children, outputTable, false, false))
	out := buf.String()
	c.False(!strings.Contains(out, "COST") || !strings.Contains(out, "TOTAL"), "output missing COST/TOTAL headers:\n%s", out)
	c.StrContains(out, "$1.75", "parent's TOTAL should roll up its child's spend ($1.75):\n")
	c.StrContains(out, "$0.25", "child's own cost missing:\n")
}

// A configured currency converts COST and TOTAL alike -- `rafiki list` reads
// the same clientstate.Currency section `rafiki config set` writes.
func TestRenderList_CostColumnsConvertCurrency(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})

	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x", CostUsd: costPtr(1.0)}}
	c.NoError(renderList(&buf, children, outputTable, false, false))
	out := buf.String()
	c.StrContains(out, "$1.38 CAD", "cost was not converted through the configured currency:\n")
}

// No cost known anywhere (no agent database) renders as "-", not "$0.00".
func TestRenderList_CostColumnsUnknown(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x"}}
	c.NoError(renderList(&buf, children, outputTable, false, false))
	out := buf.String()
	c.StrContains(out, "-", "unknown cost should render as \"-\":\n")
}

// A script row's CostUSD is already its SUBTREE's spend (the daemon prices it
// with one subtree query folding in the script's own conversations and its
// descendants'), so the walk must stop at a script node: recursing past it
// adds every descendant a second time. A script→fundi pair at 1.0 each must
// report a TOTAL of 1.0, not 2.0 — and the script's own PARENT must not be
// inflated by the double count either.
func TestSubtreeCostsStopsAtScriptNodes(t *testing.T) {
	in := []*rafikiv1.ChildSummary{
		{ChildId: "coord", Kind: "claude", CostUsd: costPtr(2)},
		{ChildId: "script", Kind: "script", Labels: map[string]string{"rafiki/parent": "coord"}, CostUsd: costPtr(1)},
		{ChildId: "fundi", Kind: "fundi", Labels: map[string]string{"rafiki/parent": "script"}, CostUsd: costPtr(1)},
		{ChildId: "claude2", Kind: "claude", Labels: map[string]string{"rafiki/parent": "fundi"}, CostUsd: costPtr(0.5)},
	}
	got := subtreeCosts(in)
	if v := got["script"]; v == nil || *v != 1 {
		t.Errorf("script's subtree total = %v, want 1 (its own cost already IS its subtree, not 2.5)", v)
	}
	if v := got["fundi"]; v == nil || *v != 1.5 {
		t.Errorf("fundi's subtree total = %v, want 1.5 (the walk only stops at scripts)", v)
	}
	if v := got["coord"]; v == nil || *v != 3 {
		t.Errorf("coord's subtree total = %v, want 3 (2 own + script's subtree 1, not double-counted)", v)
	}
}

// B3: an UNPRICED script node stays nil — the walk does not partially price it
// from its descendants — and its parent's total excludes those descendants
// entirely (the daemon prices a script's subtree in one query; when that
// failed, the client must not invent a partial sum).
func TestSubtreeCostsStopsAtScriptNodesUnpriced(t *testing.T) {
	in := []*rafikiv1.ChildSummary{
		{ChildId: "coord", Kind: "claude", CostUsd: costPtr(2)},
		{ChildId: "script", Kind: "script", Labels: map[string]string{"rafiki/parent": "coord"}}, // CostUsd nil
		{ChildId: "fundi", Kind: "fundi", Labels: map[string]string{"rafiki/parent": "script"}, CostUsd: costPtr(1)},
	}
	got := subtreeCosts(in)
	if v := got["script"]; v != nil {
		t.Errorf("unpriced script's subtree total = %v, want nil (not a partial sum of its descendants)", v)
	}
	if v := got["fundi"]; v == nil || *v != 1 {
		t.Errorf("fundi's subtree total = %v, want 1", v)
	}
	if v := got["coord"]; v == nil || *v != 2 {
		t.Errorf("coord's subtree total = %v, want 2 (the unpriced script contributes nothing, so neither does its fundi)", v)
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
	assert.NewCollecting(t).Nil(got["z"], "z's subtree total")
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
		assert.NewAborting(t).Len(got, 2, "got %d entries, want 2", len(got))
	case <-time.After(2 * time.Second):
		t.Fatal("subtreeCosts did not terminate on a cyclic parent chain")
	}
}

func TestRenderList_JSON(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{{ChildId: "c_1", Name: "x"}}
	c.NoError(renderList(&buf, children, outputJSON, false, false))
	out := buf.String()
	// Pretty-printed protojson has a space after the colon, and int64 fields
	// render as strings.
	c.StrContains(out, `"childId": "c_1"`, "JSON output")
}

func TestProfileIndicatorOnlyAppearsWhenThereIsAChoice(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"only": {Name: "only", Socket: "/s"},
	}}), "Save")
	c.Eq("", profileIndicator("only"), "indicator with one profile")

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work":     {Name: "work", Socket: "/s"},
		"personal": {Name: "personal", URL: "https://h"},
	}}), "Save")
	got := profileIndicator("work")
	c.StrContains(got, "work", "indicator with two profiles")
}

func TestProfileIndicatorIsSilentWithNoManifest(t *testing.T) {
	isolateProfiles(t)
	assert.NewAborting(t).Eq("", profileIndicator("anything"), "indicator with no manifest")
}

func TestColorEnabled_AlwaysFlag(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(colorEnabled("always", false), "always should be true")
	c.False(colorEnabled("never", true), "never should be false")
}

func TestSortChildrenAsTree(t *testing.T) {
	c := assert.NewAborting(t)
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

	c.Len(rows, 4, "got %d rows, want 4 — every child must appear exactly once", len(rows))
	// a's subtree must be contiguous and in depth order.
	want := []string{"a", "b", "c", "z"}
	for i := range want {
		c.Eq(want[i], gotIDs[i], "order = %v; want %v", gotIDs, want)
	}
	c.False(depth["a"] != 0 || depth["b"] != 1 || depth["c"] != 2 || depth["z"] != 0, "depths wrong: %v", depth)
}

func TestSortChildrenAsTreeOrphanedParent(t *testing.T) {
	c := assert.NewAborting(t)
	in := []*rafikiv1.ChildSummary{
		{ChildId: "kid", Labels: map[string]string{"rafiki/parent": "gone", "rafiki/root": "gone"}},
	}
	rows := sortChildrenAsTree(in)
	c.Len(rows, 1, "got %d rows, want 1", len(rows))
	c.Eq(0, rows[0].Depth, "depth")
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
		assert.NewAborting(t).Eq(2, n, "got")
	case <-time.After(2 * time.Second):
		t.Fatal("sortChildrenAsTree did not terminate on a cyclic parent chain")
	}
}

// driveOutputFlags runs a `list` subcommand of the real root command with the
// given args and captures outputOpts from inside its RunE — the flags must be
// parsed by cobra the way a real invocation parses them, not read by hand.
func driveOutputFlags(t *testing.T, args ...string) (outputMode, error) {
	t.Helper()
	c := assert.NewAborting(t)
	root := newRootCmd()
	list, _, err := root.Find([]string{"list"})
	c.NoError(err, "locate list")
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
	c.NoError(root.Execute(), "execute %v", args)
	return gotMode, gotErr
}

func TestResolveOutputModeTableByDefault(t *testing.T) {
	// The flip, pinned: "auto" and the legacy "table" both resolve to table
	// with no TTY probe involved. The old pipe→JSON rule is dead — table is
	// the default on TTY and pipe alike.
	for _, flag := range []string{"auto", "table", ""} {
		got := resolveOutputMode(flag)
		assert.NewCollecting(t).Eq(outputTable, got, "resolveOutputMode(%q) = %v, want table", flag, got)
	}
}

func TestResolveOutputModeJSONOnlyOnRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(outputJSON, resolveOutputMode("json"), "resolveOutputMode(json)")
	c.Eq(outputJSONL, resolveOutputMode("jsonl"), "resolveOutputMode(jsonl)")
}

func TestOutputOptsJSONLShorthand(t *testing.T) {
	c := assert.NewCollecting(t)
	mode, err := driveOutputFlags(t, "-J")
	c.Require().NoError(err)
	c.Eq(outputJSONL, mode, "-J: got mode")
	mode, err = driveOutputFlags(t, "-j")
	c.Require().NoError(err)
	c.Eq(outputJSON, mode, "-j: got mode")
	// The long spellings ride the same registration.
	mode, err = driveOutputFlags(t, "--jsonl")
	c.Require().NoError(err)
	c.Eq(outputJSONL, mode, "--jsonl: got mode")
}

func TestOutputOptsRejectsJAndBigJ(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := driveOutputFlags(t, "-j", "-J")
	c.Require().Error(err, "want an error when -j and -J are combined, got nil")
	c.Eq("cannot combine -j and -J", err.Error(), "error text")
}

func TestWriteJSONLOneCompactObjectPerLine(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	rows := []any{
		map[string]any{"id": "c_01", "cost": 1.5},
		map[string]any{"id": "c_02", "labels": []string{"a", "b"}},
	}
	c.Require().NoError(writeJSONL(&buf, rows))
	out := buf.String()
	c.True(strings.HasSuffix(out, "\n"), "missing trailing newline: %q", out)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	c.Require().Len(lines, 2, "want 2 lines, got %d: %q", len(lines), out)
	for i, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Errorf("row %d is not a bare object: %q", i+1, line)
		}
	}
	c.False(strings.Contains(out, ": ") || strings.Contains(out, ", "), "output is not compact: %q", out)
	c.False(strings.Contains(out, "\"rows\"") || strings.Contains(out, "\"children\""), "output carries an envelope: %q", out)
}

// ─── Task 4.1: list-shaped verbs under the three-way contract ───────────────

// JSONL from renderList is the ChildSummary objects themselves — one per
// line, unwrapped, compact, in canonical protojson.
func TestRenderListJSONLOnePerLine(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_01", Name: "alpha", Status: "idle"},
		{ChildId: "c_02", Name: "beta", Status: "exited", ExitCode: int32Ptr(0)},
		{ChildId: "c_03", Name: "gamma", Status: "streaming"},
	}
	c.NoError(renderList(&buf, children, outputJSONL, false, false))
	out := buf.String()
	c.True(strings.HasSuffix(out, "\n"), "missing trailing newline: %q", out)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	c.Len(lines, len(children), "got %d lines for %d children: %q", len(lines), len(children), out)
	var ids []string
	for i, line := range lines {
		var ch rafikiv1.ChildSummary
		err := protojson.Unmarshal([]byte(line), &ch)
		c.NoError(err, "line %d is not a bare ChildSummary protojson object: %v (%q)", i+1, err, line)
		ids = append(ids, ch.ChildId)
		c.NotStrContains(line, "\"children\"", "line %d carries an envelope", i+1)
		// A Timestamp must render RFC3339 (a string), never a bare number.
		if strings.Contains(line, `"startedAt":0`) || strings.Contains(line, `"startedAt":1`) {
			t.Fatalf("line %d encodes a timestamp as a number, not a protojson string: %q", i+1, line)
		}
	}
	want := []string{"c_01", "c_02", "c_03"}
	for i := range want {
		c.Eq(want[i], ids[i], "line order = %v, want %v", ids, want)
	}
}

// get's default (table) mode renders the same table `list` renders, flat.
func TestGetTextRendersListTable(t *testing.T) {
	c := assert.NewAborting(t)
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_get1", Name: "worker", Status: "streaming", Model: "anthropic/claude-sonnet-4"},
		{ChildId: "c_get2", Name: "reviewer", Status: "idle"},
	}
	var buf bytes.Buffer
	c.NoError(emitGet(&buf, []string{"worker", "reviewer"}, children, 0, outputTable, false))
	out := buf.String()
	for _, want := range []string{"ID", "NAME", "STATUS", "c_get1", "worker", "c_get2", "reviewer", "claude-sonnet-4"} {
		c.StrContains(out, want, "output missing")
	}
	c.NotStrContains(out, "\x1b[", "table mode with color off leaked ANSI:\n")
}

// get's JSON shapes are the backward-compatibility contract: single target,
// no failures → bare object; multiple targets or any failures → wrapped. The
// encoding is now the canonical protojson (camelCase, int64 as string).
func TestGetJSONShapesUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	one := &rafikiv1.ChildSummary{ChildId: "c_1", Name: "solo", Status: "idle", StartedAt: timestamppb.New(time.UnixMilli(1700000000000))}
	two := []*rafikiv1.ChildSummary{
		one,
		{ChildId: "c_2", Name: "duo", Status: "exited", ExitCode: int32Ptr(3)},
	}

	var bare bytes.Buffer
	c.NoError(emitGet(&bare, []string{"c_1"}, []*rafikiv1.ChildSummary{one}, 0, outputJSON, false))
	var obj map[string]any
	c.NoError(json.Unmarshal(bare.Bytes(), &obj), "bare shape is not a JSON object")
	if _, ok := obj["childId"]; !ok {
		t.Fatalf("single-target shape must be a bare ChildSummary, got:\n%s", bare.String())
	}
	if _, ok := obj["children"]; ok {
		t.Fatalf("single-target shape must not be wrapped, got:\n%s", bare.String())
	}
	c.StrContains(bare.String(), `"childId": "c_1"`, "JSON mode must stay pretty-printed:\n")
	// protojson: a Timestamp renders RFC3339.
	c.StrContains(bare.String(), `"startedAt": "2023-11-14T22:13:20Z"`, "startedAt must render as a protojson RFC3339 string:\n")

	var wrapped bytes.Buffer
	c.NoError(emitGet(&wrapped, []string{"a", "b"}, two, 0, outputJSON, false))
	obj = nil
	c.NoError(json.Unmarshal(wrapped.Bytes(), &obj))
	kids, ok := obj["children"].([]any)
	if !ok || len(kids) != 2 {
		t.Fatalf("multi-target shape must be {\"children\":[...]}, got:\n%s", wrapped.String())
	}
	if _, ok := obj["childId"]; ok {
		t.Fatalf("multi-target shape must not carry a bare childId:\n%s", wrapped.String())
	}

	// A single target with a failed sibling falls to the wrapped shape.
	var failed bytes.Buffer
	c.NoError(emitGet(&failed, []string{"gone"}, []*rafikiv1.ChildSummary{one}, 1, outputJSON, false))
	obj = nil
	c.NoError(json.Unmarshal(failed.Bytes(), &obj))
	if _, ok := obj["children"]; !ok {
		t.Fatalf("a failed sibling must switch to the wrapped shape:\n%s", failed.String())
	}
}

// JSONL from get is the successes only, one per line — failures have already
// gone to stderr in runGet, before emitGet runs.
func TestGetJSONLSuccessesOnly(t *testing.T) {
	c := assert.NewAborting(t)
	children := []*rafikiv1.ChildSummary{
		{ChildId: "c_ok1", Name: "kept", Status: "idle"},
		{ChildId: "c_ok2", Name: "also-kept", Status: "idle"},
	}
	var buf bytes.Buffer
	// Three targets requested, one failed: only the two successes arrive here.
	c.NoError(emitGet(&buf, []string{"kept", "also-kept", "missing"}, children, 1, outputJSONL, false))
	out := strings.TrimSuffix(buf.String(), "\n")
	c.NotEq("", out, "no output")
	lines := strings.Split(out, "\n")
	c.Len(lines, 2, "got %d lines for 2 successes: %q", len(lines), buf.String())
	c.NotStrContains(buf.String(), "\"children\"", "JSONL rows must be unwrapped:\n")
	for i, line := range lines {
		var ch rafikiv1.ChildSummary
		c.NoError(protojson.Unmarshal([]byte(line), &ch), "line %d not a bare ChildSummary protojson object", i+1)
	}
}

// status's text mode is a key/value block, one line per populated field.
func TestStatusTextKeyValue(t *testing.T) {
	c := assert.NewAborting(t)
	started := time.UnixMilli(1757000000000)
	daemon := &rafikiv1.StatusResponse{
		Version:     "1.2.3",
		StartedAt:   timestamppb.New(time.UnixMilli(1757000000000)),
		Children:    &rafikiv1.StatusResponse_ChildCounts{Live: 2, Exited: 1},
		MemoryBytes: 16 << 20,
		Socket:      "/tmp/d.sock",
		LogsDir:     "/tmp/logs",
	}
	var buf bytes.Buffer
	c.NoError(emitStatus(&buf, daemon, outputTable, false))
	out := buf.String()
	for _, want := range []string{
		"version: 1.2.3",
		"started: " + started.Format("2006-01-02 15:04"),
		"children: 2 live, 1 exited",
		"memory: 16.0 MiB",
		"socket: /tmp/d.sock",
		"logs: /tmp/logs",
	} {
		c.StrContains(out, want, "output missing")
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
		StartedAt: timestamppb.New(time.UnixMilli(1757000000000)),
		Labels:    map[string]string{"env": "prod"},
	}
	buf.Reset()
	c.NoError(emitStatus(&buf, child, outputTable, false))
	out = buf.String()
	for _, want := range []string{
		"id: c_9", "name: worker", "kind: claude", "status: exited (2)",
		"model: openrouter/x/y", "cost: $0.50", "cwd: /repo", "labels: env=prod",
	} {
		c.StrContains(out, want, "output missing")
	}
}

// status's JSONL mode is the whole payload as one compact line — the
// canonical protojson of the Status response (int64 as string).
func TestStatusJSONLCompactLine(t *testing.T) {
	c := assert.NewAborting(t)
	daemon := &rafikiv1.StatusResponse{
		Version:   "1.2.3",
		StartedAt: timestamppb.New(time.UnixMilli(1757000000000)),
		Children:  &rafikiv1.StatusResponse_ChildCounts{Live: 1},
	}
	var buf bytes.Buffer
	c.NoError(emitStatus(&buf, daemon, outputJSONL, false))
	out := buf.String()
	c.False(strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n"), "want exactly one line, got %q", out)
	line := strings.TrimSuffix(out, "\n")
	c.False(strings.Contains(line, ": ") || strings.Contains(line, ", "), "JSONL line is not compact: %q", line)
	var back map[string]any
	c.NoError(json.Unmarshal([]byte(line), &back), "line is not JSON")
	c.False(back["version"] != "1.2.3", "payload changed: %q", line)
	c.False(back["startedAt"] != "2025-09-04T15:33:20Z", "startedAt must render as a protojson RFC3339 string: %q", line)
}

// tasks' text mode is a table with every column the wire row carries, always:
// handle (the addressable id), the owning conversation (CHILD), status (with
// the drop reason), subject, assignee. UPDATED is the one framed column that
// stays gone — it was always a client-side dash, never carried on the wire.
func TestTasksTextTableColumns(t *testing.T) {
	c := assert.NewAborting(t)
	resp := &rafikiv1.ListTasksResponse{Tasks: []*rafikiv1.TaskRow{
		{Handle: "2.1", ConversationId: "conv_9", Content: "implement the parser", Status: "in_progress", Assignee: "c_worker"},
		{Handle: "3", Content: "exploratory probe", Status: "dropped", DropReason: "turned out unnecessary"},
	}}
	var buf bytes.Buffer
	c.NoError(emitTasks(&buf, resp, outputTable, false))
	out := buf.String()
	for _, want := range []string{"ID", "CHILD", "STATUS", "SUBJECT", "ASSIGNEE"} {
		c.StrContains(out, want, "output missing")
	}
	for _, want := range []string{"2.1", "conv_9", "implement the parser", "c_worker", "in_progress"} {
		c.StrContains(out, want, "output missing")
	}
	c.StrContains(out, "dropped (turned out unnecessary)", "drop reason should ride the STATUS cell:\n")
	// Row 2 carries no conversation id or assignee: those cells still render,
	// as dashes — a column's presence never depends on the rows beneath it.
	c.False(!strings.Contains(out, " - ") && !strings.Contains(out, "- "), "CHILD/ASSIGNEE columns should render even without data:\n%s", out)
}

// tasks' JSONL mode unwraps the response to one canonical TaskRow protojson
// object per line.
func TestTasksJSONLUnwrapped(t *testing.T) {
	c := assert.NewAborting(t)
	resp := &rafikiv1.ListTasksResponse{Tasks: []*rafikiv1.TaskRow{
		{Handle: "1", ConversationId: "conv_9", Content: "a", Status: "pending"},
		{Handle: "2", Content: "b", Status: "completed", Assignee: "c_1"},
	}}
	var buf bytes.Buffer
	c.NoError(emitTasks(&buf, resp, outputJSONL, false))
	out := buf.String()
	c.True(strings.HasSuffix(out, "\n"), "missing trailing newline: %q", out)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	c.Len(lines, 2, "want 2 lines, got %d: %q", len(lines), out)
	for i, line := range lines {
		if strings.Contains(line, "\"tasks\"") || strings.Contains(line, "\"rows\"") {
			t.Fatalf("line %d carries an envelope: %q", i+1, line)
		}
		var row map[string]any
		c.NoError(json.Unmarshal([]byte(line), &row), "line %d is not a bare object", i+1)
		if _, ok := row["handle"]; !ok {
			t.Fatalf("line %d lost the wire row's field names (protojson camelCase): %q", i+1, line)
		}
	}
	// The restored CHILD field rides every row's protojson under its camelCase
	// name, present only when the row carries one.
	c.StrContains(lines[0], "\"conversationId\":\"conv_9\"", "row's owning conversation must render as protojson conversationId")
	c.NotStrContains(lines[1], "conversationId", "a row without a conversation must omit the field (protojson zero semantics)")
}

// int32Ptr is a shared test helper (the watch tests' definition died with
// cmd_watch_test.go; conversations/stop/output tests use it).
func int32Ptr(v int32) *int32 { return &v }

func TestFormatChildStatusMarksUnreachableDaraja(t *testing.T) {
	ck := assert.NewCollecting(t)
	live := &rafikiv1.ChildSummary{Status: "idle", Labels: map[string]string{darajaStateLabel: "unreachable"}}
	ck.Eq("idle (unreachable)", formatChildStatus(live, false), "a live child with a lost daraja")

	plain := &rafikiv1.ChildSummary{Status: "idle"}
	ck.Eq("idle", formatChildStatus(plain, false), "a reachable child is unchanged")

	code := int32(0)
	gone := &rafikiv1.ChildSummary{Status: "exited", ExitCode: &code, Labels: map[string]string{darajaStateLabel: "unreachable"}}
	ck.Eq("exited (0)", formatChildStatus(gone, false), "an exited child carries no unreachable note")
}
