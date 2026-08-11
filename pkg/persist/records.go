// Package persist provides atomic crash-safe JSON state records for pi-controller
// children. Each child gets one file: <dir>/<childId>.json. Writes use
// write-to-temp-then-rename so a mid-write crash never leaves a partial file.
//
// Cross-reference: §11.5 of the pi-controller protocol spec.
package persist

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Record mirrors §11.5. Field names and JSON tags match the spec exactly.
// Optional fields use omitempty. APIKey is always null on disk.
type Record struct {
	ChildID        string   `json:"childId"`
	Name           string   `json:"name,omitempty"`
	Cwd            string   `json:"cwd"`
	Kind           string   `json:"kind,omitempty"`
	ConfigDir      string   `json:"configDir,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	Thinking       string   `json:"thinking,omitempty"`
	APIKey         *string  `json:"apiKey"` // always null on disk — never persisted
	SessionFile    string   `json:"sessionFile,omitempty"`
	SessionID      string   `json:"sessionId,omitempty"`
	SessionDir     string   `json:"sessionDir,omitempty"`
	NoSession      bool     `json:"noSession,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	NoTools        bool     `json:"noTools,omitempty"`
	NoBuiltinTools bool     `json:"noBuiltinTools,omitempty"`
	Extensions     []string `json:"extensions,omitempty"`
	NoExtensions   bool     `json:"noExtensions,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	NoSkills       bool     `json:"noSkills,omitempty"`
	// SkillsDirs and MCPConfig are additive fields (Task C1). Records written
	// before their introduction lack these keys, which deserialise to the zero
	// value (nil slice / empty string) — the same as never having set them.
	SkillsDirs         []string `json:"skillsDirs,omitempty"`
	MCPConfig          string   `json:"mcpConfig,omitempty"`
	PromptTemplates    []string `json:"promptTemplates,omitempty"`
	NoPromptTemplates  bool     `json:"noPromptTemplates,omitempty"`
	Themes             []string `json:"themes,omitempty"`
	NoThemes           bool     `json:"noThemes,omitempty"`
	NoContextFiles     bool     `json:"noContextFiles,omitempty"`
	SystemPrompt       string   `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	Verbose            bool     `json:"verbose,omitempty"`
	PiBinary           string   `json:"piBinary,omitempty"`
	ExtraArgs          []string `json:"extraArgs,omitempty"`

	// RecordRequests is an additive field (like SkillsDirs/MCPConfig above):
	// records written before its introduction lack the key, which
	// deserialises to false — the same as never having set it.
	RecordRequests bool `json:"recordRequests,omitempty"`

	// Resource grants (phase 05). Additive: records written before their
	// introduction lack these keys and deserialise to zero. A zero MaxDepth
	// on an old record means "cannot spawn", which is the safe direction —
	// resuming an old child does not silently grant it a subtree.
	MaxDepth    int     `json:"maxDepth,omitempty"`
	MaxCost     float64 `json:"maxCost,omitempty"`
	MaxChildren int     `json:"maxChildren,omitempty"`

	SpawnedAt     int64  `json:"spawnedAt"`
	LastSeenAlive int64  `json:"lastSeenAlive"`
	PID           int    `json:"pid"`
	LastStatus    string `json:"lastStatus"`

	// Exit info — populated when LastStatus is "exited".  Missing fields
	// deserialise to zero values, which the renderer treats as "unknown".
	ExitedAt   int64  `json:"exitedAt,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	ExitSignal string `json:"exitSignal,omitempty"`

	// Labels stores arbitrary user-defined and auto-derived key=value metadata.
	// Records without this field deserialise to nil.
	Labels map[string]string `json:"labels,omitempty"`
}

// RecordWriter writes child state records to a directory.
type RecordWriter struct {
	dir string
}

// NewRecordWriter returns a writer targeting dir (created on first Write).
func NewRecordWriter(dir string) *RecordWriter {
	return &RecordWriter{dir: dir}
}

// Write serializes rec to <dir>/<childId>.json using an atomic rename.
// The APIKey field is cleared before writing — it must never reach disk.
func (w *RecordWriter) Write(rec Record) error {
	if rec.ChildID == "" {
		return fmt.Errorf("persist: record has empty childId")
	}
	// Security: apiKey must never reach disk.
	rec.APIKey = nil

	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}

	final := filepath.Join(w.dir, rec.ChildID+".json")
	tmp, err := os.CreateTemp(w.dir, rec.ChildID+".*.tmp")
	if err != nil {
		return err
	}
	// Remove the temp file if we exit before renaming it.
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// ReadRecord deserializes a single record from path.
func ReadRecord(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// ScanRecords reads all valid records from dir. Files that are not .json,
// are .tmp, or contain malformed JSON are skipped with a warning. Returns
// (nil, nil) if the directory does not exist (fresh controller start).
func ScanRecords(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// Skip stray temp files left from crashed renames.
		if strings.Contains(name, ".tmp") {
			continue
		}
		rec, err := ReadRecord(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("persist: ignoring malformed record",
				"file", name, "error", err)
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// DeleteRecord removes <dir>/<childID>.json. It is idempotent: no error is
// returned if the file does not exist.
func DeleteRecord(dir, childID string) error {
	err := os.Remove(filepath.Join(dir, childID+".json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
