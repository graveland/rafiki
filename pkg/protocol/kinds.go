// SPDX-License-Identifier: Apache-2.0

package protocol

// Child kinds. Each names a backend by proper noun: claude is a foreign tool,
// fundi is this repo's own agent runtime — the craftsman that does the work,
// as distinct from rafiki, the friend that keeps the history.
//
// "pi" was a third kind and was retired in Phase C0 (2026-08-25). No alias for
// it: zero pi children exist in any database, and a spawn naming it should
// fail loudly rather than resolve to something else.
const (
	KindFundi  = "fundi"
	KindClaude = "claude"
)
