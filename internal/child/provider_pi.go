package child

import "encoding/json"

// PiProvider implements ProtocolProvider for pi's JSON-RPC stdio protocol. It is
// the default provider and preserves the classification logic that previously
// lived inline in Child.handleFrame.
type PiProvider struct{}

// Fresh returns a fresh PiProvider. pi is stateless on the bus (its stdout
// already IS the AgentSessionEvent stream), so the value is its own per-child
// instance.
func (PiProvider) Fresh() ProtocolProvider { return PiProvider{} }

// BootstrapFrame returns pi's get_state kickoff; pi replies with a
// response.get_state that releases the spawning→idle transition.
func (PiProvider) BootstrapFrame() []byte {
	return []byte(`{"type":"get_state","id":"__bootstrap__"}`)
}

// ReadyOnSpawn is false: pi announces readiness via response.get_state (driven
// by the BootstrapFrame probe), so the Child waits for that stdout signal.
func (PiProvider) ReadyOnSpawn() bool { return false }

// BusFrames is the identity for pi: its stdout already IS the pi
// AgentSessionEvent stream, so each raw line is published verbatim on the bus.
func (PiProvider) BusFrames(line []byte, _ int64) [][]byte {
	return [][]byte{line}
}

// EncodeOutbound is the identity for pi: clients already send native pi frames.
func (PiProvider) EncodeOutbound(frame []byte) []byte { return frame }

// OutboundEcho returns nil: a pi child's stdout already carries the user
// message_start (its stdout IS the AgentSessionEvent stream), so synthesizing an
// echo here would render the user's message twice.
func (PiProvider) OutboundEcho([]byte, int64) [][]byte { return nil }

// Normalizes is false: pi's stdout already IS the pi AgentSessionEvent stream,
// so the raw ring is renderable as-is.
func (PiProvider) Normalizes() bool { return false }

// Parse classifies one pi stdout line. It mirrors the original handleFrame:
//   - response.get_state            → FirstResponse
//   - any recognized metadata frame → Meta (via ExtractMetadata)
//   - non-response events           → one ParsedEvent, with auto_retry_start and
//     extension_ui_request carrying their payloads.
func (PiProvider) Parse(line []byte) ParseResult {
	var res ParseResult
	var hdr struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Method  string `json:"method,omitempty"`
		ID      string `json:"id,omitempty"`
	}
	if err := json.Unmarshal(line, &hdr); err != nil {
		return res
	}

	if hdr.Type == "response" && hdr.Command == "get_state" {
		res.FirstResponse = true
	}

	if md, ok := ExtractMetadata(line); ok {
		res.Meta, res.HasMeta = md, true
	}

	if hdr.Type != "" && hdr.Type != "response" {
		switch hdr.Type {
		case "auto_retry_start":
			var payload struct {
				ErrorMessage string `json:"errorMessage"`
			}
			_ = json.Unmarshal(line, &payload)
			res.Events = append(res.Events, ParsedEvent{Type: "auto_retry_start", RetryError: payload.ErrorMessage})
		case "extension_ui_request":
			res.Events = append(res.Events, ParsedEvent{Type: "extension_ui_request", UI: &PiUIRequestMeta{ID: hdr.ID, Method: hdr.Method}})
		default:
			res.Events = append(res.Events, ParsedEvent{Type: hdr.Type})
		}
	}

	return res
}
