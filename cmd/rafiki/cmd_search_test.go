// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestSearchTextGroupsByChild(t *testing.T) {
	hits := []protocol.SearchHit{
		{ChildID: "c_one", SessionName: "alpha", Snippet: "first match in one"},
		{ChildID: "c_one", SessionName: "alpha", Snippet: "second match in one"},
		{ChildID: "c_two", SessionName: "beta", Snippet: "match in two"},
	}
	var buf bytes.Buffer
	if err := renderSearchText(&buf, hits); err != nil {
		t.Fatalf("renderSearchText: %v", err)
	}
	out := buf.String()

	one := strings.Index(out, "== c_one alpha")
	two := strings.Index(out, "== c_two beta")
	if one < 0 || two < 0 {
		t.Fatalf("missing group headers; output:\n%s", out)
	}
	if two < one {
		t.Errorf("groups must appear in first-appearance order; output:\n%s", out)
	}
	// Hits render as `<ordinal>: <line>`; ordinals restart per group.
	if !strings.Contains(out[one:two], "1: first match in one") ||
		!strings.Contains(out[one:two], "2: second match in one") {
		t.Errorf("c_one hits not numbered under their header; output:\n%s", out)
	}
	if !strings.Contains(out[two:], "1: match in two") {
		t.Errorf("c_two ordinal should restart within its group; output:\n%s", out)
	}
	// c_two's hit must not have leaked into c_one's group.
	if strings.Contains(out[one:two], "match in two") {
		t.Errorf("c_two's hit rendered inside c_one's group; output:\n%s", out)
	}
}

func TestSearchTextIndentsContextLines(t *testing.T) {
	// A multi-line snippet: the first line carries the ordinal, the rest are
	// context indented two spaces. A trailing newline must not produce a
	// phantom context line.
	hits := []protocol.SearchHit{
		{ChildID: "c_one", SessionName: "alpha", Snippet: "hit line\ncontext after\n"},
	}
	var buf bytes.Buffer
	if err := renderSearchText(&buf, hits); err != nil {
		t.Fatalf("renderSearchText: %v", err)
	}
	want := "== c_one alpha\n1: hit line\n  context after\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}

func TestSearchTextHeaderFallsBackToSessionFile(t *testing.T) {
	hits := []protocol.SearchHit{
		{ChildID: "c_one", SessionFile: "/sessions/c_one.jsonl", Snippet: "hit"},
	}
	var buf bytes.Buffer
	if err := renderSearchText(&buf, hits); err != nil {
		t.Fatalf("renderSearchText: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "== c_one /sessions/c_one.jsonl\n") {
		t.Errorf("header should fall back to the session file; output:\n%s", buf.String())
	}
}

func TestSearchTextNoHitsWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSearchText(&buf, nil); err != nil {
		t.Fatalf("renderSearchText: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("zero hits should write nothing, got %q", buf.String())
	}
}

func TestSearchJSONLHitPerLine(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"childId":"c_one","snippet":"first"}`),
		json.RawMessage(`{"childId":"c_two","snippet":"second"}`),
	}
	var buf bytes.Buffer
	if err := renderSearchJSONL(&buf, raw); err != nil {
		t.Fatalf("renderSearchJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one per hit: %q", len(lines), buf.String())
	}
	// Each line is the raw hit object, passed through byte-exactly — not
	// decoded and re-encoded.
	if lines[0] != `{"childId":"c_one","snippet":"first"}` {
		t.Errorf("line 0 = %q, want the raw hit object", lines[0])
	}
	if lines[1] != `{"childId":"c_two","snippet":"second"}` {
		t.Errorf("line 1 = %q, want the raw hit object", lines[1])
	}
}
