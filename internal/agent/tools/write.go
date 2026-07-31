package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	writeDescription = "Write content to a file, replacing it entirely. path must be absolute. " +
		"Creates parent directories as needed. If the file already exists, it must " +
		"have been read (via the read tool) since its last on-disk modification — " +
		"write refuses to blindly overwrite a file it hasn't seen the current " +
		"contents of."
	writeSchema = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Absolute path to the file to write."},
			"content": {"type": "string", "description": "Full content to write to the file."}
		},
		"required": ["path", "content"]
	}`
)

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// newWriteTool builds the write ToolFunc. It consults tr to refuse
// overwriting an existing file that hasn't been read since its last
// modification, and records its own write in tr so an immediately following
// edit doesn't need a redundant read.
func newWriteTool(tr *FileTracker) ToolFunc {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var in writeInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("write: invalid input: %w", err)
		}
		if in.Path == "" {
			return "", fmt.Errorf("write: path is required")
		}
		if !filepath.IsAbs(in.Path) {
			return "", fmt.Errorf("write: path must be absolute, got %q", in.Path)
		}

		// Everything from here down is a read-modify-write (stat, verify,
		// write, record). rafiki's agentloop runs a tool batch concurrently,
		// so hold the per-path lock across the whole sequence — otherwise a
		// concurrent write or edit on the same file can interleave between the
		// verify and the write and have its change silently discarded.
		unlock := tr.Lock(in.Path)
		defer unlock()

		mode := os.FileMode(0o644)
		if existing, err := os.Stat(in.Path); err == nil {
			if existing.IsDir() {
				return "", fmt.Errorf("write: %q is a directory, not a file", in.Path)
			}
			if err := tr.Verify(in.Path); err != nil {
				return "", fmt.Errorf("write: %w", err)
			}
			mode = existing.Mode()
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("write: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
			return "", fmt.Errorf("write: create parent directories: %w", err)
		}
		if err := os.WriteFile(in.Path, []byte(in.Content), mode); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}

		info, err := os.Stat(in.Path)
		if err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		tr.RecordRead(in.Path, info.ModTime())
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
	}
}
