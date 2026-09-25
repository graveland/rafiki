// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/server"
)

// This file is the ONE authorization gate for the Control service: every
// Connect mount the daemon serves composes its interceptors through
// connectControlRoute, and every mount gets the same policy table. It exists
// because the proxy face's Connect route accepted three child credentials
// (per-child secret, per-boot secret + session header, bare per-boot secret —
// all three held by every claude child's environment) and refused none of
// them on anything but Spawn, ListExecutors and the conversation verbs: one
// curl from inside any child could kill any sibling tree, lift any budget,
// or rewrite the presets, skills and pymodules every future agent loads.

// controlPolicy classifies one Control procedure by who may call it. The zero
// value is the fail-closed one: an unclassified procedure — or a procedure
// name from some other service that reaches this interceptor — is userOnly.
type controlPolicy uint8

const (
	// policyUserOnly requires a real user credential (or a nil identity, the
	// unix socket's local trust). It is the DEFAULT: anything that acts with
	// operator authority — budgets, presets, skills, pymodules, git sources,
	// memory writes, the daraja verbs — is here, and a new RPC that misses the
	// table lands here by the completeness test (TestControlPolicyTableCoversEveryProcedure).
	policyUserOnly controlPolicy = iota

	// policyAnyCaller admits any caller, child credentials included, because
	// the procedure is read-only and answers nothing scoped to a user beyond
	// what the caller's own credential already names: ListModels,
	// ListPresets, GetPreset, GetRateLimitStatus. Anything writable must
	// never be listed here.
	policyAnyCaller

	// policyChildScoped marks the nine agent-control verbs a child credential
	// with subtree authority may call — Spawn, Kill, Close, Send, GetHistory,
	// StreamEvents, ListChildren, GetChild, ListTasks. The gate admits a
	// ProvenanceChildToken caller on them, but a procedure name carries no
	// target child id, so the gate cannot check the subtree itself: it admits
	// and the HANDLER resolves the caller's subtree authority through
	// connectapi's ChildScopeSource (cmd/rafikid connect_childscope.go),
	// which reads the stored parent chain via childstore.IsDescendant and
	// refuses the caller's own id — a child is not a descendant of itself.
	// An unwired source fails closed: the handler answers Unavailable, never
	// the operator path. ProvenanceChildAttributed and the bare per-boot
	// secret name no child with authority and stay refused here.
	policyChildScoped
)

// controlProcedurePrefix is the Connect procedure-path prefix of every
// Control service RPC, as req.Spec().Procedure reports it.
const controlProcedurePrefix = "/rafiki.v1.Control/"

// controlPolicyTable maps an RPC's proto name ("Kill", not the full
// procedure path) to its policy. It must have an entry for EVERY procedure of
// the generated Control service — TestControlPolicyTableCoversEveryProcedure
// walks the service descriptor and fails on the first gap, so a new RPC
// cannot land unclassified.
//
// Classification rule, applied verb by verb: does the verb ACT (kill, close,
// budget, write) or ANSWER AS the caller (conversations, recall, executors)?
// Then it is userOnly unless wave 1 will scope it to the caller's subtree
// (childScoped). Does it only READ daemon-wide, non-owned facts? anyCaller —
// and only these four, all read-only:
//
//	ListModels        the model catalog, no owner dimension
//	ListPresets       preset names/metadata; a child may read, not write
//	GetPreset         same
//	GetRateLimitStatus the rate-limit windows already attributed to the
//	                  caller's own (or its owner's) account
var controlPolicyTable = map[string]controlPolicy{
	// childScoped: a per-child credential may call these on its own subtree;
	// the per-verb subtree check lives in the handler behind
	// connectapi.Server.SetChildScopeSource. See policyChildScoped.
	"GetHistory":   policyChildScoped,
	"StreamEvents": policyChildScoped,
	"Send":         policyChildScoped,
	"ListChildren": policyChildScoped,
	"GetChild":     policyChildScoped,
	"Spawn":        policyChildScoped,
	"Kill":         policyChildScoped,
	"Close":        policyChildScoped,
	"ListTasks":    policyChildScoped,

	// anyCaller: the four read-only, non-scoped verbs.
	"ListModels":         policyAnyCaller,
	"ListPresets":        policyAnyCaller,
	"GetPreset":          policyAnyCaller,
	"GetRateLimitStatus": policyAnyCaller,

	// userOnly: everything that acts with operator authority or answers as
	// the caller's identity. Listed explicitly so the completeness test can
	// tell "classified userOnly" from "missing".
	"SetBudget":                policyUserOnly,
	"ListExecutors":            policyUserOnly,
	"ListSkills":               policyUserOnly,
	"GetSkill":                 policyUserOnly,
	"UpsertSkill":              policyUserOnly,
	"DeleteSkill":              policyUserOnly,
	"SetSkillEnabled":          policyUserOnly,
	"ListPymodules":            policyUserOnly,
	"GetPymodule":              policyUserOnly,
	"PutPymodule":              policyUserOnly,
	"DeletePymodule":           policyUserOnly,
	"AddPymoduleGitSource":     policyUserOnly,
	"ListPymoduleGitSources":   policyUserOnly,
	"RefreshPymoduleGitSource": policyUserOnly,
	"RemovePymoduleGitSource":  policyUserOnly,
	"PutPreset":                policyUserOnly,
	"DeletePreset":             policyUserOnly,
	"Recall":                   policyUserOnly,
	"RecallContext":            policyUserOnly,
	"GetMemory":                policyUserOnly,
	"MemoryTree":               policyUserOnly,
	"PutMemory":                policyUserOnly,
	"DeleteMemory":             policyUserOnly,
	"RecallBackfill":           policyUserOnly,
	"RecallStatus":             policyUserOnly,
	"ConversationSearch":       policyUserOnly,
	"ConversationExport":       policyUserOnly,
	"ConversationQuery":        policyUserOnly,
	"ConversationReview":       policyUserOnly,
	"ConversationFindings":     policyUserOnly,
	"DarajaLaunch":             policyUserOnly,
	"DarajaSend":               policyUserOnly,
	"DarajaWatch":              policyUserOnly,
}

// policyFor resolves a Connect procedure path to its policy. A path that is
// not a Control procedure, or a Control RPC missing from the table (the
// completeness test makes this unreachable in practice), is userOnly: the
// zero value fails closed.
func policyFor(procedure string) controlPolicy {
	name, ok := strings.CutPrefix(procedure, controlProcedurePrefix)
	if !ok {
		return policyUserOnly
	}
	return controlPolicyTable[name]
}

// connectControlRoute composes the Connect interceptors for a Control route:
// the caller's interceptors first (admission, identity resolution), the
// policy gate last, where it sees the identity the earlier ones resolved.
// Every mount of the Control service goes through here — proxy.go's proxy
// face, connect_uds.go's local socket, and the tests — so there is exactly
// one place that decides what guards the plane, and no second wiring that a
// future change can forget.
func connectControlRoute(srv *connectapi.Server, before ...connect.Interceptor) (string, http.Handler) {
	return srv.Routes(append(before, connectPolicyInterceptor())...)
}

// connectPolicyInterceptor returns the Control service's authorization gate:
// the interceptor form of the old requireUserCredential, generalized from two
// hand-placed call sites to every procedure via controlPolicyTable.
func connectPolicyInterceptor() connect.Interceptor { return controlPolicyGate{} }

type controlPolicyGate struct{}

func (controlPolicyGate) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := authorizeControlProcedure(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	})
}

func (controlPolicyGate) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (controlPolicyGate) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := authorizeControlProcedure(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	})
}

// authorizeControlProcedure is the userOnly implementation — the logic the
// old requireUserCredential applied by hand at Spawn and ListExecutors —
// applied to every procedure through the table.
//
// A nil identity is NOT refused: on the unix socket the socket itself is the
// credential and the caller is anonymous (the socket decided admission by
// filesystem permission), exactly as before provenance existed. Every
// presented credential that is not a user credential — a per-child secret,
// the per-boot secret with or without its session header, or anything else
// that resolves to the zero identity — is a child credential: userOnly
// procedures refuse it, anyCaller procedures admit it, and a childScoped
// procedure admits ONLY the per-child secret, because only that credential
// names a child with subtree authority. The admitted child caller is still
// not an operator: each childScoped handler resolves the caller's subtree
// through the wired ChildScopeSource and refuses every target outside it.
func authorizeControlProcedure(ctx context.Context, procedure string) error {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.IsUserCredential() {
		return nil
	}
	policy := policyFor(procedure)
	if policy == policyAnyCaller {
		return nil
	}
	if policy == policyChildScoped && id.Via == server.ProvenanceChildToken {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied,
		errors.New("procedure "+procedure+" requires a user credential; the presented credential is "+childCredentialKind(id)))
}

// childCredentialKind names the credential in the refusal — never its value —
// so an operator reading a denied RPC can tell which of the three child
// credentials the caller held.
func childCredentialKind(id *server.Identity) string {
	switch id.Via {
	case server.ProvenanceChildToken:
		return "a per-child secret (child " + id.ChildID + ")"
	case server.ProvenanceChildAttributed:
		return "the daemon's per-boot secret attributed to a child session"
	default:
		// ProvenanceUnknown — the empty Identity{} a bare per-boot token
		// resolves to. A child credential, not a user.
		return "the daemon's per-boot secret with no session attribution"
	}
}
