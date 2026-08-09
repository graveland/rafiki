package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"go.graveland.dev/rafiki/pkg/agentloop"
)

const (
	defaultReadLimit = 2000
	// maxLineChars caps a single line; longer lines are truncated with a
	// "… (line truncated)" suffix. Matches opencode's proven value.
	maxLineChars = 2000
	// readTrailerReserve is headroom left below agentloop.MaxToolResultSize
	// for read's continuation trailer. The trailer is appended *after* the
	// byte budget is spent, so a budget equal to the outer cap pushes the
	// result over it, and truncateToolResult — which cuts from the tail —
	// removes the trailer and the last line or two with it. The model then
	// gets a silently short file and no offset to resume from, which is the
	// precise defect these budgets were added to fix. 1 KiB is ~14x the
	// longest trailer this file can emit.
	readTrailerReserve = 1024
	// maxReadBytes is the total output byte budget for one read call. It
	// must stay strictly below agentloop.MaxToolResultSize; see
	// readTrailerReserve. TestReadBudgetLeavesRoomForTrailer pins this.
	maxReadBytes = agentloop.MaxToolResultSize - readTrailerReserve
	// binaryCheckBytes is how many bytes at the head of the file are
	// scanned for a NUL byte to detect binary content.
	binaryCheckBytes = 8192

	readDescription = "Read a file from the local filesystem. " +
		"Use `path` (or `file_path`, an alias) — absolute or relative to the " +
		"working directory. Output is numbered like `cat -n` (1-indexed). By " +
		"default reads from the start of the file, up to 2000 lines; if the " +
		"file is longer, a trailing note tells you the offset to continue from. " +
		"Pass offset/limit to page through a large file explicitly."
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

	// Binary detection: scan the first 8 KB for a NUL byte.
	header := make([]byte, binaryCheckBytes)
	n, readErr := io.ReadFull(f, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return ToolResult{}, fmt.Errorf("read: %w", readErr)
	}
	for _, b := range header[:n] {
		if b == 0 {
			return ToolResult{}, fmt.Errorf("read: %q appears to be a binary file", absPath)
		}
	}
	// Rewind to the start so the scanner sees the full file.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ToolResult{}, fmt.Errorf("read: %w", err)
	}

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

		// truncateLine, not a byte slice: line[:maxLineChars] can cut a
		// multi-byte rune in half, and the invalid UTF-8 that results is
		// silently rewritten to U+FFFD when the result is JSON-encoded for
		// the API — so a CJK or emoji line arrives as replacement characters.
		line := truncateLine(scanner.Text(), maxLineChars)

		formatted := fmt.Sprintf("%6d\t%s\n", lineNo, line)

		// Byte budget: stop if adding this line would exceed maxReadBytes,
		// but always emit at least one line.
		if out.Len()+len(formatted) > maxReadBytes && shown > 0 {
			truncated = true
			break
		}

		out.WriteString(formatted)
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
