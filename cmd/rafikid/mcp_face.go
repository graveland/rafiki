// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/mcpserver"
	"go.graveland.dev/rafiki/pkg/quota"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpFacePath is the mount path, registered by server.Handler.Mount.
//
// "/mcp" alone, with no trailing-slash twin: StreamableHTTPHandler never routes
// on the request path — it answers POST/GET/DELETE on whatever path it is
// mounted at — so "/mcp/" would be a second registration to keep in step, not
// a redirect net/http would resolve. ServeMux pattern "/mcp" is an exact match.
const mcpFacePath = "/mcp"

// mcpFace serves the MCP agent-control surface.
//
// The Controller is set after construction because the proxy face is built
// first (see main.go) — the same reason proxyFace.Control is exposed for
// late wiring rather than passed to the constructor.
type mcpFace struct {
	logger  *slog.Logger
	ledger  *mcpLedger
	quota   *quota.Store
	version string

	mu   sync.RWMutex
	ctrl *Controller

	// fallbackTasks backs the task_* tools when the Controller carries no
	// store (no database). Built once, so tasks survive between requests —
	// scoped per user by the mcpLedger's synthetic key — and lost on
	// restart: the degradation BuildRuntime documents for a pool-less agent.
	fallbackTasks   tasks.Store
	fallbackTasksMu sync.Mutex

	// sessions binds each Mcp-Session-Id to the caller that initialized it.
	// The SDK's own hijack guard never fires here — see sessionOwnedBy — so
	// this map is the per-session identity check. Entries live as long as
	// their session (pruned on DELETE; cleared by a restart, which also
	// clears the SDK's own session table).
	sessionsMu sync.Mutex
	sessions   map[string]principal
}

// principal identifies the caller a session is bound to. UserID alone is not
// enough once children hold credentials: two children of one owner share a
// UserID, so a UserID-keyed map lets child B present child A's session id and
// execute against A's bound spawner. An empty ChildID is the interactive user.
type principal struct {
	UserID  string
	ChildID string
}

func newMCPFace(logger *slog.Logger, capture *capture.CaptureStore, quotaStore *quota.Store, ver string) *mcpFace {
	return &mcpFace{
		logger:   logger,
		ledger:   newMCPLedger(capture),
		quota:    quotaStore,
		version:  ver,
		sessions: make(map[string]principal),
	}
}

// SetController binds the daemon's controller. Called once from main.go.
func (f *mcpFace) SetController(c *Controller) {
	f.mu.Lock()
	f.ctrl = c
	f.mu.Unlock()
}

// Routes returns the mount path and handler for server.Handler.
//
// The SDK handler is wrapped, not handed over raw, because the SDK's
// session-hijack guard is inert on this mount (see sessionOwnedBy): without
// the wrap, any authenticated caller presenting another caller's
// Mcp-Session-Id would execute that caller's bound tool set.
func (f *mcpFace) Routes() (string, http.Handler) {
	sdk := mcp.NewStreamableHTTPHandler(f.getServer, nil)
	return mcpFacePath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A nil server is an answer, not an empty tool list: MCP clients can
		// read a status, and the two nil reasons deserve different ones.
		// ctrl == nil is genuinely transient — the proxy face is built before
		// the controller is wired (see SetController) and a client can retry
		// and recover, contrary to what the old toolless-fallback comment
		// claimed. Every other nil is a credential that authenticated but is
		// not entitled to agent control.
		if f.controller() == nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "mcp agent-control surface is starting; retry shortly", http.StatusServiceUnavailable)
			return
		}
		id := server.IdentityFromContext(r.Context())
		if !mcpEntitled(id) {
			http.Error(w, "credential is not entitled to rafiki agent control", http.StatusForbidden)
			return
		}
		p := principal{UserID: id.UserID, ChildID: id.ChildID}
		sid := r.Header.Get("Mcp-Session-Id")
		if sid != "" && !f.sessionOwnedBy(sid, p) {
			http.Error(w, "mcp session belongs to another caller or is unknown", http.StatusForbidden)
			return
		}
		sdk.ServeHTTP(w, r)
		if r.Method == http.MethodDelete {
			// The session is gone either way: closed (204) or never existed.
			// Only requests that passed the ownership check reach here.
			f.forgetSession(sid)
		} else if sid == "" {
			// A new session is named only in the initialize response
			// (streamable.go:1263-1265) and the id does not exist when
			// getServer runs, so the binding is recorded here, after the
			// handler has returned. Nobody can present the id before this
			// response reaches them.
			if newSID := w.Header().Get("Mcp-Session-Id"); newSID != "" {
				f.bindSession(newSID, p)
			}
		}
	})
}

// sessionOwnedBy reports whether the principal p (user id + child id) may
// use sid.
//
// SDK evidence for why the face must check this itself (vendored v1.6.1):
// the handler's hijack guard (mcp/streamable.go:311-315) runs only when
// sessInfo.userID is non-empty, and that field is captured solely from
// auth.TokenInfoFromContext at session creation (mcp/streamable.go:499-504).
// The only SDK code that ever stores a TokenInfo in the request context is
// its RequireBearerToken middleware (auth/auth.go:93) — a credential path
// rafiki does not run, because UserTokenAuth is the one credential path.
// sessInfo.userID is therefore always empty here and the guard is skipped,
// so without this map the session id alone would route bob onto alice's
// bound tools.
func (f *mcpFace) sessionOwnedBy(sid string, p principal) bool {
	f.sessionsMu.Lock()
	defer f.sessionsMu.Unlock()
	owner, ok := f.sessions[sid]
	// An unknown sid is refused, never dispatched unvalidated: the SDK would
	// answer "session not found", and a miss here must not be the one path
	// that bypasses the check.
	return ok && owner == p
}

func (f *mcpFace) bindSession(sid string, p principal) {
	f.sessionsMu.Lock()
	defer f.sessionsMu.Unlock()
	f.sessions[sid] = p
}

func (f *mcpFace) forgetSession(sid string) {
	f.sessionsMu.Lock()
	defer f.sessionsMu.Unlock()
	delete(f.sessions, sid)
}

// getServer builds an MCP server bound to ONE caller, per request.
//
// The binding is the point: tools.AgentSpawner takes no caller identity in any
// method, so the only way to serve two callers from one process is to
// construct a different tool set per request. A single server built at
// startup and shared would put a user id back into a method parameter, which
// is one refactor from being a tool argument the model can be
// prompt-injected into naming.
//
// The identity arrives on the context, not the headers: UserTokenAuth.Middleware
// has already authenticated the request and stored it there. Reading the
// Authorization header here would be a second credential path.
//
// The gate reads the credential's provenance, never the resolved UserID: a
// child-attributed identity carries the owner's UserID (that is exactly
// the attribution path /v1/messages bills turns through), so a
// non-empty-UserID check would hand the full tool set to whatever holds
// the per-boot child token plus its own child id. Two provenances get agent
// control: a real user credential binds the user spawner, and a per-child
// token binds a controllerSpawner scoped to that child's own position in the
// tree — its spawns are authorized against its subtree, nothing more.
// Every other identity returns nil, which Routes answers with a status (503
// for a not-yet-wired controller, 403 for an unentitled credential) rather
// than a toolless server: an MCP client can read a status, and the two
// reasons deserve different answers.
// mcpEntitled is the ONE spelling of "may this credential reach the
// agent-control surface": exactly ProvenanceUser (a real user token, via
// IsUserCredential) or ProvenanceChildToken (a per-child secret). Routes
// refuses everything else with 403 before dispatch, and getServer builds the
// spawner, so the two gates must admit the same set -- writing the set once
// is what keeps a widened IsUserCredential from silently falling through to
// the SDK's bare 400 on a request Routes already admitted (the divergence
// the final review flagged as fail-closed but unrecognizable).
func mcpEntitled(id *server.Identity) bool {
	return id != nil && (id.IsUserCredential() || id.Via == server.ProvenanceChildToken)
}

func (f *mcpFace) getServer(r *http.Request) *mcp.Server {
	id := server.IdentityFromContext(r.Context())
	ctrl := f.controller()
	if ctrl == nil {
		return nil // caller returns 503; see Routes
	}
	if !mcpEntitled(id) {
		return nil
	}
	owner := users.Identity{UserID: id.UserID, Username: id.Username}
	var spawner tools.AgentSpawner
	switch {
	case id.IsUserCredential():
		spawner = newUserSpawner(ctrl, owner)
	case id.Via == server.ProvenanceChildToken:
		spawner = newControllerSpawner(ctrl, id.ChildID)
	default:
		return nil
	}
	// A nil *quota.Store must yield a nil INTERFACE value so
	// QuotaStatusBlueprint's documented decline fires on a DB-less daemon;
	// a non-nil quotaReader wrapping a nil store would materialize a tool that
	// can only ever answer "no data captured yet".
	var quotaReader tools.QuotaReader
	if f.quota != nil {
		quotaReader = newMCPQuotaReader(f.quota, owner)
	}
	opts := tools.ToolOpts{
		Agents: spawner,
		Tasks:  f.taskStoreFor(ctrl),
		Quota:  quotaReader,
	}

	return mcpserver.New(mcpserver.Options{
		Tools:        mcpToolset(opts, f.logger),
		Descriptions: mcpToolDescriptions,
		ResolveConversationID: func(ctx context.Context) (string, error) {
			return f.ledger.ConversationID(ctx, owner)
		},
		ServerOptions:   settlementHooksFor(owner),
		RegisterSession: settlementRegistrationFor(owner),
		Version:         f.version,
	})
}

// mcpToolset materializes the surface's blueprints for one caller's opts.
// Materialized by hand — this surface does not use tools.Registry — and
// nil-checked rather than trusted to mcpserver.New's skip: a Materializer may
// decline with (nil, nil), and the decline is informational (no spawner, no
// quota source), not an error.
func mcpToolset(opts tools.ToolOpts, logger *slog.Logger) []tools.Tool {
	built := make([]tools.Tool, 0, len(mcpBlueprints))
	for _, bp := range mcpBlueprints {
		var (
			t   tools.Tool
			err error
		)
		if m, ok := bp.(tools.Materializer); ok {
			t, err = m.Materialize(opts)
		} else {
			t = bp
		}
		if err != nil {
			logger.Warn("mcp face: materialize failed", "tool", bp.Name(), "error", err)
			continue
		}
		if t == nil {
			continue
		}
		built = append(built, t)
	}
	return built
}

func (f *mcpFace) controller() *Controller {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ctrl
}

// taskStoreFor returns the Controller's ledger, falling back to a face-wide
// in-memory store when the Controller has none. ToolOpts.Tasks is never nil:
// the task_* tools do not nil-check it.
//
// Pairing assumption: the mcpLedger's DB-less fallback key "user:<id>" is only
// ever fed to a store that accepts it — the nil-ness of ctrl.tasks and of the
// ledger's capture store move together today. A future config with a nil
// capture store beside a Postgres task store would feed "user:<id>" into a
// UUID NOT NULL column (migrations/0001_baseline), so the two must stay
// paired.
func (f *mcpFace) taskStoreFor(ctrl *Controller) tasks.Store {
	if ctrl.tasks != nil {
		return ctrl.tasks
	}
	f.fallbackTasksMu.Lock()
	defer f.fallbackTasksMu.Unlock()
	if f.fallbackTasks == nil {
		f.fallbackTasks = tasks.NewMemoryStore()
	}
	return f.fallbackTasks
}

// mcpBlueprints is the agent-control surface's tool set: the spawner verbs,
// the shared task ledger, and the caller's own quota view. Each blueprint is a
// Tool that implements Materializer; the nil-check in getServer covers the
// decline.
var mcpBlueprints = []tools.Tool{
	&tools.AgentSpawnBlueprint{},
	&tools.AgentListBlueprint{},
	&tools.AgentViewBlueprint{},
	&tools.AgentSendBlueprint{},
	&tools.AgentKillBlueprint{},
	&tools.AgentSetBudgetBlueprint{},
	&tools.AgentModelsBlueprint{},
	&tools.TaskAddBlueprint{},
	&tools.TaskUpdateBlueprint{},
	&tools.TaskDropBlueprint{},
	&tools.TaskListBlueprint{},
	&tools.QuotaStatusBlueprint{},
}

// mcpNotificationNote replaces the settlement promise the fundi blueprint
// descriptions carry. The push this surface delivers is best-effort by
// construction — the SDK silently drops every notifications/message until the
// client has sent logging/setLevel — so an agent told to wait for a message
// could still wait forever; every override that would otherwise promise a
// notification carries this instead.
const mcpNotificationNote = "A settlement notification may be pushed to your client as a " +
	"best-effort log message, and many clients drop those unless they have set a logging " +
	"level (logging/setLevel), so never wait for one: when you need to know whether an agent " +
	"finished, check agent_list or task_list deliberately."

// mcpSpawnPrefix reframes agent_spawn for a client that has its own native
// subagent tool.
const mcpSpawnPrefix = "Creates a separate, cross-process, potentially cross-machine, " +
	"dollar-metered rafiki agent. It outlives this conversation, appears in `rafiki list`, " +
	"can run as fundi or claude, and is budget/depth/executor-constrained. For a lightweight " +
	"subagent scoped to just this conversation, use your own Task tool instead. Reach for " +
	"this one when the work should survive independently, run on different hardware, use a " +
	"different model, or be watched/steered from outside this session."

// mcpLedgerPrefix reframes the task_* tools for a client that has its own
// native per-session checklist.
const mcpLedgerPrefix = "This is a shared, durable, cross-agent ledger. Rows persist beyond " +
	"this session, a spawned agent can be assigned one, and a human can read them later. It " +
	"is not your private per-session checklist — for that, use your own TodoWrite tool."

// mcpSurfacePrefix marks the agent-steering verbs as operating on daemon-managed
// processes rather than the client's own subagents.
const mcpSurfacePrefix = "rafiki agents are independent daemon-managed processes, not " +
	"subagents inside your own session — a human or another client may have spawned some, " +
	"and this surface has no ownership filter. "

// mcpSpawnNotifyStart begins the fundi-only settlement promise inside the
// agent_spawn blueprint text: the composition cuts from here to
// mcpSpawnKeepDoing, dropping the promise ("You will be notified when it
// settles … nothing sooner than the notification will") so the shipped text
// notifies conditionally exactly once, via mcpNotificationNote. If the
// blueprint text drifts and the markers stop matching, the guard in
// TestMCPFaceDescriptionsCarryTheBlueprintText fails loudly rather than
// letting the promise ship silently.
const mcpSpawnNotifyStart = "You will be notified"

// mcpSpawnKeepDoing begins the sentence after the excised span.
const mcpSpawnKeepDoing = "Keep doing your own work"

// mcpToolDescriptions overrides a tool's model-facing description on this
// surface. The blueprint texts are written for a fundi child, which has a
// native task tool of its own and a live notification channel; an MCP client
// has neither, and agent_list's scope is the whole daemon here, not a subtree.
// Composed from the blueprints' own descriptions at init so the shared text
// cannot drift from the fundi surface.
var mcpToolDescriptions = func() map[string]string {
	spawn := &tools.AgentSpawnBlueprint{}
	taskAdd := &tools.TaskAddBlueprint{}
	taskUpdate := &tools.TaskUpdateBlueprint{}
	taskDrop := &tools.TaskDropBlueprint{}
	taskList := &tools.TaskListBlueprint{}
	send := &tools.AgentSendBlueprint{}
	kill := &tools.AgentKillBlueprint{}
	// Prefix and the remainder of the blueprint text stay verbatim; only the
	// two-sentence notification promise between the markers goes.
	spawnText := spawn.Description()
	if start := strings.Index(spawnText, mcpSpawnNotifyStart); start >= 0 {
		if end := strings.Index(spawnText[start:], mcpSpawnKeepDoing); end >= 0 {
			spawnText = spawnText[:start] + spawnText[start+end:]
		}
	}
	return map[string]string{
		"agent_spawn": mcpSpawnPrefix + "\n\n" + spawnText + "\n\n" + mcpNotificationNote,
		"agent_list": mcpSurfacePrefix + "Lists every agent the daemon knows, each with its " +
			"id, name, model, current status, working directory and assigned task handle. " +
			"Takes no arguments. Use it before agent_send or agent_kill to find the id you " +
			"mean, or to look something up. " + mcpNotificationNote,
		"agent_view": "Read the recent transcript of a rafiki agent: its prompts, what it " +
			"said, and the tools it called with their results. Use it to check on a worker " +
			"that seems stuck, or to understand a result before acting on it — deliberately, " +
			"not as a polling loop. " + mcpNotificationNote + " For \"what is it actually " +
			"working on\", prefer task_list with assignee set — that is one indexed read of " +
			"what the agent decided, where this is a wall of transcript you have to interpret.",
		"agent_send": mcpSurfacePrefix + send.Description(),
		"agent_kill": mcpSurfacePrefix + kill.Description(),
		// agent_set_budget, agent_models and quota_status have no native-client
		// equivalent to be confused with, so their blueprint texts stand as-is.
		"task_add":    mcpLedgerPrefix + "\n\n" + taskAdd.Description(),
		"task_update": mcpLedgerPrefix + "\n\n" + taskUpdate.Description(),
		"task_drop":   mcpLedgerPrefix + "\n\n" + taskDrop.Description(),
		"task_list":   mcpLedgerPrefix + "\n\n" + taskList.Description(),
	}
}()

// registerSettlementSession adds one session to the settlement registry under
// userID and, when the session was newly added, starts the per-session Wait
// goroutine that removes it when the connection closes. Add reports whether
// it inserted, so a session reachable from two registration points —
// InitializedHandler for legacy-handshake clients, the bridge's tool-call
// hook for SEP-2575 clients — spawns exactly one goroutine.
func registerSettlementSession(userID string, ss *mcp.ServerSession) {
	if ss == nil || userID == "" {
		return
	}
	if !mcpSettlements.Add(userID, ss) {
		return
	}
	go func() {
		_ = ss.Wait()
		mcpSettlements.Remove(userID, ss)
	}()
}

// settlementRegistrationFor builds the bridge's RegisterSession hook for one
// caller, the owner closed over exactly as settlementHooksFor closes over it.
// It exists because a go-sdk v1.7.0+ client completes its handshake through
// the SEP-2575 server/discover probe and never sends
// notifications/initialized, so the face's InitializedHandler never fires for
// it. A tool call is the earliest hook both handshakes share, and the only
// sessions that can produce a settlement — ones that spawn agents — call
// tools, so registration on first tool call strictly precedes any settlement
// this session could receive.
func settlementRegistrationFor(owner users.Identity) func(*mcp.ServerSession) {
	return func(ss *mcp.ServerSession) {
		registerSettlementSession(owner.UserID, ss)
	}
}

// settlementHooksFor builds the server-side SDK options that register and
// unregister one caller's MCP session with the settlement fan-out. The owner
// rides the closure, never a hook signature: the bridge carries these options
// through untouched, so no user id reaches pkg/mcpserver.
//
// Registration fires on notifications/initialized, which a legacy-handshake
// client sends as the last step of its initialize — the go-sdk did so
// unconditionally through v1.6.1, and non-SDK clients following the base
// spec still do. A go-sdk v1.7.0+ client negotiates through SEP-2575
// server/discover instead and never sends that notification, so this hook
// alone no longer covers every client: settlementRegistrationFor registers
// those on their first tool call. A client that does neither — no
// notifications/initialized, no tool call — is not registered, and the
// session is never offered a notification.
//
// v1.6.1 has no session-closed hook, so removal rides a per-session goroutine
// on Wait, which returns when the session's connection closes (client DELETE,
// handler timeout, teardown). The alternative — pruning on Notify's failure
// path — would leave a closed session registered until the next settlement
// happened to fire: unbounded in time, and growing with every reconnect. One
// goroutine for one session's lifetime is the scale MCP sessions run at.
//
// The face's own sessions map (Mcp-Session-Id → user id, the per-request
// identity check) and mcpSessions.byUID (user id → *ServerSession) are two
// keyed worlds that never meet: the SDK mints its session object with an id
// of its own and nothing links either map's key to the other. The cost of
// that is one-directional — a DELETE unbinds the sid immediately while this
// pointer lingers until Wait returns — and harmless: a Log to a closed
// session fails at debug and is skipped.
func settlementHooksFor(owner users.Identity) *mcp.ServerOptions {
	return &mcp.ServerOptions{
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			if req == nil || req.Session == nil {
				return
			}
			registerSettlementSession(owner.UserID, req.Session)
		},
	}
}
