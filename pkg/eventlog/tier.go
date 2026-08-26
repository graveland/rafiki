// SPDX-License-Identifier: Apache-2.0

package eventlog

import rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

// Tier is whether an event is replayable.
//
// The split is about the GUARANTEE a consumer gets, not about storage:
// durable events carry an ordinal and can be resumed from a cursor, ephemeral
// events are best-effort and are dropped rather than blocking a turn. Storage
// follows from that, not the other way round — which is why the volume
// argument ("just persist everything") is the wrong way to reason here.
type Tier int

const (
	// TierDurable is persisted, ordinal-carrying and exactly resumable.
	TierDurable Tier = iota
	// TierEphemeral is best-effort and never replayed.
	TierEphemeral
	// TierAll is a subscription-side value only: it admits both. TierOf never
	// returns it.
	TierAll
)

// admits reports whether a subscription at tier t accepts an event of tier of.
func (t Tier) admits(of Tier) bool {
	return t == TierAll || t == of
}

// durableTypes is the §3.1 classification. Everything not named here is
// ephemeral. Tool events are the volume edge and the first thing to demote if
// measurement says the durable tier is too chatty — demote with numbers, not
// intuition.
var durableTypes = map[string]bool{
	"user_message":         true,
	"assistant_message":    true,
	"agent_status":         true,
	"child_spawned":        true,
	"child_exited":         true,
	"turn_start":           true,
	"turn_end":             true,
	"tool_execution_start": true,
	"tool_execution_end":   true,
	"error":                true,
	"retry":                true,
}

// ephemeralTypes is listed explicitly rather than left as a default so that
// TestEveryEventTypeHasATier can prove every payload was classified on
// purpose. A new event type that nobody classifies fails the build.
var ephemeralTypes = map[string]bool{
	"content_block_delta": true,
}

// TierOf classifies an event. An unrecognised payload is EPHEMERAL, which is
// the safe direction: it is dropped from a resumable stream rather than
// written to the log with an ordinal nobody can interpret.
func TierOf(ev *rafikiv1.Event) Tier {
	if durableTypes[TypeName(ev)] {
		return TierDurable
	}
	return TierEphemeral
}

// AllTypeNames returns every classified type name. Used by the exhaustiveness
// test to prove the classification covers the proto.
func AllTypeNames() []string {
	out := make([]string, 0, len(durableTypes)+len(ephemeralTypes))
	for n := range durableTypes {
		out = append(out, n)
	}
	for n := range ephemeralTypes {
		out = append(out, n)
	}
	return out
}
