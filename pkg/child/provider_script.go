// SPDX-License-Identifier: Apache-2.0

package child

// ScriptProvider is the ProtocolProvider for kind=script children: a saved
// pymodule run as a process, whose stdout is free-form text — NOT a JSONL
// agent protocol. The provider's job is narrower than any translator's:
//
//   - every line means "the child is alive and speaking". Parse reports
//     FirstResponse (driving spawning→idle, which closes Idle() and unblocks
//     activateLiveChild's post-spawn wait) and one synthesized agent_start
//     (idle→streaming), so the rail renders a running script as WORKING and
//     the busy checks treat it as mid-flight. Both are idempotent on every
//     later line: OnFirstResponse transitions only from spawning, and
//     streaming→streaming is not a transition.
//   - BusFrames is identity: the raw line is published verbatim, so the ring,
//     the log dumps, logs/tail/watch and the cockpit transcript show exactly
//     what the script printed, like any child's output.
//   - stdin carries no protocol, so BootstrapFrame is nil and EncodeOutbound
//     drops every frame: a script child's messages arrive through its inbox
//     and its Receive stream (deliverInbox defers there), never through
//     stdin. Dropping rather than forwarding is the backstop that keeps a
//     stray frame write from corrupting a script's own stdin reads.
type ScriptProvider struct{}

// ScriptProvider is the canonical provider for script children. It is
// stateless, so Fresh returns the same value.
func (ScriptProvider) Fresh() ProtocolProvider { return ScriptProvider{} }

// BootstrapFrame returns nil: nothing is ever written to a script's stdin.
func (ScriptProvider) BootstrapFrame() []byte { return nil }

func (ScriptProvider) ReadyOnSpawn() bool { return false }

// Parse classifies one stdout line of a script child. Every line counts as
// liveness, regardless of its content.
func (ScriptProvider) Parse(line []byte) ParseResult {
	var res ParseResult
	res.FirstResponse = true
	res.Events = append(res.Events, ParsedEvent{Type: "agent_start"})
	return res
}

// BusFrames is identity: each raw line is published verbatim.
func (ScriptProvider) BusFrames(line []byte, _ int64) [][]byte {
	return [][]byte{line}
}

// EncodeOutbound drops every frame: a script child has no stdin protocol.
func (ScriptProvider) EncodeOutbound([]byte) []byte { return nil }

// OutboundEcho returns nil: there is no outbound channel to echo.
func (ScriptProvider) OutboundEcho([]byte, int64) [][]byte { return nil }

func (ScriptProvider) Normalizes() bool { return false }
