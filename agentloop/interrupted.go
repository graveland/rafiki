package agentloop

import (
	"encoding/json"
	"fmt"
)

// InterruptedSentinel prefixes every synthetic tool_result injected by
// Resume — stable and greppable in captures and transcripts.
const InterruptedSentinel = "[rafiki:interrupted]"

// interruptedGuidance is the directive text the model receives with a
// synthetic result. Deliberately directive ("call it again") rather than
// neutral: smaller models otherwise tend to apologize and give up instead of
// re-investigating.
const interruptedGuidance = "The process was interrupted before this tool returned. " +
	"It is unknown whether the tool executed. If the tool only reads data, call it " +
	"again now. If it changes state, verify current state before re-running."

// interruptedInputEcho bounds the echoed tool input (helps smaller models
// re-issue the call without hunting through history).
const interruptedInputEcho = 1024

// InterruptedToolResult builds the synthetic tool_result content for an
// orphaned tool_use: sentinel, tool name, truncated input echo, directive
// guidance. No timestamps — the block is a typed constant shape; resume
// metadata belongs on the turn row, not in the prompt.
func InterruptedToolResult(name string, input json.RawMessage) string {
	echo := string(input)
	if len(echo) > interruptedInputEcho {
		echo = echo[:interruptedInputEcho] + "…(truncated)"
	}
	return fmt.Sprintf("%s tool=%s input=%s\n%s", InterruptedSentinel, name, echo, interruptedGuidance)
}
