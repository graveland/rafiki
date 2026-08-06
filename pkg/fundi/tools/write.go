package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	writeDescription = "Write content to a file, replacing it entirely. " +
		"Use `path` (or `file_path`, an alias) — absolute or relative to the " +
		"working directory. Creates parent directories as needed. If the file " +
		"already exists, it must have been read (via the read tool) since its " +
		"last on-disk modification — write refuses to blindly overwrite a file " +
		"it hasn't seen the current contents of."
	writeSchema = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file to write (absolute or relative). Also accepts file_path as an alias."},
			"file_path": {"type": "string", "description": "Alias for path."},
			"content": {"type": "string", "description": "Full content to write to the file."}
		},
		"required": ["content"]
	}`
)

type writeInput struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// newWriteTool builds the write ToolFunc. It consults tr to refuse
// overwriting an existing file that hasn't been read since its last
// modification, and records its own write in tr so an immediately following
// edit doesn't need a redundant read.
func newWriteTool(tr *FileTracker, cwd string) ToolFunc {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in writeInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("write: invalid input: %w", err)
		}

		absPath, err := resolveToolPath(in.Path, in.FilePath, cwd)
		if err != nil {
			return "", fmt.Errorf("write: %w", err)
		}

		// Fail fast on an already-aborted turn rather than doing work whose
		// result nobody will see — agentloop runs a batch of tool calls
		// concurrently, so a call can still be starting after the turn's
		// context was canceled.
		if err := ctx.Err(); err != nil {
			return "", err
		}

		// Everything from here down is a read-modify-write (stat, verify,
		// write, record). rafiki's agentloop runs a tool batch concurrently,
		// so hold the per-path lock across the whole sequence — otherwise a
		// concurrent write or edit on the same file can interleave between the
		// verify and the write and have its change silently discarded.
		unlock := tr.Lock(absPath)
		defer unlock()

		mode := os.FileMode(0o644)
		if existing, err := os.Stat(absPath); err == nil {
			if existing.IsDir() {
				return "", fmt.Errorf("write: %q is a directory, not a file", absPath)
			}
			if err := tr.Verify(absPath); err != nil {
				return "", fmt.Errorf("write: %w", err)
			}
			mode = existing.Mode()
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("write: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return "", fmt.Errorf("write: create parent directories: %w", err)
		}
		if err := os.WriteFile(absPath, []byte(in.Content), mode); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}

		info, err := os.Stat(absPath)
		if err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		tr.RecordRead(absPath, info.ModTime())
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), absPath), nil
	}
}
