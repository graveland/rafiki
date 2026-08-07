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

func init() { DefaultBlueprint.Register(&EditBlueprint{}) }

type EditBlueprint struct{}

func (EditBlueprint) Name() string        { return "edit" }
func (EditBlueprint) Description() string { return editDescription }
func (EditBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "Path to the file to edit (absolute or relative). Also accepts file_path as an alias."},
			{Name: "file_path", Type: "string", Description: "Alias for path."},
			{Name: "edits", Type: "array", Description: "One or more targeted replacements. Each edit is matched against the original file, not incrementally.",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "old_string", Type: "string", Description: "Exact text to replace. Must match exactly once in the original file."},
						{Name: "new_string", Type: "string", Description: "Text to replace old_string with."},
					},
					Required: []string{"old_string", "new_string"},
				},
			},
			{Name: "old_string", Type: "string", Description: "Exact text to replace. Must match exactly once unless replace_all is true."},
			{Name: "new_string", Type: "string", Description: "Text to replace old_string with."},
			{Name: "replace_all", Type: "boolean", Description: "Replace every occurrence of old_string instead of requiring exactly one match."},
		},
	}
}
func (EditBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (EditBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &editTool{EditBlueprint: EditBlueprint{}, tr: opts.FileTracker, cwd: opts.Cwd}, nil
}

type editTool struct {
	EditBlueprint
	tr  *FileTracker
	cwd string
}

func (et *editTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in editInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("edit: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, in.FilePath, et.cwd)
	if err != nil {
		return ToolResult{}, fmt.Errorf("edit: %w", err)
	}

	edits := in.Edits
	if len(edits) == 0 && in.OldString != "" {
		edits = []editPair{{OldString: in.OldString, NewString: in.NewString}}
	}
	if len(edits) == 0 {
		return ToolResult{}, fmt.Errorf("edit: at least one edit in edits[] or old_string is required")
	}

	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	unlock := et.tr.Lock(absPath)
	defer unlock()

	if err := et.tr.Verify(absPath); err != nil {
		return ToolResult{}, fmt.Errorf("edit: %w", err)
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("edit: %w", err)
	}

	rawStr := string(raw)
	hasBOM := strings.HasPrefix(rawStr, "\uFEFF")
	lfContent, origLE := prepContent(rawStr)

	if in.ReplaceAll && in.OldString != "" {
		lfOld := normalizeToLF(in.OldString)
		lfNew := normalizeToLF(in.NewString)
		count := strings.Count(lfContent, lfOld)
		if count == 0 {
			return ToolResult{}, fmt.Errorf("edit: old_string not found in %s", absPath)
		}
		updated := strings.ReplaceAll(lfContent, lfOld, lfNew)
		final := restoreLineEndings(updated, origLE)
		if hasBOM {
			final = "\uFEFF" + final
		}
		if err := writeFinal(absPath, rawStr, final); err != nil {
			return ToolResult{}, fmt.Errorf("edit: %w", err)
		}
		et.tr.RecordRead(absPath, fileMtime(absPath))
		return NewTextResult(fmt.Sprintf("replaced %d occurrence(s) in %s", count, absPath)), nil
	}

	_, newContent, err := applyEdits(lfContent, edits)
	if err != nil {
		return ToolResult{}, fmt.Errorf("edit: %w", err)
	}

	final := restoreLineEndings(newContent, origLE)
	if hasBOM {
		final = "\uFEFF" + final
	}

	if err := writeFinal(absPath, rawStr, final); err != nil {
		return ToolResult{}, fmt.Errorf("edit: %w", err)
	}

	et.tr.RecordRead(absPath, fileMtime(absPath))
	n := len(edits)
	if n == 1 {
		return NewTextResult(fmt.Sprintf("replaced 1 block in %s", absPath)), nil
	}
	return NewTextResult(fmt.Sprintf("replaced %d blocks in %s", n, absPath)), nil
}

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

		unlock := tr.Lock(absPath)
		defer unlock()

		if err := tr.Verify(absPath); err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		raw, err := os.ReadFile(absPath)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

		rawStr := string(raw)
		hasBOM := strings.HasPrefix(rawStr, "\uFEFF")
		lfContent, origLE := prepContent(rawStr)

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

		_, newContent, err := applyEdits(lfContent, edits)
		if err != nil {
			return "", fmt.Errorf("edit: %w", err)
		}

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

func writeFinal(path, original, content string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), info.Mode())
}

func fileMtime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
