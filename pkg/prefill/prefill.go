// Package prefill parses and validates a spawn's pre-fill list: the files (or
// globs) a fundi child reads through its own Read tool before its first turn,
// recorded as real tool_use/tool_result history. It is deliberately pgx-free
// (imports only pkg/protocol) so the client binary can link it too.
package prefill

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// MaxEntries bounds one pre-fill.
const MaxEntries = 500

// rangeRe matches a range suffix, and only when the entry ends in `:` +
// optional digits + `-` + optional digits — so a path containing `:` still
// works, and `:N` without a dash is kept verbatim as part of the path. The
// two groups are the 1-based inclusive bounds; either may be empty (open).
var rangeRe = regexp.MustCompile(`:(\d*)-(\d*)$`)

// Parse reads the list syntax (one entry per line) into entries.
func Parse(text string) ([]protocol.PrefillRead, error) {
	return ParseEntries(strings.Split(text, "\n"))
}

// ParseEntries parses entries already split one per element (agent_spawn's
// array form). Comment and blank handling is identical to Parse.
func ParseEntries(lines []string) ([]protocol.PrefillRead, error) {
	entries := make([]protocol.PrefillRead, 0, len(lines))
	for i, line := range lines {
		lineNo := i + 1
		// # starts a comment, to end of line.
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		path := line
		var start, end int
		if m := rangeRe.FindStringSubmatch(line); m != nil {
			if m[1] == "" && m[2] == "" {
				return nil, fmt.Errorf("prefill: line %d: empty range %q", lineNo, ":-")
			}
			var err error
			if m[1] != "" {
				if start, err = strconv.Atoi(m[1]); err != nil {
					return nil, fmt.Errorf("prefill: line %d: bad range start %q: %v", lineNo, m[1], err)
				}
				if start == 0 {
					return nil, fmt.Errorf("prefill: line %d: zero is not a valid line number (ranges are 1-based)", lineNo)
				}
			}
			if m[2] != "" {
				if end, err = strconv.Atoi(m[2]); err != nil {
					return nil, fmt.Errorf("prefill: line %d: bad range end %q: %v", lineNo, m[2], err)
				}
				if end == 0 {
					return nil, fmt.Errorf("prefill: line %d: zero is not a valid line number (ranges are 1-based)", lineNo)
				}
			}
			if start != 0 && end != 0 && start > end {
				return nil, fmt.Errorf("prefill: line %d: range start %d is after end %d", lineNo, start, end)
			}
			path = line[:len(line)-len(m[0])]
			if IsGlob(path) {
				return nil, fmt.Errorf("prefill: line %d: glob %q cannot carry a line range", lineNo, path)
			}
		}
		entries = append(entries, protocol.PrefillRead{Path: path, Start: start, End: end})
	}
	if err := Validate(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Validate checks entries that arrived already structured (Connect, JSON):
// non-empty path, Start/End >= 0, Start <= End when both non-zero, no range
// on a glob, 1..MaxEntries entries.
func Validate(entries []protocol.PrefillRead) error {
	if len(entries) == 0 {
		return fmt.Errorf("prefill: no entries")
	}
	if len(entries) > MaxEntries {
		return fmt.Errorf("prefill: too many entries: %d (max %d)", len(entries), MaxEntries)
	}
	for i, e := range entries {
		entryNo := i + 1
		if e.Path == "" {
			return fmt.Errorf("prefill: entry %d: empty path", entryNo)
		}
		if e.Start < 0 {
			return fmt.Errorf("prefill: entry %d: start %d is negative", entryNo, e.Start)
		}
		if e.End < 0 {
			return fmt.Errorf("prefill: entry %d: end %d is negative", entryNo, e.End)
		}
		if e.Start != 0 && e.End != 0 && e.Start > e.End {
			return fmt.Errorf("prefill: entry %d: start %d is after end %d", entryNo, e.Start, e.End)
		}
		if IsGlob(e.Path) && (e.Start != 0 || e.End != 0) {
			return fmt.Errorf("prefill: entry %d: glob %q cannot carry a line range", entryNo, e.Path)
		}
	}
	return nil
}

// IsGlob reports whether a path contains any of *?[{.
func IsGlob(path string) bool {
	return strings.ContainsAny(path, "*?[{")
}
