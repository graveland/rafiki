package tools

import (
	"context"
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

func init() { DefaultBlueprint.Register(&WriteBlueprint{}) }

type WriteBlueprint struct{}

func (WriteBlueprint) Name() string        { return "write" }
func (WriteBlueprint) Description() string { return writeDescription }
func (WriteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "Path to the file to write (absolute or relative). Also accepts file_path as an alias."},
			{Name: "file_path", Type: "string", Description: "Alias for path."},
			{Name: "content", Type: "string", Description: "Full content to write to the file."},
		},
		Required: []string{"content"},
	}
}
func (WriteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (WriteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &writeTool{WriteBlueprint: WriteBlueprint{}, tr: opts.FileTracker, cwd: opts.Cwd}, nil
}

type writeTool struct {
	WriteBlueprint
	tr  *FileTracker
	cwd string
}

func (wt *writeTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in writeInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("write: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, in.FilePath, wt.cwd)
	if err != nil {
		return ToolResult{}, fmt.Errorf("write: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	unlock := wt.tr.Lock(absPath)
	defer unlock()

	mode := os.FileMode(0o644)
	if existing, err := os.Stat(absPath); err == nil {
		if existing.IsDir() {
			return ToolResult{}, fmt.Errorf("write: %q is a directory, not a file", absPath)
		}
		if err := wt.tr.Verify(absPath); err != nil {
			return ToolResult{}, fmt.Errorf("write: %w", err)
		}
		mode = existing.Mode()
	} else if !os.IsNotExist(err) {
		return ToolResult{}, fmt.Errorf("write: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return ToolResult{}, fmt.Errorf("write: create parent directories: %w", err)
	}
	if err := os.WriteFile(absPath, []byte(in.Content), mode); err != nil {
		return ToolResult{}, fmt.Errorf("write: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("write: %w", err)
	}
	wt.tr.RecordRead(absPath, info.ModTime())
	return NewTextResult(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), absPath)), nil
}

type writeInput struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}
