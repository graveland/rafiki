package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	editDescription = "Edit a file using exact text replacement. Matching is " +
		"exact first, then fuzzy (smart quotes, unicode dashes, trailing " +
		"whitespace), then whitespace-insensitive: if old_string differs only " +
		"in indentation or tabs-vs-spaces the matching lines are still edited " +
		"and new_string is re-indented to the file's style, with the response " +
		"saying so — verify the result. " +
		"Use `path` (or `file_path`, an alias) — absolute or relative to the " +
		"working directory. The file must have been read via the read tool in " +
		"this session, or the edit will fail. " +
		"Provide one or more replacements in `edits[]`, each with `old_string` and " +
		"`new_string`. By default all edits are matched against the same original " +
		"content (not incrementally) — overlapping or nested edits are rejected. " +
		"Set `sequential: true` to apply edits in order, each seeing the result " +
		"of the previous one. " +
		"Also accepts legacy `old_string` + `new_string` top-level fields for a " +
		"single replacement. Set `replace_all: true` to replace every occurrence."
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
			{Name: "edits", Type: "array", Description: "One or more targeted replacements. By default matched against the original file, not incrementally.",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "old_string", Type: "string", Description: "Exact text to replace. Must match exactly once in the target content."},
						{Name: "new_string", Type: "string", Description: "Text to replace old_string with."},
					},
					Required: []string{"old_string", "new_string"},
				},
			},
			{Name: "old_string", Type: "string", Description: "Exact text to replace. Must match exactly once unless replace_all is true."},
			{Name: "new_string", Type: "string", Description: "Text to replace old_string with."},
			{Name: "replace_all", Type: "boolean", Description: "Replace every occurrence of old_string instead of requiring exactly one match."},
			{Name: "sequential", Type: "boolean", Description: "When true, apply edits in order, each seeing the result of the previous edit. Default (false) matches all edits against the original content."},
		},
	}
}
func (EditBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (EditBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &editTool{EditBlueprint: EditBlueprint{}, tr: opts.FileTracker, cwd: opts.Cwd, changed: opts.FileChanged}, nil
}

type editTool struct {
	EditBlueprint
	tr  *FileTracker
	cwd string
	// changed keeps a language server's view in sync after a write. See
	// notifyFileChanged.
	changed FileChangeNotifier
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

	var edits []editPair
	for i, e := range in.Edits {
		if e.NewString == nil {
			return ToolResult{}, fmt.Errorf("edit: edits[%d] is missing new_string (use \"\" to delete the text)", i)
		}
		edits = append(edits, editPair{OldString: e.OldString, NewString: *e.NewString})
	}
	if len(edits) == 0 && in.OldString != "" {
		if in.NewString == nil {
			return ToolResult{}, fmt.Errorf("edit: old_string given without new_string (use \"\" to delete the text)")
		}
		edits = []editPair{{OldString: in.OldString, NewString: *in.NewString}}
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
		if in.NewString == nil {
			return ToolResult{}, fmt.Errorf("edit: old_string given without new_string (use \"\" to delete the text)")
		}
		lfNew := normalizeToLF(*in.NewString)
		count := strings.Count(lfContent, lfOld)
		if count == 0 {
			msg := fmt.Sprintf("old_string not found in %s", absPath)
			if hint := diagnoseMismatch(lfContent, lfOld); hint != "" {
				msg += "\n\n" + hint
			}
			return ToolResult{}, fmt.Errorf("edit: %s", msg)
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
		notifyFileChanged(ctx, et.changed, absPath)
		return NewTextResult(fmt.Sprintf("replaced %d occurrence(s) in %s", count, absPath)), nil
	}

	newContent, whitespaceCorrected, err := resolveEdits(lfContent, edits, in.Sequential)
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
	notifyFileChanged(ctx, et.changed, absPath)
	n := len(edits)
	msg := fmt.Sprintf("replaced %d block in %s", n, absPath)
	if n != 1 {
		msg = fmt.Sprintf("replaced %d blocks in %s", n, absPath)
	}
	if whitespaceCorrected {
		msg += "\n" + whitespaceCorrectedNote
	}
	return NewTextResult(msg), nil
}

type editInput struct {
	Path       string     `json:"path"`
	FilePath   string     `json:"file_path"`
	Edits      []editWire `json:"edits"`
	OldString  string     `json:"old_string"`
	NewString  *string    `json:"new_string"`
	ReplaceAll bool       `json:"replace_all"`
	Sequential bool       `json:"sequential"`
}

// editWire is editPair as decoded off the wire: a pointer NewString tells an
// omitted field apart from an intentional empty replacement.
type editWire struct {
	OldString string  `json:"old_string"`
	NewString *string `json:"new_string"`
}

type editPair struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
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
