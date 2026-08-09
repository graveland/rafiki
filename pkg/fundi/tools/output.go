package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// defaultOutputBudget is OutputPolicy's byte budget when Budget is left at
// its zero value.
const defaultOutputBudget = 30_000

// headFraction is head's share (in percent) of the retained budget when a
// result is clipped; the remainder goes to the tail. Tail-weighted (80%)
// because build and test verdicts live at the end of output — the model
// needs the last lines far more than the middle.
const headFraction = 20

// OutputPolicy implements "spill, never destroy": a tool result within
// Budget bytes is returned unchanged; a result over budget is written in
// full to SpillDir, and the model instead sees a head+tail clip naming the
// spill path and the elided byte count, so it can grep/read the remainder
// itself rather than losing it.
type OutputPolicy struct {
	// Budget is the byte budget for a single tool result. Zero means the
	// default of 30_000.
	Budget int
	// SpillDir is the directory full (unclipped) results are written to
	// when a result exceeds Budget. Created if it does not already exist.
	// Empty means os.TempDir(): a zero-value SpillDir must never degrade
	// into "destroy the output and log about it".
	SpillDir string
}

// spillTarget resolves the directory and file name a clip spills to.
//
// name reaches us from agentloop.ToolCallID(ctx) — a provider-supplied
// string — so it is reduced to its last path element: a value containing
// "/" or ".." would otherwise let a tool result be written anywhere the
// process can write. Path elements that don't name a file after that
// (".", "..", "/") fall back to a fixed name rather than targeting the
// directory itself.
func (p OutputPolicy) spillTarget(name string) (dir, file string) {
	dir = p.SpillDir
	if dir == "" {
		dir = os.TempDir()
	}
	file = filepath.Base(name)
	switch file {
	case ".", "..", string(filepath.Separator):
		file = "spill"
	}
	return dir, file
}

// Clip returns s unchanged when len(s) <= Budget. Otherwise it writes the
// FULL s to SpillDir/filepath.Base(name) (see spillTarget), then returns
// head(20% of budget) + marker + tail(80% of budget), where marker is
// "\n[... elided N bytes: full output at <path> ...]\n" and N is the number
// of bytes cut from the middle.
func (p OutputPolicy) Clip(s, name string) string {
	budget := p.Budget
	if budget <= 0 {
		budget = defaultOutputBudget
	}
	if len(s) <= budget {
		return s
	}

	spillDir, spillFile := p.spillTarget(name)
	spillPath := filepath.Join(spillDir, spillFile)
	if err := os.MkdirAll(spillDir, 0o755); err != nil {
		slog.Error("agent/tools: output policy: failed to create spill directory", "dir", spillDir, "error", err)
	} else if err := os.WriteFile(spillPath, []byte(s), 0o644); err != nil {
		// Per "spill, never destroy": if we can't spill, we must still say
		// so loudly rather than quietly handing the model a clip whose
		// marker points at a file that doesn't exist.
		slog.Error("agent/tools: output policy: failed to spill full output", "path", spillPath, "error", err)
	}

	headLen := budget * headFraction / 100
	tailLen := budget - headLen
	head := s[:headLen]
	tail := s[len(s)-tailLen:]
	elided := len(s) - headLen - tailLen

	marker := fmt.Sprintf("\n[... elided %d bytes: full output at %s ...]\n", elided, spillPath)
	return head + marker + tail
}

// lineTruncSuffix marks a line shortened by Budget.MaxLineChars.
const lineTruncSuffix = "… (line truncated)"

// linesTruncSuffix marks a result shortened by Budget.MaxLines. The line
// cap used to cut silently, which contradicts the rule every tool here
// follows: if output was dropped, the model has to be told, or it reasons
// confidently about a file it only partly saw.
const linesTruncSuffix = "\n[... line limit reached; earlier lines only ...]"

// truncateLine shortens s to at most maxChars characters and appends
// lineTruncSuffix. It cuts on a rune boundary: slicing a string mid-rune
// yields invalid UTF-8, which json.Marshal silently rewrites to U+FFFD on
// the way to the model, so a CJK or emoji line degrades into replacement
// characters instead of being honestly truncated.
func truncateLine(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s // byte length bounds rune count, so this is a safe fast path
	}
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + lineTruncSuffix
}

// Budget bounds one tool result in three dimensions. A zero field means
// that dimension is unbounded (MaxBytes zero falls back to
// defaultOutputBudget, matching Clip).
type Budget struct {
	MaxBytes     int
	MaxLines     int
	MaxLineChars int
}

// ClipBudget applies b to s and then spills and clips exactly as Clip
// does. Per-line truncation and the line cap are applied first, so the
// byte budget sees the already-reduced text.
func (p OutputPolicy) ClipBudget(s, name string, b Budget) string {
	if b.MaxLineChars > 0 || b.MaxLines > 0 {
		lines := strings.Split(s, "\n")
		linesCut := false
		if b.MaxLines > 0 && len(lines) > b.MaxLines {
			lines = lines[:b.MaxLines]
			linesCut = true
		}
		if b.MaxLineChars > 0 {
			for i, line := range lines {
				lines[i] = truncateLine(line, b.MaxLineChars)
			}
		}
		s = strings.Join(lines, "\n")
		if linesCut {
			s += linesTruncSuffix
		}
	}
	if b.MaxBytes > 0 {
		return OutputPolicy{Budget: b.MaxBytes, SpillDir: p.SpillDir}.Clip(s, name)
	}
	return p.Clip(s, name)
}
