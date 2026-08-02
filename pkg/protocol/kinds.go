// SPDX-License-Identifier: Apache-2.0

package protocol

// Child kinds. Each names a backend by proper noun: pi and claude are foreign
// tools, fundi is this repo's own agent runtime — the craftsman that does the
// work, as distinct from rafiki, the friend that keeps the history.
//
// Was "agent" until the rafiki consolidation. That was a poor value: every
// child is an agent, including pi and claude, so the one kind that was not a
// proper noun was the one naming our own runtime. No alias for the old
// spelling — session records are disposable and the durable data is in the
// database.
const (
	KindFundi  = "fundi"
	KindPi     = "pi"
	KindClaude = "claude"
)
