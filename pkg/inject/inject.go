// SPDX-License-Identifier: Apache-2.0

// Package inject renders and recognises the envelope around a message rafiki's
// daemon injects into an agent's conversation — a restart nudge, a background
// job's exit, a settled subagent, a third-party notification.
//
// Two things are load-bearing about the envelope, and both are why it is a
// package rather than a string concatenation at each call site:
//
//   - An agent must be able to tell an injected message from something its
//     user typed. The opening element says so, and it says WHAT KIND of thing
//     it is, which is what lets the cockpit decorate a restart differently
//     from a job exit.
//   - The body is not always the daemon's own prose: a subagent's report and a
//     background job's command line are model and operator text, and can
//     contain anything at all. Parsing is anchored at BOTH ends, so a body
//     carrying a closing tag cannot truncate the frame — and Render refuses a
//     kind that would break the opening element.
//
// The package is stdlib-only on purpose: the pgx-free client, the daemon and
// the TUI all import it.
package inject

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind names what a message is: the daemon's reason for speaking, not who
// wrote the words.
//
// The spellings are the event-buffer SOURCES the daemon has always emitted, not
// a new vocabulary: an envelope already in a stored conversation carries one of
// these strings in its source attribute, and renaming one would make every old
// row unclassifiable. A new kind is a new constant here.
type Kind string

const (
	// KindSubagent reports a child agent settling (or failing).
	KindSubagent Kind = "subagents"
	// KindHeartbeat is a periodic "still working" check-in to a parent.
	KindHeartbeat Kind = "subagents-heartbeat"
	// KindJob reports a background bash job's exit.
	KindJob Kind = "jobs"
	// KindSchedule is a schedule fire the operator configured.
	KindSchedule Kind = "schedule"
	// KindExecutor reports an executor-side condition (workspace loss, a
	// container problem) the agent's own tools cannot see.
	KindExecutor Kind = "executor"
	// KindScript is a script child's report to its parent.
	KindScript Kind = "script"
	// KindBudget reports a budget breach or a raised budget.
	KindBudget Kind = "budget"
	// KindDaemonRestart tells a recovered child its in-flight turn was lost.
	KindDaemonRestart Kind = "daemon-restart"
	// KindRateLimit tells a child an upstream rate limit has reset.
	KindRateLimit Kind = "rate-limit"
	// KindContext is an injected context update the harness itself wrote
	// (an environment change, a file-changed notice).
	KindContext Kind = "context"
	// KindClaudeReminder is Claude Code's own <system-reminder> context. It is
	// not rafiki's envelope, but it is injected text all the same, and the
	// cockpit decorates it as its own kind rather than as user speech.
	KindClaudeReminder Kind = "claude-reminder"

	// KindNotification is the FAMILY a third-party notification belongs to.
	// Notifications are emitted under NotificationPrefix + their source
	// ("notify:github"), because the source is also the coalescing group, so
	// this constant is what Family returns rather than what a frame carries.
	KindNotification Kind = "notification"
)

// NotificationPrefix begins every notification's kind. A notification's body
// carries pkg/notify's own <notification> envelope, so the prefix is what tells
// a reader which family the outer frame belongs to.
const NotificationPrefix = "notify:"

// Family maps a raw kind to the group a client decorates by: every
// notification source is one family, and everything else is its own.
func Family(k Kind) Kind {
	if strings.HasPrefix(string(k), NotificationPrefix) {
		return KindNotification
	}
	return k
}

// The envelope. It is deliberately shaped like Claude Code's
// <system-reminder>: an XML-ish element around a plain-text body, so a model
// reads it as harness context rather than as something the user said.
const (
	openPrefix = `<rafiki-event source="`
	openSuffix = `">`
	closeTag   = "</rafiki-event>"
	bodySep    = "\n"
)

// kindPattern bounds the kind that may appear inside the opening element. It is
// wide enough for a notification's source (pkg/notify allows 64 characters)
// after its prefix, and the colon is allowed for exactly that prefix.
var kindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,95}$`)

// Render wraps body in the envelope for kind.
//
// body is inserted verbatim: the frame is parsed by its first line and its
// last line, so nothing inside the body can end it early. The empty body
// renders an empty frame rather than being refused — a caller with nothing to
// say has a bug, but silently dropping the message would hide it.
func Render(kind Kind, body string) (string, error) {
	k := string(kind)
	if !kindPattern.MatchString(k) {
		return "", fmt.Errorf("inject: invalid kind %q", k)
	}
	return openPrefix + k + openSuffix + bodySep + body + bodySep + closeTag, nil
}

// Classify reports whether s is an envelope, and if so its kind and body.
//
// The match is anchored at both ends, so a body that itself contains a
// <rafiki-event> element — a subagent quoting one, say — is still one frame
// with its own kind, never a truncated or misattributed one. A trailing
// newline after the closing element is tolerated; anything else after it is
// not, which is what keeps a message that merely STARTS with an envelope from
// being mistaken for one.
func Classify(s string) (kind Kind, body string, ok bool) {
	// One trailing newline is tolerated, and it is not cosmetic: the envelope
	// is stored in real conversations, so a capture path or an editor that
	// leaves a final newline after the frame must not silently turn the
	// message back into bare user text — the exact defect this package exists
	// to prevent. claudecode.body() tolerates the same one, and a CRLF ending
	// is the same fact on a different platform.
	s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	if !strings.HasPrefix(s, openPrefix) {
		return "", "", false
	}
	rest := s[len(openPrefix):]
	quote := strings.Index(rest, openSuffix)
	if quote < 0 {
		return "", "", false
	}
	k := rest[:quote]
	if !kindPattern.MatchString(k) {
		return "", "", false
	}
	after := rest[quote+len(openSuffix):]
	if !strings.HasPrefix(after, bodySep) {
		return "", "", false
	}
	inner := after[len(bodySep):]
	if strings.HasSuffix(inner, "\n"+closeTag) {
		inner = strings.TrimSuffix(inner, "\n"+closeTag)
	} else if strings.HasSuffix(inner, closeTag) {
		inner = strings.TrimSuffix(inner, closeTag)
	} else {
		return "", "", false
	}
	return Kind(k), inner, true
}

// Coerce maps an arbitrary source string onto a kind Render accepts.
//
// It exists because a caller can hold a source this package's charset refuses
// (a hand-written row, a future pusher, a source with a capital letter), and
// the consequence of REFUSING is worse than the consequence of renaming: a
// frame that cannot be rendered cannot be delivered, and a delivery that fails
// leaves the row pending and blocks every later message to that child. So an
// unrepresentable source is folded into the charset (lowercased, invalid runs
// collapsed to "-") and, if nothing survives, becomes KindContext.
//
// A source already in the charset is returned UNCHANGED — this never rewrites a
// kind the daemon meant.
func Coerce(source string) Kind {
	if source == "" {
		return KindContext
	}
	if kindPattern.MatchString(source) {
		return Kind(source)
	}
	var sb strings.Builder
	lastDash := true // leading separators are dropped, not collapsed into "-"
	for _, r := range strings.ToLower(source) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			lastDash = false
		case r == '.' || r == '_' || r == ':' || r == '-':
			if !lastDash {
				sb.WriteRune(r)
				lastDash = true
			}
		default:
			if !lastDash {
				sb.WriteRune('-')
				lastDash = true
			}
		}
		if sb.Len() >= 96 {
			break
		}
	}
	coerced := strings.Trim(sb.String(), ".-_")
	if !kindPattern.MatchString(coerced) {
		return KindContext
	}
	return Kind(coerced)
}

// Label is the human-facing name of a kind, for a client that has to render
// one. An unrecognised kind is returned verbatim: a daemon newer than the
// client must still produce a readable line.
func Label(k Kind) string {
	switch Family(k) {
	case KindSubagent:
		return "subagent"
	case KindHeartbeat:
		return "heartbeat"
	case KindJob:
		return "job"
	case KindSchedule:
		return "schedule"
	case KindExecutor:
		return "executor"
	case KindScript:
		return "script"
	case KindBudget:
		return "budget"
	case KindDaemonRestart:
		return "daemon restart"
	case KindRateLimit:
		return "rate limit"
	case KindContext:
		return "context"
	case KindNotification:
		return "notification"
	case KindClaudeReminder:
		return "harness reminder"
	}
	return string(k)
}
