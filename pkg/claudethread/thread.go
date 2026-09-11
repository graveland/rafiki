// SPDX-License-Identifier: Apache-2.0

// Package claudethread parses the two signals a Claude Code request carries
// about which conversational thread it belongs to. It is pure: no database, no
// HTTP, no state, and nothing beyond encoding/json and strings.
//
// Claude Code sends every thread in one process (the main thread, each Task
// subagent, the titler) to the same endpoint with the same session header, so
// the proxy cannot tell them apart from transport alone. These two fields are
// how it can.
package claudethread

import (
	"encoding/json"
	"strings"
)

// billingPrefix is the line prefix inside system block 0's text. Measured
// present on 4920 of 4922 stored prefixes carrying a system array.
const billingPrefix = "x-anthropic-billing-header:"

// Billing is the parsed x-anthropic-billing-header line.
type Billing struct {
	Version    string // cc_version, e.g. "2.1.267.019"
	Entrypoint string // cc_entrypoint: "cli" for the main thread, "sdk-cli" for a Task subagent
	IsSubagent bool   // cc_is_subagent
	PrevReq    string // cc_prev_req, the previous request id in this thread
	PromptID   string // cc_prompt_id. NOT a thread key: concurrent subagents from one template share it.
}

// ParseBillingHeader extracts the billing line from a system block's text.
// ok is false when the text carries no such line.
//
// IsSubagent is present-only-when-true: cc_is_subagent=false appears zero
// times in 4922 measured turns, so an absent key means the main thread, not
// an unknown one.
func ParseBillingHeader(systemText string) (Billing, bool) {
	for _, text := range strings.Split(systemText, "\n") {
		if !strings.HasPrefix(text, billingPrefix) {
			continue
		}
		// Line-anchored on purpose: the literal can appear mid-line in prose
		// (a future block 0 quoting the header), and a substring match would
		// read the wrong line.
		line := text[len(billingPrefix):]
		var b Billing
		for _, field := range strings.Split(line, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
			if !ok {
				continue
			}
			switch k {
			case "cc_version":
				b.Version = v
			case "cc_entrypoint":
				b.Entrypoint = v
			case "cc_is_subagent":
				// Monotonic: a duplicate key must not clear a set flag.
				if v == "true" {
					b.IsSubagent = true
				}
			case "cc_prev_req":
				b.PrevReq = v
			case "cc_prompt_id":
				b.PromptID = v
			}
		}
		return b, true
	}
	return Billing{}, false
}

// BillingFromRequest reads system block 0 out of an Anthropic Messages request
// and parses its billing line. Every shape other than "an array whose first
// element has a text field" answers ok=false rather than failing: system is
// absent on 30 of 4952 stored prefixes, and a non-Claude-Code client may send
// a plain string.
func BillingFromRequest(reqBody []byte) (Billing, bool) {
	var req struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return Billing{}, false
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(req.System, &blocks); err != nil || len(blocks) == 0 {
		return Billing{}, false
	}
	return ParseBillingHeader(blocks[0].Text)
}

// DeclaresClientTools reports whether the request declares at least one tool
// the CLIENT must execute — an entry carrying an input_schema.
//
// This separates a Task subagent from Claude Code's per-tool-call model
// helpers, which cc_is_subagent alone does not: the flag means "not the main
// thread", and Claude Code stamps it on the haiku one-shots it fires to
// summarize a WebFetch page and to drive a WebSearch. Measured on one session
// (c_01M28E9XZQTHR7SRFZE0N5S0VE, 2026-09-11): 299 helper branches, every one
// with zero client tools, against 10 real subagents with 7-11 each. The
// WebSearch helper does carry a tool, but a SERVER one
// ({"name":"web_search","type":"web_search_20250305"}) — the model never
// hands it back to the caller, so it does not make the request an agent.
//
// The rule is "can this request act", which is why it keys on input_schema
// rather than on the prompt text or the model: an agent that cannot call a
// tool can only answer once, which is the same argument that keeps a
// skill-less skill tool out of tools[].
//
// Anthropic's type-shorthand CLIENT tools (computer_*, text_editor_*, bash_*)
// carry no input_schema and would read as no-tools here. Claude Code does not
// use them — it declares its own Bash with a full schema — and the failure
// direction is a real subagent losing its rail row, never a helper gaining
// one.
func DeclaresClientTools(reqBody []byte) bool {
	var req struct {
		Tools []struct {
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return false
	}
	for _, t := range req.Tools {
		// A literal null decodes to the four bytes "null", not to an empty
		// RawMessage, so length alone would read a malformed declaration as a
		// client tool.
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			return true
		}
	}
	return false
}

// PreviousMessageID returns diagnostics.previous_message_id, the id of the
// assistant message this request's thread last received, or "" when absent.
//
// This is the thread chain. Measured on conversation
// 01a08be4-4315-7ca5-afd2-ccc0ecf6a3b2: 177 turns, 172 carried it, all 172
// distinct, no forks, across three concurrent Task subagents. Across the whole
// database only 6 of 3583 linked turns share a predecessor (retries), max
// fan-out 4. Absent means a thread root.
func PreviousMessageID(reqBody []byte) string {
	var req struct {
		Diagnostics struct {
			PreviousMessageID string `json:"previous_message_id"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return ""
	}
	return req.Diagnostics.PreviousMessageID
}
