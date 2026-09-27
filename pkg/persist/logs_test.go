package persist_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/persist"

	"github.com/multigres/testkit/assert"
)

func TestLogDump_AlwaysMode_WritesAllStreams(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)

	exitInfo := persist.ExitInfo{ExitCode: 0}
	in := [][]byte{[]byte(`{"type":"prompt","message":"hi"}`)}
	out := [][]byte{[]byte(`{"type":"agent_start"}`), []byte(`{"type":"agent_end"}`)}
	err := []byte("warning: trivial\n")
	meta := persist.Meta{ChildID: "c_1", Cwd: "/x"}

	c.NoError(d.Dump("c_1", in, out, nil, err, meta, exitInfo))

	childDir := filepath.Join(dir, "c_1")
	for _, name := range []string{"in.jsonl.gz", "out.jsonl.gz", "err.log.gz", "meta.json"} {
		_, e := os.Stat(filepath.Join(childDir, name))
		c.NoError(e, "missing: %s (%v)", name, e)
	}

	// Check that out.jsonl.gz contains the two events in order.
	got := readGzLines(t, filepath.Join(childDir, "out.jsonl.gz"))
	c.False(len(got) != 2 ||
		!strings.Contains(got[0], "agent_start") ||
		!strings.Contains(got[1], "agent_end"), "out content wrong: %v", got)
}

func TestLogDump_OnFailure_SkipsCleanExit(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 0}
	c.NoError(d.Dump("c_1", nil, nil, nil, nil, persist.Meta{ChildID: "c_1"}, exitInfo))
	_, err := os.Stat(filepath.Join(dir, "c_1"))
	c.True(os.IsNotExist(err), "dir created on clean exit in ModeOnFailure: %v", err)
}

func TestLogDump_OnFailure_DumpsBadExit(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 1}
	c.NoError(d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil, nil,
		persist.Meta{ChildID: "c_1"}, exitInfo))
	_, err := os.Stat(filepath.Join(dir, "c_1", "out.jsonl.gz"))
	c.NoError(err, "expected dump on bad exit")
}

func TestLogDump_NeverMode(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeNever)
	c.NoError(d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil, nil,
		persist.Meta{ChildID: "c_1"}, persist.ExitInfo{ExitCode: 1}))
	_, err := os.Stat(filepath.Join(dir, "c_1"))
	c.True(os.IsNotExist(err), "ModeNever wrote to disk")
}

func TestDumpWritesRenderJSONL(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)
	render := [][]byte{[]byte(`{"type":"message_start"}`), []byte(`{"type":"message_end"}`)}
	err := d.Dump("c1", nil, [][]byte{[]byte(`{"type":"system"}`)}, render, nil, persist.Meta{ChildID: "c1"}, persist.ExitInfo{})
	c.NoError(err, "Dump")
	got, err := persist.ReadGzLines(filepath.Join(dir, "c1", "render.jsonl.gz"))
	c.NoError(err, "ReadGzLines")
	c.False(len(got) != 2 || string(got[0]) != `{"type":"message_start"}`, "render.jsonl.gz = %v, want the 2 render frames", got)
}

func TestDumpSkipsRenderWhenNil(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)
	c.NoError(d.Dump("c1", nil, nil, nil, nil, persist.Meta{ChildID: "c1"}, persist.ExitInfo{}), "Dump")
	_, err := os.Stat(filepath.Join(dir, "c1", "render.jsonl.gz"))
	c.ErrorIs(err, fs.ErrNotExist, "render.jsonl.gz should not exist when render frames are nil")
}

func readGzLines(t *testing.T, path string) []string {
	t.Helper()
	c := assert.NewAborting(t)
	f, err := os.Open(path)
	c.NoError(err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	c.NoError(err)
	defer gz.Close()
	b, err := io.ReadAll(gz)
	c.NoError(err)
	parts := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func TestLogDump_MetaJsonRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(d.Dump("c_1", nil, nil, nil, nil, in, persist.ExitInfo{ExitCode: 1}))

	b, err := os.ReadFile(filepath.Join(dir, "c_1", "meta.json"))
	c.NoError(err)
	var got persist.Meta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal meta.json: %v\ncontent: %s", err, b)
	}
	c.False(got.ChildID != in.ChildID || got.Name != in.Name || got.Cwd != in.Cwd ||
		got.Model != in.Model || got.SessionFile != in.SessionFile ||
		got.SpawnedAt != in.SpawnedAt || got.ExitedAt != in.ExitedAt ||
		got.ExitCode != in.ExitCode || got.ExitSignal != in.ExitSignal, "meta round-trip mismatch:\n got %+v\n want %+v", got, in)
	c.False(len(got.Argv) != len(in.Argv) || got.Argv[0] != in.Argv[0], "argv mismatch: got %v, want %v", got.Argv, in.Argv)
}
