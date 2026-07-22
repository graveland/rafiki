package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	SpillDir string
}

// Clip returns s unchanged when len(s) <= Budget. Otherwise it writes the
// FULL s to SpillDir/name, then returns
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

	spillPath := filepath.Join(p.SpillDir, name)
	if err := os.MkdirAll(p.SpillDir, 0o755); err != nil {
		slog.Error("agent/tools: output policy: failed to create spill directory", "dir", p.SpillDir, "error", err)
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
