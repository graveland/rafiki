package main

import (
	"encoding/json"
	"fmt"
)

type interceptType string

const (
	interceptNewSession    interceptType = "new_session"
	interceptSwitchSession interceptType = "switch_session"
)

type interceptDecision struct {
	Type        interceptType
	PiRequestID string
	SessionPath string // for switch_session only
}

// inspect decodes a ctrl_send frame payload and returns a interceptDecision if the
// command should be intercepted rather than forwarded to pi.
func inspect(frame []byte) (interceptDecision, bool) {
	if len(frame) == 0 {
		return interceptDecision{}, false
	}
	var hdr struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		SessionPath string `json:"sessionPath"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return interceptDecision{}, false
	}
	switch hdr.Type {
	case "new_session":
		return interceptDecision{
			Type:        interceptNewSession,
			PiRequestID: hdr.ID,
		}, true
	case "switch_session":
		return interceptDecision{
			Type:        interceptSwitchSession,
			PiRequestID: hdr.ID,
			SessionPath: hdr.SessionPath,
		}, true
	}
	return interceptDecision{}, false
}

// synthesizeResponse produces the JSON bytes for a fake pi response to an
// intercepted command. The controller sends this to the client in place of a
// real pi response.
func synthesizeResponse(command, piRequestID string) []byte {
	type data struct {
		Cancelled bool `json:"cancelled"`
	}
	type response struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		ID      string `json:"id,omitempty"`
		Success bool   `json:"success"`
		Data    data   `json:"data"`
	}
	b, err := json.Marshal(response{
		Type:    "response",
		Command: command,
		ID:      piRequestID,
		Success: true,
		Data:    data{Cancelled: false},
	})
	if err != nil {
		// json.Marshal of these fixed types cannot realistically fail.
		panic(fmt.Sprintf("synth response: %v", err))
	}
	return b
}
