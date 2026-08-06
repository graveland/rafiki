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
	// defaultReadLimit is the line cap applied when the caller doesn't
	// specify limit. Large files are handled by paging (offset/limit), not
	// by spilling to a side channel — that's Task 9's output-policy layer,
	// not this tool's job.
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

type readInput struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// newReadTool builds the read ToolFunc, recording every successful read's
// path+mtime in tr so write/edit can verify freshness later.
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

		// Fail fast on an already-aborted turn rather than doing work whose
		// result nobody will see — agentloop runs a batch of tool calls
		// concurrently, so a call can still be starting after the turn's
		// context was canceled.
		if err := ctx.Err(); err != nil {
			return "", err
		}

		// rafiki's agentloop runs a tool batch concurrently, so a read can land
		// in the middle of a write or edit on the same path. os.WriteFile is
		// O_TRUNC then Write — not atomic — so an unlocked read can scan the
		// file between the two and hand the model torn content, then record an
		// mtime for that non-state. Hold the same per-path lock write and edit
		// hold, across the stat, the scan, and the RecordRead.
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

		// Record the read BEFORE returning any error-shaped output below —
		// a paging hint or empty-file marker is still a completed, honest
		// read of what's on disk right now.
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
