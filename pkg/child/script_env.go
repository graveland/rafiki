// SPDX-License-Identifier: Apache-2.0

package child

import "strings"

// ScriptEnvStripPrefixes are the variable-name prefixes a script child's
// process environment is stripped of, wherever that environment is built:
//
//   - RAFIKI_* — the daemon's and the executor's own control-plane variables
//     (the control socket, the profile, the launch ticket, the per-child
//     secret). The deliberate exception is RAFIKI_CHILD_CONNECT, the one
//     channel the child is GIVEN, which is appended after the strip.
//   - ANTHROPIC_* / OPENROUTER_* — LLM credentials the child has no business
//     holding.
//
// The rule is applied at every plane the environment crosses: the daemon
// strips its own environ before building a local script child's env AND
// before putting a forwarded env on a launch payload; the executor strips
// again before applying the payload; the daraja host strips its own inherited
// environ before building the hosted process's env. Three applications of one
// rule, because a credential that survives ANY one plane is out. This
// constant is the single definition; the three sites delegate to it (and
// ScriptEnvStripped) rather than keeping literals that can drift.
//
// OPERATOR NOTE (intentional, reviewed): an executor's own environment may
// carry FUNCTIONAL variables under these prefixes — RAFIKI_PYMODULE_PYTHON
// above all, which the executor reads when resolving a script launch. The
// strip is still correct: those variables name executor-side behaviour, they
// are consumed before the process is built, and a script has no business
// reading the executor's control plane even when the value is not a
// credential.
var ScriptEnvStripPrefixes = []string{"RAFIKI_", "ANTHROPIC_", "OPENROUTER_"}

// ScriptEnvStripped reports whether an environment entry — a bare name or a
// full "NAME=value" pair — carries one of ScriptEnvStripPrefixes.
func ScriptEnvStripped(e string) bool {
	k := e
	if i := strings.IndexByte(e, '='); i >= 0 {
		k = e[:i]
	}
	for _, p := range ScriptEnvStripPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
