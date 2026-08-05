package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	editDescription = "Replace text in a file. Use parameter `path` (absolute path, required) " +
		"— the file must have been read via the read tool in this session, or the " +
		"edit will fail. `old_string` must match the file's current contents " +
		"exactly once; set `replace_all: true` to replace every occurrence instead."
	editSchema = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Absolute path to the file to edit."},
			"old_string": {"type": "string", "description": "Exact text to replace. Must match exactly once unless replace_all is true."},
			"new_string": {"type": "string", "description": "Text to replace old_string with."},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence of old_string instead of requiring exactly one match."}
		},
		"required": ["path", "old_string", "new_string"]
	}`
)

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// newEditTool builds the edit ToolFunc. It requires tr to already hold a
// fresh read of path (see FileTracker.Verify) and records its own edit in tr
// so a chained edit on the same path doesn't need a redundant read.
func newEditTool(tr *FileTracker) ToolFunc {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in editInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("edit: invalid input: %w", err)
		}
		if in.Path == "" {
			return "", fmt.Errorf("edit: path is required")
		}
		if !filepath.IsAbs(in.Path) {
			return "", fmt.Errorf("edit: path must be absolute, got %q", in.Path)
		}
		if in.OldString == "" {
			return "", fmt.Errorf("edit: old_string is required")
		}
		// Fail fast on an already-aborted turn rather than doing work whose
		// result nobody will see — agentloop runs a batch of tool calls
		// concurrently, so a call can still be starting after the turn's
		// context was canceled.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Everything from here down is a read-modify-write. rafiki's agentloop
		// runs a tool batch concurrently, and a model emitting two edits on
		// one file in a single batch is routine — without this lock both would
		// verify, both would read the pre-state, and the second write would
		// silently discard the first while reporting success. Hold it across
		// verify, read, compute, write, and RecordRead.
		unlock := tr.Lock(in.Path)
		defer unlock()

		if err := tr.Verify(in.Path); err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		content, err := os.ReadFile(in.Path)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}
		text := string(content)
		count := strings.Count(text, in.OldString)
		switch {
		case count == 0:
			return "", fmt.Errorf("edit: old_string not found in %s", in.Path)
		case count > 1 && !in.ReplaceAll:
			return "", fmt.Errorf("edit: old_string matches %d times in %s; add more surrounding context to make it unique, or set replace_all", count, in.Path)
		}

		var updated string
		if in.ReplaceAll {
			updated = strings.ReplaceAll(text, in.OldString, in.NewString)
		} else {
			updated = strings.Replace(text, in.OldString, in.NewString, 1)
		}

		existing, err := os.Stat(in.Path)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}
		if err := os.WriteFile(in.Path, []byte(updated), existing.Mode()); err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		info, err := os.Stat(in.Path)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}
		tr.RecordRead(in.Path, info.ModTime())
		return fmt.Sprintf("replaced %d occurrence(s) in %s", count, in.Path), nil
	}
}
