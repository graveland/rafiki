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
	editDescription = "Replace text in a file. path must be absolute and must have been read " +
		"(via the read tool) since its last on-disk modification. old_string must " +
		"match the file's current contents exactly once unless replace_all is set, " +
		"in which case every occurrence is replaced."
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
	return func(_ context.Context, input json.RawMessage) (string, error) {
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
