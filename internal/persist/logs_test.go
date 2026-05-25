package persist_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graveland.dev/pi-controller/internal/persist"
)

func TestLogDump_AlwaysMode_WritesAllStreams(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)

	exitInfo := persist.ExitInfo{ExitCode: 0}
	in := [][]byte{[]byte(`{"type":"prompt","message":"hi"}`)}
	out := [][]byte{[]byte(`{"type":"agent_start"}`), []byte(`{"type":"agent_end"}`)}
	err := []byte("warning: trivial\n")
	meta := persist.Meta{ChildID: "c_1", Cwd: "/x"}

	if e := d.Dump("c_1", in, out, err, meta, exitInfo); e != nil {
		t.Fatal(e)
	}

	childDir := filepath.Join(dir, "c_1")
	for _, name := range []string{"in.jsonl.gz", "out.jsonl.gz", "err.log.gz", "meta.json"} {
		if _, e := os.Stat(filepath.Join(childDir, name)); e != nil {
			t.Fatalf("missing: %s (%v)", name, e)
		}
	}

	// Check that out.jsonl.gz contains the two events in order.
	got := readGzLines(t, filepath.Join(childDir, "out.jsonl.gz"))
	if len(got) != 2 ||
		!strings.Contains(got[0], "agent_start") ||
		!strings.Contains(got[1], "agent_end") {
		t.Fatalf("out content wrong: %v", got)
	}
}

func TestLogDump_OnFailure_SkipsCleanExit(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 0}
	d.Dump("c_1", nil, nil, nil, persist.Meta{ChildID: "c_1"}, exitInfo)
	if _, err := os.Stat(filepath.Join(dir, "c_1")); !os.IsNotExist(err) {
		t.Fatalf("dir created on clean exit in ModeOnFailure: %v", err)
	}
}

func TestLogDump_OnFailure_DumpsBadExit(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 1}
	d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil,
		persist.Meta{ChildID: "c_1"}, exitInfo)
	if _, err := os.Stat(filepath.Join(dir, "c_1", "out.jsonl.gz")); err != nil {
		t.Fatalf("expected dump on bad exit: %v", err)
	}
}

func TestLogDump_NeverMode(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeNever)
	d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil,
		persist.Meta{ChildID: "c_1"}, persist.ExitInfo{ExitCode: 1})
	if _, err := os.Stat(filepath.Join(dir, "c_1")); !os.IsNotExist(err) {
		t.Fatal("ModeNever wrote to disk")
	}
}

func readGzLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func TestLogDump_MetaJsonRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)
	in := persist.Meta{
		ChildID:     "c_1",
		Name:        "afk",
		Cwd:         "/tmp/x",
		Model:       "claude-sonnet-4",
		SessionFile: "/tmp/x/session.jsonl",
		SpawnedAt:   1716636789,
		ExitedAt:    1716636900,
		ExitCode:    1,
		ExitSignal:  "SIGTERM",
		Argv:        []string{"pi", "--mode", "rpc"},
	}
	if err := d.Dump("c_1", nil, nil, nil, in, persist.ExitInfo{ExitCode: 1}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "c_1", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got persist.Meta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal meta.json: %v\ncontent: %s", err, b)
	}
	if got.ChildID != in.ChildID || got.Name != in.Name || got.Cwd != in.Cwd ||
		got.Model != in.Model || got.SessionFile != in.SessionFile ||
		got.SpawnedAt != in.SpawnedAt || got.ExitedAt != in.ExitedAt ||
		got.ExitCode != in.ExitCode || got.ExitSignal != in.ExitSignal {
		t.Fatalf("meta round-trip mismatch:\n got %+v\n want %+v", got, in)
	}
	if len(got.Argv) != len(in.Argv) || got.Argv[0] != in.Argv[0] {
		t.Fatalf("argv mismatch: got %v, want %v", got.Argv, in.Argv)
	}
}
