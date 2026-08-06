package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	editDescription = "Edit a file using exact text replacement (fuzzy fallback). " +
		"Use `path` (or `file_path`, an alias) — absolute or relative to the " +
		"working directory. The file must have been read via the read tool in " +
		"this session, or the edit will fail. " +
		"Provide one or more replacements in `edits[]`, each with `old_string` and " +
		"`new_string`. All edits are matched against the same original content " +
		"(not incrementally). Overlapping or nested edits are rejected — merge " +
		"adjacent changes into one edit instead. " +
		"Also accepts legacy `old_string` + `new_string` top-level fields for a " +
		"single replacement. Set `replace_all: true` to replace every occurrence."
	editSchema = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file to edit (absolute or relative). Also accepts file_path as an alias."},
			"file_path": {"type": "string", "description": "Alias for path."},
			"edits": {
				"type": "array",
				"description": "One or more targeted replacements. Each edit is matched against the original file, not incrementally.",
				"items": {
					"type": "object",
					"properties": {
						"old_string": {"type": "string", "description": "Exact text to replace. Must match exactly once in the original file."},
						"new_string": {"type": "string", "description": "Text to replace old_string with."}
					},
					"required": ["old_string", "new_string"]
				}
			},
			"old_string": {"type": "string", "description": "Exact text to replace. Must match exactly once unless replace_all is true."},
			"new_string": {"type": "string", "description": "Text to replace old_string with."},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence of old_string instead of requiring exactly one match."}
		},
		"required": []
	}`
)

type editInput struct {
	Path       string     `json:"path"`
	FilePath   string     `json:"file_path"`
	Edits      []editPair `json:"edits"`
	OldString  string     `json:"old_string"`
	NewString  string     `json:"new_string"`
	ReplaceAll bool       `json:"replace_all"`
}

type editPair struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// newEditTool builds the edit ToolFunc. It requires tr to already hold a fresh
// read of path (see FileTracker.Verify) and records its own edit in tr so a
// chained edit on the same path doesn't need a redundant read.
func newEditTool(tr *FileTracker, cwd string) ToolFunc {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in editInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("edit: invalid input: %w", err)
		}

		absPath, err := resolveToolPath(in.Path, in.FilePath, cwd)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		// Normalize: legacy top-level old_string/new_string → single-entry edits.
		edits := in.Edits
		if len(edits) == 0 && in.OldString != "" {
			edits = []editPair{{OldString: in.OldString, NewString: in.NewString}}
		}
		if len(edits) == 0 {
			return "", fmt.Errorf("edit: at least one edit in edits[] or old_string is required")
		}

		if err := ctx.Err(); err != nil {
			return "", err
		}

		// Lock → verify → read → match → write → record.
		unlock := tr.Lock(absPath)
		defer unlock()

		if err := tr.Verify(absPath); err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		raw, err := os.ReadFile(absPath)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		// Prepare content: strip BOM, normalize line endings.
		rawStr := string(raw)
		hasBOM := strings.HasPrefix(rawStr, "\uFEFF")
		lfContent, origLE := prepContent(rawStr)

		// Handle replace_all with the legacy interface.
		if in.ReplaceAll && in.OldString != "" {
			lfOld := normalizeToLF(in.OldString)
			lfNew := normalizeToLF(in.NewString)
			count := strings.Count(lfContent, lfOld)
			if count == 0 {
				return "", fmt.Errorf("edit: old_string not found in %s", absPath)
			}
			updated := strings.ReplaceAll(lfContent, lfOld, lfNew)
			final := restoreLineEndings(updated, origLE)
			if hasBOM {
				final = "\uFEFF" + final
			}
			if err := writeFinal(absPath, rawStr, final); err != nil {
				return "", fmt.Errorf("edit: %w", err)
			}
			tr.RecordRead(absPath, fileMtime(absPath))
			return fmt.Sprintf("replaced %d occurrence(s) in %s", count, absPath), nil
		}

		// Apply edits through the fuzzy-aware pipeline.
		_, newContent, err := applyEdits(lfContent, edits)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		// Restore BOM and line endings, then write.
		final := restoreLineEndings(newContent, origLE)
		if hasBOM {
			final = "\uFEFF" + final
		}

		if err := writeFinal(absPath, rawStr, final); err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		tr.RecordRead(absPath, fileMtime(absPath))
		n := len(edits)
		if n == 1 {
			return fmt.Sprintf("replaced 1 block in %s", absPath), nil
		}
		return fmt.Sprintf("replaced %d blocks in %s", n, absPath), nil
	}
}

// writeFinal overwrites path with content, preserving the original file mode.
func writeFinal(path, original, content string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), info.Mode())
}

// fileMtime returns the mod time of path, panicking on error since we just
// verified and wrote it.
func fileMtime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		// We just wrote the file; this shouldn't happen.
		return time.Time{}
	}
	return info.ModTime()
}
