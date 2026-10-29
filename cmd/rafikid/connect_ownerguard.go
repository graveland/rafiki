// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/server"
)

// This file is the non-admin user credential's owner isolation for the
// Connect Control plane — the handler-layer half of owner isolation, the way
// connect_childscope.go is the child credential's subtree half. The policy
// gate (connect_policy.go) admits every user credential to every procedure;
// the per-user boundary therefore lives where the request body is visible, in
// the handlers, behind connectapi's OwnerGuardSource.
//
// ownerGuardFor resolves ONE caller's guard from the request context:
//
//   - nil identity (unix-socket local trust): nil — full authority, as before.
//   - admin user credential: nil — full authority, the executor precedent
//     (connect_executoradmin.go's executorAuthority).
//   - non-admin user credential: a guard bound to id.UserID.
//   - ProvenanceChildToken: nil — the existing ChildScope governs; resolving
//     a guard here would double-check nothing and mis-scope everything.
//   - anything else (bare per-boot secret, attributed): nil — the policy gate
//     already refused it before a handler ran.
//
// ownerGuard reads ownership through Controller.OwnerUserIDForChild, the
// parent-chain attribution walk the proxy face uses (controller.go): a child
// stamped at spawn resolves on its own row; a legacy NULL-owner row under an
// owned ancestor resolves to that ancestor's owner; only a lineage with NO
// owner anywhere — the anonymous UDS spawn shape, and every pre-column
// top-level row — resolves to nothing, and is admin/UDS-only. Foreign,
// unknown, empty and unowned ids all get the ONE refusal text, so a caller
// cannot probe which children exist by asking.

// ownerGuard implements connectapi.OwnerGuard for one non-admin user
// credential.
type ownerGuard struct {
	c      *Controller
	userID string
}

var _ connectapi.OwnerGuard = ownerGuard{}

func (g ownerGuard) owns(childID string) bool {
	owner, ok := g.c.OwnerUserIDForChild(childID)
	return ok && owner == g.userID
}

func (g ownerGuard) Authorize(childID string) error {
	if childID == "" || !g.owns(childID) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
			"agent %s is not owned by you; you may only view, steer or kill agents you own", childID))
	}
	return nil
}

func (g ownerGuard) Owns(childID string) bool {
	return childID != "" && g.owns(childID)
}

// OwnsConversation is ChildScope.ConversationInScope with the ownership
// boundary swapped for the subtree one: resolve the conversation to the live
// child row that owns it (the same conversationIDForChild mapping), then
// answer owns on that child. Unresolvable is not owned — refuse, never guess.
//
// The cost is ConversationInScope's, deliberately: one pass over the LIVE
// childstore, and one indexed query per claude-kind row (a fundi row carries
// its conversation UUID on the snapshot). Both are bounded by the live fleet,
// and only ListTasks consults this. A single conversation→owner lookup would
// be cheaper AND would answer for a closed child, but it would also be a
// second resolution path that could disagree with the one ChildScope uses —
// the drift risk this seam exists to avoid. Revisit both together.
func (g ownerGuard) OwnsConversation(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	for _, snap := range g.c.st.List() {
		if g.c.conversationIDForChild(snap) == conversationID {
			return g.owns(snap.ChildID)
		}
	}
	return false
}

func (c *Controller) ownerGuardFor(ctx context.Context) connectapi.OwnerGuard {
	id := server.IdentityFromContext(ctx)
	if id == nil || !id.IsUserCredential() || id.IsAdmin {
		return nil
	}
	return ownerGuard{c: c, userID: id.UserID}
}

// ownerIsolation classifies one Control procedure by how a non-admin user
// credential is bounded on it. The zero value is ownerExempt, which requires
// a reason string — an unreasoned exemption fails the completeness test.
type ownerIsolation uint8

const (
	ownerExempt   ownerIsolation = iota
	ownerGuarded                 // handler calls Authorize on the target before any side effect
	ownerFiltered                // handler narrows the answer to children the caller owns
)

// ownerIsolationTable names every Control procedure's owner isolation, the
// owner twin of controlPolicyTable. TestControlOwnerIsolationCoversEveryProcedure
// walks the Control service descriptor and fails on a gap, a stale name, or an
// exempt entry without a reason.
//
// It is NOT consulted at runtime — the guard runs in the handlers, not in an
// interceptor — so its only job is to make an isolation decision mandatory for
// every RPC that lands.
//
// Classification rule: does the verb act on a child id (or a child's
// conversation)? ownerGuarded. Does it list children or aggregate them?
// ownerFiltered. Otherwise ownerExempt with the reason the procedure's own
// owner boundary already exists (or is deliberately out of scope).
var ownerIsolationTable = map[string]ownerIsolation{
	// Guarded: the handler authorizes the named target through the caller's
	// OwnerGuard before any side effect.
	"Send":         ownerGuarded,
	"Kill":         ownerGuarded,
	"Close":        ownerGuarded,
	"GetChild":     ownerGuarded,
	"GetHistory":   ownerGuarded,
	"SetBudget":    ownerGuarded,
	"SetRouting":   ownerGuarded,
	"Rename":       ownerGuarded,
	"Resume":       ownerGuarded,
	"SetLabels":    ownerGuarded,
	"SendFrame":    ownerGuarded,
	"GetStreams":   ownerGuarded,
	"ListTasks":    ownerGuarded,
	"StreamEvents": ownerGuarded, // child/subtree subject; the All subject is filtered below
	// Spawn is guarded on parent_child_id only: the owner stamp itself comes
	// from the credential, so a top-level spawn needs no target check, while a
	// spawn under a foreign parent would put the new child in another user's
	// tree and break owner inheritance for every cascade below it.
	"Spawn": ownerGuarded,

	// Filtered: the handler narrows its answer to children the caller owns.
	// StreamEvents' All subject is filtered by a send-side wrapper; the rest
	// filter the rows they return.
	"ListChildren":   ownerFiltered,
	"Search":         ownerFiltered,
	"CloseAllExited": ownerFiltered,
	"Status":         ownerFiltered,

	// Exempt, with the reason recorded in ownerExemptReasons.
	"ConversationSearch":   ownerExempt,
	"ConversationExport":   ownerExempt,
	"ConversationQuery":    ownerExempt,
	"ConversationStats":    ownerExempt,
	"ConversationReview":   ownerExempt,
	"ConversationFindings": ownerExempt,

	"Recall":        ownerExempt,
	"RecallContext": ownerExempt,
	"GetMemory":     ownerExempt,
	"MemoryTree":    ownerExempt,
	"PutMemory":     ownerExempt,
	"DeleteMemory":  ownerExempt,

	"ListSandboxes": ownerExempt,

	"CreateSandbox": ownerExempt,
	"RemoveSandbox": ownerExempt,
	"SyncPath":      ownerExempt,
	"SyncRepo":      ownerExempt,

	"ListPymodules":  ownerExempt,
	"GetPymodule":    ownerExempt,
	"PutPymodule":    ownerExempt,
	"DeletePymodule": ownerExempt,

	"PutPreset":    ownerExempt,
	"DeletePreset": ownerExempt,

	"ListModelAliases": ownerExempt,
	"PutModelAlias":    ownerExempt,
	"DeleteModelAlias": ownerExempt,

	"Report":    ownerExempt,
	"Receive":   ownerExempt,
	"SetResult": ownerExempt,

	"ListModels":         ownerExempt,
	"ListPresets":        ownerExempt,
	"GetPreset":          ownerExempt,
	"GetRateLimitStatus": ownerExempt,
	"ModelInfo":          ownerExempt,
	"ModelRoutes":        ownerExempt,
	"ListProviderBans":   ownerExempt,
	"ListRoutes":         ownerExempt,

	"ListExecutors":   ownerExempt,
	"EnrollExecutor":  ownerExempt,
	"CreateExecutor":  ownerExempt,
	"LabelExecutor":   ownerExempt,
	"DisableExecutor": ownerExempt,
	"EnableExecutor":  ownerExempt,
	"DeleteExecutor":  ownerExempt,

	"ExecutorSession": ownerExempt,

	"CreateUser": ownerExempt,
	"ListUsers":  ownerExempt,
	"RemoveUser": ownerExempt,
	"UpdateUser": ownerExempt,

	"MintToken":   ownerExempt,
	"ListTokens":  ownerExempt,
	"RevokeToken": ownerExempt,

	"BeginProviderLogin":       ownerExempt,
	"CompleteProviderLogin":    ownerExempt,
	"DeleteProviderCredential": ownerExempt,

	"ListSkills":      ownerExempt,
	"GetSkill":        ownerExempt,
	"DeleteSkill":     ownerExempt,
	"SetSkillEnabled": ownerExempt,

	"AddRepo":     ownerExempt,
	"ListRepos":   ownerExempt,
	"RefreshRepo": ownerExempt,
	"SetRepo":     ownerExempt,
	"RemoveRepo":  ownerExempt,

	"RecallBackfill": ownerExempt,
	"RecallStatus":   ownerExempt,

	"Notify": ownerExempt,

	"DarajaLaunch": ownerExempt,
	"DarajaSend":   ownerExempt,
	"DarajaWatch":  ownerExempt,

	"BanProvider":   ownerExempt,
	"UnbanProvider": ownerExempt,
	"SetRoute":      ownerExempt,
	"DeleteRoute":   ownerExempt,

	"ShutdownDaemon": ownerExempt,
}

// ownerExemptReasons is why each ownerExempt procedure needs no handler-layer
// owner guard. The completeness test requires one non-empty entry per exempt
// procedure: an exemption nobody justified is how a leak ships.
var ownerExemptReasons = map[string]string{
	"ConversationSearch":   "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",
	"ConversationExport":   "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",
	"ConversationQuery":    "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",
	"ConversationStats":    "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",
	"ConversationReview":   "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",
	"ConversationFindings": "conversation verbs are owner-scoped by scopeFor to insights.ScopeOwner for a non-admin user",

	"Recall":        "recallOwner binds the caller's owner's non-admin identity",
	"RecallContext": "recallOwner binds the caller's owner's non-admin identity",
	"GetMemory":     "recallOwner binds the caller's owner's non-admin identity",
	"MemoryTree":    "recallOwner binds the caller's owner's non-admin identity",
	"PutMemory":     "recallOwner binds the caller's owner's non-admin identity",
	"DeleteMemory":  "recallOwner binds the caller's owner's non-admin identity",

	"ListSandboxes": "ownerScoped: answers the caller's owner's rows",

	"CreateSandbox": "sandboxCaller resolves the owner from the credential; the Controller enforces",
	"RemoveSandbox": "sandboxCaller resolves the owner from the credential; the Controller enforces",
	"SyncPath":      "sandboxCaller resolves the owner from the credential; the Controller enforces",
	"SyncRepo":      "sandboxCaller resolves the owner from the credential; the Controller enforces",

	"ListModelAliases": "model aliases resolve against the connection's owner",
	"PutModelAlias":    "model aliases resolve against the connection's owner",
	"DeleteModelAlias": "model aliases resolve against the connection's owner",

	"ListPymodules":  "the caller's owner's pymodule corpus",
	"GetPymodule":    "the caller's owner's pymodule corpus",
	"PutPymodule":    "the caller's owner's pymodule corpus",
	"DeletePymodule": "the caller's owner's pymodule corpus",

	"PutPreset":    "presets resolve against the connection's owner",
	"DeletePreset": "presets resolve against the connection's owner",

	"Report":    "identity-position verbs: a user credential names no child and the handler refuses by construction",
	"Receive":   "identity-position verbs: a user credential names no child and the handler refuses by construction",
	"SetResult": "identity-position verbs: a user credential names no child and the handler refuses by construction",

	"ListModels":         "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"ListPresets":        "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"GetPreset":          "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"GetRateLimitStatus": "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"ModelInfo":          "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"ModelRoutes":        "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"ListProviderBans":   "read-only, non-scoped facts; no owner dimension (anyCaller)",
	"ListRoutes":         "read-only, non-scoped facts; no owner dimension (anyCaller)",

	"ListExecutors":   "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"EnrollExecutor":  "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"CreateExecutor":  "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"LabelExecutor":   "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"DisableExecutor": "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"EnableExecutor":  "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",
	"DeleteExecutor":  "executorAuthority scopes to the caller's rows; enroll/create stamp the owner from the connection",

	"ExecutorSession": "executor surface; owner checks are the executor admin seam's concern (out of scope)",

	"CreateUser": "admin check in the handler",
	"ListUsers":  "admin check in the handler",
	"RemoveUser": "admin check in the handler",
	"UpdateUser": "admin check in the handler",

	"MintToken":   "self-scoped: a user manages its own credentials",
	"ListTokens":  "self-scoped: a user manages its own credentials",
	"RevokeToken": "self-scoped: a user manages its own credentials",

	"BeginProviderLogin":       "self-scoped: a user manages its own provider logins",
	"CompleteProviderLogin":    "self-scoped: a user manages its own provider logins",
	"DeleteProviderCredential": "self-scoped: a user manages its own provider logins",

	"ListSkills":      "owner-layered skill store",
	"GetSkill":        "owner-layered skill store",
	"DeleteSkill":     "owner-layered skill store",
	"SetSkillEnabled": "owner-layered skill store",

	"AddRepo":     "per-owner repos",
	"ListRepos":   "per-owner repos",
	"RefreshRepo": "per-owner repos",
	"SetRepo":     "per-owner repos",
	"RemoveRepo":  "per-owner repos",

	"RecallBackfill": "recall owner binding",
	"RecallStatus":   "recall owner binding",

	"Notify": "the owner is resolved from the connection identity; a user targets only its own top-level agents",

	"DarajaLaunch": "daraja children carry no childstore row or owner; cross-user daraja isolation is a documented gap (out of scope)",
	"DarajaSend":   "daraja children carry no childstore row or owner; cross-user daraja isolation is a documented gap (out of scope)",
	"DarajaWatch":  "daraja children carry no childstore row or owner; cross-user daraja isolation is a documented gap (out of scope)",

	"BanProvider":   "daemon-wide operator settings; admin-gating management verbs is out of scope",
	"UnbanProvider": "daemon-wide operator settings; admin-gating management verbs is out of scope",
	"SetRoute":      "daemon-wide operator settings; admin-gating management verbs is out of scope",
	"DeleteRoute":   "daemon-wide operator settings; admin-gating management verbs is out of scope",

	"ShutdownDaemon": "unimplemented stub; no child data",
}
