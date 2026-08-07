package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	defaultReadLimit = 2000

	readDescription = "Read a file from the local filesystem. " +
		"Use `path` (or `file_path`, an alias) — absolute or relative to the " +
		"working directory. Output is numbered like `cat -n` (1-indexed). By " +
		"default reads from the start of the file, up to 2000 lines; if the " +
		"file is longer, a trailing note tells you the offset to continue from. " +
		"Pass offset/limit to page through a large file explicitly."
	readSchema = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file to read (absolute or relative). Also accepts file_path as an alias."},
			"file_path": {"type": "string", "description": "Alias for path."},
			"offset": {"type": "integer", "description": "1-indexed line number to start reading from. Defaults to 1."},
			"limit": {"type": "integer", "description": "Maximum number of lines to return. Defaults to 2000."}
		},
		"required": []
	}`
)

func init() { DefaultBlueprint.Register(&ReadBlueprint{}) }

// ReadBlueprint is the static metadata for the read tool.
type ReadBlueprint struct{}

func (ReadBlueprint) Name() string        { return "read" }
func (ReadBlueprint) Description() string { return readDescription }
func (ReadBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "Path to the file to read (absolute or relative). Also accepts file_path as an alias."},
			{Name: "file_path", Type: "string", Description: "Alias for path."},
			{Name: "offset", Type: "integer", Description: "1-indexed line number to start reading from. Defaults to 1."},
			{Name: "limit", Type: "integer", Description: "Maximum number of lines to return. Defaults to 2000."},
		},
	}
}
func (ReadBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ReadBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &readTool{
		ReadBlueprint: ReadBlueprint{},
		tr:            opts.FileTracker,
		cwd:           opts.Cwd,
	}, nil
}

type readTool struct {
	ReadBlueprint
	tr  *FileTracker
	cwd string
}

func (rt *readTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in readInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("read: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, in.FilePath, rt.cwd)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	unlock := rt.tr.Lock(absPath)
	defer unlock()

	info, err := os.Stat(absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read: %w", err)
	}
	if info.IsDir() {
		return ToolResult{}, fmt.Errorf("read: %q is a directory, not a file", absPath)
	}

	f, err := os.Open(absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read: %w", err)
	}
	defer f.Close()

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	var out strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	lineNo := 0
	shown := 0
	lastShown := offset - 1
	truncated := false
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if shown == limit {
			truncated = true
			break
		}
		fmt.Fprintf(&out, "%6d\t%s\n", lineNo, scanner.Text())
		shown++
		lastShown = lineNo
	}
	if err := scanner.Err(); err != nil {
		return ToolResult{}, fmt.Errorf("read: %w", err)
	}

	rt.tr.RecordRead(absPath, info.ModTime())

	if shown == 0 {
		if lineNo == 0 {
			return NewTextResult("(empty file)\n"), nil
		}
		return NewTextResult(fmt.Sprintf("(no lines at or after offset %d; file has %d lines)\n", offset, lineNo)), nil
	}
	if truncated {
		fmt.Fprintf(&out, "\n[showing lines %d-%d; more lines remain — pass offset=%d to continue]\n", offset, lastShown, lastShown+1)
	}
	return NewTextResult(out.String()), nil
}

type readInput struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

func newReadTool(tr *FileTracker, cwd string) ToolFunc {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in readInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("read: invalid input: %w", err)
		}

		absPath, err := resolveToolPath(in.Path, in.FilePath, cwd)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}

		if err := ctx.Err(); err != nil {
			return "", err
		}

		unlock := tr.Lock(absPath)
		defer unlock()

		info, err := os.Stat(absPath)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("read: %q is a directory, not a file", absPath)
		}

		f, err := os.Open(absPath)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		defer f.Close()

		offset := in.Offset
		if offset < 1 {
			offset = 1
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultReadLimit
		}

		var out strings.Builder
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

		lineNo := 0
		shown := 0
		lastShown := offset - 1
		truncated := false
		for scanner.Scan() {
			lineNo++
			if lineNo < offset {
				continue
			}
			if shown == limit {
				truncated = true
				break
			}
			fmt.Fprintf(&out, "%6d\t%s\n", lineNo, scanner.Text())
			shown++
			lastShown = lineNo
		}
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read: %w", err)
		}

		tr.RecordRead(absPath, info.ModTime())

		if shown == 0 {
			if lineNo == 0 {
				return "(empty file)\n", nil
			}
			return fmt.Sprintf("(no lines at or after offset %d; file has %d lines)\n", offset, lineNo), nil
		}
		if truncated {
			fmt.Fprintf(&out, "\n[showing lines %d-%d; more lines remain — pass offset=%d to continue]\n", offset, lastShown, lastShown+1)
		}
		return out.String(), nil
	}
}
