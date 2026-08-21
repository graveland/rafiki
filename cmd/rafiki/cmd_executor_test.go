package main

import (
	"bytes"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executors"
)

// Two rows minted in the same window: UUIDv7s share their leading timestamp
// bits, so everything before the final group is identical and only the tail
// distinguishes them.
const (
	fixturePrefix = "0198e5f2-9c3a-7def-8a1b-"
	fixtureTailA  = "4c2d9e0f1a2b"
	fixtureTailB  = "998877665544"
)

func TestShortExecutorIDKeepsTheTail(t *testing.T) {
	full := fixturePrefix + fixtureTailA
	if got := shortExecutorID(full); got != fixtureTailA {
		t.Fatalf("shortExecutorID(%s) = %s, want the tail %s", full, got, fixtureTailA)
	}
	if got := shortExecutorID(fixtureTailA); got != fixtureTailA {
		t.Fatalf("short ids must pass through unchanged, got %s", got)
	}
	if got := shortExecutorID("exactly12ch"); got != "exactly12ch" {
		t.Fatalf("ids of exactly %d chars must not be mangled, got %q", executorShortIDLen, got)
	}
}

func TestRenderExecutorTableShowsTailIDs(t *testing.T) {
	execs := []executors.Executor{
		{ID: fixturePrefix + fixtureTailA, DisplayName: "laptop",
			Enabled: true, Connected: true,
			Labels: map[string]string{"b": "2", "a": "1"}},
		{ID: fixturePrefix + fixtureTailB, DisplayName: "rack",
			Enabled: false,
			Labels:  map[string]string{"env": "work"}},
	}

	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, execs, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	out := buf.String()

	for _, want := range []string{fixtureTailA, fixtureTailB, "laptop", "rack", "live", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, fixturePrefix) {
		t.Errorf("the shared timestamp prefix must not be displayed:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("useColor=false must emit no ANSI escapes:\n%s", out)
	}
	if !strings.Contains(out, "a=1,b=2") {
		t.Errorf("labels must render sorted for stable output:\n%s", out)
	}
	for _, header := range []string{"ID", "NAME", "STATUS", "LABELS", "ADMITS", "LAST SEEN"} {
		if !strings.Contains(out, header) {
			t.Errorf("missing header %q:\n%s", header, out)
		}
	}
}

func TestRenderExecutorTableEmptyAndLastSeen(t *testing.T) {
	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, nil, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	if got := buf.String(); got != "No enrolled executors.\n" {
		t.Fatalf("empty pool renders %q, want the friendly line", got)
	}

	buf.Reset()
	execs := []executors.Executor{{ID: fixtureTailA, Enabled: true}} // LastSeenAt zero
	if err := renderExecutorTable(&buf, execs, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	if strings.Contains(buf.String(), "ago") {
		t.Fatalf("a zero last-seen must render as '-', not a relative time:\n%s", buf.String())
	}
}
