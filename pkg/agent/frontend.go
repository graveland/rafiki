// Package agent implements fundi's native direct-API agent runtime: the
// child process that speaks pi's rpc protocol on stdio in place of Claude
// Code.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// Frontend speaks pi's rpc protocol over stdio: ndjson frames in (prompt,
// steer, abort, get_state), AgentSessionEvent frames out. It is the only
// writer to out; Emit is safe from any goroutine.
type Frontend struct {
	in      io.Reader
	out     io.Writer
	mu      sync.Mutex
	handler Handler
}

// Handler drives turn execution in response to inbound frames. Implemented by
// the Engine (Task 7); Frontend only dispatches to it.
//
// Run calls these methods synchronously, inline in its reader loop.
// Implementations MUST return promptly: queue the work and drive the actual
// turn (e.g. an LLM call) on their own goroutine. A Handler that blocks for
// the duration of a turn stalls the reader loop, so it can no longer read an
// inbound "abort" or "steer" frame until the call returns — breaking in-band
// abort/steer entirely.
type Handler interface {
	HandlePrompt(text string)
	HandleSteer(text string)
	HandleAbort()
	State() StateData
}

// StateData feeds the get_state response the daemon sniffs for session id +
// model (internal/child/sniff.go expects data.sessionId and data.model{id,provider}).
type StateData struct {
	SessionID   string
	SessionName string
	ModelID     string
	Provider    string
}

// NewFrontend constructs a Frontend reading inbound ndjson frames from in and
// writing outbound frames to out, dispatching to h.
func NewFrontend(in io.Reader, out io.Writer, h Handler) *Frontend {
	return &Frontend{in: in, out: out, handler: h}
}

// Emit marshals v and writes it as one ndjson line to out. Safe to call
// concurrently from any goroutine; writes are mutex-serialized so lines never
// interleave. A marshal failure is reported as an agent_error frame rather
// than silently dropped.
func (f *Frontend) Emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, werr := fmt.Fprintf(f.out, `{"type":"agent_error","error":%q}`+"\n", err.Error()); werr != nil {
			slog.Warn("agent frontend write failed", "error", werr)
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.out.Write(b); err != nil {
		slog.Warn("agent frontend write failed", "error", err)
		return
	}
	if _, err := io.WriteString(f.out, "\n"); err != nil {
		slog.Warn("agent frontend newline write failed", "error", err)
	}
}

// stateResponse is the get_state reply shape internal/child/sniff.go parses:
// data.sessionId, data.sessionFile, data.sessionName, data.model{id,provider}.
type stateResponse struct {
	Type    string    `json:"type"`
	Command string    `json:"command"`
	ID      string    `json:"id,omitempty"`
	Success bool      `json:"success"`
	Data    stateData `json:"data"`
}

type stateData struct {
	SessionID   string     `json:"sessionId"`
	SessionFile string     `json:"sessionFile"`
	SessionName string     `json:"sessionName"`
	Model       modelField `json:"model"`
}

type modelField struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// Run reads ndjson frames from in until EOF, dispatching each to the
// handler, and blocks until stdin is exhausted or a scan error occurs.
// Dispatch to the Handler is synchronous and inline, so the Handler contract
// (see Handler) requires prompt returns to keep the reader loop responsive to
// abort/steer frames while a turn is in flight.
func (f *Frontend) Run() error {
	sc := bufio.NewScanner(f.in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var hdr struct {
			Type    string `json:"type"`
			ID      string `json:"id,omitempty"`
			Message string `json:"message,omitempty"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			continue // unparseable input is dropped, matching pi's tolerance
		}
		switch hdr.Type {
		case "get_state":
			s := f.handler.State()
			f.Emit(stateResponse{Type: "response", Command: "get_state", ID: hdr.ID, Success: true,
				Data: stateData{SessionID: s.SessionID, SessionFile: "", SessionName: s.SessionName,
					Model: modelField{ID: s.ModelID, Provider: s.Provider}}})
		case "prompt":
			f.handler.HandlePrompt(hdr.Message)
		case "steer":
			f.handler.HandleSteer(hdr.Message)
		case "abort":
			f.handler.HandleAbort()
		default:
			if hdr.ID != "" { // request-shaped: answer so clients don't hang
				f.Emit(map[string]any{"type": "response", "command": hdr.Type,
					"id": hdr.ID, "success": false, "error": "unsupported by fundid agent"})
			}
		}
	}
	return sc.Err()
}
