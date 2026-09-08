// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/mcpserver"
	"go.graveland.dev/rafiki/pkg/quota"
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
}

func newMCPFace(logger *slog.Logger, capture *capture.CaptureStore, quotaStore *quota.Store, ver string) *mcpFace {
	return &mcpFace{
		logger:  logger,
		ledger:  newMCPLedger(capture),
		quota:   quotaStore,
		version: ver,
	}
}

// SetController binds the daemon's controller. Called once from main.go.
func (f *mcpFace) SetController(c *Controller) {
	f.mu.Lock()
	f.ctrl = c
	f.mu.Unlock()
}

// Routes returns the mount path and handler for server.Handler.
func (f *mcpFace) Routes() (string, http.Handler) {
	return mcpFacePath, mcp.NewStreamableHTTPHandler(f.getServer, nil)
}

// getServer builds an MCP server bound to ONE caller, per request.
//
// The binding is the point: tools.AgentSpawner takes no caller identity in any
// method, so the only way to serve two users from one process is to construct
// a different tool set per request. A single server built at startup and
// shared would put a user id back into a method parameter, which is one
// refactor from being a tool argument the model can be prompt-injected into
// naming.
//
// The identity arrives on the context, not the headers: UserTokenAuth.Middleware
// has already authenticated the request and stored it there. Reading the
// Authorization header here would be a second credential path.
func (f *mcpFace) getServer(r *http.Request) *mcp.Server {
	owner := spawnOwner(r.Context())

	// A toolless server, never nil: StreamableHTTPHandler answers a nil from
	// getServer with a bare 400 "no server available", which tells an MCP
	// client nothing it can recover from. The daemon is still starting
	// (no Controller yet) and a non-user credential (the per-boot child
	// token) reaches this face legitimately and must not get agent control —
	// both get an empty server and, once initialized, an empty tool list.
	ctrl := f.controller()
	if ctrl == nil || !owner.IsUser() {
		return mcpserver.New(mcpserver.Options{Version: f.version})
	}

	spawner := newUserSpawner(ctrl, owner)
	opts := tools.ToolOpts{
		Agents: spawner,
		Tasks:  f.taskStoreFor(ctrl),
		Quota:  mcpQuota{store: f.quota, owner: owner},
	}

	// Materialized by hand — this surface does not use tools.Registry — and
	// nil-checked here rather than trusted to mcpserver.New's skip: a
	// Materializer may decline with (nil, nil), and the decline is
	// informational (no spawner, no quota source), not an error.
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
			f.logger.Warn("mcp face: materialize failed", "tool", bp.Name(), "error", err)
			continue
		}
		if t == nil {
			continue
		}
		built = append(built, t)
	}

	return mcpserver.New(mcpserver.Options{
		Tools:        built,
		Descriptions: mcpToolDescriptions,
		ResolveConversationID: func(ctx context.Context) (string, error) {
			return f.ledger.ConversationID(ctx, owner)
		},
		Version: f.version,
	})
}

func (f *mcpFace) controller() *Controller {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ctrl
}

// taskStoreFor returns the Controller's ledger, falling back to a face-wide
// in-memory store when the Controller has none. ToolOpts.Tasks is never nil:
// the task_* tools do not nil-check it.
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
// descriptions carry. Nothing delivers one on this surface yet (Task 3.1,
// best-effort even then), so an agent told to wait for a message would wait
// forever; every override that would otherwise promise a notification carries
// this instead.
const mcpNotificationNote = "On this surface there is no settlement notification yet: " +
	"when you need to know whether an agent finished, check agent_list or task_list " +
	"deliberately rather than waiting for a message that will not arrive."

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
	return map[string]string{
		"agent_spawn": mcpSpawnPrefix + "\n\n" + spawn.Description() + "\n\n" + mcpNotificationNote,
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

// mcpQuota implements tools.QuotaReader for the MCP caller, bound to owner at
// construction — NOT resolved from ctx, so no tool argument can read another
// user's usage. This is the simpler half of the split controllerQuotaReader
// documents: an MCP call always has the real owner id on hand, where a resumed
// fundi child may not.
type mcpQuota struct {
	store *quota.Store
	owner users.Identity
}

func (q mcpQuota) RateLimitStatus(ctx context.Context) (tools.QuotaStatus, bool, error) {
	if q.owner.UserID == "" {
		return tools.QuotaStatus{}, false, nil
	}
	st, ok, err := q.store.Get(ctx, q.owner.UserID)
	if err != nil || !ok {
		return tools.QuotaStatus{}, ok, err
	}
	return tools.QuotaStatus{
		OrganizationID: st.OrganizationID,
		FiveH: tools.QuotaWindow{
			Utilization: st.FiveH.Utilization, ResetAt: st.FiveH.ResetAt, Status: st.FiveH.Status,
		},
		SevenD: tools.QuotaWindow{
			Utilization: st.SevenD.Utilization, ResetAt: st.SevenD.ResetAt, Status: st.SevenD.Status,
		},
		OverallStatus: st.OverallStatus,
		UpdatedAt:     st.UpdatedAt,
	}, true, nil
}
