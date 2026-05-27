package main

import (
	"bytes"
	"strings"
	"testing"

	"git.graveland.dev/brent/pi-controller/protocol"
)

func TestRenderList_Table(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: 1716636789},
	}
	if err := renderList(&buf, children, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"c_01HXABC", "afk-impl", "streaming", "claude-sonnet-4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderList_JSON(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Pretty-printed JSON has a space after the colon.
	if !strings.Contains(out, `"childId": "c_1"`) {
		t.Fatalf("JSON output: %s", out)
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
