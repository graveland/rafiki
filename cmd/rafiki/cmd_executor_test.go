package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestFilterExecutorsForDelete(t *testing.T) {
	execs := []executors.Executor{
		{ID: "disabled-online", Enabled: false, Connected: true},
		{ID: "enabled-offline", Enabled: true, Connected: false},
		{ID: "enabled-online", Enabled: true, Connected: true},
		{ID: "disabled-offline", Enabled: false, Connected: false},
	}

	ids := func(got []executors.Executor) []string {
		out := make([]string, len(got))
		for i, e := range got {
			out[i] = e.ID
		}
		return out
	}

	t.Run("all-disabled selects Enabled==false regardless of connection", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, true, false))
		want := []string{"disabled-online", "disabled-offline"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("all-offline selects Connected==false regardless of enabled", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, false, true))
		want := []string{"enabled-offline", "disabled-offline"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("both flags union rather than intersect", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, true, true))
		want := []string{"disabled-online", "enabled-offline", "disabled-offline"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("neither flag selects nothing", func(t *testing.T) {
		got := filterExecutorsForDelete(execs, false, false)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
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

// Connected is a live-pool view field distinct from Enabled/LastSeenAt — a
// client wants to know how long the CURRENT connection has held, separate
// from whether the row is enabled or when it was last seen at all.
func TestRenderExecutorTableShowsConnectedSince(t *testing.T) {
	connectedAt := time.Now().Add(-90 * time.Second)
	execs := []executors.Executor{
		{ID: fixtureTailA, Enabled: true, Connected: true, ConnectedAt: &connectedAt},
		{ID: fixtureTailB, Enabled: true}, // not connected: ConnectedAt nil
	}
	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, execs, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CONNECTED") {
		t.Errorf("output missing the CONNECTED column header:\n%s", out)
	}
	if !strings.Contains(out, "ago") {
		t.Errorf("expected a relative connected-since time for the live executor:\n%s", out)
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

func TestApplyMachineLabelAddsIt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	labels := map[string]string{"role": "laptop"}

	if err := applyMachineLabel(labels); err != nil {
		t.Fatal(err)
	}
	if labels["machine"] == "" {
		t.Fatal("a durable executor on this box must carry machine=<id> " +
			"or an interactive client can never find it")
	}
	if labels["role"] != "laptop" {
		t.Fatal("existing labels must survive")
	}
}

func TestApplyMachineLabelDoesNotOverrideAnExplicitOne(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	labels := map[string]string{"machine": "operator-chose-this"}

	if err := applyMachineLabel(labels); err != nil {
		t.Fatal(err)
	}
	if labels["machine"] != "operator-chose-this" {
		t.Fatalf("an explicit --label machine= must win, got %q", labels["machine"])
	}
}
