// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
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
	// operator authority — budgets, skills, pymodule repos,
	// the daraja verbs — is here, and a new RPC that misses the
	// table lands here by the completeness test (TestControlPolicyTableCoversEveryProcedure).
	policyUserOnly controlPolicy = iota

	// policyAnyCaller admits any caller, child credentials included, because
	// the procedure is read-only and answers nothing scoped to a user beyond
	// what the caller's own credential already names: ListModels,
	// ListPresets, GetPreset, GetRateLimitStatus, ListProviderBans,
	// ListRoutes, ModelInfo, ModelRoutes. Anything writable must never be
	// listed here.
	policyAnyCaller

	// policyChildScoped marks the verbs a child credential with subtree
	// authority may call — the agent-control verbs Spawn, Kill, Close, Send,
	// SetBudget, SetRouting, GetHistory, StreamEvents, ListChildren, GetChild,
	// ListTasks;
	// the three script-child verbs Report, Receive, SetResult; the
	// conversation reads ConversationSearch/Export/Query, answered through the
	// caller's subtree (childConversationScope); the pymodule verbs, read
	// from and written to the owner's corpus exactly as the MCP face's
	// pymodule tools do; and PutPreset/DeletePreset, which the handler admits
	// only for a top-level child; and the sandbox verbs CreateSandbox and
	// RemoveSandbox, which a child may call on its OWN containers (the
	// Controller clamps and bounds what it may create or remove). The gate admits a
	// ProvenanceChildToken caller on them, but a procedure name carries no
	// target child id, so the gate cannot check the subtree itself: it admits
	// and the HANDLER resolves the caller's subtree authority through
	// connectapi's ChildScopeSource (cmd/rafikid connect_childscope.go),
	// which reads the stored parent chain via childstore.IsDescendant and
	// refuses the caller's own id — a child is not a descendant of itself.
	// SetRouting from a child is bounded to prefer/sort/quant: SetChildRouting
	// refuses only= from child provenance.
	// (Receive and SetResult never consult Authorize at all: they are
	// self-only verbs, checked by identity, not by subtree.)
	// The source never resolves nil — the operator path — for a child-shaped
	// credential: the empty-ChildID and vanished-row shapes resolve an
	// always-refusing scope, and the daemon wiring itself is pinned end to
	// end by TestConnectChildScopedOnTheConnectPlane (test/integration).
	// ProvenanceChildAttributed and the bare per-boot secret name no child
	// with authority and stay refused here.
	policyChildScoped

	// policyOwnerScoped marks the verbs that answer from the caller's OWNER's
	// data — Recall, RecallContext, GetMemory, MemoryTree, PutMemory,
	// DeleteMemory, ListSandboxes. The gate admits a ProvenanceChildToken caller only (the
	// other two child credentials name no owner and stay refused); the handler
	// resolves the owner's NON-admin identity through recallOwner, so a child
	// of an admin never reads daemon-wide. No subtree check: the surface is the
	// owner's, exactly as the fundi recall tools give a fundi child.
	policyOwnerScoped
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
// and only these eight, all read-only:
//
//	ListModels         the model catalog, no owner dimension
//	ListPresets        preset names/metadata; a child may read, not write
//	GetPreset          same
//	GetRateLimitStatus the rate-limit windows already attributed to the
//	                   caller's own (or its owner's) account
//	ListProviderBans   the ban list names providers, not users
//	ListRoutes         routing defaults name providers, not users
//	ModelInfo          per-model catalog lookup, the twin of ListModels
//	ModelRoutes        per-model endpoint prices/eligibility, the twin of
//	                   ListRoutes and ModelInfo: providers, not users
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
	// SetBudget from a child is NOT operator authority: connectLifecycle
	// routes it to SetChildBudget (direct parentage, bounded by the caller's
	// own remaining grant) — the agent_set_budget tool's rule.
	"SetBudget": policyChildScoped,
	// Rename is operator/owner only: no child credential may rename.
	"Rename": policyUserOnly,
	// Notify is userOnly in the table, and a token whose only capability is
	// {notify} is admitted to it — and ONLY to it — by the ProvenanceNotify
	// branch at the top of authorizeControlProcedure, which runs before any
	// table policy. A notify token is therefore refused every other procedure,
	// the anyCaller reads included, whatever this table says.
	"Notify": policyUserOnly,
	// SetRouting from a child is subtree-scoped and refused only= — see
	// SetChildRouting. It edits the stored spec, never re-resolves policy.
	"SetRouting": policyChildScoped,
	// Conversation reads a child may make on its own subtree, the MCP face's
	// conversation_* tools' boundary (insights.ScopeSubtree). Review,
	// Findings and Stats stay userOnly: nothing a child runs needs them.
	"ConversationSearch": policyChildScoped,
	"ConversationExport": policyChildScoped,
	"ConversationQuery":  policyChildScoped,
	// The owner's pymodule corpus: a child reads what its executor can run
	// and authors into it, as the MCP face's pymodule tools let it.
	"ListPymodules":  policyChildScoped,
	"GetPymodule":    policyChildScoped,
	"PutPymodule":    policyChildScoped,
	"DeletePymodule": policyChildScoped,
	// Preset authoring: the gate admits the per-child secret and the handler
	// (connectPresets.authoringChild) refuses it unless the child is
	// top-level — the MCP face's presetStoreForChild rule.
	"PutPreset":    policyChildScoped,
	"DeletePreset": policyChildScoped,
	// Script children (Report / Receive / SetResult). Report acts OUTWARD on
	// the caller's own parent; Receive and SetResult are self-only. All three
	// resolve the caller's position from the credential — the request carries
	// no address to authorize against.
	"Report":    policyChildScoped,
	"Receive":   policyChildScoped,
	"SetResult": policyChildScoped,

	// owner-scoped recall and memory: a child reads and writes its OWNER's
	// rows, never admin — the fundi recall tools' surface.
	"Recall":        policyOwnerScoped,
	"RecallContext": policyOwnerScoped,
	"GetMemory":     policyOwnerScoped,
	"MemoryTree":    policyOwnerScoped,
	"PutMemory":     policyOwnerScoped,
	"DeleteMemory":  policyOwnerScoped,

	// anyCaller: the read-only, non-scoped verbs — see the classification
	// rule above for all eight.
	"ListModels":         policyAnyCaller,
	"ListPresets":        policyAnyCaller,
	"GetPreset":          policyAnyCaller,
	"GetRateLimitStatus": policyAnyCaller,
	// ModelInfo is a read-only catalog lookup, the per-model twin of
	// ListModels: same non-owned answer, same reasoning.
	"ModelInfo": policyAnyCaller,
	// ModelRoutes is a read-only routing explanation — endpoint prices,
	// quantization and eligibility for one model line. It names providers and
	// stats, never a user's data.
	"ModelRoutes": policyAnyCaller,

	// userOnly: everything that acts with operator authority or answers as
	// the caller's identity. Listed explicitly so the completeness test can
	// tell "classified userOnly" from "missing".
	"ListExecutors":        policyUserOnly,
	"ListSkills":           policyUserOnly,
	"GetSkill":             policyUserOnly,
	"DeleteSkill":          policyUserOnly,
	"SetSkillEnabled":      policyUserOnly,
	"AddRepo":              policyUserOnly,
	"ListRepos":            policyUserOnly,
	"RefreshRepo":          policyUserOnly,
	"SetRepo":              policyUserOnly,
	"RemoveRepo":           policyUserOnly,
	"RecallBackfill":       policyUserOnly,
	"RecallStatus":         policyUserOnly,
	"ConversationReview":   policyUserOnly,
	"ConversationFindings": policyUserOnly,
	"DarajaLaunch":         policyUserOnly,
	"DarajaSend":           policyUserOnly,
	"DarajaWatch":          policyUserOnly,
	// :batch's provider-ban verbs. List is a read-only, non-scoped surface
	// (the ban list names providers, not users); Ban/Unban are operator
	// writes, so userOnly.
	"ListProviderBans": policyAnyCaller,
	"BanProvider":      policyUserOnly,
	"UnbanProvider":    policyUserOnly,
	// The route-policy verbs. List is a read-only, non-scoped surface —
	// routing defaults name providers, not users — but Set/Delete reroute
	// every user's requests, so they are operator writes: userOnly.
	"ListRoutes":  policyAnyCaller,
	"SetRoute":    policyUserOnly,
	"DeleteRoute": policyUserOnly,

	// Model aliases are per-owner names, so listing is a read of the caller's
	// OWN rows — any caller, child included (a child reads its owner's). A
	// write needs a user credential: no child and no MCP tool may author one,
	// which the adapter refuses a second time (connectModelAliases).
	"ListModelAliases": policyAnyCaller,
	"PutModelAlias":    policyUserOnly,
	"DeleteModelAlias": policyUserOnly,

	// Sandboxes. CreateSandbox/RemoveSandbox are childScoped: a per-child
	// credential may provision and tear down its OWN containers, bounded
	// inside the Controller (a child's resources are clamped, its host paths
	// are bounded by the launcher's mount roots, and it may remove only a row
	// it or a descendant created). ListSandboxes is ownerScoped: it answers
	// from the caller's OWNER's rows — never an admin's whole fleet — the same
	// surface the recall tools give a child.
	// SyncPath/SyncRepo are childScoped: the handler bounds a child to
	// executors it can reach and sandboxes it or a descendant created.
	"CreateSandbox": policyChildScoped,
	"RemoveSandbox": policyChildScoped,
	"ListSandboxes": policyOwnerScoped,
	"SyncPath":      policyChildScoped,
	"SyncRepo":      policyChildScoped,

	// userOnly: the framed-protocol retirement verbs — operator verbs; no
	// child tool reaches them; CreateUser/ListUsers/RemoveUser add an admin
	// check in the handler.
	"Resume":            policyUserOnly,
	"CloseAllExited":    policyUserOnly,
	"SetLabels":         policyUserOnly,
	"Status":            policyUserOnly,
	"Search":            policyUserOnly,
	"ShutdownDaemon":    policyUserOnly,
	"ConversationStats": policyUserOnly,
	"EnrollExecutor":    policyUserOnly,
	"CreateExecutor":    policyUserOnly,
	"LabelExecutor":     policyUserOnly,
	"DisableExecutor":   policyUserOnly,
	"EnableExecutor":    policyUserOnly,
	"DeleteExecutor":    policyUserOnly,
	"ExecutorSession":   policyUserOnly,
	"CreateUser":        policyUserOnly,
	"ListUsers":         policyUserOnly,
	"RemoveUser":        policyUserOnly,
	// The token RPCs are userOnly at the gate but NOT admin-gated in the
	// handler: a user manages its own credentials (MintToken for self,
	// ListTokens of self, RevokeToken of own). Create/Update keep the admin
	// check in the handler (connect_users.go).
	"UpdateUser":  policyUserOnly,
	"MintToken":   policyUserOnly,
	"ListTokens":  policyUserOnly,
	"RevokeToken": policyUserOnly,
	// The provider-login RPCs are userOnly for the same reason as the token
	// RPCs, one notch stricter in effect: the credential they manage is a
	// provider OAuth grant stored against the CALLER's own user id, and the
	// handler refuses an identity with no user id at all (a unix-socket
	// caller has no user to store a grant for). No child credential reaches
	// them, so no fork of an owner's tokens can be planted by a child.
	"BeginProviderLogin":       policyUserOnly,
	"CompleteProviderLogin":    policyUserOnly,
	"DeleteProviderCredential": policyUserOnly,
	"GetStreams":               policyUserOnly,
	"SendFrame":                policyUserOnly,
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
// the tracing interceptor (tel) FIRST when present, then the caller's
// interceptors (admission, identity resolution), the policy gate after them,
// where it sees the identity the earlier ones resolved, and the
// stream-revocation interceptor innermost, behind the policy gate — so a call
// the gate refuses never registers a stream, and only an admitted one becomes
// cuttable.
//
// tel sits before the policy gate on purpose: a refused call is exactly the
// one an operator wants to see in a trace, and a tracer placed after the gate
// would record only the calls that were already admitted. It is safe there
// because it only observes — it reads no identity and cannot admit or refuse
// anything.
//
// Every mount of the Control service goes through here — proxy.go's proxy
// face, connect_uds.go's local socket, and the tests — so there is exactly one
// place that decides what guards the plane, and no second wiring that a future
// change can forget. reg is the daemon's one stream registry, built in main.go
// and shared by both mounts and the Controller; nil leaves the route served
// with no stream cut. tel may be nil, which is tracing off.
func connectControlRoute(srv *connectapi.Server, reg *streamRegistry, tel connect.Interceptor, before ...connect.Interceptor) (string, http.Handler) {
	inters := make([]connect.Interceptor, 0, len(before)+3)
	if tel != nil {
		inters = append(inters, tel)
	}
	inters = append(inters, before...)
	inters = append(inters, connectPolicyInterceptor(), streamRevocationInterceptor(reg))
	return srv.Routes(inters...)
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
// procedures refuse it, anyCaller procedures admit it, and a childScoped or
// ownerScoped procedure admits ONLY the per-child secret, because only that
// credential names a child with subtree (or owner) authority. The admitted
// child caller is still not an operator: each childScoped handler resolves
// the caller's subtree through the wired ChildScopeSource and refuses every
// target outside it.
//
// A ProvenanceNotify identity is neither a user nor a child credential: it is
// admitted to Control.Notify and refused everything else (see the first
// branch below), so the two admissions just described never apply to it.
func authorizeControlProcedure(ctx context.Context, procedure string) error {
	id := server.IdentityFromContext(ctx)
	// A notify-scoped token ({notify}) reaches exactly one procedure and is
	// checked FIRST: before the nil/user-credential early return and before
	// any policyAnyCaller admission, so the read-only verbs a child credential
	// is admitted to stay closed to it too. It is not a user credential
	// (IsUserCredential is false), but it is also not a child credential —
	// without this branch a retarget of the table could hand it an
	// anyCaller verb, which is why the confinement cannot live in the table.
	if id != nil && id.Via == server.ProvenanceNotify {
		if procedure == rafikiv1connect.ControlNotifyProcedure {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("notify-scoped token may only call Notify"))
	}
	if id == nil || id.IsUserCredential() {
		return nil
	}
	policy := policyFor(procedure)
	if policy == policyAnyCaller {
		return nil
	}
	if (policy == policyChildScoped || policy == policyOwnerScoped) && id.Via == server.ProvenanceChildToken {
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
