package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"git.graveland.dev/brent/pi-controller/protocol"
)

// errDaemonShutdown is returned by render when a ctrl_daemon_shutdown frame
// is received.  The run loop in cmd_tail.go uses this to exit cleanly.
var errDaemonShutdown = errors.New("daemon_shutdown")

// linePrefixWriter is an io.Writer that prepends Prefix to each non-empty,
// non-blank Write call.  It is used by pic tail in label-filtered mode to
// prefix each event line with the source child's short ID.  The Prefix field
// is updated between events — safe because tail's render loop is single-
// threaded with respect to writes.
type linePrefixWriter struct {
	w      io.Writer
	Prefix string // updated per-event by the run loop
}

func (pw *linePrefixWriter) Write(p []byte) (int, error) {
	// Don't prefix blank lines (e.g. the separator fmt.Fprintln(w, "") emits).
	if pw.Prefix == "" || len(p) == 0 || p[0] == '\n' {
		return pw.w.Write(p)
	}
	buf := make([]byte, 0, len(pw.Prefix)+len(p))
	buf = append(buf, pw.Prefix...)
	buf = append(buf, p...)
	_, err := pw.w.Write(buf)
	return len(p), err
}

// tailRenderer formats incoming event frames (raw bytes) onto w.
// It handles the ctrl_event wrapper (pi events) and ctrl_child_* lifecycle events.
//
// Pi writes responses to its stdout firehose alongside events, and the daemon
// fans the entire stream to all subscribers — so 'pic tail' sees responses
// to other connections' RPC calls (e.g. pic-attach's autocomplete
// get_commands fetch).  These are internal plumbing the user usually does
// not care about, so we suppress them by default.  `--verbose` (verbose=true)
// includes them with pretty-printed JSON.
type tailRenderer struct {
	w        io.Writer
	useColor bool
	mode     outputMode
	verbose  bool
	width    int // terminal columns, for clamping abridged tool detail
}

func newTailRenderer(w io.Writer, useColor bool, mode outputMode, verbose bool) *tailRenderer {
	return &tailRenderer{w: w, useColor: useColor, mode: mode, verbose: verbose, width: tailDisplayWidth()}
}

// render writes a human-readable (or JSON) representation of frame to w.
// In JSON mode it pretty-prints the raw frame unchanged.
func (r *tailRenderer) render(frame []byte) error {
	if r.mode == outputJSON {
		var v any
		if err := json.Unmarshal(frame, &v); err != nil {
			// Malformed JSON; emit as-is.
			fmt.Fprintln(r.w, string(frame))
			return nil
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintln(r.w, string(frame))
			return nil
		}
		fmt.Fprintln(r.w, string(b))
		return nil
	}

	// Decode just the envelope fields we need for routing.
	var hdr struct {
		Type     string            `json:"type"`
		ChildID  string            `json:"childId"`
		Event    json.RawMessage   `json:"event,omitempty"`
		Status   string            `json:"status,omitempty"`
		Previous string            `json:"previous,omitempty"`
		ExitCode *int              `json:"exitCode,omitempty"`
		Signal   string            `json:"signal,omitempty"`
		Name     string            `json:"name,omitempty"`
		Labels   map[string]string `json:"labels,omitempty"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		fmt.Fprintln(r.w, string(frame))
		return nil
	}

	switch hdr.Type {
	case protocol.TypeCtrlDaemonShutdown:
		var ev protocol.CtrlDaemonShutdown
		_ = json.Unmarshal(frame, &ev)
		fmt.Fprintf(os.Stderr, "daemon shutting down (reason: %s)\n", ev.Reason)
		return errDaemonShutdown

	case protocol.TypeCtrlEvent:
		return r.renderPiEvent(hdr.Event)

	case protocol.TypeCtrlChildStatus:
		r.printDim(fmt.Sprintf("[%s] status: %s → %s",
			time.Now().Format("15:04:05"), hdr.Previous, hdr.Status))

	case protocol.TypeCtrlChildExited:
		code := "?"
		if hdr.ExitCode != nil {
			code = fmt.Sprintf("%d", *hdr.ExitCode)
		}
		divider := "─── child exited"
		if hdr.Signal != "" {
			divider += fmt.Sprintf(" (signal %s)", hdr.Signal)
		}
		divider += fmt.Sprintf(" (code %s) ───", code)
		r.printRed(divider)

	case protocol.TypeCtrlChildSpawned:
		r.printDim(fmt.Sprintf("─── child spawned (%s) ───", hdr.ChildID))

	case protocol.TypeCtrlChildRenamed:
		r.printDim(fmt.Sprintf("[rename] %s → %s", hdr.Previous, hdr.Name))

	case protocol.TypeCtrlChildLabeled:
		if r.verbose {
			var v any
			if err := json.Unmarshal(frame, &v); err == nil {
				if b, err := json.MarshalIndent(v, "", "  "); err == nil {
					r.printDim("[labels]")
					fmt.Fprintln(r.w, string(b))
					break
				}
			}
		}
		// In a streaming tail, show ALL labels including pic/* — user is
		// actively observing changes and likely wants the full picture.
		r.printDim(fmt.Sprintf("[labels] %s", formatLabels(hdr.Labels, 60, true)))

	default:
		fmt.Fprintln(r.w, string(frame))
	}
	return nil
}

// renderResponseFrame pretty-prints an RPC response frame in verbose mode.
// Dims the wrapper for visibility and indents nested data for readability.
func (r *tailRenderer) renderResponseFrame(frame []byte) error {
	var v any
	if err := json.Unmarshal(frame, &v); err != nil {
		fmt.Fprintln(r.w, string(frame))
		return nil
	}
	indented, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		fmt.Fprintln(r.w, string(frame))
		return nil
	}
	r.printDim("[response]")
	fmt.Fprintln(r.w, "  "+string(indented))
	return nil
}

// renderPiEvent handles the pi event payload inside a ctrl_event wrapper.
func (r *tailRenderer) renderPiEvent(event json.RawMessage) error {
	var hdr struct {
		Type            string          `json:"type"`
		ToolName        string          `json:"toolName,omitempty"`
		ToolCallID      string          `json:"toolCallId,omitempty"`
		IsError         bool            `json:"isError,omitempty"`
		Message         json.RawMessage `json:"message,omitempty"`
		Args            json.RawMessage `json:"args,omitempty"`
		Result          json.RawMessage `json:"result,omitempty"`
		ParentToolUseID string          `json:"parentToolUseId,omitempty"`
		Usage           json.RawMessage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(event, &hdr); err != nil {
		fmt.Fprintln(r.w, string(event))
		return nil
	}

	// In non-verbose mode we render the conversation, suppressing internal
	// plumbing and the redundant half of each message pair.  Backends differ:
	// pi emits turn_end carrying the assistant message; claude emits no turn_end
	// and instead carries the reply in message_end.  Both emit message_end with
	// the assistant content, so we render assistant text/thinking there (works
	// for both) and treat turn_end as structural-only.  Specifically:
	//
	//   - response       → internal RPC chatter
	//   - turn_start/end → lifecycle; assistant text comes via message_end
	//   - message_start  → render only the user prompt (assistant start is an
	//                      empty placeholder for pi, a duplicate for claude)
	//   - message_end    → render only the assistant reply (user message_end is
	//                      the echo's duplicate, already shown at message_start)
	if !r.verbose {
		switch hdr.Type {
		case "response", "turn_start", "turn_end":
			return nil
		case "message_start":
			return r.renderConversationMessage(event, false, nestPrefix(hdr.ParentToolUseID))
		case "message_end":
			return r.renderConversationMessage(event, true, nestPrefix(hdr.ParentToolUseID))
		}
	}

	if hdr.Type == "response" {
		return r.renderResponseFrame(event)
	}

	switch hdr.Type {
	case "agent_start":
		r.printDim("─── agent_start ───")

	case "agent_end":
		// Blank line before the divider for visual separation.
		fmt.Fprintln(r.w, "")
		if usage := r.formatUsage(hdr.Usage); usage != "" {
			r.printDim("─── agent_end · " + usage + " ───")
		} else {
			r.printDim("─── agent_end ───")
		}

	case "tool_execution_start":
		nest := nestPrefix(hdr.ParentToolUseID)
		fmt.Fprintf(r.w, "%s  %s %s\n", nest, r.cyan("↻"), hdr.ToolName)
		r.printToolDetail(hdr.Args, nest)

	case "tool_execution_end":
		nest := nestPrefix(hdr.ParentToolUseID)
		mark, fn := "✓", r.green
		if hdr.IsError {
			mark, fn = "✗", r.red
		}
		fmt.Fprintf(r.w, "%s  %s %s\n", nest, fn(mark), hdr.ToolName)
		r.printToolDetail(hdr.Result, nest)

	case "extension_ui_request":
		var ui struct {
			Method  string `json:"method"`
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event, &ui)
		r.printYellow(fmt.Sprintf("❓ %s: %s", ui.Method, ui.Title))
		if ui.Message != "" {
			fmt.Fprintln(r.w, "  "+ui.Message)
		}

	case "compaction_start":
		r.printDim("─── compaction_start ───")

	case "compaction_end":
		r.printDim("─── compaction_end ───")

	case "auto_retry_start":
		r.printDim("[auto-retry]")

	default:
		// Unknown event: dim type tag + compact JSON body.
		var compact bytes.Buffer
		if err := json.Compact(&compact, event); err != nil {
			fmt.Fprintln(r.w, string(event))
			return nil
		}
		r.printDim(fmt.Sprintf("[%s] %s", hdr.Type, compact.String()))
	}
	return nil
}

// Tool-detail abridging bounds: large args/results collapse to the first and
// last N lines, and each printed line is clamped to the terminal width.
const (
	toolDetailHeadLines = 5
	toolDetailTailLines = 5
	toolDetailIndent    = 4 // spaces; aligns detail beneath the "  ↻ Tool" line
)

// printToolDetail renders an abridged tool args/result payload indented beneath
// the tool line.  nest is the sub-agent indent prefix ("" at top level).  No-op
// on empty payloads (e.g. a tool with no args).
func (r *tailRenderer) printToolDetail(raw json.RawMessage, nest string) {
	indent := nest + strings.Repeat(" ", toolDetailIndent)
	for _, ln := range r.abridgeText(toolPayloadText(raw), len(indent)) {
		fmt.Fprintln(r.w, r.applyColor(dim, indent+ln))
	}
}

// abridgeText bounds free text for tail display: head+tail line elision when it
// exceeds toolDetail{Head,Tail}Lines, then a per-line clamp to the terminal
// width (less indentCols, the columns the caller will prepend).  Shared by tool
// args/results and assistant thinking blocks.
func (r *tailRenderer) abridgeText(text string, indentCols int) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")

	// Elide the middle only when doing so actually removes lines — at exactly
	// head+tail+1 the marker would replace a single line, which is pointless.
	if n := len(lines); n > toolDetailHeadLines+toolDetailTailLines+1 {
		omitted := n - toolDetailHeadLines - toolDetailTailLines
		merged := make([]string, 0, toolDetailHeadLines+toolDetailTailLines+1)
		merged = append(merged, lines[:toolDetailHeadLines]...)
		merged = append(merged, fmt.Sprintf("… (%d more lines)", omitted))
		merged = append(merged, lines[n-toolDetailTailLines:]...)
		lines = merged
	}

	avail := r.width - indentCols
	if avail < 16 {
		avail = 16
	}
	for i, ln := range lines {
		ln = strings.ReplaceAll(ln, "\t", "    ")
		lines[i] = runewidth.Truncate(ln, avail, "…")
	}
	return lines
}

// nestPrefix returns the indent for sub-agent (Task) activity.  A non-empty
// parentToolUseId means the event was produced inside a Task call, so it is
// indented one level beneath the spawning tool line; top-level events get "".
func nestPrefix(parentToolUseID string) string {
	if parentToolUseID == "" {
		return ""
	}
	return strings.Repeat(" ", toolDetailIndent)
}

// formatUsage renders a compact token/cost summary from an agent_end usage
// payload, e.g. "8.4k in / 79 out · 55.1k cached · $0.4095".  Returns "" when
// no usage data is present (the field is omitted for backends that don't
// report it).
func (r *tailRenderer) formatUsage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var u struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Cost       struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return ""
	}
	var parts []string
	if u.Input > 0 || u.Output > 0 {
		parts = append(parts, fmt.Sprintf("%s in / %s out", humanCount(u.Input), humanCount(u.Output)))
	}
	if cached := u.CacheRead + u.CacheWrite; cached > 0 {
		parts = append(parts, humanCount(cached)+" cached")
	}
	if u.Cost.Total > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", u.Cost.Total))
	}
	return strings.Join(parts, " · ")
}

// humanCount renders a token count compactly: 79 → "79", 8444 → "8.4k".
func humanCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// toolPayloadText converts a tool args/result payload into display text.
// A bare JSON string is used verbatim; an AgentToolResult-shaped object
// ({content:[{type,text}]}) is flattened to its text blocks; anything else
// (notably args objects) falls back to pretty-printed JSON.
func toolPayloadText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && len(res.Content) > 0 {
		var b strings.Builder
		for _, c := range res.Content {
			switch c.Type {
			case "text":
				b.WriteString(c.Text)
				if !strings.HasSuffix(c.Text, "\n") {
					b.WriteByte('\n')
				}
			case "image":
				b.WriteString("[image]\n")
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// renderConversationMessage renders a message_start (isEnd=false) or message_end
// (isEnd=true) frame as conversation text.  To avoid double-printing each
// message (both halves of the start/end pair carry content), the user prompt is
// rendered on message_start and the assistant reply on message_end — the
// authoritative final content for both the pi (streamed) and claude backends.
func (r *tailRenderer) renderConversationMessage(event json.RawMessage, isEnd bool, nest string) error {
	var p struct {
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Usage   json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(event, &p); err != nil {
		return nil
	}

	switch p.Message.Role {
	case "user":
		if isEnd {
			return nil // already shown at message_start
		}
		if text := messageText(p.Message.Content); text != "" {
			fmt.Fprintf(r.w, "%s%s %s\n", nest, r.applyColor(cyan, "[user]"), text)
		}
	case "assistant":
		if !isEnd {
			return nil // start is an empty placeholder (pi) or a duplicate (claude)
		}
		r.renderAssistantContent(p.Message.Content, nest)
		// pi reports token usage per assistant message; the claude provider
		// reports the turn total on agent_end and emits a zero per-message usage
		// (which formatUsage renders as empty), so this footer fires only for pi.
		if u := r.formatUsage(p.Message.Usage); u != "" {
			r.printDim(nest + "· " + u)
		}
	}
	return nil
}

// renderAssistantContent prints an assistant message's text and thinking blocks
// in source order, prefixed by nest (the sub-agent indent).  tool_use blocks are
// skipped here — they surface via the tool_execution_start/end events.  Thinking
// is dimmed and abridged (it can be long); text is the agent's reply.
func (r *tailRenderer) renderAssistantContent(content json.RawMessage, nest string) {
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		// Legacy / partial frames may carry content as a plain string.
		var s string
		if json.Unmarshal(content, &s) == nil && s != "" {
			r.writeLines(s, nest)
		}
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			r.printDim(nest + "[thinking]")
			indent := nest + strings.Repeat(" ", toolDetailIndent)
			for _, ln := range r.abridgeText(b.Thinking, len(indent)) {
				fmt.Fprintln(r.w, r.applyColor(dim, indent+ln))
			}
		case "text":
			if b.Text != "" {
				r.writeLines(b.Text, nest)
			}
		}
	}
}

// writeLines prints text with nest prepended to each line (a no-op prefix at top
// level, so a multi-line reply still renders verbatim).
func (r *tailRenderer) writeLines(text, nest string) {
	if nest == "" {
		fmt.Fprintln(r.w, text)
		return
	}
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintln(r.w, nest+ln)
	}
}

// messageText flattens a message's content into plain text.  Content is a
// JSON string for claude user prompts (the synthesized echo) and an array of
// typed blocks for everything else; both are handled.
func messageText(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// ─── Color helpers ────────────────────────────────────────────────────────────
// These delegate to the package-level ANSI functions from output.go, guarded
// by r.useColor so tests can run without color sequences.

func (r *tailRenderer) printDim(s string)    { fmt.Fprintln(r.w, r.applyColor(dim, s)) }
func (r *tailRenderer) printRed(s string)    { fmt.Fprintln(r.w, r.applyColor(red, s)) }
func (r *tailRenderer) printYellow(s string) { fmt.Fprintln(r.w, r.applyColor(yellow, s)) }

func (r *tailRenderer) red(s string) string    { return r.applyColor(red, s) }
func (r *tailRenderer) green(s string) string  { return r.applyColor(green, s) }
func (r *tailRenderer) cyan(s string) string   { return r.applyColor(cyan, s) }

func (r *tailRenderer) applyColor(fn func(string) string, s string) string {
	if r.useColor {
		return fn(s)
	}
	return s
}
