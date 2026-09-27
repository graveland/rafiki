// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestSearchTextGroupsByChild(t *testing.T) {
	c := assert.NewCollecting(t)
	hits := []*rafikiv1.SearchResponse_SearchHit{
		{ChildId: "c_one", SessionName: "alpha", Snippet: "first match in one"},
		{ChildId: "c_one", SessionName: "alpha", Snippet: "second match in one"},
		{ChildId: "c_two", SessionName: "beta", Snippet: "match in two"},
	}
	var buf bytes.Buffer
	c.Require().NoError(renderSearchText(&buf, hits), "renderSearchText")
	out := buf.String()

	one := strings.Index(out, "== c_one alpha")
	two := strings.Index(out, "== c_two beta")
	c.Require().False(one < 0 || two < 0, "missing group headers; output:\n%s", out)
	c.GreaterOrEqual(one, two, "groups must appear in first-appearance order; output:\n%s", out)
	// Hits render as `<ordinal>: <line>`; ordinals restart per group.
	c.False(!strings.Contains(out[one:two], "1: first match in one") ||
		!strings.Contains(out[one:two], "2: second match in one"), "c_one hits not numbered under their header; output:\n%s", out)
	c.StrContains(out[two:], "1: match in two", "c_two ordinal should restart within its group; output:\n%s", out)
	// c_two's hit must not have leaked into c_one's group.
	c.NotStrContains(out[one:two], "match in two", "c_two's hit rendered inside c_one's group; output:\n%s", out)
}

func TestSearchTextIndentsContextLines(t *testing.T) {
	c := assert.NewCollecting(t)
	// A multi-line snippet: the first line carries the ordinal, the rest are
	// context indented two spaces. A trailing newline must not produce a
	// phantom context line.
	hits := []*rafikiv1.SearchResponse_SearchHit{
		{ChildId: "c_one", SessionName: "alpha", Snippet: "hit line\ncontext after\n"},
	}
	var buf bytes.Buffer
	c.Require().NoError(renderSearchText(&buf, hits), "renderSearchText")
	want := "== c_one alpha\n1: hit line\n  context after\n"
	c.Eq(want, buf.String(), "output")
}

func TestSearchTextHeaderFallsBackToSessionFile(t *testing.T) {
	hits := []*rafikiv1.SearchResponse_SearchHit{
		{ChildId: "c_one", SessionFile: "/sessions/c_one.jsonl", Snippet: "hit"},
	}
	var buf bytes.Buffer
	assert.NewAborting(t).NoError(renderSearchText(&buf, hits), "renderSearchText")
	if !strings.HasPrefix(buf.String(), "== c_one /sessions/c_one.jsonl\n") {
		t.Errorf("header should fall back to the session file; output:\n%s", buf.String())
	}
}

func TestSearchTextNoHitsWritesNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(renderSearchText(&buf, nil), "renderSearchText")
	c.Eq(0, buf.Len(), "zero hits should write nothing, got %q", buf.String())
}

// Search's JSONL is one canonical protojson hit object per line, unwrapped.
func TestSearchJSONLHitPerLine(t *testing.T) {
	c := assert.NewCollecting(t)
	hits := []*rafikiv1.SearchResponse_SearchHit{
		{ChildId: "c_one", Snippet: "first"},
		{ChildId: "c_two", Snippet: "second"},
	}
	var buf bytes.Buffer
	c.Require().NoError(emitProtoRows(&buf, hits, outputJSONL), "emitProtoRows")
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	c.Require().Len(lines, 2, "got %d lines, want one per hit: %q", len(lines), buf.String())
	// Each line is the hit's canonical protojson, compact.
	c.Eq(`{"childId":"c_one","snippet":"first"}`, lines[0], "line 0")
	c.Eq(`{"childId":"c_two","snippet":"second"}`, lines[1], "line 1")
}
