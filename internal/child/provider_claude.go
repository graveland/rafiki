package child

import "encoding/json"

// ClaudeProvider implements ProtocolProvider for Claude Code's stream-json stdio
// protocol (claude -p --input-format stream-json --output-format stream-json
// --verbose). It maps claude's system/assistant/user/result objects onto the
// daemon's normalized state-machine vocabulary, and encodes outbound prompts as
// claude stream-json user messages.
type ClaudeProvider struct{}

// BootstrapFrame is nil: claude begins working only when it receives a user
// message, and emits its system/init line on startup without any kickoff.
func (ClaudeProvider) BootstrapFrame() []byte { return nil }

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

// EncodeOutbound is implemented in Task 5.
func (ClaudeProvider) EncodeOutbound(frame []byte) []byte { return frame }
