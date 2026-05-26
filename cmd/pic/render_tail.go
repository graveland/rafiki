package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
)

// errDaemonShutdown is returned by render when a ctrl_daemon_shutdown frame
// is received.  The run loop in cmd_tail.go uses this to exit cleanly.
var errDaemonShutdown = errors.New("daemon_shutdown")

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
}

func newTailRenderer(w io.Writer, useColor bool, mode outputMode, verbose bool) *tailRenderer {
	return &tailRenderer{w: w, useColor: useColor, mode: mode, verbose: verbose}
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
		Type       string          `json:"type"`
		ToolName   string          `json:"toolName,omitempty"`
		ToolCallID string          `json:"toolCallId,omitempty"`
		IsError    bool            `json:"isError,omitempty"`
		Message    json.RawMessage `json:"message,omitempty"`
	}
	if err := json.Unmarshal(event, &hdr); err != nil {
		fmt.Fprintln(r.w, string(event))
		return nil
	}

	// In non-verbose mode, suppress events that are either internal plumbing
	// (RPC responses, fanned by the daemon to every subscriber) or already
	// covered by another event.  Specifically:
	//
	//   - response           → internal RPC chatter
	//   - turn_start         → lifecycle noise; agent_start is enough
	//   - message_start      → except for user messages, where it's the
	//                          earliest signal of new user input.  Assistant
	//                          message_start is empty (content streamed in).
	//   - message_end        → user echo already shown via message_start;
	//                          assistant text is shown by turn_end.
	if !r.verbose {
		switch hdr.Type {
		case "response", "turn_start", "message_end":
			return nil
		case "message_start":
			return r.renderUserMessage(event)
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
		r.printDim("─── agent_end ───")

	case "turn_end":
		r.renderTurnEnd(hdr.Message)

	case "tool_execution_start":
		fmt.Fprintf(r.w, "  %s %s\n", r.cyan("↻"), hdr.ToolName)

	case "tool_execution_end":
		mark, fn := "✓", r.green
		if hdr.IsError {
			mark, fn = "✗", r.red
		}
		fmt.Fprintf(r.w, "  %s %s\n", fn(mark), hdr.ToolName)

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

// renderUserMessage prints a user message_start as `[user] text`, suppressing
// non-user messages (assistant message_starts are empty placeholders — their
// content streams in over message_update events and the final text appears
// via turn_end).  Returns nil to short-circuit further dispatch.
func (r *tailRenderer) renderUserMessage(event json.RawMessage) error {
	var p struct {
		Message struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(event, &p); err != nil {
		return nil
	}
	if p.Message.Role != "user" {
		return nil
	}
	var text strings.Builder
	for _, raw := range p.Message.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return nil
	}
	fmt.Fprintf(r.w, "%s %s\n", r.applyColor(cyan, "[user]"), text.String())
	return nil
}

// renderTurnEnd extracts and prints the assistant's text from a turn_end message.
func (r *tailRenderer) renderTurnEnd(messageRaw json.RawMessage) {
	if len(messageRaw) == 0 {
		return
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content,omitempty"`
	}
	if err := json.Unmarshal(messageRaw, &msg); err != nil {
		return
	}

	// Content is normally an array of typed blocks; extract text blocks.
	var content []map[string]any
	if err := json.Unmarshal(msg.Content, &content); err == nil {
		var text strings.Builder
		for _, block := range content {
			if t, _ := block["type"].(string); t == "text" {
				if s, _ := block["text"].(string); s != "" {
					text.WriteString(s)
				}
			}
		}
		if text.Len() > 0 {
			fmt.Fprintln(r.w, text.String())
		}
		return
	}

	// Fallback: content may be a plain string (legacy / partial frames).
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil && s != "" {
		fmt.Fprintln(r.w, s)
	}
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
