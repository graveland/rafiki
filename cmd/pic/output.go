package main

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"golang.org/x/term"

	"git.graveland.dev/brent/pi-controller/protocol"
)

type outputMode string

const (
	outputAuto  outputMode = "auto"
	outputJSON  outputMode = "json"
	outputTable outputMode = "table"
)

// resolveOutputMode resolves "auto" by checking if stdout is a TTY:
// TTY → table, otherwise → json.
func resolveOutputMode(flag string, isTTY bool) outputMode {
	switch outputMode(flag) {
	case outputJSON:
		return outputJSON
	case outputTable:
		return outputTable
	default:
		if isTTY {
			return outputTable
		}
		return outputJSON
	}
}

// colorEnabled decides whether to emit ANSI color codes.
// flag: always|never|auto. isTTY: whether stdout is a terminal.
// Honors NO_COLOR env var (always disables).
func colorEnabled(flag string, isTTY bool) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch flag {
	case "always":
		return true
	case "never":
		return false
	default:
		return isTTY
	}
}

// isStdoutTTY is a small wrapper for testability.
func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// renderList writes a list of ChildSummary either as JSON or as a table.
func renderList(w io.Writer, children []protocol.ChildSummary, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		return writeJSON(w, map[string]any{"children": children})
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(w)

	// Use StyleLight (single-line Unicode box-drawing) with go-pretty's own
	// auto-coloring disabled so our hand-rolled ANSI codes take precedence.
	st := table.StyleLight
	st.Color = table.ColorOptions{}
	tw.SetStyle(st)

	colNames := []string{"ID", "NAME", "STATUS", "PROVIDER", "MODEL", "CWD", "STARTED", "LABELS"}
	headerRow := make(table.Row, len(colNames))
	for i, name := range colNames {
		if useColor {
			headerRow[i] = dim(name)
		} else {
			headerRow[i] = name
		}
	}
	tw.AppendHeader(headerRow)

	for _, ch := range children {
		started := "-"
		if ch.StartedAt > 0 {
			// StartedAt arrives from the daemon as Unix milliseconds (see
			// dispatch.go: snap.StartedAt.UnixMilli()).  time.UnixMilli ensures
			// the printed date is in the present millennium.
			started = time.UnixMilli(ch.StartedAt).Format("2006-01-02 15:04")
		}
		provider, model := splitProviderModel(ch.Model)
		tw.AppendRow(table.Row{
			ch.ChildID,
			defaultDash(ch.Name),
			formatStatus(ch.Status, ch.ExitCode, ch.ExitSignal, useColor),
			defaultDash(provider),
			defaultDash(model),
			defaultDash(shortenCwd(ch.Cwd)),
			started,
			formatLabels(ch.Labels, 40, false),
		})
	}

	tw.Render()
	return nil
}

func defaultDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitProviderModel splits a "provider/model" string into its two halves.
// When no slash is present, the entire input is treated as the model name and
// provider is returned empty.  This is the inverse of dispatch.go's join logic.
func splitProviderModel(combined string) (provider, model string) {
	if i := strings.Index(combined, "/"); i >= 0 {
		return combined[:i], combined[i+1:]
	}
	return "", combined
}

// shortenCwd returns a more compact representation of a working directory:
// the user's home is replaced with "~".  Useful for table display where
// full paths blow the row width past the terminal margin.
func shortenCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(cwd, home) {
		return "~" + cwd[len(home):]
	}
	return cwd
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// formatStatus renders the STATUS cell for a child row.  For exited children
// it appends the exit code (or signal name) in parentheses so users can tell
// clean exits (0) from failures (!= 0) and signal-driven terminations at a
// glance — useful for spotting cleanup bugs vs. legitimate non-zero exits.
func formatStatus(status string, exitCode *int, exitSignal string, useColor bool) string {
	colored := colorStatus(status, useColor)
	if status != "exited" {
		return colored
	}
	switch {
	case exitSignal != "":
		return colored + " (signal: " + exitSignal + ")"
	case exitCode != nil:
		return colored + " (" + strconv.Itoa(*exitCode) + ")"
	default:
		return colored + " (?)"
	}
}

func colorStatus(status string, useColor bool) string {
	if !useColor {
		return status
	}
	switch status {
	case "idle":
		return cyan(status)
	case "streaming", "tool_running", "compacting":
		return green(status)
	case "exited":
		return red(status)
	case "shutting_down":
		return yellow(status)
	case "blocked_ui":
		return magenta(status)
	default:
		return status
	}
}

// ANSI color helpers — simple, no external dep.
func dim(s string) string     { return "\x1b[2m" + s + "\x1b[0m" }
func red(s string) string     { return "\x1b[31m" + s + "\x1b[0m" }
func green(s string) string   { return "\x1b[32m" + s + "\x1b[0m" }
func yellow(s string) string  { return "\x1b[33m" + s + "\x1b[0m" }
func cyan(s string) string    { return "\x1b[36m" + s + "\x1b[0m" }
func magenta(s string) string { return "\x1b[35m" + s + "\x1b[0m" }
