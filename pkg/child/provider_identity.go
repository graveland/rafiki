// SPDX-License-Identifier: Apache-2.0

package child

import "encoding/json"

// identityProvider is the identity ProtocolProvider: its stdout already carries
// the AgentSessionEvent vocabulary, so every method passes through unchanged.
// Used by fundi (fundi.Engine emits pi frames directly) and was historically
// the provider for pi children (retired in B4).
type IdentityProvider struct{}

// IdentityProvider is the canonical identity provider for children whose stdout
// already carries the AgentSessionEvent stream.

// Fresh returns the same value: identity is stateless.
func (IdentityProvider) Fresh() ProtocolProvider { return IdentityProvider{} }

// BootstrapFrame returns the get_state kickoff probe.
func (IdentityProvider) BootstrapFrame() []byte {
	return []byte(`{"type":"get_state","id":"__bootstrap__"}`)
}

func (IdentityProvider) ReadyOnSpawn() bool { return false }

// BusFrames is identity: each raw line is published verbatim.
func (IdentityProvider) BusFrames(line []byte, _ int64) [][]byte {
	return [][]byte{line}
}

// EncodeOutbound is identity: outbound frames are passed through unchanged.
func (IdentityProvider) EncodeOutbound(frame []byte) []byte { return frame }

// OutboundEcho returns nil: the stdout carries the user echo, so no synthesis.
func (IdentityProvider) OutboundEcho([]byte, int64) [][]byte { return nil }

func (IdentityProvider) Normalizes() bool { return false }

// Parse classifies one stdout line.
func (IdentityProvider) Parse(line []byte) ParseResult {
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
