package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"graveland.dev/pi-controller/internal/protocol"
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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, headerLine(useColor, "ID", "NAME", "STATUS", "MODEL", "STARTED"))
	for _, ch := range children {
		started := "-"
		if ch.StartedAt > 0 {
			started = time.Unix(ch.StartedAt, 0).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ch.ChildID,
			defaultDash(ch.Name),
			colorStatus(ch.Status, useColor),
			defaultDash(ch.Model),
			started)
	}
	return tw.Flush()
}

func defaultDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// headerLine builds a tab-separated header row from column names.
func headerLine(useColor bool, cols ...string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		s := c
		if useColor {
			s = dim(s)
		}
		parts[i] = s
	}
	return strings.Join(parts, "\t")
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
