package persist_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/persist"
	"git.graveland.dev/brent/fundi/protocol"
)

func TestRecordWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	apiKey := "secret-key"
	rec := persist.Record{
		ChildID:       "c_1",
		Name:          "afk",
		Cwd:           "/tmp/x",
		Model:         "claude-sonnet-4",
		SessionFile:   "/tmp/x/session.jsonl",
		SpawnedAt:     time.Now().Unix(),
		LastSeenAlive: time.Now().Unix(),
		PID:           12345,
		LastStatus:    string(protocol.StatusStreaming),
		APIKey:        &apiKey,
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}

	got, err := persist.ReadRecord(filepath.Join(dir, "c_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ChildID != "c_1" || got.Name != "afk" {
		t.Fatalf("got %+v", got)
	}
	// apiKey must be nil on disk regardless of what was passed.
	if got.APIKey != nil {
		t.Fatalf("apiKey leaked to disk: %v", *got.APIKey)
	}
}

func TestRecordWriter_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	if err := w.Write(persist.Record{ChildID: "c_1", PID: 1}); err != nil {
		t.Fatal(err)
	}
	// No .tmp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestScanRecords_LoadsAndIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)
	for i := 0; i < 3; i++ {
		w.Write(persist.Record{ChildID: "c_" + string(rune('a'+i)), PID: i})
	}
	// Write garbage that should be skipped.
	os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("ignore"), 0o600)

	recs, err := persist.ScanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
}

func TestScanRecords_MissingDir(t *testing.T) {
	recs, err := persist.ScanRecords("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if recs != nil {
		t.Fatalf("expected nil records for missing dir, got: %v", recs)
	}
}

func TestRecordWriter_LabelsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	rec := persist.Record{
		ChildID: "c_label_test",
		Cwd:     "/tmp",
		Labels: map[string]string{
			"env":       "prod",
			"pic/model": "claude-sonnet-4",
		},
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}

	got, err := persist.ReadRecord(filepath.Join(dir, "c_label_test.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["env"] != "prod" {
		t.Errorf("env label: got %q, want %q", got.Labels["env"], "prod")
	}
	if got.Labels["pic/model"] != "claude-sonnet-4" {
		t.Errorf("pic/model label: got %q", got.Labels["pic/model"])
	}
}

func TestRecordWriter_LabelsNilRoundTrip(t *testing.T) {
	// Records without labels should deserialise to nil map (not empty map).
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	rec := persist.Record{ChildID: "c_no_labels", Cwd: "/tmp"}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}

	got, err := persist.ReadRecord(filepath.Join(dir, "c_no_labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	// omitempty means no "labels" key in JSON, so decoded value is nil.
	if got.Labels != nil {
		t.Fatalf("expected nil Labels for record without labels, got %v", got.Labels)
	}
}

func TestDeleteRecord(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)
	w.Write(persist.Record{ChildID: "c_1", PID: 1})
	if err := persist.DeleteRecord(dir, "c_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c_1.json")); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
	// Idempotent — deleting again must not error.
	if err := persist.DeleteRecord(dir, "c_1"); err != nil {
		t.Fatalf("second delete errored: %v", err)
	}
}
