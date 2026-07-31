package intercept

import (
	"encoding/json"
	"fmt"
)

type InterceptType string

const (
	InterceptNewSession    InterceptType = "new_session"
	InterceptSwitchSession InterceptType = "switch_session"
)

type Decision struct {
	Type        InterceptType
	PiRequestID string
	SessionPath string // for switch_session only
}

// Inspect decodes a ctrl_send frame payload and returns a Decision if the
// command should be intercepted rather than forwarded to pi.
func Inspect(frame []byte) (Decision, bool) {
	if len(frame) == 0 {
		return Decision{}, false
	}
	var hdr struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		SessionPath string `json:"sessionPath"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return Decision{}, false
	}
	switch hdr.Type {
	case "new_session":
		return Decision{
			Type:        InterceptNewSession,
			PiRequestID: hdr.ID,
		}, true
	case "switch_session":
		return Decision{
			Type:        InterceptSwitchSession,
			PiRequestID: hdr.ID,
			SessionPath: hdr.SessionPath,
		}, true
	}
	return Decision{}, false
}

// SynthesizeResponse produces the JSON bytes for a fake pi response to an
// intercepted command. The controller sends this to the client in place of a
// real pi response.
func SynthesizeResponse(command, piRequestID string) []byte {
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
