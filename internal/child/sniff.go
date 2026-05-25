package child

import "encoding/json"

// SnifferMetadata holds session and model fields extracted from pi response
// frames. Fields are only set when the originating frame carries them.
type SnifferMetadata struct {
	SessionID   string
	SessionFile string
	SessionName string
	Model       string // formatted as "provider/id"
}

// ExtractMetadata inspects a pi-RPC frame and returns metadata fields found in
// known response shapes. ok is false when the frame is not a recognised
// response type, carries no relevant fields, or cannot be decoded.
func ExtractMetadata(frame []byte) (md SnifferMetadata, ok bool) {
	var generic struct {
		Type    string          `json:"type"`
		Command string          `json:"command"`
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &generic); err != nil {
		return md, false
	}
	if generic.Type != "response" || !generic.Success {
		return md, false
	}

	switch generic.Command {
	case "get_state":
		var d struct {
			SessionID   string          `json:"sessionId"`
			SessionFile string          `json:"sessionFile"`
			SessionName string          `json:"sessionName"`
			Model       json.RawMessage `json:"model"`
		}
		if err := json.Unmarshal(generic.Data, &d); err != nil {
			return md, false
		}
		md.SessionID = d.SessionID
		md.SessionFile = d.SessionFile
		md.SessionName = d.SessionName
		md.Model = parseModelField(d.Model)
		return md, true

	case "set_session_name":
		var d struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(generic.Data, &d)
		md.SessionName = d.Name
		return md, md.SessionName != ""

	case "set_model", "cycle_model":
		// Response may wrap the model in data.model or put fields directly.
		var d struct {
			Model json.RawMessage `json:"model"`
		}
		_ = json.Unmarshal(generic.Data, &d)
		md.Model = parseModelField(d.Model)
		return md, md.Model != ""
	}

	return md, false
}

// parseModelField decodes a model JSON value ({"id":"...","provider":"..."})
// and returns the "provider/id" string. Empty string is returned when the
// field is absent, null, or missing required sub-fields.
func parseModelField(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	switch {
	case m.Provider != "" && m.ID != "":
		return m.Provider + "/" + m.ID
	case m.ID != "":
		return m.ID
	default:
		return ""
	}
}
