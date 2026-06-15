package child

import "encoding/json"

// ClaudeProvider implements ProtocolProvider for Claude Code's stream-json stdio
// protocol (claude -p --input-format stream-json --output-format stream-json
// --verbose). It maps claude's system/assistant/user/result objects onto the
// daemon's normalized state-machine vocabulary, and encodes outbound prompts as
// claude stream-json user messages.
//
// ClaudeProvider is the stateless factory value stored in a SpawnSpec; its
// Fresh() yields a per-child *claudeProvider that carries the translation state
// (accumulated messages, pending tool calls, turn flag, model) used by
// BusFrames. The factory's own BusFrames is a no-op — only the per-child
// instance translates.
type ClaudeProvider struct{}

// Fresh returns a per-child stateful translator instance. Each Child gets its
// own so accumulated messages / pending tool calls never leak across children.
func (ClaudeProvider) Fresh() ProtocolProvider { return newClaudeProvider() }

// BootstrapFrame is nil: claude needs no kickoff RPC (unlike pi's get_state
// probe). It is ready for input the instant the process launches.
func (ClaudeProvider) BootstrapFrame() []byte { return nil }

// ReadyOnSpawn is true: an un-prompted `claude -p --input-format stream-json`
// emits NOTHING on stdout (verified: zero bytes with stdin open + no input) —
// the SessionStart hooks, system/init, and the turn all stream only once the
// first user message arrives. So there is no stdout signal that could drive
// spawning→idle; readiness is process-up. claude buffers stdin, so accepting a
// send the moment the process is live is safe (the message is processed, hooks
// and all, once claude reads it). session_id/model are unknown until that first
// turn's init and are synced to the store then (controller monitorChild).
func (ClaudeProvider) ReadyOnSpawn() bool { return true }

// BusFrames on the factory value is a no-op; translation happens on the
// per-child *claudeProvider returned by Fresh(). The real translator is added in
// Task 3.
func (ClaudeProvider) BusFrames(_ []byte, _ int64) [][]byte { return nil }

// claudeFrame is the minimal envelope shared by claude stream-json objects.
type claudeFrame struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

// Normalizes is true: claude's stdout is its native stream-json, translated to
// pi vocabulary on the bus by the per-child translator. The raw ring is not
// renderable, so the Child captures the bus output into a render-ring.
func (ClaudeProvider) Normalizes() bool { return true }

// Parse classifies one claude stdout line:
//   - system/init                 → FirstResponse (+ session id, model)
//   - assistant (with content)    → agent_start, then one tool_execution_start
//     per tool_use content block
//   - user (with tool_result)     → one tool_execution_end per tool_result block
//   - result                      → agent_end (+ session id)
func (ClaudeProvider) Parse(line []byte) ParseResult {
	var res ParseResult
	var f claudeFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return res
	}

	switch f.Type {
	case "system":
		// system/init carries the resolved session id + model. It does NOT drive
		// readiness — claude is silent until prompted, so spawning→idle is
		// process-up (ReadyOnSpawn), fired by the Child on launch. init arrives
		// only with the first turn; FirstResponse here is harmless (the child is
		// already idle) and is kept so a future non-ReadyOnSpawn path still works.
		if f.Subtype == "init" {
			res.FirstResponse = true
			if f.SessionID != "" || f.Model != "" {
				res.Meta = SnifferMetadata{SessionID: f.SessionID, Model: f.Model}
				res.HasMeta = true
			}
		}

	case "assistant":
		if f.Message != nil && len(f.Message.Content) > 0 {
			res.Events = append(res.Events, ParsedEvent{Type: "agent_start"})
			for _, blk := range f.Message.Content {
				if blk.Type == "tool_use" {
					res.Events = append(res.Events, ParsedEvent{Type: "tool_execution_start"})
				}
			}
		}

	case "user":
		if f.Message != nil {
			for _, blk := range f.Message.Content {
				if blk.Type == "tool_result" {
					res.Events = append(res.Events, ParsedEvent{Type: "tool_execution_end"})
				}
			}
		}

	case "result":
		res.Events = append(res.Events, ParsedEvent{Type: "agent_end"})
		if f.SessionID != "" {
			res.Meta = SnifferMetadata{SessionID: f.SessionID}
			res.HasMeta = true
		}
	}

	return res
}

// EncodeOutbound translates the daemon's normalized outbound frames into claude
// stream-json stdin messages. prompt/steer become a user message; everything
// else (including pi-only frames like set_session_name) is dropped, since claude
// has no equivalent and silently writing them would corrupt the input stream.
func (ClaudeProvider) EncodeOutbound(frame []byte) []byte {
	var in struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(frame, &in); err != nil {
		return nil
	}
	switch in.Type {
	case "prompt", "steer":
		// claude -p --input-format stream-json accepts a user message with the
		// Anthropic message shape. String content is accepted (confirmed in
		// Task 0 Step 3.7); switch to a [{type:text,text:...}] block array here
		// if your capture required block content.
		env := struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}{Type: "user"}
		env.Message.Role = "user"
		env.Message.Content = in.Message
		out, err := json.Marshal(env)
		if err != nil {
			return nil
		}
		return out
	default:
		return nil
	}
}

// OutboundEcho returns the user-message echo frames for a prompt/steer, without
// recording state. This stateless template is never the live per-child provider
// (Fresh returns a *claudeProvider whose OutboundEcho override also records the
// message for agent_end); it exists so a bare ClaudeProvider satisfies
// ProtocolProvider.
func (ClaudeProvider) OutboundEcho(frame []byte, ts int64) [][]byte {
	_, frames, _ := claudeUserEcho(frame, ts)
	return frames
}
