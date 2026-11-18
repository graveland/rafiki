// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// resolveWorkspaceTools is the ONE place a spawn's workspace_tools becomes the
// value the child carries. It returns the resolved mode, the InvalidArgument
// text to refuse the request with ("" when it can be honoured), and the reason
// the DEFAULT degraded ("" unless a daemon that has the executor pool still
// could not serve rafiki mode — the caller logs it).
//
// An UNDECLARED mode ("") is the daemon's default for a claude child, and that
// default is rafiki's executor-routed workspace tools wherever this daemon can
// actually serve them. Claude Code's own built-ins are the fallback, not the
// default: they act on whatever host the child was launched on with no way for
// the model to say which machine it meant, and a claude child on an executor is
// very often NOT the machine whose files the work is about.
//
// The default is a CAPABILITY question, not a preference, which is why the
// fallback is silent and an explicit pin is not. A daemon with no executor pool
// has nowhere to route a workspace tool to, and a daemon that serves no MCP
// face for claude children has no replacements to offer; on either, a claude
// child declared nothing and simply keeps its built-ins. Declaring `rafiki`
// there is a request this daemon cannot honour, so it is refused with the
// reason (workspaceToolsRafikiBlocker) rather than downgraded — a caller that
// asked for executor-routed tools must never get a child that quietly reads the
// launch host instead.
//
// `native` is a PIN of the built-ins and always means what it says, on every
// daemon.
func (c *Controller) resolveWorkspaceTools(kind, requested string) (mode, refusal, degraded string) {
	mode, ok := protocol.NormalizeWorkspaceTools(requested)
	if !ok {
		return "", fmt.Sprintf("workspace_tools %q is not one of %q, %q",
			requested, protocol.WorkspaceToolsNative, protocol.WorkspaceToolsRafiki), ""
	}
	blocker := c.workspaceToolsRafikiBlocker(kind)
	if mode == protocol.WorkspaceToolsRafiki {
		return mode, blocker, ""
	}
	// A declared native mode, or a kind that never had built-ins to replace:
	// nothing left to decide.
	if requested != "" || kind != protocol.KindClaude {
		return mode, "", ""
	}
	if blocker == "" {
		return protocol.WorkspaceToolsRafiki, "", ""
	}
	// The default could not be served. On a daemon with NO executor pool that
	// is the ordinary laptop shape and the built-ins are simply correct, so
	// nothing is reported. On a daemon that HAS the pool, the missing piece is
	// the MCP surface — a misconfiguration (a proxy URL that went away, or
	// RAFIKI_PROXY_KINDS dropping claude) whose only other symptom is a tool
	// surface quietly reverting, so it is reported.
	if c.claudeExecutorRouted() {
		return mode, "", blocker
	}
	return mode, "", ""
}

// workspaceToolsResumeRefusal reports why a RESUMED child's stored mode cannot
// be launched by this daemon, or "" when it can.
//
// A resume rebuilds the launch from the child's row and never re-resolves the
// mode (a preset edit must not reach a running or resumed child), so nothing
// else on that path asks whether the daemon can still serve it. When it cannot,
// the argv still disallows Claude Code's built-ins while no MCP face replaces
// them: the child comes back toolless, burns a turn discovering that, and its
// row keeps claiming a mode nothing serves. Refusing is the only option that
// keeps the row true — downgrading the child to the built-ins would silently
// change its tools mid-conversation, which is exactly what the mode exists to
// prevent, and it would rewrite a decision nobody re-made.
//
// The remedy is named in the message because both halves are operator-fixable:
// restore the pool/proxy surface, or spawn a replacement pinned native.
func (c *Controller) workspaceToolsResumeRefusal(kind, mode string) string {
	if kind != protocol.KindClaude || mode != protocol.WorkspaceToolsRafiki {
		return ""
	}
	blocker := c.workspaceToolsRafikiBlocker(kind)
	if blocker == "" {
		return ""
	}
	return "cannot resume: this child was spawned in workspace_tools \"rafiki\", and this daemon can no longer " +
		"serve it (" + blocker + "). Launching it anyway would disable every built-in workspace tool with " +
		"nothing serving their place, so the resume is refused: restore what is missing, or spawn a " +
		"replacement with workspace_tools \"native\"."
}

// workspaceToolsRafikiBlocker reports why this daemon cannot serve rafiki's
// workspace tools to a child of kind, or "" when it can.
//
// The refusals are one class — a mode the child could not honour — and each is
// decided before anything is minted, forked or written, so a refusal starts no
// process. The two capability refusals are also the default's fallback
// condition, so the default and the refusal can never disagree about what this
// daemon can do.
func (c *Controller) workspaceToolsRafikiBlocker(kind string) string {
	// Only a claude child has built-in workspace tools to replace: a fundi
	// child already runs on rafiki's, so the mode is meaningless there
	// rather than merely unsupported.
	if kind != protocol.KindClaude {
		return "workspace_tools applies to claude children; fundi always uses rafiki's workspace tools"
	}
	// The replacements are served over the executor pool: a claude child on
	// the daemon's own host has no executor to route a workspace tool to,
	// and falling back to running them in the daemon process is the one
	// thing the executor architecture exists to prevent.
	if !c.claudeExecutorRouted() {
		return "workspace_tools \"rafiki\" needs the executor pool: a claude child on the daemon host has no executor to route to"
	}
	// ...and over the MCP face, which darajaClaudeParams gates on a
	// configured proxy URL AND this kind being proxied at all. Without both,
	// it injects no mcp__rafiki__* surface: the eight built-ins are
	// disallowed and nothing replaces them, so the child would be toolless.
	// Both halves come from the same resolvers that gate reads
	// (proxyEndpoint, proxyRoutesKind) — never a second notion of
	// "configured" — because a URL whose RAFIKI_PROXY_KINDS excludes claude
	// produces exactly the same toolless child, only harder to diagnose.
	url, _ := c.proxyEndpoint()
	if url == "" || !proxyRoutesKind(kind) {
		return "workspace_tools \"rafiki\" needs the MCP surface: this daemon serves no proxy for claude children (no proxy URL, or claude outside RAFIKI_PROXY_KINDS), so the built-in workspace tools would be disallowed and unreplaced"
	}
	return ""
}
