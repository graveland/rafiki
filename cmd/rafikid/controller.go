package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/eventlogdb"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/promptfile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/ring"
	"go.graveland.dev/rafiki/pkg/routepolicy"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/tasksdb"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/version"
)

// Controller wires together the store, child lifecycle and persistence behind
// the Connect control plane. It is safe for concurrent use.
type Controller struct {
	st          *childstore.Store
	cm          *ChildManager
	dumper      *persist.LogDumper
	startedAt   time.Time
	socketPath  string
	logsDir     string
	stateDir    string
	graceWindow time.Duration
	sweeperWg   sync.WaitGroup

	// pool is the daemon's shared database pool, handed to every in-process
	// agent child (fundi.RuntimeOptions.Pool). Nil means every agent
	// conversation is in-memory. Owned and closed by main.go, not here.
	pool *pgxpool.Pool

	// captureStore, when non-nil, resolves which captured thread a supervisor
	// observation belongs to (HandleSubagentObservation). Nil on a DB-less
	// daemon, which captures no turns and so never attributes a subagent.
	captureStore *capture.CaptureStore

	// children is the durable child-state store. Nil when pool is nil, which
	// means children live in memory only and do not survive a restart.
	children childstore.ChildStore

	// lineage resolves an ancestor's FULL subtree for the reads that must cover
	// closed descendants too — subtree spend (checkBudget, sweepBudgets,
	// SetChildBudget) and a child credential's conversation scope. Unlike c.st,
	// which is live-state only and forgets a child the moment it is closed, this
	// source reads the database (tombstoned rows included). Wired once at
	// startup via SetLineageSource from the Postgres child store — see
	// wireLineageSource. Nil on a DB-less daemon or in a test, where
	// subtreeSelector falls back to the live set alone.
	lineage childstore.LineageSource

	// stopping is set once the daemon's own shutdown sequence begins
	// (ShutdownAllChildren). Child exits handled after that point skip the row
	// persist on purpose: the process is dying and its children die with it,
	// so the rows must keep their live statuses for the next daemon's
	// recovery — writing "exited" here would make every redeploy read as mass
	// terminal exit. A child that ends while the daemon is healthy (kill,
	// close, engine fatal) persists status=exited and stays dead across
	// restarts.
	stopping atomic.Bool

	// sessionIDEnded holds the ids of claude children already being ended for
	// reporting a different session id, so a second report during the same
	// launch ends nothing twice. activateLiveChild clears the entry when the
	// child is relaunched.
	sessionIDEnded sync.Map

	// leases gates who may WRITE to a conversation. Nil under the same
	// condition as children.
	leases *store.LeaseStore

	// daemonID is this daemon's stable identity: what a lease records as its
	// holder, and what says whose pid namespace the pid column belongs to.
	daemonID string

	// nsToken identifies this daemon's PID namespace so a recorded pid can
	// be proven to belong to the same namespace before it is signalled.
	nsToken string

	// pendingResumes holds recovered children whose auto-resume waits for an
	// executor to connect; the sweep fires on every executor connection. See
	// pending_resume.go.
	pendingResumes pendingResumes

	// recoveryWalk gates the pending sweep on the boot walk's completion: an
	// executor connecting DURING the walk must not sweep a half-filled set.
	recoveryWalk recoveryGate

	// heldLeases maps childID to the conversation lease this daemon holds for
	// it. Guarded by heldLeasesMu.
	heldLeasesMu sync.Mutex
	heldLeases   map[string]store.Lease

	// rawTrace, when non-nil, is the store raw LLM API request/response
	// capture writes to (the debug raw_http_request hypertable). Created at
	// daemon startup whenever a DB pool exists, independent of
	// RAFIKI_RECORD_REQUESTS — whether a given spawn actually records is
	// req.RecordRequests OR rawTraceAll, decided in agentRuntimeOptions.
	// Handed to agent children via fundi.RuntimeOptions.RawTrace.
	rawTrace *rawtrace.RawTraceStore

	// rawTraceAll mirrors MessagesProxy.rawTraceAll for native fundi children:
	// RAFIKI_RECORD_REQUESTS=1 must capture every spawn regardless of whether
	// it passed --record-requests, the same "global switch beats per-request
	// opt-in" rule the proxy face already applies.
	rawTraceAll bool

	// insights answers the conversation-insight RPCs. Always constructed —
	// agentcli/local.New is nil-pool-safe, so a nil pool just means every
	// read method below returns local.ErrNoPool instead of panicking.
	insights agentcli.Backend

	// proxyURL and proxyToken address the daemon's own proxy face, which pi
	// and claude children are pointed at so their turns are captured and
	// routed by the same code the agent kind uses in-process. Set once at
	// startup via SetProxy; empty means no face was started.
	proxyURL   string
	proxyToken string

	// mcpTokens maps a per-child MCP secret to the child that holds it. In
	// memory and per boot, matching the lifetime of the proxy bearer these
	// same children carry: a child that survives a daemon restart has a stale
	// proxy token and cannot reach /v1/messages regardless, so persisting
	// this one would widen its lifetime past anything that can use it.
	// mcpTokensByChild is the reverse index mintMCPToken consults, so resume
	// and RespawnChild — which rebuild the spawn environment through the same
	// proxyChildEnv/darajaClaudeParams path — reuse the secret the child
	// already holds instead of minting a second one it can never learn about.
	// Both guarded by mcpTokensMu. mcpSweepAt is the size at which the next
	// lazy sweep fires; zero means "not armed", which sweepMCPTokensIfDue
	// treats as mcpTokenSweepThreshold.
	mcpTokensMu      sync.RWMutex
	mcpTokens        map[string]string // secret -> childID
	mcpTokensByChild map[string]string // childID -> secret
	mcpSweepAt       int

	// catalog answers ChildSummary's ContextWindow/
	// MaxCompletionTokens fields). Set once at startup via SetCatalog, from
	// the SAME *routing.ModelCatalog instance the proxy face's llm.Client
	// uses (main.go builds one and hands it to both) — a nil catalog here
	// just means ContextWindow always returns ok=false, matching "the proxy
	// face failed to start" or any other reason main.go has none to give.
	catalog *routing.ModelCatalog

	// batcher is the daemon's ONE parked-call batcher (see newBatcher), set
	// once at startup via SetBatcher next to SetCatalog. nil means the daemon
	// runs without batch transport: every :batch first call fails with
	// pkg/llm's "no batcher configured" error, and no child parks. The field
	// is the CONCRETE *batch.Batcher (not the llm.Batcher interface) exactly
	// so SetBatcher can refuse a typed nil before it can ever reach an
	// interface field — see that setter's doc comment.
	batcher *batch.Batcher

	// providerGuard is the daemon's ONE provider cache guard (operator bans
	// plus cache ejections), set once at startup via SetProviderGuard — the
	// same instance the proxy face routes through. agentRuntimeOptions hands
	// it to every in-process child; nil means their requests carry no
	// provider.ignore at all.
	providerGuard *routing.ProviderGuard

	// routePolicy is the daemon's in-memory routing-policy view (routepolicy.
	// Policy), consulted by resolveRouting at spawn — it fills the gaps the
	// spawn/preset specs leave — and by RoutingFor, the proxy face's per-
	// session resolver. Set once at startup via SetRoutePolicy; nil means no
	// policy is loaded (a database-less daemon, or a failed startup load,
	// which is logged and absorbed): resolution degrades to spawn+preset
	// only, never a refusal to start.
	routePolicy *routepolicy.Policy

	// reviewQ is the conversation-review worker's bounded queue. Non-nil
	// only when pool is non-nil (main.go constructs and starts it there): a
	// DSN-less daemon gets no reviewer, and ConversationReview answers
	// FailedPrecondition rather than silently accepting jobs nothing will
	// ever run.
	reviewQ *reviewQueue

	// reviewInsights answers the review verbs' scoped reads (FilterByScope,
	// Findings, RecentAnalyses). An interface rather than *insights.Insights
	// so the accept-path tests run without a database; production sets it to
	// insights.New(pool) alongside coster. The methods live on
	// *insights.Insights (pkg/insights owns scope-to-SQL translation), NOT
	// on the agentcli.Backend interface c.insights carries — that seam has no
	// review methods, so this field is the one place the Controller reaches
	// them. Nil exactly when pool is nil, same condition as reviewQ.
	reviewInsights reviewReads

	// baseCtx is the daemon's own context, threaded into inproc.Options.Parent
	// so cancelling it stops every in-process agent child at once. Distinct
	// from the per-request ctx passed to Spawn/Resume/RespawnChild, which only
	// bounds the spawn call itself.
	baseCtx context.Context

	// spawnClaims serializes Resume and RespawnChild per childID. Both methods
	// share the same check-then-act shape (read exited status, fork a real OS
	// process, then replace the store record) and both operate on a childID
	// that is reused across the exited->live transition rather than minted
	// fresh — see the doc comment on childClaimSet for why a shared claim set
	// covers both.
	spawnClaims childClaimSet

	// tasks is the task ledger, nil when there is no database pool (daemon
	// with no database has no ledger to sweep). Populated in NewController.
	tasks tasks.Store

	// evbuf coalesces externally-injected agent events (subagent settles,
	// budget warnings, executor loss) into debounced frames so N events
	// cost one model turn instead of N. Nil means the buffer is disabled.
	evbuf *eventbuf.Buffer

	// jobs watches the background jobs children start through their bound
	// executor and pushes an exit fragment into the starting child's buffer,
	// the same injection path subagent settles ride. Nil only in tests that
	// build a Controller by hand and never bind an executor.
	jobs *jobWatcher

	// bound retains each child's boundExecutor for the job watcher: the
	// executor client handed to the fundi runtime at spawn is otherwise held
	// nowhere the daemon can reach, and a watch needs it to poll JobOutput.
	boundMu sync.Mutex
	bound   map[string]*boundExecutor

	// scriptOutputs holds the per-child ScriptOutput coalescer of every live
	// script child, keyed by child id. An entry is created lazily on the
	// child's FIRST line of output (scriptOutputHook) and taken and closed by
	// handleChildExit — before the exit event publishes, so the child's last
	// output precedes child_exited in ordinal order. scriptOutputState is the
	// per-spawn hook state registered at wiring time; taking the coalescer
	// marks its state exited in the same critical section, so a line arriving
	// after the exit (the abandon path) is dropped rather than registering a
	// coalescer nobody would ever close.
	scriptOutputsMu   sync.Mutex
	scriptOutputs     map[string]*scriptOutputCoalescer
	scriptOutputState map[string]*scriptOutputHookState

	// native fans rafiki-native events out per child, for the Connect
	// control plane's StreamEvents.
	native *nativebus.Registry

	// evlog is the durable event log.
	evlog eventlog.Store

	// inbox is the durable queue every turn-bound message rides: a prompt, a
	// steer or an abort is persisted before it is written to a child, so a
	// daemon that dies between accepting and delivering replays it on restart
	// instead of stranding whoever was waiting for it. Nil only in tests that
	// build a Controller by hand and never send.
	inbox *inbox.Queue

	// inboxBatch caps one delivered batch. Held here as well as inside the
	// queue because the orphan path (eventbuf fragments whose durable write
	// failed) coalesces with the same rules and must not drift from them.
	inboxBatch inbox.BatchConfig

	// sentFrames maps a written frame id to the rows it accounts for, until
	// the child confirms it took them into a turn. In memory on purpose: see
	// sentFrame's doc comment.
	sentMu     sync.Mutex
	sentFrames map[string]sentFrame

	// coster resolves what an agent subtree has spent. An interface rather
	// than *insights.Insights so the admission logic is testable without a
	// database — the number's correctness is insights' problem, what the
	// controller does with it is this package's.
	coster subtreeCoster

	// breaches bounds the budget sweep to one steer per breach.
	breaches budgetBreaches

	// rateWatch is the claude rate-limit auto-resume watch: per-child 429
	// marks, reset times and at most one pending resume timer. In-memory on
	// purpose (a restart loses the pending resume, the same stall there was
	// before the feature). See ratelimit_resume.go.
	rateWatch rateLimitWatch

	// heartbeats tracks long-running children's working spells for
	// sweepHeartbeats. See heartbeat_sweep.go.
	heartbeats heartbeatState
	// heartbeatInterval is how long a child must be continuously working
	// before its parent gets a check-in push. Zero disables the feature.
	heartbeatInterval time.Duration

	// turnOutcomes remembers the most recent fundi.TurnOutcome per child, so
	// handleStatusChange's idle-transition settle notification can name the
	// real reason (a cost budget, an upstream error) instead of a generic
	// "it's idle now". See turn_outcomes.go.
	turnOutcomes turnOutcomeStore

	// selfKilled marks children a coordinator killed itself via agent_kill,
	// so handleChildExit can suppress the redundant "exited" notification.
	// See self_kill.go.
	selfKilled selfKillStore

	// nudgedOnce bounds prompting.md's enforcement ladder to one nudge per
	// child. Guarded by nudgedMu.
	nudgedMu   sync.Mutex
	nudgedOnce map[string]bool

	// execPool is the live executor connection registry. Nil when the
	// executor listener is not configured.
	execPool executorPool

	// streamRevoke is the daemon's one stream-revocation registry, shared
	// with both Connect mounts (see connectControlRoute). Revocation through
	// the daemon's own paths — connectUserAdmin.RevokeToken, UserRm below —
	// cuts the open streams it names; revocation written to the database
	// behind a running daemon's back cannot reach it and is not attempted.
	// Nil in hand-built test Controllers, where every method is inert.
	streamRevoke *streamRegistry

	// execPoolConn is the SAME pool as execPool, at its concrete type.
	// relayTransport needs execpool.NewProxyTransport, which only the
	// concrete *execpool.Pool can build (it reaches a private method,
	// connectClientFor, that the narrow executorPool interface does not
	// expose); everything else in this file deliberately goes through the
	// interface so selection stays testable without a listener. Nil under
	// exactly the same condition as execPool.
	execPoolConn *execpool.Pool

	// skillPusher and pymodulePusher deliver the daemon's corpora to
	// executors: skills whole-corpus, pymodules owner-scoped. Both are
	// constructed at startup (main.go) before skill-manager registration —
	// connectSkills holds a push callback into the skill pusher — and before
	// the executor pool's on-connect wiring, which one callback serves for
	// both. Nil when the daemon lacks the corresponding store or an executor
	// pool: nothing to read, or nothing to push to.
	skillPusher    *skillPusher
	pymodulePusher *pymodulePusher

	// gitpymodulePusher fans a registered git source's refresh out to the
	// owner's eligible executors and caches each latest reported inventory.
	// Nil under the same condition as pymodulePusher.
	gitpymodulePusher *gitPymodulePusher

	// pymoduleStore is the owner-scoped pymodule backend, read by the
	// pymoduleWriter bound into each fundi child (agent_pymodules.go) and by
	// pymodulePusher. Nil when the daemon has no database: pymodule_put
	// declines to materialize and the dynamic python-modules skill is not
	// advertised. Deliberately NOT conditioned on the executor pool -- a
	// DB-but-no-executors daemon still lets a child save and list modules.
	pymoduleStore pymodules.Store

	// presetStore is the owner-scoped preset backend; nil on a DB-less
	// daemon, which refuses any spawn naming a preset.
	presetStore presets.Store

	// recall is the wired recall subsystem (store, optional embedder, the
	// background indexer and summarizer). Nil on a DB-less daemon: the
	// recall/recall_context/memory_* tools decline and conversation close
	// stamping has no indexer to nudge.
	recall *recallRuntime

	// recallWg tracks the recall indexer goroutine; waited on by Stop, the
	// same contract as sweeperWg.
	recallWg sync.WaitGroup

	// gitpymoduleStore is the owner-scoped git-source backend (the `repo`
	// scopes the pymodule tool surface addresses). Nil when the daemon has no
	// database: `rafiki python repo` answers that the backend is not wired,
	// the same DB-less posture the blob store has.
	gitpymoduleStore gitpymodules.Store

	// execStore is the durable executor registry. Nil when the executor
	// listener is not configured (require the pool to mint tokens).
	execStore executors.Store

	// sandboxStore is the durable sandbox registry (conversations.sandbox).
	// Nil on a database-less daemon: every sandbox verb then returns
	// ErrInternal "sandboxes require a database" — there is no DB-less path.
	sandboxStore sandbox.Store
	// sandboxCfg is the daemon's sandbox configuration, resolved once from the
	// environment at startup (main.go). Zero on a database-less daemon, where
	// no sandbox verb runs.
	sandboxCfg sandbox.Config
	// sandboxEngine builds the Docker Engine client for one launcher executor.
	// In production it is sandbox.NewEngine over execpool.NewProxyTransport for
	// the executor's own docker proxy; tests install a seam over an httptest
	// transport. Nil only when the feature is unwired (a hand-built test
	// Controller), where the sandbox verbs fail closed.
	sandboxEngine func(launcherID string) *sandbox.Engine
	// sandboxBootTime is when this daemon process started, captured once at
	// startup (main.go). The sandbox sweeper's FIRST pass abandons a `creating`
	// row only when the row was created BEFORE this instant: a row created after
	// it is an in-flight create THIS process is running (the first pass fires a
	// few seconds after the socket is already serving clients), so the boot pass
	// leaves it to the age gate. Injectable so a test can pin it; the zero value
	// disables the boot abandonment (only the age gate then applies).
	sandboxBootTime time.Time

	// pathSync is the path-sync backend, set once at boot by main (wirePathSync);
	// nil means path sync is unavailable on this daemon and the SyncPath/SyncRepo
	// RPCs answer Unavailable. It is a plain accessor target: the authorization
	// lives entirely in pathSyncer.resolve, never here.
	pathSync *pathSyncer

	// skillStore is the database-backed skills tier. nil when the daemon has
	// no store, in which case children get only their on-disk tiers.
	skillStore skills.Store

	// users is the identity store backing the user RPCs. Nil when RAFIKI_DB is
	// unset — every user verb then returns errNoUserStore rather than
	// pretending an empty user table.
	users users.Store

	// providers is the loaded provider registry, shared across every child
	// spawn. Nil means providers.Default() — which is what a daemon without a
	// providers.toml file (the historical case) uses.
	providers *providers.Set

	// wsLabels holds workspace IDs and executor IDs provisioned for children
	// whose spawn is in flight. Keyed by childID; set by agentRunner before
	// Spawn builds the store record, consumed by Spawn for label insertion
	// and by handleChildExit for release.
	wsLabels   map[string]workspaceLabels
	wsLabelsMu sync.Mutex

	// sessionExecMu guards sessionExecs, keyed by an opaque session key:
	// Connect's ExecutorSession stream uses its own per-call context, which is
	// unique already and needs no separate lookup table.
	sessionExecMu sync.Mutex
	sessionExecs  map[any]sessionExecutor

	// sessionExecWg tracks each session's ctx.Done() watcher goroutine so it
	// is provably gone, not just unreferenced, after its context ends.
	sessionExecWg sync.WaitGroup

	// darajaReg holds the in-memory credential registry, recorded by WireDaraja
	// so Close and Kill can revoke credentials before the row vanishes.
	darajaReg *darajapool.Registry
	// darajaPool and darajaDialAddr let claudeRunner (agent_runtime.go) launch
	// a daraja and drive it the same way the DarajaLaunch RPC handler does,
	// from inside Controller.Spawn/Resume rather than over a Connect call.
	darajaPool     *darajapool.Pool
	darajaDialAddr string
	// darajaLost holds the children whose daraja never reconnected within the
	// Runner's grace window (Pool.OnLost). handleChildExit takes the entry and
	// relaunches the child, so a host that vanished mid-session heals once its
	// executor is back instead of leaving the child exited.
	darajaLost sync.Map

	// lastRecentSource records which branch GetRecent took ("db", "live",
	// "exited"). Test seam: the branches are otherwise indistinguishable when
	// every source is empty, which is exactly the failure this guards against.
	// Atomic because GetRecent runs concurrently per control connection; reset
	// at entry so a not-found call cannot report the previous call's branch.
	lastRecentSource atomic.Value
}

type workspaceLabels struct {
	workspaceID   string
	executorID    string
	mode          string // "ephemeral" or "pinned"
	executorState string // "unbound" until the first successful NoteBinding
	// pgid is set only by claudeRunner's stashDarajaBinding — the daraja
	// process group's pid from the Launch result, recorded as
	// rafiki/daraja-pgid. Zero for every fundi tool-binding use of this
	// struct.
	pgid int32
}

// NewController constructs a Controller. Call loadOrphans() after construction
// to pre-populate the store from persisted state.
//
// dumper may be nil; when nil, no log dumps are written on child exit.
// The grace window defaults to 7 days but can be overridden with the
// RAFIKI_GRACE_HOURS environment variable.
//
// pool is the shared database pool for in-process agent children (nil means
// in-memory conversations); baseCtx is the daemon's own context, threaded into
// every in-process child so cancelling it stops them all at once. Both are
// owned by main.go — this constructor only stores them.
// SetProxy records the address of the daemon's own proxy face, which pi and
// claude children are pointed at. Called once at startup, before the socket
// accepts anything, so no child can observe it unset.
func (c *Controller) SetProxy(url, token string) {
	c.proxyURL, c.proxyToken = url, token
}

// SetCatalog records the daemon's shared model catalog, consulted by
// ContextWindow and by the insights backend (pricing). Called once at startup,
// before the socket accepts anything — mirrors SetProxy. A nil cat is legal
// (main.go passes one through regardless of whether the proxy face itself
// started) and just means every ContextWindow call returns ok=false and every
// cost reads as unpriced.
func (c *Controller) SetCatalog(cat *routing.ModelCatalog) {
	c.catalog = cat
	if cat != nil {
		// Re-create the insights backend with a pricer wrapped from the catalog.
		// The initial backend in NewController is pricer-less because the catalog
		// is not yet available; by the time SetCatalog returns the socket is not
		// yet listening, so no concurrent read can observe the intermediate state.
		c.insights = local.New(local.Options{Pool: c.pool, Pricer: cat.Pricing})
		// The COSTER gets the same pricer, for the same reason. NewController
		// built it as a bare insights.New(pool), and CostsByConversation
		// short-circuits to NO rows while unpriced (pkg/insights/subtree.go) —
		// so every child seeded a non-nil zero, the cockpit adopted it, and an
		// attached session showed no cost for anything predating the client's
		// own turn_end stream, even though every turn was captured. Priced here
		// rather than in NewController because the catalog does not exist yet.
		if c.pool != nil {
			c.coster = insights.New(c.pool).WithPricer(cat.Pricing)
		}
	}
}

// SetRoutePolicy records the daemon's routing-policy view, consulted by
// resolveRouting at spawn and by RoutingFor on the proxy face. Called once at
// startup, before the socket accepts anything — mirrors SetCatalog. A nil
// policy is refused and leaves the field nil (the same rule connectapi's
// Set*Manager setters enforce): an absent policy must stay "absent", degrading
// resolution to spawn+preset only, never a nil-panicking *routepolicy.Policy.
func (c *Controller) SetRoutePolicy(p *routepolicy.Policy) {
	if p == nil {
		return
	}
	c.routePolicy = p
}

// SetBatcher records the daemon's parked-call batcher, consulted by
// agentRuntimeOptions (ro.Batcher) so every in-process child's llm.Client can
// park :batch first calls. Called once at startup, before the socket accepts
// anything — mirrors SetCatalog. Passing nil (the no-batcher case) leaves the
// controller with none, which is fine; passing a nil *batch.Batcher is REFUSED
// and leaves the field nil, never stored: a typed-nil pointer assigned to the
// llm.Batcher interface field downstream would be a NON-nil interface value,
// so pkg/llm would believe a batcher exists and nil-panic on the first :batch
// send. Same rule as connectapi's Set*Manager setters — the interface widening
// must happen only over a value that is actually usable.
func (c *Controller) SetBatcher(b *batch.Batcher) {
	if b == nil {
		return
	}
	c.batcher = b
}

// SetProviderGuard records the daemon's provider cache guard, consulted by
// agentRuntimeOptions (ro.ProviderGuard). Called once at startup, before the
// socket accepts anything — mirrors SetCatalog.
func (c *Controller) SetProviderGuard(g *routing.ProviderGuard) {
	c.providerGuard = g
}

// SetLineageSource records the daemon's lineage source: the child store's view
// of an ancestor's full subtree, closed descendants included. Consulted by
// subtreeSelector (limits.go) so subtree spend and a child credential's
// conversation scope keep covering a child that has been closed. Wired once at
// startup, before the socket accepts anything.
//
// A nil interface is REFUSED and leaves the field nil — the same discipline
// SetRoutePolicy, SetBatcher and connectapi's Set*Manager setters apply. An
// absent source must stay absent (subtreeSelector then falls back to the live
// in-memory set), never be stored as a non-nil interface over a nil pointer
// that nil-panics on the first Lineage call.
func (c *Controller) SetLineageSource(src childstore.LineageSource) {
	if src == nil {
		return
	}
	c.lineage = src
}

// SetPathSyncer installs the path-sync backend, wired once at boot by main
// through wirePathSync. A nil pointer is REFUSED: the field is left untouched
// (so an already-installed syncer survives a stray nil) and a warning is
// logged — the same discipline SetLineageSource and connectapi's Set*Manager
// setters apply. Storing nil is what would make an unwired daemon claim a
// non-nil syncer and nil-panic on the first SyncPath call.
func (c *Controller) SetPathSyncer(p *pathSyncer) {
	if p == nil {
		slog.Warn("SetPathSyncer refused a nil syncer; path sync stays unavailable")
		return
	}
	c.pathSync = p
}

// syncer returns the path-sync backend, nil until SetPathSyncer installs one.
// It is a plain accessor for the per-child tool adapters: it grants no
// authority, because every endpoint a caller names is still resolved (and
// authorized) through pathSyncer.resolve.
func (c *Controller) syncer() *pathSyncer {
	return c.pathSync
}

// wireLineageSource gives ctrl the child store's lineage view when the store
// can provide one. It is the production wiring path, extracted from
// NewController so TestControllerSatisfiesConnectSeams can pin it with a stub:
// the Controller's children field is typed as the narrower ChildStore, which
// does NOT include Lineage, so a store that lost its Lineage method would
// still satisfy that field and silently leave subtree spend and child scope
// blind to closed children. The assertion is explicit at the wiring site for
// exactly that reason.
func wireLineageSource(ctrl *Controller, store childstore.ChildStore) {
	if src, ok := store.(childstore.LineageSource); ok {
		ctrl.SetLineageSource(src)
	}
}

func NewController(st *childstore.Store, stateDir, logsDir, socketPath string, dumper *persist.LogDumper, pool *pgxpool.Pool, rawTrace *rawtrace.RawTraceStore, rawTraceAll bool, baseCtx context.Context, execStore executors.Store, userStore users.Store, skillStore skills.Store, prov *providers.Set) *Controller {
	gw := 7 * 24 * time.Hour
	if h := paths.Get(paths.GraceHours); h != "" {
		if n, err := strconv.ParseFloat(h, 64); err == nil && n > 0 {
			gw = time.Duration(n * float64(time.Hour))
		}
	}
	// Strictly coarser than sweepTickInterval, which quantizes delivery — see
	// that constant. At a 1m tick the lazy seed in heartbeatState.due costs one
	// tick rather than one full interval, so the first check-in of a spell now
	// lands around 5m instead of around 10m.
	hb := defaultHeartbeatInterval
	if h := paths.Get(paths.HeartbeatInterval); h != "" {
		if d, err := time.ParseDuration(h); err == nil && d >= 0 {
			hb = d
		}
	}
	c := &Controller{
		st:                st,
		cm:                newChildManager(),
		dumper:            dumper,
		startedAt:         time.Now(),
		socketPath:        socketPath,
		logsDir:           logsDir,
		stateDir:          stateDir,
		graceWindow:       gw,
		heartbeatInterval: hb,
		pool:              pool,
		rawTrace:          rawTrace,
		rawTraceAll:       rawTraceAll,
		insights:          local.New(local.Options{Pool: pool}),
		baseCtx:           baseCtx,
		tasks:             taskStore(pool),
		evbuf:             newEventBuffer(),
		execStore:         execStore,
		users:             userStore,
		skillStore:        skillStore,
		providers:         prov,
		heldLeases:        make(map[string]store.Lease),
		native:            nativebus.New(),
		evlog:             eventLogStore(pool),
		inboxBatch:        inboxBatchConfig(),
		sentFrames:        make(map[string]sentFrame),
		rateWatch: rateLimitWatch{
			m:          make(map[string]*rateLimitState),
			buffer:     rateLimitResumeBuffer,
			minBackoff: rateLimitMinBackoff,
			maxBackoff: rateLimitMaxBackoff,
		},
	}
	// After the literal: the queue's Validate and Deliver are methods on c.
	c.inbox = c.newInboxQueue(inboxStore(pool))
	// Pool-gated like taskStore: a nil pool must leave captureStore nil, not a
	// store over a nil pool, because HandleSubagentObservation's nil check is
	// the only thing between a supervisor hook and a panic.
	if pool != nil {
		c.captureStore = capture.NewCaptureStore(pool)
	}
	c.bound = make(map[string]*boundExecutor)
	c.jobs = c.newControllerJobWatcher()

	if id, source, err := paths.DaemonID(); err != nil {
		// Not fatal: a daemon with no writable data dir and no env var can
		// still run, it just cannot hold a lease, so it will not auto-resume
		// anything. Failing to start would be worse.
		slog.Warn("no daemon id; conversation leases disabled", "error", err)
	} else {
		c.daemonID = id
		slog.Info("daemon identity", "daemonId", id, "source", source)
	}

	if tok, ok := paths.PIDNamespaceToken(); ok {
		c.nsToken = tok
	} else {
		slog.Info("pid namespace token unavailable; orphan pids will not be signalled")
	}

	// Wire the coster only when there is a database: without one every
	// budgeted spawn fails closed (which is what checkBudget does when
	// coster is nil), while unbudgeted ones are unaffected. PRICER-LESS here
	// on purpose — the catalog does not exist yet; SetCatalog rebuilds it
	// with the catalog's Pricing so ListChildren can fill CostUSD.
	if pool != nil {
		c.coster = insights.New(pool)
		childStore := childstoredb.New(pool)
		c.children = childStore
		// The same store also resolves an ancestor's full lineage (closed
		// descendants included); wire it so subtree spend and child conversation
		// scope keep covering closed children.
		wireLineageSource(c, childStore)
		c.leases = store.NewLeases(pool)
		// The review verbs' read surface. Pricer-less like coster's initial
		// value: RecentAnalyses reads cost off the stored analysis row, it
		// does not re-price anything, so the catalog's pricer is irrelevant
		// here and this needs no SetCatalog rebuild.
		c.reviewInsights = insights.New(pool)
	}
	return c
}

// taskStore returns a task ledger or nil when there is no database.
func taskStore(pool *pgxpool.Pool) tasks.Store {
	if pool == nil {
		return nil
	}
	return tasksdb.NewPostgresStore(pool)
}

// eventLogStore returns a durable event log store.
func eventLogStore(pool *pgxpool.Pool) eventlog.Store {
	if pool == nil {
		return eventlog.NewMemory()
	}
	return eventlogdb.New(pool)
}

// sweepTickInterval is how often sweepTick runs. It bounds the resolution of
// everything riding the tick, so it must stay strictly finer than
// heartbeatInterval: delivery is quantized to it, and a tick EQUAL to the
// interval is the worst case of all — due() fires on `elapsed >= interval`, so
// with the two equal the cadence flips between one and two ticks on drift of
// microseconds. It was 5m alongside a 5m heartbeat default, which is exactly
// that.
//
// Not finer than this without more thought: the tick itself is free, but the
// work is not — sweepBudgets and sweepHeartbeats each run a subtreeSpend query
// per working child, so the DB load scales with 1/interval. The real fix is a
// deadline-scheduler that wakes only when something is actually due; this
// constant is the interim.
const sweepTickInterval = time.Minute

// startSweeper launches a background goroutine that periodically forgets
// exited children whose age exceeds the configured grace window. It stops
// when ctx is cancelled. Call Stop() to wait for the goroutine to exit.
func (c *Controller) startSweeper(ctx context.Context) {
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
		ticker := time.NewTicker(sweepTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.sweepTick(ctx)
			}
		}
	}()
}

// sweepTick is one pass of the periodic reconciliation: children whose grace
// window has expired, budget breaches, long-running heartbeat check-ins, and
// the inbox's terminal rows.
//
// Budget breaches, heartbeats, and the inbox sweep ride the expiry tick
// rather than timers of their own: all four are periodic reconciliations of
// stored state, and a separate ticker for any one of them would be a
// separate thing to reason about at shutdown. Extracted from startSweeper so
// what the tick does is testable without waiting five minutes for one.
//
// sweepHeartbeats shares sweepBudgets' bounded sweepCtx (not
// context.Background()): both do a subtreeSpend query, and a stalled DB call
// in either must not hang the shared sweep goroutine indefinitely.
func (c *Controller) sweepTick(ctx context.Context) {
	c.sweepExpired()
	sweepCtx, cancel := context.WithTimeout(ctx, budgetSweepTimeout)
	c.sweepBudgets(sweepCtx)
	c.sweepHeartbeats(sweepCtx, time.Now())
	cancel()
	c.sweepInbox()
}

// Stop waits for background goroutines (the sweeper and the recall indexer)
// to exit. The caller is responsible for cancelling the context passed to
// startSweeper AND to startRecall before calling Stop.
func (c *Controller) Stop() {
	c.sweeperWg.Wait()
	c.recallWg.Wait()
}

// sweepExpired closes all exited children whose ExitedAt is older than
// graceWindow. Called periodically by the sweeper goroutine.
func (c *Controller) sweepExpired() {
	cutoff := time.Now().Add(-c.graceWindow)
	var toClose []string
	for _, s := range c.st.FindByStatus(protocol.StatusExited) {
		if !s.ExitedAt.IsZero() && s.ExitedAt.Before(cutoff) {
			toClose = append(toClose, s.ChildID)
		}
	}
	for _, id := range toClose {
		_ = c.Close(id)
	}
	if len(toClose) > 0 {
		slog.Info("sweep: closed expired children", "count", len(toClose))
	}
}

// ─── Control-plane queries and lifecycle ─────────────────────────────────────

// publishEvent is the ONE path by which a native event reaches anybody.
//
// Durable events are appended to the log first and the assigned ordinal is
// stamped onto the copy that goes to subscribers, because a subscriber builds
// its cursor from what it received: an event delivered without its ordinal is
// unresumable, which defeats the entire tier.
//
// Every c.native.Publish call site goes through here. A rule enforced at N
// call sites is a rule the N+1th will break.
func (c *Controller) publishEvent(childID string, ev *rafikiv1.Event) {
	if eventlog.TierOf(ev) == eventlog.TierDurable && c.evlog != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ord, err := c.evlog.Append(ctx, childID, ev)
		cancel()
		if err != nil {
			// Best-effort, and deliberately so: a log write must never stop a
			// turn. The consequence is a hole in one child's ordinal sequence,
			// which a resuming consumer sees as a gap rather than as silence.
			slog.Warn("event log append", "childId", childID,
				"type", eventlog.TypeName(ev), "error", err)
		} else {
			ev.Ordinal = &ord
		}
	}
	if c.native != nil {
		c.native.Publish(childID, ev)
	}
}

// childHooks builds the per-child callbacks every SpawnSpec carries.
//
// All three are installed for EVERY kind, deliberately. NativeSink used to be
// set only inside `if runner != nil`, which is fundi-only (agentRunner returns
// a nil Runner for every other kind), so the claude translator in pkg/child
// was unreachable and attaching to a claude child showed an empty pane. A
// child whose provider has no native translation simply produces no events;
// there is nothing to gate.
//
// OnMeta persists the sniffed session id immediately. The store's own sync runs
// in monitorChild off BUS frames, and claude's system/init produces none, so
// without this the id reaches the database only on the first bus frame of the
// first turn — and resume reads that column.
//
// OnSubagent attributes a native subagent to the Task call that spawned it.
// Unlike the other two it runs on its own goroutine (see SpawnSpec.OnSubagent).
func (c *Controller) childHooks(childID string) (func(*rafikiv1.Event), func(child.SnifferMetadata), func(child.SubagentObservation)) {
	sink := func(ev *rafikiv1.Event) {
		if ev.GetChildId() == "" {
			ev.ChildId = childID
		}
		c.publishEvent(childID, ev)
	}
	onMeta := func(md child.SnifferMetadata) {
		if md.SessionID == "" {
			return
		}
		// The `changed` guard is load-bearing, not an optimisation. This runs on
		// the child's readStdout goroutine and writeRecord is a database upsert
		// with a 5s timeout, so an unguarded write would stall stdout once per
		// TURN: claudeProvider.Parse reports metadata on every `result` frame,
		// not only on `system/init`. Guarded, it writes once per child.
		changed := false
		held := ""
		if err := c.st.Update(childID, func(s *childstore.Session) {
			held = s.SessionID
			if s.SessionID != md.SessionID && (s.Kind != protocol.KindClaude || s.SessionID == "") {
				s.SessionID = md.SessionID
				changed = true
			}
		}); err != nil {
			return
		}
		if c.refuseClaudeSessionIDChange(childID, held, md.SessionID) {
			return
		}
		if !changed {
			return
		}
		if err := c.writeRecord(childID); err != nil {
			slog.Warn("write state record (after session sniff)", "childId", childID, "error", err)
		}
	}
	return sink, onMeta, func(obs child.SubagentObservation) {
		c.HandleSubagentObservation(childID, obs)
	}
}

func (c *Controller) List(filter protocol.ListFilter) []childstore.Snapshot {
	snaps := c.st.List()
	if filter.Status == "" && filter.Name == "" && filter.NameContains == "" &&
		filter.CwdContains == "" && filter.Since.IsZero() &&
		len(filter.Labels) == 0 && len(filter.HasLabel) == 0 {
		return snaps
	}
	out := snaps[:0]
	for _, s := range snaps {
		if filter.Status != "" && string(s.Status) != filter.Status {
			continue
		}
		if filter.Name != "" && s.Name != filter.Name {
			continue
		}
		if filter.NameContains != "" && !strings.Contains(s.Name, filter.NameContains) {
			continue
		}
		if filter.CwdContains != "" && !strings.Contains(s.Cwd, filter.CwdContains) {
			continue
		}
		if !filter.Since.IsZero() && s.StartedAt.Before(filter.Since) {
			continue
		}
		if !matchesLabelFilter(s.Labels, filter.Labels, filter.HasLabel) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (c *Controller) Get(childID string) (childstore.Snapshot, bool) {
	return c.st.Get(childID)
}

// OwnerUserIDForChild implements server.ChildOwnerLookup: it lets the proxy
// face attribute a child-secret-authenticated request to the user whose
// SUBTREE the child belongs to. Every child is stamped at spawn with its
// subtree's owner — a user credential's spawn carries the id, and the
// controller spawner hands its own row's id down (agent_spawner.go) — so the
// common case resolves on the child's own row. The parent-chain walk exists
// for rows written before that inheritance landed: resume preserves the
// stored row without backfilling it, so a child spawned by an agent under an
// older daemon keeps an empty id for its whole life. Only a lineage with NO
// owner anywhere — the anonymous local-unix-socket spawn shape — refuses,
// falling back to the anonymous identity as before. The walk is in-memory
// childstore hops, never a database round trip: it runs per request on the
// attribution face.
func (c *Controller) OwnerUserIDForChild(childID string) (string, bool) {
	cur := childID
	// The bound is a cycle guard, not a depth limit: ParentOf links only point
	// at earlier children, so the chain terminates. hopsTo bounds the same
	// walk the same way.
	for hops := 0; hops < 64; hops++ {
		snap, ok := c.st.Get(cur)
		if !ok {
			return "", false
		}
		if snap.OwnerUserID != "" {
			return snap.OwnerUserID, true
		}
		parent, ok := c.st.ParentOf(cur)
		if !ok || parent == "" {
			return "", false
		}
		cur = parent
	}
	return "", false
}

// mintMCPToken returns the per-child MCP secret for childID, minting one on
// the first call for that child and REUSING it on every later call. Resume and
// RespawnChild rebuild the spawn environment through the same proxyChildEnv /
// darajaClaudeParams path a fresh spawn takes, and the design requires resume
// to reuse the stored secret rather than mint a fresh one — a second secret
// would orphan the credential the still-running child holds and break its next
// MCP connection. The secret is 32 crypto/rand bytes, hex encoded; it is never
// logged and never reaches argv (it travels to the child by environment only).
//
// The maps are lazily initialized here rather than in NewController because a
// hand-built Controller (the proxyenv tests' zero-value literal) also mints.
func (c *Controller) mintMCPToken(childID string) string {
	c.mcpTokensMu.RLock()
	if tok, ok := c.mcpTokensByChild[childID]; ok && tok != "" {
		c.mcpTokensMu.RUnlock()
		return tok
	}
	c.mcpTokensMu.RUnlock()

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// A per-child credential that cannot be random must not ship: better
		// a loud spawn failure than a predictable MCP credential.
		panic(fmt.Sprintf("mintMCPToken: crypto/rand failed: %v", err))
	}
	tok := hex.EncodeToString(secret)

	c.mcpTokensMu.Lock()
	defer c.mcpTokensMu.Unlock()
	if c.mcpTokens == nil {
		c.mcpTokens = make(map[string]string)
	}
	if c.mcpTokensByChild == nil {
		c.mcpTokensByChild = make(map[string]string)
	}
	c.mcpTokens[tok] = childID
	c.mcpTokensByChild[childID] = tok
	return tok
}

// mcpTokenSweepThreshold is how much the credential map grows past its last
// sweep before the next one runs. Sweeping is O(map size) store lookups on
// the spawn path, so it must not fire per mint — the offset after each sweep
// keeps the cost amortized over that many new mints and bounds the map at
// roughly live-children + one threshold.
const mcpTokenSweepThreshold = 256

// sweepMCPTokensIfDue drops credential entries whose child no longer exists
// or has exited, both of which ChildForMCPToken already refuses — the sweep
// is memory hygiene, not a correctness fix. Triggered lazily from
// mintMCPToken: the daemon spawns a child, and if the map has grown one
// threshold past the last sweep it pays one bounded scan. A controller with
// no store (the hand-built zero-value fixtures that mint) never sweeps.
func (c *Controller) sweepMCPTokensIfDue() {
	if c.st == nil {
		return
	}
	c.mcpTokensMu.RLock()
	at := c.mcpSweepAt
	if at == 0 {
		at = mcpTokenSweepThreshold
	}
	n := len(c.mcpTokensByChild)
	var children []string
	if n >= at {
		children = make([]string, 0, n)
		for id := range c.mcpTokensByChild {
			children = append(children, id)
		}
	}
	c.mcpTokensMu.RUnlock()
	if children == nil {
		return
	}

	for _, id := range children {
		// forgetMCPToken takes the write lock itself, so the candidate list is
		// snapshotted first and the lock is released — see the mint call site.
		if snap, ok := c.st.Get(id); !ok || snap.Status == protocol.StatusExited {
			c.forgetMCPToken(id)
		}
	}

	c.mcpTokensMu.Lock()
	c.mcpSweepAt = len(c.mcpTokensByChild) + mcpTokenSweepThreshold
	c.mcpTokensMu.Unlock()
}

// ChildForMCPToken implements server.ChildTokenLookup: it resolves a per-child
// MCP secret to the child that holds it and that child's owner — the row's
// own id, or the nearest ancestor's when the row predates spawn-time
// inheritance (see OwnerUserIDForChild). ok is false for an unknown secret, a
// child that has exited, or a lineage with no recorded owner (the
// anonymous-spawn case OwnerUserIDForChild also refuses).
func (c *Controller) ChildForMCPToken(token string) (childID, ownerUserID string, ok bool) {
	c.mcpTokensMu.RLock()
	childID, known := c.mcpTokens[token]
	c.mcpTokensMu.RUnlock()
	if !known || childID == "" {
		return "", "", false
	}
	// forgetMCPToken runs in handleChildExit beside MarkExited, so a dead
	// child's mapping is normally already gone; the status check covers the
	// ordering window an exit record and the forget can land in either order
	// around.
	if snap, exists := c.st.Get(childID); !exists || snap.Status == protocol.StatusExited {
		return "", "", false
	}
	owner, owned := c.OwnerUserIDForChild(childID)
	if !owned {
		return "", "", false
	}
	return childID, owner, true
}

// forgetMCPToken drops childID's per-child MCP secret, called from
// handleChildExit — already the one place an exit is recorded — so a dead
// child's secret stops resolving. Both indexes go together: keeping the
// child->secret half would make the next mint for a respawned child return a
// credential whose secret->child half no longer exists, i.e. one that can
// never authenticate.
func (c *Controller) forgetMCPToken(childID string) {
	c.mcpTokensMu.Lock()
	defer c.mcpTokensMu.Unlock()
	if tok, ok := c.mcpTokensByChild[childID]; ok {
		delete(c.mcpTokensByChild, childID)
		delete(c.mcpTokens, tok)
	}
}

// ConversationID satisfies connectapi.ConversationResolver: it maps a child
// id to the conversation UUID that owns its persisted message history.
//
// Delegates to conversationIDForChild, which GetRecent already used, rather
// than keeping a second resolver: this one gated on KindFundi, so Connect
// GetHistory answered NotFound for every claude child while GetRecent served
// the same child's rows happily. A real claude child at least streamed live
// through the event log and merely lost its backfill; a synthetic thread child
// (rafiki/native-subagent) has no event log entries at all, so GetHistory is
// its ONLY source and the cockpit rendered it permanently empty.
func (c *Controller) ConversationID(childID string) (string, bool) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return "", false
	}
	id := c.conversationIDForChild(snap)
	return id, id != ""
}

// recentSource reads the GetRecent branch seam atomically. "" before the
// first call of this Controller.
func (c *Controller) recentSource() string {
	v, _ := c.lastRecentSource.Load().(string)
	return v
}

// recentQuery carries the parameters for GetRecent. It used to be
// pkg/control's RecentQuery — the framed dispatch's request struct — and
// moves here with the dispatch retired, since GetRecent is now reachable
// only through the Connect adapters and this package's own callers.
type recentQuery struct {
	Limit    int
	Since    int64
	Include  []string
	Exclude  []string
	Rendered bool
}

// searchQuery carries the parameters for Search, with the same history as
// recentQuery.
type searchQuery struct {
	Query         string
	Regex         bool
	Limit         int
	Context       int
	SessionFilter protocol.SearchSessionFilter
}

func (c *Controller) GetRecent(childID string, q recentQuery) (protocol.GetRecentResponseData, error) {
	c.lastRecentSource.Store("")
	snap, ok := c.st.Get(childID)
	if !ok {
		return protocol.GetRecentResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	ch, alive := c.cm.Get(childID)

	// Select the source event slice based on whether the child's persisted
	// conversation resolves, then on liveness. A resolvable conversation (fundi
	// children, claude children the proxy captured) reads from the database
	// (canonical store); the rest keep the ring-buffer + on-disk-dump path.
	var events []ring.Event
	var total int
	var oldestTS int64

	convID := c.conversationIDForChild(snap)
	if convID != "" || snap.Kind == protocol.KindFundi {
		// Fundi children stay in this branch even when the conversation id does
		// not resolve (an exited-before-first-turn child): their contract is
		// db-only, with no disk fallback (TestGetRecentFundiNoDB).
		c.lastRecentSource.Store("db")
		events = c.dbRecent(convID, q)
		total = len(events)
		if len(events) > 0 {
			oldestTS = events[0].Timestamp
		}
	} else if alive {
		c.lastRecentSource.Store("live")
		if q.Rendered && ch.Normalizes() {
			events = ch.RenderRecent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, oldestTS = ch.RenderStats()
		} else {
			r := ch.Ring()
			events = r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, _, oldestTS = r.Stats()
		}
	} else {
		c.lastRecentSource.Store("exited")
		// Exited: pick the snapshot, falling back to the on-disk dump for
		// orphans reloaded after a restart (in-memory snapshots are lost then).
		var all []ring.Event
		if q.Rendered {
			all = snap.ExitedRenderRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "render.jsonl.gz")
			}
		}
		// Fall back to the raw stream UNLESS this is a rendered request for a
		// normalizing (claude) child: claude's raw stdout is NOT renderable, so
		// an empty render-ring must stay empty rather than dumping raw frames
		// into the rendered view (matches the live path). pi's raw ring already
		// IS pi-vocabulary, so pi rendered requests still fall through here.
		if len(all) == 0 && (!q.Rendered || snap.Kind != protocol.KindClaude) {
			all = snap.ExitedRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "out.jsonl.gz")
			}
		}
		total = len(all)
		if len(all) > 0 {
			oldestTS = all[0].Timestamp
		}
		if q.Since > 0 {
			i := 0
			// A zero timestamp means "unknown" (render frames sourced from the
			// on-disk render.jsonl.gz carry no timestamp) — keep those rather
			// than dropping the whole disk-sourced rendered backfill.
			for i < len(all) && all[i].Timestamp != 0 && all[i].Timestamp < q.Since {
				i++
			}
			all = all[i:]
		}
		if q.Limit > 0 && len(all) > q.Limit {
			all = all[len(all)-q.Limit:]
		}
		events = all
	}

	out := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		if framePassesTypeFilter(ev.Bytes, q.Include, q.Exclude) {
			out = append(out, jsonSafeEvent(ev.Bytes))
		}
	}

	// The response is a single JSONL frame and every reader caps frames at
	// protocol.MaxFrameBytes; keep the newest events that fit half that
	// budget (headroom for the response envelope and other data fields).
	size := 0
	cut := len(out)
	for i := len(out) - 1; i >= 0; i-- {
		if size+len(out[i])+1 > recentResponseBudget {
			break
		}
		size += len(out[i]) + 1
		cut = i
	}
	truncatedBySize := cut > 0
	out = out[cut:]

	return protocol.GetRecentResponseData{
		Events:           out,
		TotalInBuffer:    total,
		OldestTimestamp:  oldestTS,
		TruncatedByLimit: q.Limit > 0 && len(out) == q.Limit,
		TruncatedBySize:  truncatedBySize,
	}, nil
}

// recentResponseBudget bounds the summed event bytes in one GetRecent
// response so the marshaled frame stays well under protocol.MaxFrameBytes.
const recentResponseBudget = protocol.MaxFrameBytes / 2

// jsonSafeEvent returns b unchanged when it is already valid JSON, and as a
// JSON string otherwise. The ring is a JSONL store whose appenders are
// supposed to emit frames — but a script child's stdout is free-form text
// (its provider publishes every line verbatim), so its ring events are bare
// lines, and one non-JSON event among them made json.Marshal drop the ENTIRE
// response's data (okResponse swallows the marshal error) — `rafiki logs` and
// every other GetRecent consumer read an empty payload for a child whose logs
// were sitting right there. Quoting the strays keeps the content verbatim (a
// JSON string escapes it) and makes the response always marshalable.
func jsonSafeEvent(b []byte) json.RawMessage {
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	quoted, err := json.Marshal(string(b))
	if err != nil {
		// Cannot happen for any Go string, but a fallback that is still valid
		// JSON beats reintroducing the silent drop.
		return json.RawMessage(`"<unmarshalable event>"`)
	}
	return json.RawMessage(quoted)
}

// readDiskEvents reads a per-child on-disk dump file (out.jsonl.gz /
// render.jsonl.gz) into ring.Events with zero timestamps. Returns nil when the
// dump is absent or unreadable (best-effort backfill after a restart).
func (c *Controller) readDiskEvents(childID, name string) []ring.Event {
	if c.logsDir == "" {
		return nil
	}
	frames, err := persist.ReadGzLines(filepath.Join(c.logsDir, childID, name))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("readDiskEvents: backfill dump unreadable", "child", childID, "file", name, "error", err)
		}
		return nil
	}
	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}

func (c *Controller) GetStreams(childID string, which string) (protocol.GetStreamsResponseData, error) {
	if _, ok := c.st.Get(childID); !ok {
		return protocol.GetStreamsResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	ch, alive := c.cm.Get(childID)
	if !alive {
		return protocol.GetStreamsResponseData{Alive: false}, nil
	}
	res := protocol.GetStreamsResponseData{Alive: true}
	if which == "" || which == "all" || which == "in" {
		res.In = ch.InSnapshot()
	}
	// Live stderr is intentionally omitted: errBuf is an unguarded bytes.Buffer
	// written by the readStderr goroutine, so StderrSnapshot races until Done()
	// is closed. Stderr for a live child is therefore left nil; callers fall
	// back to the on-disk dump, which becomes available after the child exits.
	return res, nil
}

func (c *Controller) Search(q searchQuery) protocol.SearchResponseData {
	start := time.Now()
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	snaps := c.st.List()
	var hits []protocol.SearchHit
	scanned := 0

	for _, snap := range snaps {
		if !matchesSessionFilter(snap, q.SessionFilter) {
			continue
		}
		ch, alive := c.cm.Get(snap.ChildID)
		if !alive {
			continue // exited children have no ring buffer in v1
		}

		events := ch.Ring().Recent(ring.Query{})
		for _, ev := range events {
			scanned++
			idx := strings.Index(string(ev.Bytes), q.Query)
			if idx < 0 {
				continue
			}
			snippet := string(ev.Bytes)
			const maxSnippet = 256
			if len(snippet) > maxSnippet {
				snippet = snippet[:maxSnippet]
			}
			hits = append(hits, protocol.SearchHit{
				ChildID:     snap.ChildID,
				SessionFile: snap.SessionFile,
				SessionID:   snap.SessionID,
				SessionName: snap.Name,
				Timestamp:   time.UnixMilli(ev.Timestamp),
				Snippet:     snippet,
				MatchStart:  idx,
				MatchEnd:    idx + len(q.Query),
			})
			if len(hits) >= limit {
				return protocol.SearchResponseData{
					Hits:      hits,
					TotalHits: len(hits),
					Scanned:   scanned,
					Elapsed:   time.Since(start),
				}
			}
		}
	}
	return protocol.SearchResponseData{
		Hits:      hits,
		TotalHits: len(hits),
		Scanned:   scanned,
		Elapsed:   time.Since(start),
	}
}

func (c *Controller) Status() protocol.StatusResponseData {
	snaps := c.st.List()
	var live, exited int
	for _, s := range snaps {
		if s.Status == protocol.StatusExited {
			exited++
		} else {
			live++
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return protocol.StatusResponseData{
		Version:     version.String(),
		StartedAt:   c.startedAt,
		Children:    protocol.ChildCounts{Live: live, Exited: exited},
		MemoryBytes: int64(ms.Sys),
		Socket:      c.socketPath,
		LogsDir:     c.logsDir,
	}
}

// ─── Conversation insights (backed by the agent database) ────────────────────

// translateInsightsErr translates errors from the agentcli/local backend into
// the wire error codes clients can act on, distinguishing expected,
// actionable states from a genuine query failure:
//   - agentcli/local.ErrNoPool ("no database pool configured") means the
//     daemon has no agent database configured at all.
//   - insights.ErrNotFound means the request named a specific conversation
//     (ConversationStatsByID / ConversationExport) that does not exist.
//
// Any other error is returned unchanged, so dispatch's mapErr falls back to
// protocol.ErrInternal.
func translateInsightsErr(err error) error {
	if errors.Is(err, local.ErrNoPool) {
		return &connectapi.ControllerError{
			Code:    protocol.ErrNoAgentDB,
			Message: "no agent database configured (RAFIKI_DB unset); set it and run `rafiki service install`",
		}
	}
	if errors.Is(err, insights.ErrNotFound) {
		return &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: err.Error(),
		}
	}
	return err
}

// The scope threading through these five methods is derived per connection by
// pkg/control's scopeForConnection (empty identity → ScopeAll, admin →
// ScopeAll, user → ScopeOwner) and passed through untouched — the Controller
// adds no scope decision of its own.
func (c *Controller) ConversationStats(ctx context.Context, scope insights.Scope, f insights.StatsFilter) (*insights.Stats, error) {
	st, err := c.insights.Stats(ctx, scope, f)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationStatsByID(ctx context.Context, scope insights.Scope, id string) (*insights.Stats, error) {
	st, err := c.insights.ConversationStats(ctx, scope, id)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationSearch(ctx context.Context, scope insights.Scope, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
	rows, err := c.insights.Search(ctx, scope, f)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return rows, nil
}

func (c *Controller) ConversationExport(ctx context.Context, scope insights.Scope, id string) (*insights.Transcript, error) {
	tr, err := c.insights.Export(ctx, scope, id)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return tr, nil
}

func (c *Controller) ConversationQuery(ctx context.Context, scope insights.Scope, name string, f insights.StatsFilter) (insights.QueryResult, error) {
	res, err := c.insights.Query(ctx, scope, name, f)
	if err != nil {
		return insights.QueryResult{}, translateInsightsErr(err)
	}
	return res, nil
}

// reviewReads is the pkg/insights surface the two review verbs consume. The
// methods live on *insights.Insights (pkg/insights alone owns scope-to-SQL
// translation — see constraints.md), so an interface over exactly these
// three is what lets ConversationReview/ConversationFindings stay testable
// without a database while production wires the real *insights.Insights.
type reviewReads interface {
	FilterByScope(ctx context.Context, scope insights.Scope, ids []string) (canonical, spellings []string, err error)
	RecentAnalyses(ctx context.Context, scope insights.Scope, conversationIDs []string, limit int) ([]insights.AnalysisRow, error)
	Findings(ctx context.Context, scope insights.Scope, f insights.FindingsFilter) ([]insights.Finding, error)
}

// errReviewNoDB is the review verbs' answer on a daemon with no agent
// database: neither reviewQ nor reviewInsights exists. Shaped here (the
// adapters return the Controller's error verbatim) with the same code
// controllerConnectCode gives ErrNoAgentDB — a configuration gap, not a
// transient failure.
var errReviewNoDB = connect.NewError(connect.CodeFailedPrecondition,
	errors.New("no agent database configured (RAFIKI_DB unset); conversation review is unavailable"))

// ConversationReview validates req.Model (if set, or its env default)
// against c.catalog, resolves every optional field per design §3 (request >
// RAFIKI_REVIEW_* env > profile defaults — profile defaults are resolved
// lazily inside the worker via resolveProfile, so this method only fills the
// env tier), clamps budget_usd against RAFIKI_REVIEW_MAX_BUDGET_USD when set
// (downward only; a negative ceiling is treated as 0 — no ceiling — before
// the max-env logic), and calls c.reviewQ.tryAccept once per (scope-filtered)
// conversation id. Requested ids may be conversation uuids or child ids (the
// client's Resolve produces child ids); FilterByScope canonicalizes the
// survivors to their conversation uuids index-aligned with the surviving
// caller spellings, and this method queues on the CANONICAL value while the
// response echoes the CALLER's spelling — so two spellings of one
// conversation (its uuid and its child id) collide correctly in the
// single-flight map. Returns one connectapi.ReviewAccept per surviving id, in
// input order. An id matching neither arm — nonexistent or out of scope — is
// dropped silently — the same not-found-shaped scope miss pkg/insights uses
// elsewhere; never a permission error, which would leak the id's existence.
// Never blocks on an LLM call: the worker owns the run.
//
// Model validation happens HERE, not inside the worker: a request naming an
// unresolvable model must fail the whole request (design §6), not surface
// as a per-id status. budget_usd and min_turns apply per conversation, never
// divided or aggregated across a batch (design §4).
func (c *Controller) ConversationReview(ctx context.Context, scope insights.Scope, req connectapi.ReviewRequest) ([]connectapi.ReviewAccept, error) {
	if c.reviewQ == nil || c.reviewInsights == nil {
		return nil, errReviewNoDB
	}

	// Stage: connectapi's proto mapping leaves only "detect"/"rank", but
	// this method is also called directly (tests, future internal callers),
	// so normalize defensively — StopAfter must never be "" (design §7,
	// draft must never run from this verb).
	stage := req.Stage
	if stage != "rank" {
		stage = "detect"
	}

	// Config resolution, design §3. The request tier wins; the env tier
	// fills gaps; the profile tier is the worker's business.
	model := req.Model
	if model == "" {
		model = os.Getenv("RAFIKI_REVIEW_MODEL")
	}
	if model != "" && !c.reviewModelResolves(model) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("conversation review: model %q does not resolve in the daemon's model catalog or provider registry", model))
	}
	profileName := req.Profile
	if profileName == "" {
		profileName = os.Getenv("RAFIKI_REVIEW_PROFILE")
	}
	budgetUSD := 0.0
	if req.HasBudgetUSD {
		budgetUSD = req.BudgetUSD
	} else if v, ok := envFloat("RAFIKI_REVIEW_BUDGET_USD"); ok {
		budgetUSD = v
	}
	// The clamp is the one daemon-side cost knob (design §6): when set, a
	// request's budget_usd is clamped DOWN to it — unset env means the
	// request's own cap is the only cap, and a zero (unrequested) ceiling
	// becomes the max rather than staying unlimited.
	// A negative ceiling is malformed, not "spend nothing": 0 already means
	// no ceiling here, so fold the negative to 0 BEFORE the max-env logic.
	if budgetUSD < 0 {
		budgetUSD = 0
	}
	if max, ok := envFloat("RAFIKI_REVIEW_MAX_BUDGET_USD"); ok && (budgetUSD == 0 || budgetUSD > max) {
		budgetUSD = max
	}
	minTurns := int32(0)
	if req.HasMinTurns {
		minTurns = req.MinTurns
	} else if v, ok := envInt32("RAFIKI_REVIEW_MIN_TURNS"); ok {
		minTurns = v
	}

	canonical, admitted, err := c.reviewInsights.FilterByScope(ctx, scope, req.ConversationIDs)
	if err != nil {
		return nil, err
	}

	accepted := make([]connectapi.ReviewAccept, 0, len(admitted))
	for k, spelling := range admitted {
		accepted = append(accepted, connectapi.ReviewAccept{
			ConversationID: spelling,
			Status: c.reviewQ.tryAccept(reviewJob{
				conversationID: canonical[k],
				stage:          stage,
				model:          model,
				profileName:    profileName,
				budgetUSD:      budgetUSD,
				minTurns:       minTurns,
				force:          req.Force,
			}),
		})
	}
	return accepted, nil
}

// reviewModelResolves reports whether model names something the daemon can
// actually serve a review job with. providers.Set.Split is the base gate —
// the worker's LLM client routes through the same registry, so an unknown
// provider fails here with an actionable message rather than mid-run as a
// per-conversation error. Beyond it, two sources, mirroring ModelInfo's own
// resolution: the provider's declared model alias (a custom or locally-served
// provider's model is never in the OpenRouter catalog — the alias table is
// how the operator declares it) and the OpenRouter catalog. The catalog
// resolves the id Split already SUBSTITUTED — the provider-local spelling,
// which is what real catalog keys are ("z-ai/glm-5.3-flash",
// "anthropic/claude-sonnet-5"), never the qualified request string: Split
// only accepts "openrouter/<id>", which is the spelling the daemon's own
// surfaces (ListModelRows, agent_models, the picker) offer, and no catalog
// carries that prefix.
func (c *Controller) reviewModelResolves(model string) bool {
	set := c.providers
	if set == nil {
		set = providers.Default()
	}
	p, resolvedID, err := set.Split(model)
	if err != nil {
		return false
	}
	// The request-side local id is the alias KEY on the provider Split
	// returned; a bare id resolved against DefaultProvider inside Split, so
	// no provider-name handling is needed here. A declared alias means the
	// operator registered the model with the daemon.
	if _, localID := providers.SplitRaw(model); localID != "" {
		if _, declared := p.Models[localID]; declared {
			return true
		}
	}
	if c.catalog == nil {
		return false
	}
	_, ok := c.catalog.ResolveID(resolvedID)
	return ok
}

// ConversationFindings delegates to the review read surface (pkg/insights
// owns the scoped SQL), scoped exactly like ConversationReview. Both halves
// come back: findings for the filter, and the recent analysis rows that show
// an enqueued review's outcome over the wire.
func (c *Controller) ConversationFindings(ctx context.Context, scope insights.Scope, f connectapi.ReviewFindingsFilter) ([]connectapi.ReviewFinding, []connectapi.ReviewAnalysis, error) {
	if c.reviewInsights == nil {
		return nil, nil, errReviewNoDB
	}
	rows, err := c.reviewInsights.Findings(ctx, scope, insights.FindingsFilter{
		Axis: f.Axis, Skill: f.Skill, Status: f.Status,
		ConversationIDs: f.ConversationIDs, Limit: f.Limit,
	})
	if err != nil {
		return nil, nil, err
	}
	analyses, err := c.reviewInsights.RecentAnalyses(ctx, scope, f.ConversationIDs, f.Limit)
	if err != nil {
		return nil, nil, err
	}
	findings := make([]connectapi.ReviewFinding, 0, len(rows))
	for _, r := range rows {
		findings = append(findings, connectapi.ReviewFinding{
			ID: r.ID, AnalysisID: r.AnalysisID, ConversationID: r.ConversationID,
			Axis: r.Axis, TopicKey: r.TopicKey, SkillName: r.SkillName, Title: r.Title,
			ExpectedSavingsTokens: r.ExpectedSavingsTokens, Status: r.Status,
		})
	}
	analysisRows := make([]connectapi.ReviewAnalysis, 0, len(analyses))
	for _, a := range analyses {
		analysisRows = append(analysisRows, connectapi.ReviewAnalysis{
			ID: a.ID, ConversationID: a.ConversationID, Model: a.Model, Profile: a.Profile,
			Status: a.Status, Error: a.Error,
			InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
			CostUSD: a.CostUSD, CreatedAt: a.CreatedAt,
		})
	}
	return findings, analysisRows, nil
}

// inheritSkipDerivedIndex ORs the parent's flag into a parented spawn: a child
// can switch derived indexing off for its subtree and can never switch it back
// on beneath a parent that has it off.
func (c *Controller) inheritSkipDerivedIndex(req protocol.SpawnRequest) protocol.SpawnRequest {
	if req.ParentChildID == "" {
		return req
	}
	if parent, ok := c.st.Get(req.ParentChildID); ok && parent.SkipDerivedIndex {
		req.SkipDerivedIndex = true
	}
	return req
}

func (c *Controller) Spawn(ctx context.Context, req protocol.SpawnRequest, owner users.Identity) (protocol.SpawnResponseData, error) {
	// The model the REQUEST named, captured before applyPreset can replace it
	// with the preset's: resolveRouting parses both and merges spawn over
	// preset, so it needs the original spelled-out here.
	spawnModel := req.Model

	// Preset resolution runs FIRST, before anything reads req — notably
	// checksCwdLocally below, which reads req.Kind, and every later step that
	// overrides or narrows a preset-supplied field. The error is already a
	// *connectapi.ControllerError.
	req, presetRec, err := c.applyPreset(ctx, req, owner.UserID)
	if err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// Routing resolves ONCE, immediately after the preset it merges under:
	// the spawn/preset specs are parsed out of Model (brackets stripped into
	// the base id), the policy row fills the gaps, and the merged spec rides
	// req.Routing from here on — stored on the session, plumbed to the child,
	// labelled, and kept verbatim across resume. Never re-resolved.
	req, err = c.resolveRouting(spawnModel, presetRec, req)
	if err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// An empty kind is the fundi default, and it is resolved ONCE here so
	// every consumer agrees on it. resolveSpawnPlan applies the same default
	// when it builds argv, but that is too late for agentRunner's switch: a
	// raw empty kind fell through the switch (no case matches "") to the
	// nil-Runner subprocess path, where the fundi engine runs as a
	// `rafikid fundi` subprocess that cannot know its owner — the child's
	// conversation row landed unattributed (owner_user_id NULL) and every
	// owner-scoped read over it (ConversationExport first among them)
	// answered not-found. A preset's kind, if any, already replaced req.Kind
	// above, so this only fills the genuinely-absent case.
	if req.Kind == "" {
		req.Kind = protocol.KindFundi
	}

	// A spawn block confines the child to a sandbox workspace, which only a
	// fundi child can be. A claude or script child is LAUNCHED, not
	// workspace-bound, so a sandbox on one would be built and then never used —
	// refused here, before the sandbox is created. Checked after preset
	// resolution so the effective kind is the one that will actually run.
	if req.Sandbox != nil && req.Kind != protocol.KindFundi {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "a sandbox spawn block is only supported for kind=fundi: claude and script children are launched, not workspace-bound",
		}
	}

	// A pre-fill the child could not run is refused here, before anything
	// else reads req — same ordering argument as applyPreset above: the
	// preset's kind and tool shaping are already resolved, so the check sees
	// the request the child would actually receive.
	if err := validatePrefill(req); err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// A kind=script request that names any fundi/claude-only field is
	// refused here, before lineage or limits read req. It runs after
	// applyPreset so the kind is resolved and preset-filled fields are seen
	// as they would reach the runner.
	if err := validateScriptSpawn(req); err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// Validate cwd exists on THIS machine (dispatch already checks it's
	// absolute) — but only for kinds the daemon itself forks a subprocess
	// for. A fundi child never touches the daemon's own filesystem: its
	// tools, if any, run against whichever executor gets bound after this
	// point, and that executor validates its own root independently. A
	// claude child is the same story once an executor pool is configured —
	// its daraja (and the claude process it hosts) run on the EXECUTOR's
	// machine, not this one, so cwd lives there too. Stat-ing req.Cwd here
	// for either case checks the wrong machine — wrongly rejecting a valid
	// path that exists only on a remote executor, or wrongly accepting a
	// coincidentally-existing but unrelated path on the daemon host. Only a
	// claude child with NO executor pool configured actually forks locally
	// (claudeRunner's local-subprocess fallback), so that is the only
	// remaining case this check applies to.
	checksCwdLocally := req.Kind != protocol.KindFundi &&
		(req.Kind != protocol.KindClaude || c.execPoolConn == nil) &&
		(req.Kind != protocol.KindScript || !c.scriptExecutorRouted())
	if checksCwdLocally {
		if _, err := os.Stat(req.Cwd); err != nil {
			return protocol.SpawnResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: "cwd: " + err.Error(),
			}
		}
	}

	// Validate user-supplied labels: no invalid keys, no rafiki/ prefix.
	if err := validateUserLabelKeys(req.Labels); err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	// Resolve lineage before spawning: a bad parentChildId must fail without
	// leaving a started process behind.
	parentLabel, rootLabel, err := computeLineageLabels(c.st, req.ParentChildID)
	if err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// Before the grant is inherited, not after: this asks what the PARENT was
	// confined to, and inheritExecutorGrant would copy that grant onto a child
	// whose kind cannot honour it, making the two indistinguishable.
	if err := checkKindNarrowing(c.st, req, c.claudeExecutorRouted(), c.scriptExecutorRouted()); err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// A :batch model is submitted with the DAEMON's OpenRouter key (the
	// batcher built at startup, SetBatcher), so a spawn carrying its own
	// per-spawn key could never honour it — and running the batch on the
	// daemon's key while the caller believed their own was billing would be
	// worse than refusing. Refused here, in the same request-validation block
	// as the other refusals, before anything is minted. The resolved model id
	// (alias substitution included) is what carries the suffix.
	if req.APIKey != "" && req.Model != "" {
		if _, modelID, err := providersOrDefault(c.providers).Split(req.Model); err == nil && llm.IsBatchModel(modelID) {
			return protocol.SpawnResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: ":batch models are submitted with the daemon's OpenRouter key; this spawn carries its own API key",
			}
		}
	}

	// A silent executor grant INHERITS the spawner's. Done here, before
	// anything reads req, so every path gets it: the runtime's
	// resolveExecutor, the workspace provisioning check, and the selector
	// stored on the new session.
	req = c.inheritExecutorGrant(req)

	// A child inherits its parent's derived-index suppression. Done in the same
	// pre-read block as the executor grant, before anything reads req.
	req = c.inheritSkipDerivedIndex(req)

	// Computed before the executor-grant normalization and before agentRunner,
	// rather than alongside the rest of initLabels below: the normalization's
	// ref resolution and agentRunner's own executor selection (chooseExecutor)
	// both run admission against labels this child does not have a childstore
	// entry to carry yet (see admissionLabels). The owner must already be in
	// hand or a session executor's "admits: owner=<user>" (every one — see
	// ExecutorSession) refuses every top-level spawn outright.
	ownerName := attestOwner(c.st, req, owner)

	// A bare machine name is a REF, not a selector: ParseSelector would read
	// "greyshift" as "must carry a label named greyshift" — a guaranteed zero
	// match against {owner, machine} labels, and exactly why an MCP caller's
	// executor: "greyshift" was refused while the same word works on the CLI
	// (--executor is a ref). Promotion rewrites it so resolveRef matches the
	// machine label against the SAME confinement-narrowed candidate set.
	req = promoteBareExecutorRef(req)

	// A ref-only grant is persisted as a selector, or it never reaches the
	// stored session: lineage narrowing would see "" and the whole subtree
	// would escape the pin, and resume/respawn would rebuild without it.
	// Runs while nothing is minted, so a refusal starts no process.
	req, err = c.persistRefAsSelector(req, executorOwner{Name: ownerName, UserID: owner.UserID})
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "executor grant: " + err.Error(),
		}
	}

	// Resource admission. Deliberately before the childID is minted and long
	// before anything is registered: a refusal must leave no process, no
	// store entry, no record and — with phase 04's ordering — no task
	// assignment to roll back.
	if err := c.checkSpawnLimits(req); err != nil {
		return protocol.SpawnResponseData{}, err
	}

	// childID is minted before resolveSpawnPlan (rather than after, as
	// before) because the "fundi" kind needs it to pin --spill-dir
	// (see buildAgentArgv/agentSpillDir).
	childID := newChildID()

	// A spawn block: create the sandbox NOW, before the runtime is built.
	// agentRuntimeOptions eagerly binds below, and a correct bind needs the
	// sandbox row in place; the row itself records every access-gating fact the
	// bind relies on. A failure REFUSES the spawn — a sandboxed child must never
	// start toolless or native.
	//
	// On success the request's sibling grant is rewritten to the sandbox's
	// machine label. This is NOT routed through persistRefAsSelector: that
	// resolves a ref by selection, which now (correctly) excludes child-owned
	// sandbox rows, so it could never express the pin. The stored selector is
	// what confines the child's DESCENDANTS through lineage narrowing.
	//
	// sandboxOwnedChild, while non-empty, is the child whose sandbox a later
	// failure must tear down. It is cleared once the child's own row is written.
	sandboxOwnedChild := ""
	if req.Sandbox != nil {
		executor, rowID, containerWorkdir, err := c.sandboxCreateForSpawn(ctx, owner, req.ParentChildID, childID, *req.Sandbox)
		if err != nil {
			return protocol.SpawnResponseData{}, err
		}
		machine := executor.Labels["machine"]
		if machine == "" {
			machine = sandboxSpawnMachineName(rowID)
		}
		req.ExecutorRef = ""
		req.ExecutorSelector = "machine=" + machine
		// The child's cwd is a path in the CONTAINER, not on the daemon host.
		// agent_spawn (and the operator CLI) default cwd to the PARENT's — a host
		// path the executor, running inside the container, cannot see, so every
		// workspace tool would fail at Provision. Rewrite it to the sandbox's
		// resolved workdir (the container root when the spec names none), which
		// is the only vocabulary the sandbox executor shares with us. Nothing
		// else rewrites Cwd; an inherited host path would otherwise reach the
		// executor verbatim.
		req.Cwd = containerWorkdir
		sandboxOwnedChild = childID
		defer func() {
			if sandboxOwnedChild == "" {
				return
			}
			// Every failure below means the child never ran, so its sandbox goes
			// with it rather than waiting for the reaper. WithoutCancel: teardown
			// must still run when the spawn's own context was cancelled.
			c.removeSandboxesOwnedBy(context.WithoutCancel(ctx), sandboxOwnedChild)
		}()
	}

	env, vals := c.buildEnv(req, childID, c.socketPath)
	// claudeEnv is only meaningful for the local-subprocess path — once
	// claudeRunner returns a non-nil daraja-backed Runner, child.Spawn never
	// reads spec.Env at all (see the "if runner != nil" branch below), so
	// building it for the executor-routed case is dead weight that invites a
	// future reader to think it does something.
	if req.Kind == protocol.KindClaude && c.execPoolConn == nil {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir, vals)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	runner, err := c.agentRunner(req, childID, false, ownerName, owner.UserID, nil)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
		}
	}
	if req.Kind == protocol.KindClaude {
		if bin, err = resolveClaudeBinaryIfNeeded(req, runner); err != nil {
			return protocol.SpawnResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrSpawnFailed,
				Message: "spawn plan: " + err.Error(),
			}
		}
	}

	spec := child.SpawnSpec{
		ChildID:     childID,
		Cwd:         req.Cwd,
		PiBinary:    bin,
		Argv:        argv,
		Env:         env,
		EnvOverride: req.EnvOverride,
		Provider:    prov,
		Runner:      runner,
	}
	spec.NativeSink, spec.OnMeta, spec.OnSubagent = c.childHooks(childID)
	// A script child's raw output is published as coalesced durable
	// ScriptOutput events (script_output.go). Every other kind leaves the
	// hook nil — its output keeps the old paths and nothing else changes.
	if req.Kind == protocol.KindScript {
		spec.OnScriptOutput = c.scriptOutputHook(childID)
	}
	if runner != nil {
		// The agent kind's argv is parsed into RuntimeOptions above, not
		// executed; leave PiBinary/Argv empty so nothing accidentally execs it.
		spec.PiBinary = ""
		spec.Argv = nil
	}

	now := time.Now()

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Register the child in the ChildManager immediately after spawn, so any
	// watcher of the spawned announcement (below) finds the child ready in
	// the manager. monitorChild is still started after Idle().
	c.cm.Add(childID, ch)

	// Build initial labels: user-supplied labels (already validated) plus
	// static auto-labels. Model/provider labels are added after Idle() in
	// activateLiveChild once pi reports its resolved model.
	initLabels := copyLabels(req.Labels)
	if initLabels == nil {
		initLabels = make(map[string]string)
	}
	// ownerName was attested above, before agentRunner ran.
	if ownerName != "" {
		initLabels["owner"] = ownerName
	}
	initLabels["rafiki/cwd"] = req.Cwd
	initLabels["rafiki/pid"] = strconv.Itoa(ch.PID())
	initLabels["rafiki/kind"] = spawnKindLabel(req.Kind)
	// The resolved routing spec mirrors the session's stored value (req.Routing
	// below), the way rafiki/model mirrors the session's model. Daemon-written
	// at spawn only; resume keeps the label the original spawn set.
	if req.Routing != "" {
		initLabels["rafiki/routing"] = req.Routing
	}
	stampPresetLabels(initLabels, presetRec)
	if req.ConfigDir != "" {
		initLabels["rafiki/config_dir"] = req.ConfigDir
	}
	if req.ResumedFromSession != "" {
		initLabels["rafiki/resumed-from-session"] = req.ResumedFromSession
	}
	if parentLabel != "" {
		initLabels[childstore.LabelParent] = parentLabel
		initLabels[childstore.LabelRoot] = rootLabel
	}
	// Merge the binding agentRuntimeOptions made before this record existed.
	// A binding made AFTER it exists goes straight to the store — see NoteBinding.
	//
	// Guarded on non-empty rather than written unconditionally: a claude
	// child's daraja binding (stashDarajaBinding) has no workspaceID at all —
	// claude has no workspace concept — and writing rafiki/workspace="" would
	// be misleading noise on every daraja-hosted claude child. Every fundi
	// tool-binding call into this stash always sets workspaceID/executorID/
	// mode, so this guard is a no-op for that path.
	if wl, ok := c.takeWorkspaceLabels(childID); ok {
		if wl.workspaceID != "" {
			initLabels["rafiki/workspace"] = wl.workspaceID
		}
		if wl.executorID != "" {
			initLabels["rafiki/executor"] = wl.executorID
		}
		if wl.mode != "" {
			initLabels["rafiki/workspace-mode"] = wl.mode
		}
		if wl.executorState != "" {
			initLabels["rafiki/executor-state"] = wl.executorState
		}
		if wl.pgid != 0 {
			initLabels["rafiki/daraja-pgid"] = strconv.Itoa(int(wl.pgid))
		}
	}

	// FIX 5: Insert a minimal record at StatusSpawning immediately after the
	// process is confirmed running. A crash between exec and Idle() would
	// otherwise leave an orphan pi process with no persisted record.
	sess := &childstore.Session{
		ChildID:      childID,
		OwnerUserID:  owner.UserID,
		PID:          ch.PID(),
		Status:       protocol.StatusSpawning,
		Name:         req.Name,
		Cwd:          req.Cwd,
		Kind:         req.Kind,
		ConfigDir:    req.ConfigDir,
		Provider:     req.Provider,
		Model:        req.Model,
		Thinking:     req.Thinking,
		StartedAt:    now,
		LastActivity: now,
		Labels:       initLabels,

		NoSession:          req.NoSession,
		SessionDir:         req.SessionDir,
		ResumeSession:      req.ResumeSession,
		ForkSession:        req.ForkSession,
		Tools:              splitComma(req.Tools),
		NoTools:            req.NoTools,
		NoBuiltinTools:     req.NoBuiltinTools,
		Routing:            req.Routing,
		Extensions:         req.Extensions,
		NoExtensions:       req.NoExtensions,
		Skills:             req.Skills,
		NoSkills:           req.NoSkills,
		SkillsDirs:         req.SkillsDirs,
		MCPConfig:          req.MCPConfig,
		MCPServers:         req.MCPServers,
		NoMCP:              req.NoMCP,
		PromptTemplates:    req.PromptTemplates,
		NoPromptTemplates:  req.NoPromptTemplates,
		Themes:             req.Themes,
		NoThemes:           req.NoThemes,
		NoContextFiles:     req.NoContextFiles,
		SystemPrompt:       req.SystemPrompt,
		AppendSystemPrompt: req.AppendSystemPrompt,
		Prefill:            req.Prefill,
		Verbose:            req.Verbose,
		PiBinary:           bin,
		ExtraArgs:          req.ExtraArgs,
		RecordRequests:     req.RecordRequests,
		ExecutorSelector:   req.ExecutorSelector,
		SkipDerivedIndex:   req.SkipDerivedIndex,
		WorkspaceMode:      req.WorkspaceMode,
		MaxDepth:           grantedDepth(req, childDepthFor(c.st, req.ParentChildID), resolveAbsoluteDepthCeiling()),
		MaxCost:            grantedCost(req),
		MaxChildren:        grantedChildren(req),
	}
	c.st.Insert(sess)

	if err := c.writeRecord(childID); err != nil {
		// FIX 5's minimal row must exist before anything else: the lineage
		// walk cannot cross a MISSING intermediate row, so a lost initial
		// insert would hide a whole sub-subtree from its ancestor's spend.
		// With a persistence store configured, a failed insert REFUSES the
		// spawn and unwinds every trace of it. A store-less controller keeps
		// the best-effort warn (there is no row to write in a DB-less test).
		if c.children != nil {
			c.abortSpawn(childID, ch)
			return protocol.SpawnResponseData{}, fmt.Errorf("spawn %s: %w", childID, err)
		}
		slog.Warn("write state record (spawning)", "childId", childID, "error", err)
	}

	// The child's own row is written (or the daemon has no store to write to).
	// From here the spawn cannot be unwound, so its sandbox must NOT be: clear
	// the failure-path teardown armed above.
	sandboxOwnedChild = ""

	// Assign the ledger row now that the child is admitted and registered.
	// Ordering is load-bearing: phase 05 refuses spawns for depth, cost and
	// concurrency, and every one of those refusals returns BEFORE this point,
	// so a refused spawn can never leave a row pointing at a child that never
	// started — no rollback, no compensating write.
	if req.Task != "" && c.tasks != nil && req.SpawnerConversationID != "" {
		assignCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if _, err := c.tasks.Assign(assignCtx, req.SpawnerConversationID, req.Task, childID); err != nil {
			// Best-effort, and deliberately not fatal: the child is already
			// running, and killing a healthy agent because a bookkeeping row
			// would not update trades a recoverable inconsistency for an
			// unrecoverable one. The warning is the record.
			slog.Warn("spawn: could not assign task to new child",
				"childId", childID, "task", req.Task, "error", err)
		} else {
			_, _ = c.st.SetLabels(childID, map[string]string{labelTaskHandle: req.Task}, nil)
		}
		cancel()
	}

	// Announce the spawn. Published on the SPAWNED child's bus, not the
	// parent's, so child_id and the delivery bus agree -- a subtree subscriber
	// sees it because the child is in the subtree, and every subject predicate
	// stays free of a lifecycle special case.
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId:  childID,
			ParentId: req.ParentChildID,
			Name:     req.Name,
		}},
	})

	// Post-Idle: name reconciliation, metadata update, status transition,
	// monitorChild start. Shared with the Resume/RespawnChild paths via
	// activateLiveChild (baseSnap==nil selects the Spawn-specific branch).
	// noSession/resumeSession/forkSession are unused by that branch (req is used
	// instead); pass zero values for clarity.
	return c.activateLiveChild(childID, ch, bin, req, nil, false, "", "")
}

// activateLiveChild handles the post-spawn registration sequence. It waits for
// the child to become idle, resolves provider/model from metadata, and then
// takes one of two paths based on whether baseSnap is nil.
//
// Fresh Spawn (baseSnap == nil): the caller has already inserted a
// StatusSpawning session, called cm.Add, and announced the spawn.
// This method performs name reconciliation (if req.Name is set), updates the
// existing session with post-Idle metadata, persists a record, emits the
// spawning→idle status transition, and starts monitorChild.
//
// Resume / RespawnChild (baseSnap != nil): the caller has already deleted the
// old exited session. noSession/resumeSession/forkSession are the session-
// continuity fields that differ between the two callers. This method builds
// the full Session from baseSnap with those overrides, inserts it, calls
// cm.Add, persists a record, announces the spawn, and starts monitorChild.
func (c *Controller) activateLiveChild(
	childID string,
	ch *child.Child,
	piBin string,
	req protocol.SpawnRequest,
	baseSnap *childstore.Snapshot,
	noSession bool,
	resumeSession string,
	forkSession string,
) (protocol.SpawnResponseData, error) {
	c.sessionIDEnded.Delete(childID)
	stalled := false
	select {
	case <-ch.Idle():
	case <-time.After(5 * time.Second):
		stalled = true
		slog.Warn("child did not become idle after spawn", "childId", childID)
	}

	meta := ch.Metadata()
	now := time.Now()

	if baseSnap == nil {
		// Fresh Spawn path. Perform name reconciliation before reading final
		// metadata so the returned SessionName reflects any rename.
		//
		// kind=script is excluded with claude: the reconciliation writes a
		// set_session_name frame to the child's stdin, and a script child has
		// no stdin protocol — the frame would be dropped by the provider (and
		// the rename could never apply anyway). The name the spawn asked for
		// is recorded on the row at insert time.
		if !stalled && req.Kind != protocol.KindClaude && req.Kind != protocol.KindScript && req.Name != "" && meta.SessionName != req.Name {
			renameID := "controller-rename-1"
			frame := []byte(fmt.Sprintf(`{"type":"set_session_name","id":%q,"name":%q}`, renameID, req.Name))
			if err := ch.Send(frame); err == nil {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if ch.Metadata().SessionName == req.Name {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if ch.Metadata().SessionName != req.Name {
					slog.Warn("set_session_name timed out", "childId", childID, "want", req.Name)
				}
			}
			meta = ch.Metadata()
		}

		provider, model := splitModel(meta.Model)
		if provider == "" {
			provider = req.Provider
		}
		if model == "" {
			model = req.Model
		}

		// Update the StatusSpawning session inserted before Idle() with the
		// session metadata that is only available after pi responds.
		_ = c.st.Update(childID, func(s *childstore.Session) {
			s.SessionID = meta.SessionID
			s.SessionFile = meta.SessionFile
			s.Provider = provider
			s.Model = model
			// Update model auto-labels now that pi has reported the resolved model.
			if s.Labels == nil {
				s.Labels = make(map[string]string)
			}
			if provider != "" {
				s.Labels["rafiki/provider"] = provider
			} else {
				delete(s.Labels, "rafiki/provider")
			}
			if model != "" {
				s.Labels["rafiki/model"] = model
			} else {
				delete(s.Labels, "rafiki/model")
			}
		})
		if err := c.writeRecord(childID); err != nil {
			slog.Warn("write state record (after idle)", "childId", childID, "error", err)
		}
		// Emit the spawning→idle transition — by draining what the child
		// actually recorded, not by asserting the pair, so this cannot disagree
		// with the state machine. handleStatusChange also fixes the byStatus
		// index that Insert left at StatusSpawning.
		//
		// Draining HERE, before monitorChild starts, is what keeps the status
		// visible in the store by the time Spawn returns.
		//
		// The old `if !stalled` guard is gone because the drain emits exactly
		// what the state machine recorded, which is right in BOTH stalled cases:
		//
		//   - Nothing was recorded (the usual stall: the child answered nothing).
		//     The drain emits nothing and the record stays spawning, as before.
		//   - Something WAS recorded despite the stall. This is reachable: for pi
		//     only response.get_state sets FirstResponse, while agent_start
		//     transitions to streaming unconditionally — so a child that streams
		//     without ever answering the readiness probe is stalled AND has
		//     recorded spawning→streaming. Emitting it is the fix, not a
		//     regression: the old guard suppressed that event, and monitorChild's
		//     initial `lastStatus := ch.Status()` sample already read streaming,
		//     so the store sat at spawning for the child's whole life.
		//
		// A child that reaches idle just after the timeout expires is likewise
		// now reported correctly rather than left as spawning.
		c.drainChildStatus(childID, ch)

		go c.monitorChild(childID, ch)

		return protocol.SpawnResponseData{
			ChildID:     childID,
			SessionID:   meta.SessionID,
			SessionFile: meta.SessionFile,
			Model:       joinModel(provider, model),
		}, nil
	}

	// Resume / RespawnChild path.
	snap := *baseSnap
	provider, model := splitModel(meta.Model)
	if provider == "" {
		provider = snap.Provider
	}
	if model == "" {
		model = snap.Model
	}

	// Discard the transitions the new process made while starting up, rather than
	// emitting them — and do it BEFORE the ch.Status() read that populates the
	// record below. This path inserts its record already post-idle, so no
	// subscriber ever saw the resumed child as spawning, and announcing
	// spawning→idle after the spawned announcement below would describe a
	// transition none of them could have observed.
	//
	// The ORDER is the point: draining after the Status() read leaves a window
	// where a transition landing in between is both discarded AND absent from
	// the inserted record — the store would say idle while the state machine
	// said streaming, with no event to correct it until the next transition.
	// Draining first means anything that arrives after it stays queued, wakes
	// monitorChild, and is delivered. Practically unreachable (a just-resumed pi
	// child emits nothing unprompted) but it is the same class of bug as the one
	// this queue exists to fix.
	//
	// This is the one place a transition is deliberately dropped, and it drops
	// nothing a consumer is owed.
	ch.DrainTransitions()

	// Recompute auto-labels from the snapshot's user labels with fresh pid/model.
	resumeLabels := copyLabels(snap.Labels)
	if resumeLabels == nil {
		resumeLabels = make(map[string]string)
	}
	resumeLabels["rafiki/cwd"] = snap.Cwd
	resumeLabels["rafiki/pid"] = strconv.Itoa(ch.PID())
	resumeLabels["rafiki/kind"] = spawnKindLabel(snap.Kind)
	delete(resumeLabels, "rafiki/session-error")
	if snap.ConfigDir != "" {
		resumeLabels["rafiki/config_dir"] = snap.ConfigDir
	}
	if provider != "" {
		resumeLabels["rafiki/provider"] = provider
	} else {
		delete(resumeLabels, "rafiki/provider")
	}
	if model != "" {
		resumeLabels["rafiki/model"] = model
	} else {
		delete(resumeLabels, "rafiki/model")
	}

	// A claude child is silent until prompted, so a freshly resumed process has
	// reported no session id yet. Keep the one it was resumed from: the next
	// relaunch before the first turn would otherwise have no --resume token and
	// start a brand-new conversation under the same child.
	sessionID := meta.SessionID
	if sessionID == "" {
		sessionID = snap.SessionID
	}

	sess := &childstore.Session{
		ChildID:            childID,
		OwnerUserID:        snap.OwnerUserID,
		PID:                ch.PID(),
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
		Provider:           provider,
		Model:              model,
		Thinking:           snap.Thinking,
		SessionID:          sessionID,
		SessionFile:        meta.SessionFile,
		Status:             ch.Status(),
		StartedAt:          now,
		LastActivity:       now,
		NoSession:          noSession,
		SessionDir:         snap.SessionDir,
		ResumeSession:      resumeSession,
		ForkSession:        forkSession,
		Labels:             resumeLabels,
		Tools:              snap.Tools,
		NoTools:            snap.NoTools,
		NoBuiltinTools:     snap.NoBuiltinTools,
		Routing:            snap.Routing, // resolved ONCE at the original spawn; never re-resolved
		Extensions:         snap.Extensions,
		NoExtensions:       snap.NoExtensions,
		Skills:             snap.Skills,
		NoSkills:           snap.NoSkills,
		SkillsDirs:         snap.SkillsDirs,
		MCPConfig:          snap.MCPConfig,
		MCPServers:         snap.MCPServers,
		NoMCP:              snap.NoMCP,
		PromptTemplates:    snap.PromptTemplates,
		NoPromptTemplates:  snap.NoPromptTemplates,
		Themes:             snap.Themes,
		NoThemes:           snap.NoThemes,
		NoContextFiles:     snap.NoContextFiles,
		SystemPrompt:       snap.SystemPrompt,
		AppendSystemPrompt: snap.AppendSystemPrompt,
		Prefill:            snap.Prefill,
		Verbose:            snap.Verbose,
		PiBinary:           piBin,
		ExtraArgs:          snap.ExtraArgs,
		RecordRequests:     snap.RecordRequests,
		ExecutorSelector:   snap.ExecutorSelector,
		SkipDerivedIndex:   snap.SkipDerivedIndex,
		WorkspaceMode:      snap.WorkspaceMode,
		MaxDepth:           snap.MaxDepth,
		MaxCost:            snap.MaxCost,
		MaxChildren:        snap.MaxChildren,
	}
	// Replace the old exited entry HERE, not back in Resume/RespawnChild
	// before the Idle() wait above. Deleting there left the child absent from
	// the store — and so from every list and get — for
	// as long as the new process took to answer, up to activateLiveChild's
	// full 5s stall timeout. That hole is reached on every daemon restart now
	// that recovery auto-resumes children, and it made a recovered child
	// vanish from `rafiki list` while it was in fact starting fine.
	//
	// Deleting immediately before the Insert keeps the original property that
	// motivated the late delete (a FAILED spawn returns before this point and
	// leaves the exited record intact) while closing the window: the store
	// goes from the old record straight to the new one. snap is a copy taken
	// before the spawn, so nothing here reads what Delete removes. The Delete
	// is still needed rather than relying on Insert's overwrite because the
	// secondary byName/byCwd/byStatus buckets are keyed by the OLD values.
	c.st.Delete(childID)
	c.st.Insert(sess)
	c.cm.Add(childID, ch)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record", "childId", childID, "error", err)
	}

	// Announce the resumed/respawned child. Published on the SPAWNED child's
	// bus, not the parent's, so child_id and the delivery bus agree -- a
	// subtree subscriber sees it because the child is in the subtree, and every
	// subject predicate stays free of a lifecycle special case.
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId:  childID,
			ParentId: snap.Labels[childstore.LabelParent],
			Name:     snap.Name,
		}},
	})

	go c.monitorChild(childID, ch)

	return protocol.SpawnResponseData{
		ChildID:     childID,
		SessionID:   sessionID,
		SessionFile: meta.SessionFile,
		Model:       joinModel(provider, model),
	}, nil
}

// resumeRequestFromSnapshot rebuilds a SpawnRequest from an exited child's
// snapshot. The resume token differs by kind: pi re-opens its session file via
// --session <path> (ResumeSession=SessionFile); claude re-attaches its stored
// conversation via --resume <id> (ResumeSession=SessionID).
func resumeRequestFromSnapshot(snap childstore.Snapshot, apiKey string) protocol.SpawnRequest {
	req := protocol.SpawnRequest{
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Provider:           snap.Provider,
		Model:              snap.Model,
		Thinking:           snap.Thinking,
		APIKey:             apiKey,
		NoSession:          snap.NoSession,
		SessionDir:         snap.SessionDir,
		ForkSession:        snap.ForkSession,
		Tools:              strings.Join(snap.Tools, ","),
		NoTools:            snap.NoTools,
		NoBuiltinTools:     snap.NoBuiltinTools,
		Routing:            snap.Routing, // resolved ONCE at the original spawn; resume never re-resolves
		Extensions:         snap.Extensions,
		NoExtensions:       snap.NoExtensions,
		Skills:             snap.Skills,
		NoSkills:           snap.NoSkills,
		SkillsDirs:         snap.SkillsDirs,
		MCPConfig:          snap.MCPConfig,
		MCPServers:         snap.MCPServers,
		NoMCP:              snap.NoMCP,
		PromptTemplates:    snap.PromptTemplates,
		NoPromptTemplates:  snap.NoPromptTemplates,
		Themes:             snap.Themes,
		NoThemes:           snap.NoThemes,
		NoContextFiles:     snap.NoContextFiles,
		SystemPrompt:       snap.SystemPrompt,
		AppendSystemPrompt: snap.AppendSystemPrompt,
		Prefill:            snap.Prefill,
		Verbose:            snap.Verbose,
		PiBinary:           snap.PiBinary,
		ExtraArgs:          snap.ExtraArgs,
		ExecutorSelector:   snap.ExecutorSelector,
		SkipDerivedIndex:   snap.SkipDerivedIndex,
		WorkspaceMode:      snap.WorkspaceMode,
		RecordRequests:     snap.RecordRequests,
		MaxDepth:           &snap.MaxDepth,
		MaxCost:            &snap.MaxCost,
		MaxChildren:        &snap.MaxChildren,
	}
	if snap.Kind == protocol.KindClaude {
		req.ResumeSession = snap.SessionID
	} else {
		req.ResumeSession = snap.SessionFile
	}
	if snap.Kind == protocol.KindFundi {
		// The fundi kind carries its provider inside the model id and
		// resolveSpawnPlan rejects a separate Provider outright - but the
		// snapshot stores the two halves split, because splitModel took them
		// apart at spawn time. Rejoin them or resume fails for every
		// agent-kind child.
		//
		// ResumeSession stays empty on purpose: an agent child has no pi
		// session file to reopen. It reattaches its stored conversation by
		// external ref instead - `rafikid agent --ref` defaults to
		// $RAFIKI_CHILD_ID, and resume reuses the same childID, so the
		// conversation is found without a resume token.
		req.Provider = ""
		req.Model = joinModel(snap.Provider, snap.Model)
	}
	return req
}

// checkClaudeResumeToken refuses a claude resume that would start a brand-new
// session under a child that already has captured history. A claude child
// resumes by --resume <session id>; with no id there is nothing to name, claude
// starts fresh, and its first request lands in the old child's conversation
// (the capture layer cannot tell the two histories apart). hasHistory is
// whether the child already has a captured conversation — a child that never
// reached a turn has none and resumes fresh harmlessly.
func checkClaudeResumeToken(snap childstore.Snapshot, hasHistory bool) error {
	if snap.Kind != protocol.KindClaude || snap.SessionID != "" || !hasHistory {
		return nil
	}
	return &connectapi.ControllerError{
		Code: protocol.ErrNotResumable,
		Message: "claude child has a captured conversation but no recorded session id, so a resume would " +
			"start a new session inside the old conversation; forget it and spawn a new child instead",
	}
}

// childClaimSet is a per-childID mutual-exclusion set guarding the
// check-then-act window shared by Controller.Resume and
// Controller.RespawnChild: both read the exited snapshot, fork a real OS
// process (child.Spawn — real wall-clock time, up to a 5s idle-wait), and
// only then delete-and-replace the store record for the same childID. Two
// concurrent callers for the same childID (a client retry, two attached
// clients, or a resume racing an intercepted new_session/switch_session) can
// both pass the status check before either mutates the record, producing two
// live processes sharing one ref.
//
// A map[string]*sync.Mutex was considered and rejected: entries would have to
// live forever, because a mutex can never be safely deleted while another
// goroutine might be about to Lock it — so the map would grow by one entry
// for every childID ever resumed or respawned over the daemon's lifetime.
// A claim set instead only ever holds entries for IDs currently in flight:
// membership *is* the state, so release (delete) is always safe — "absent"
// and "never claimed" are indistinguishable to any other goroutine, so a
// delete can never race with a concurrent locker the way freeing a live
// mutex could. The set's size is bounded by concurrently in-flight
// resumes/respawns, not by history.
//
// The mutex here is a leaf lock: every method takes it, does an O(1) map
// operation, and releases it before returning. It is never held while
// calling into c.st, c.cm, or child.Spawn, so it cannot participate in any
// lock-ordering cycle with the store, ChildManager, or connsMu.
type childClaimSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

// tryClaim attempts to claim id for the calling goroutine. It returns true
// iff this call now has exclusive ownership of id; a false return means
// another Resume/RespawnChild for the same id is already in flight and the
// caller must not proceed (report a clear error to its caller instead of
// blocking — blocking would just turn a client bug/retry into a hang for the
// duration of a spawn).
func (s *childClaimSet) tryClaim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = make(map[string]struct{})
	}
	if _, busy := s.ids[id]; busy {
		return false
	}
	s.ids[id] = struct{}{}
	return true
}

// release relinquishes a claim taken by tryClaim. Must be called exactly
// once per successful tryClaim, on every return path — callers use `defer`
// immediately after a successful tryClaim so release runs on every error
// return and on panic unwind, never just the success path.
func (s *childClaimSet) release(id string) {
	s.mu.Lock()
	delete(s.ids, id)
	s.mu.Unlock()
}

// isClaimed reports whether id has a resume/respawn in flight. The stored
// snapshot reads exited for the child's whole claimed window (recoverOne, and
// Resume/RespawnChild's own check-then-act sequence, both write that status
// before the claim is taken and only replace it once the new engine is fully
// live) — so this is what lets a caller tell "exited" from "about to be
// live again" apart. See validateSendTarget's use.
func (s *childClaimSet) isClaimed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}

// resumeOwnerUserID resolves the owner's user id for a resumed child.
//
// snap.OwnerUserID is authoritative when present: it was written at the
// child's original spawn by a daemon new enough to carry the column, and the
// upsert's COALESCE keeps it alive across later resume upserts. Older rows
// carry only the owner's USERNAME in Labels["owner"] — resolved through the
// users store here, active rows only, because a tombstone must never receive
// an attribution (see users.Store.LookupUsername).
//
// A failed resolution is NOT fatal: this is a best-effort recovery of a name
// an older daemon wrote, and refusing a resume over attribution would trade a
// working child for a bookkeeping field. The child continues unattributed,
// exactly the shape an anonymous spawn has always been. The same resolution
// feeds quota_status (agentRuntimeOptions), which is why a resumed child's
// quota_status returns real data after a daemon restart instead of always
// reporting "no data captured".
func (c *Controller) resumeOwnerUserID(ctx context.Context, childID string, snap childstore.Snapshot) string {
	if snap.OwnerUserID != "" {
		return snap.OwnerUserID
	}
	name := snap.Labels["owner"]
	if name == "" || c.users == nil {
		return ""
	}
	id, err := c.users.LookupUsername(ctx, name)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			slog.Warn("resume: owner label does not name an active user; continuing unattributed",
				"child", childID, "owner", name)
		} else {
			slog.Warn("resume: owner lookup failed; continuing unattributed",
				"child", childID, "owner", name, "error", err)
		}
		return ""
	}
	return id
}

// resolveUsernameToUserID resolves a username to a conversations.users id for
// the pymodule pushers' owner attribution, using the same users-store lookup
// resumeOwnerUserID performs (active rows only — a tombstone must never
// receive an attribution). It returns (id, found, err): found=false with a
// NIL error means the label names no active user (empty name, no store, or
// users.ErrNotFound) — the git pusher reads that as the UNATTRIBUTED owner,
// the blob pusher as "push nothing". found=false with an ERROR means the
// store itself failed: a caller must skip on that, never read it as
// unattributed, or a store outage would hand anonymous sources to a user's
// executor.
func (c *Controller) resolveUsernameToUserID(ctx context.Context, username string) (string, bool, error) {
	if username == "" || c.users == nil {
		return "", false, nil
	}
	id, err := c.users.LookupUsername(ctx, username)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

func (c *Controller) Resume(ctx context.Context, childID string, apiKey string) (protocol.SpawnResponseData, error) {
	return c.resumeInternal(ctx, childID, apiKey, false)
}

// resumeClaimedWithAutoRecovery is the auto-recovery version of Resume: it
// sets AutoResume on the engine so the worker calls agentloop.Resume on
// startup (finalising any incomplete previous turn) before accepting inbound
// prompts. The caller already holds childID's claim (recoverOne, which claims
// before it starts the resume goroutine so the child never reads as plainly
// exited in between).
func (c *Controller) resumeClaimedWithAutoRecovery(ctx context.Context, childID string) (protocol.SpawnResponseData, error) {
	return c.resumeClaimed(ctx, childID, "", true)
}

// resumeInternal is the shared implementation of Resume and auto-recovery resume.
func (c *Controller) resumeInternal(ctx context.Context, childID string, apiKey string, autoResume bool) (protocol.SpawnResponseData, error) {
	// Claim childID for the whole check-then-act window below: from before
	// the exited-status check, through the child.Spawn fork, to after the
	// old exited record is replaced by activateLiveChild. See childClaimSet's
	// doc comment for why this is a set rather than a per-ID mutex, and for
	// the lock-ordering argument (this is always the outermost and
	// shortest-held lock in the call path).
	//
	// A losing concurrent caller gets ErrNotResumable immediately rather than
	// blocking: from its perspective the child genuinely is not resumable
	// right now (a resume for it is already in flight), and blocking for the
	// duration of a spawn would just convert a client bug/retry into a
	// confusing hang instead of an actionable error.
	if !c.spawnClaims.tryClaim(childID) {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "resume already in progress for child: " + childID,
		}
	}
	defer c.spawnClaims.release(childID)
	return c.resumeClaimed(ctx, childID, apiKey, autoResume)
}

// resumeClaimed is resumeInternal's body for a caller that already holds
// childID's spawnClaims claim; it does not release it.
func (c *Controller) resumeClaimed(ctx context.Context, childID string, apiKey string, autoResume bool) (protocol.SpawnResponseData, error) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "child is not exited (status: " + string(snap.Status) + ")",
		}
	}

	kind := snap.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	if kind == protocol.KindScript {
		// A script's exit IS its result: re-running the pymodule from a resume
		// would silently start the work over, and the childstore row carries
		// no ScriptSpec to rebuild the spawn from anyway. Re-spawn it.
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "script children cannot be resumed: a script's exit is its result; spawn it again",
		}
	}

	if kind == protocol.KindClaude {
		hasHistory := snap.SessionID == "" && c.conversationIDForChild(snap) != ""
		if err := checkClaudeResumeToken(snap, hasHistory); err != nil {
			return protocol.SpawnResponseData{}, err
		}
		slog.Info("resuming claude child", "childId", childID, "resumeSession", snap.SessionID, "autoResume", autoResume)
	}

	req := resumeRequestFromSnapshot(snap, apiKey)

	env, vals := c.buildEnv(req, childID, c.socketPath)
	// claudeEnv is only meaningful for the local-subprocess path — once
	// claudeRunner returns a non-nil daraja-backed Runner, child.Spawn never
	// reads spec.Env at all (see the "if runner != nil" branch below), so
	// building it for the executor-routed case is dead weight that invites a
	// future reader to think it does something.
	if kind == protocol.KindClaude && c.execPoolConn == nil {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir, vals)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	// The owner was already attested at the child's original spawn (see
	// Spawn's ownerName computation) and lives on in snap.Labels; a resume
	// re-derives nothing, it reuses that value so chooseExecutor's admission
	// check (for an ExecutorSelector carried over from snap) sees the same
	// owner the executor's row was minted to admit. The owner's USER ID comes
	// from resumeOwnerUserID: snap.OwnerUserID when the row carries it, else
	// the Labels["owner"] username resolved through the users store — which
	// is what attributes the resumed conversation and feeds quota_status.
	runner, err := c.agentRunner(req, childID, autoResume, snap.Labels["owner"], c.resumeOwnerUserID(ctx, childID, snap), &snap)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
		}
	}
	if req.Kind == protocol.KindClaude {
		if bin, err = resolveClaudeBinaryIfNeeded(req, runner); err != nil {
			return protocol.SpawnResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrSpawnFailed,
				Message: "spawn plan: " + err.Error(),
			}
		}
	}

	spec := child.SpawnSpec{
		ChildID:     childID,
		Cwd:         req.Cwd,
		PiBinary:    bin,
		Argv:        argv,
		Env:         env,
		EnvOverride: req.EnvOverride,
		Provider:    prov,
		Runner:      runner,
	}
	spec.NativeSink, spec.OnMeta, spec.OnSubagent = c.childHooks(childID)
	// A script child's raw output is published as coalesced durable
	// ScriptOutput events (script_output.go). Every other kind leaves the
	// hook nil — its output keeps the old paths and nothing else changes.
	if req.Kind == protocol.KindScript {
		spec.OnScriptOutput = c.scriptOutputHook(childID)
	}
	if runner != nil {
		// The agent kind's argv is parsed into RuntimeOptions above, not
		// executed; leave PiBinary/Argv empty so nothing accidentally execs it.
		spec.PiBinary = ""
		spec.Argv = nil
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// activateLiveChild (baseSnap != nil path): waits for Idle, replaces the
	// old exited entry, builds the full Session from snap with Resume's
	// session-continuity values, inserts it,
	// adds to cm, persists, announces the spawn, starts monitorChild.
	return c.activateLiveChild(childID, ch, bin, protocol.SpawnRequest{}, &snap,
		snap.NoSession, snap.SessionFile, snap.ForkSession)
}

// RespawnChild kills the existing child (the caller must have already done so
// and waited for StatusExited) and starts a replacement with a session
// override. sessionPath controls session continuity:
//
//   - "" (empty): fresh pi session — no --session flag, pi creates a new one.
//   - non-empty: pi resumes that specific session file via --session <path>.
//
// All other spawn configuration is inherited from the child's persisted
// snapshot. The childID is preserved across the respawn. This is the
// implementation path for new_session and switch_session interception (spec §5.1).
//
// Shares Controller.spawnClaims with Resume: RespawnChild has the identical
// check-then-act-around-a-fork shape (read exited status, child.Spawn, then
// delete-and-replace the record), reached via a concurrent Send
// {new_session|switch_session} on the same childID (see
// handleInterceptedSend), and a shared claim set also blocks the cross-path
// case of a resume racing an intercepted respawn for the same exited
// childID. See childClaimSet's doc comment for the full rationale.
func (c *Controller) RespawnChild(ctx context.Context, childID, sessionPath string) (protocol.SpawnResponseData, error) {
	if !c.spawnClaims.tryClaim(childID) {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "respawn already in progress for child: " + childID,
		}
	}
	defer c.spawnClaims.release(childID)

	snap, ok := c.st.Get(childID)
	if !ok {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "child is not exited (status: " + string(snap.Status) + ")",
		}
	}

	// Reconstruct the same SpawnRequest Resume feeds to resolveSpawnPlan
	// (rather than a separately hand-built one) so every kind — not just
	// "pi" — resolves through the correct dispatch branch, and so no field
	// (e.g. Kind itself, agent's split Provider/Model) can drift between the
	// two respawn paths. Then apply RespawnChild's own session-continuity
	// override: fresh start (no --no-session, no --fork), sessionPath
	// non-empty adds --session <sessionPath>.
	req := resumeRequestFromSnapshot(snap, "")
	req.NoSession = false
	req.ResumeSession = sessionPath
	req.ForkSession = ""

	kind := snap.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	// Legacy rows written before the empty-kind default existed carry Kind
	// "" in their snapshot; resumeRequestFromSnapshot copies it verbatim.
	// agentRunner's switch has no case for "" — it would route a fundi argv
	// through the owner-less `rafikid fundi` subprocess path — so the
	// resolved default rides on the request too, mirroring Spawn.
	req.Kind = kind

	env, vals := c.buildEnv(req, childID, c.socketPath)
	// See Resume's identical guard: claudeEnv is dead weight once
	// claudeRunner returns a daraja-backed Runner (child.Spawn never reads
	// spec.Env in that case).
	if req.Kind == protocol.KindClaude && c.execPoolConn == nil {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir, vals)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	// See Resume's identical call: the owner was attested at the child's
	// original spawn and lives on in snap.Labels; the USER ID comes from
	// resumeOwnerUserID (snap.OwnerUserID, else the label resolved through
	// the users store).
	runner, err := c.agentRunner(req, childID, false, snap.Labels["owner"], c.resumeOwnerUserID(ctx, childID, snap), &snap)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
		}
	}
	if req.Kind == protocol.KindClaude {
		if bin, err = resolveClaudeBinaryIfNeeded(req, runner); err != nil {
			return protocol.SpawnResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrSpawnFailed,
				Message: "spawn plan: " + err.Error(),
			}
		}
	}

	spec := child.SpawnSpec{
		ChildID:  childID,
		Cwd:      req.Cwd,
		PiBinary: bin,
		Argv:     argv,
		Env:      env,
		Provider: prov,
		Runner:   runner,
	}
	spec.NativeSink, spec.OnMeta, spec.OnSubagent = c.childHooks(childID)
	// A script child's raw output is published as coalesced durable
	// ScriptOutput events (script_output.go). Every other kind leaves the
	// hook nil — its output keeps the old paths and nothing else changes.
	if req.Kind == protocol.KindScript {
		spec.OnScriptOutput = c.scriptOutputHook(childID)
	}
	if runner != nil {
		// The agent kind's argv is parsed into RuntimeOptions above, not
		// executed; leave PiBinary/Argv empty so nothing accidentally execs it.
		spec.PiBinary = ""
		spec.Argv = nil
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return protocol.SpawnResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// activateLiveChild (baseSnap != nil path): waits for Idle, replaces the
	// old exited entry, builds the full Session from snap with RespawnChild's
	// session-continuity values (fresh
	// start: noSession=false, resumeSession=sessionPath, forkSession=""),
	// inserts, adds to cm, persists, announces the spawn, starts monitorChild.
	return c.activateLiveChild(childID, ch, bin, protocol.SpawnRequest{}, &snap,
		false, sessionPath, "")
}

// exitPersistDeadline bounds the wait for handleChildExit to finish after a
// child process has been reaped.
//
// It is a backstop, not a timeout anyone should hit in the common case, but
// "just MarkExited and cm.Remove" stopped being true once handleChildExit
// grew synchronous durable I/O on the critical path ahead of cm.Remove:
// notifySubagentSettled's eventbuf Push (acceptTimeout, 5s), releaseInboxOnExit's
// Queue.Reset (10s), the task orphan sweep (3s), and releaseLease's
// LeaseStore.Release (5s) — writeRecordLastStatus's own DB upsert (5s) runs
// even earlier. Under real DB latency those can stack to ~28s worst case; 2s
// was already too short to survive a single one of them, let alone all four,
// which is what let a completed Kill still read "shutting_down" — the exact
// failure CLAUDE.md records as misdiagnosed as a timing flake for months.
// Raised with headroom above that worst case rather than to the sum exactly,
// since a slow-but-live database should still resolve within the wait.
const exitPersistDeadline = 45 * time.Second

// waitForChildRemoval blocks until handleChildExit has finished for childID,
// or the deadline passes. It reports whether the child was removed.
//
// cm.Remove is the final step of handleChildExit, so a child's absence from
// the manager is the observable signal that MarkExited has already run and
// the store snapshot therefore reports "exited". Every caller that reports a
// kill as complete, or that touches on-disk state afterwards, has to wait
// for this. Two of them were each spinning on it with a private copy of the
// loop and their own comment; Kill was missing it entirely.
func waitForChildRemoval(cm *ChildManager, childID string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, alive := cm.Get(childID); !alive {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (c *Controller) Kill(ctx context.Context, childID string, shutdownTimeout, killTimeout time.Duration) (protocol.KillResponseData, error) {
	// Revoke the daraja's reconnect credential so a dead child cannot
	// re-authenticate on a later port scan or stale-connection replay. The
	// belt dies with the credential: a killed child is settled by the kill
	// ladder's Shutdown RPC, never from the belt, so leaving its events would
	// only leak them (≤8 MiB per child, unbounded across children) and a
	// later Watch could revive frames of a child that no longer exists.
	if c.darajaReg != nil {
		c.darajaReg.Forget(childID)
	}
	if c.darajaPool != nil {
		c.darajaPool.DropReplay(childID)
	}

	// An operator's kill must not be undone by a pending rate-limit resume:
	// drop the watch here, where the intent is known, not in handleChildExit
	// (a passive stdin close makes the same death read as the child's own —
	// fake-pi and claude alike exit 0 on EOF). Also covers the native-child
	// early return below, which never reaches the shutdown sequence.
	c.rateWatch.drop(childID)
	// A pending recovery resume must not fire for a child the operator killed
	// while it waited for its executor.
	c.dropPendingResume(childID)

	// A synthetic thread child has no process, so every lookup below misses and
	// this used to answer "child not found" for a child the operator could see
	// in the rail. Ending one is a store write; exitNativeChild does it and
	// emits the same exit events a real one does.
	if snap, ok := c.st.Get(childID); ok && snap.Native {
		if snap.Status == protocol.StatusExited {
			return protocol.KillResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrChildExited,
				Message: "child has already exited",
			}
		}
		c.exitNativeChild(childID)
		code := 0
		return protocol.KillResponseData{ExitCode: &code}, nil
	}

	ch, ok := c.cm.Get(childID)
	if !ok {
		if snap, ok2 := c.st.Get(childID); ok2 && snap.Status == protocol.StatusExited {
			return protocol.KillResponseData{}, &connectapi.ControllerError{
				Code:    protocol.ErrChildExited,
				Message: "child has already exited",
			}
		}
		return protocol.KillResponseData{}, &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	// Drive the SM to shutting_down so every watcher sees the
	// transition before the graceful-shutdown sequence begins (spec §6.5).
	if changed, prev := ch.BeginShutdown(); changed {
		c.handleStatusChange(childID, protocol.StatusShuttingDown, prev)
	}

	shutdown := durOrDefault(shutdownTimeout, 180*time.Second)
	kill := durOrDefault(killTimeout, 30*time.Second)

	res, err := ch.Shutdown(shutdown, kill)
	if err != nil {
		return protocol.KillResponseData{}, fmt.Errorf("shutdown: %w", err)
	}

	// ch.Shutdown returns when the child PROCESS is reaped, but the status
	// only becomes "exited" once handleChildExit calls MarkExited, and that
	// runs asynchronously on monitorChild's goroutine. Returning here left a
	// window in which a client that killed a child and immediately read its
	// status saw "shutting_down": the kill had succeeded, but the state the
	// caller can observe said otherwise, so the operation reported itself
	// complete before it was. Forget and ShutdownAllChildren already wait on
	// exactly this; Kill was the one caller that did not.
	waitForChildRemoval(c.cm, childID, exitPersistDeadline)

	var exitCode *int
	if res.Signal == "" {
		code := res.ExitCode
		exitCode = &code
	}
	return protocol.KillResponseData{
		ExitCode:  exitCode,
		Signal:    res.Signal,
		Duration:  res.Duration,
		Escalated: res.Escalated,
		Abandoned: res.Abandoned,
	}, nil
}

// ShutdownAllChildren gracefully shuts down every live child concurrently.
// For each child it:
//  1. Calls BeginShutdown to drive the SM to shutting_down and emit the
//     status-change event to any connected subscribers.
//  2. Launches a goroutine that calls ch.Shutdown(perChildShutdown, perChildKill).
//
// ctx bounds the total wait. If it expires before all children exit the
// function returns ctx.Err() and logs a warning; the outstanding Shutdown
// goroutines continue and will SIGKILL the remaining children on their own
// schedule — they die via pipe-death otherwise.
//
// Per-child errors are logged and collected; all of them are returned as a
// joined error so the caller can decide whether to treat them as fatal.
func (c *Controller) ShutdownAllChildren(ctx context.Context, perChildShutdown, perChildKill time.Duration) error {
	// The daemon is dying. From here on, child exits must not persist rows:
	// recovery on the next daemon reads status, and a row saying "exited"
	// would keep a child that was merely stopped by its daemon's death from
	// ever resuming. The latch is one-way and single-writer — the daemon's own
	// shutdown sequence is the only production caller — so the CAS buys no
	// race safety (the atomic type already makes the latch torn-read-free and
	// a duplicate Store would be a harmless no-op); it marks the TRANSITION so
	// the log fires exactly once even if the sequence is entered twice (its
	// per-child goroutines can outlive an expired context), and gives any
	// future one-time work a first-flipper hook. Set before LiveIDs so an
	// exit racing the start of the sequence is covered too.
	if c.stopping.CompareAndSwap(false, true) {
		slog.Info("daemon stopping; child exits will no longer be persisted — rows keep their live statuses for restart recovery")
	}

	// The daemon is dying: pending rate-limit resumes go with it (the watch is
	// in-memory by design — see ratelimit_resume.go). Killing every timer here
	// also means a child killed by the shutdown cannot be prompted by a timer
	// that outlived the decision to stop it.
	c.rateWatch.dropAll()

	ids := c.cm.LiveIDs()
	if len(ids) == 0 {
		return nil
	}

	slog.Info("shutting down children", "count", len(ids))

	type result struct {
		id        string
		err       error
		abandoned bool
		duration  time.Duration
		escalated bool
	}
	done := make(chan result, len(ids))

	for _, id := range ids {
		id := id
		ch, ok := c.cm.Get(id)
		if !ok {
			// Already removed between LiveIDs() and Get(); count it as done.
			done <- result{id: id}
			continue
		}
		// Drive SM to shutting_down and publish the transition
		// before the Shutdown sequence begins (mirrors what Kill does).
		if changed, prev := ch.BeginShutdown(); changed {
			c.handleStatusChange(id, protocol.StatusShuttingDown, prev)
		}
		go func() {
			res, err := ch.Shutdown(perChildShutdown, perChildKill)
			// ch.Shutdown returns when the child *process* is reaped, but
			// handleChildExit — which persists the exit code/signal to the
			// state record — runs asynchronously in monitorChild's goroutine.
			// Wait for that goroutine to finish (signalled by cm.Remove)
			// before reporting this child done, so a racing daemon shutdown
			// doesn't close before exit info is persisted.
			deadline := time.Now().Add(perChildShutdown + perChildKill)
			for time.Now().Before(deadline) {
				if _, alive := c.cm.Get(id); !alive {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			done <- result{id: id, err: err, abandoned: res.Abandoned, duration: res.Duration, escalated: res.Escalated}
		}()
	}

	var errs []error
	remaining := len(ids)
	for remaining > 0 {
		select {
		case r := <-done:
			remaining--
			switch {
			case r.err != nil:
				slog.Warn("child shutdown error", "childId", r.id, "error", r.err, "duration", r.duration, "escalated", r.escalated)
				errs = append(errs, fmt.Errorf("child %s: %w", r.id, r.err))
			case r.abandoned:
				// Not an error — Shutdown did everything it could — but "shut
				// down" would be a lie: the goroutine is still in there.
				slog.Error("child abandoned rather than reaped; its execution context is leaked", "childId", r.id, "duration", r.duration, "escalated", r.escalated)
			default:
				slog.Info("child shut down", "childId", r.id, "duration", r.duration, "escalated", r.escalated)
			}
		case <-ctx.Done():
			slog.Warn("graceful shutdown deadline exceeded", "remaining", remaining)
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

// ownsChildRow reports whether this daemon may destroy a child's durable state.
//
// Child rows are shared, so loadChildren inserts every daemon's children into
// the local store as exited — including children that are alive on another
// daemon right now. Hard-deleting one of those rows destroys the durable state
// of a running child, and irrecoverably if its daemon crashes before its next
// writeRecord.
//
// A row with no owner label is ours: it predates the label, and refusing to
// forget it would strand it forever.
func (c *Controller) ownsChildRow(snap childstore.Snapshot) bool {
	if c.daemonID == "" {
		return true
	}
	owner := snap.Labels["rafiki/daemon"]
	return owner == "" || owner == c.daemonID
}

// abortSpawn unwinds a spawn whose initial record could not be persisted,
// leaving no half-spawned child behind. It is the teardown Close and Kill
// perform, minus the tombstone — there is no row to tombstone, which is
// exactly why the spawn is being refused. Any process already launched is
// terminated, both the in-memory store and the process map drop the child, and
// the executor/daraja/inbox state taken for it is released so nothing dangles.
func (c *Controller) abortSpawn(childID string, ch *child.Child) {
	if ch != nil {
		if _, err := ch.Shutdown(5*time.Second, 5*time.Second); err != nil {
			slog.Warn("abort spawn: shutdown", "childId", childID, "errorType", fmt.Sprintf("%T", err))
		}
	}
	c.st.Delete(childID)
	c.cm.Remove(childID)
	c.rateWatch.drop(childID)
	c.dropPendingResume(childID)
	c.forgetBoundExecutor(childID)
	if c.darajaReg != nil {
		c.darajaReg.Forget(childID)
	}
	if c.darajaPool != nil {
		c.darajaPool.DropReplay(childID)
	}
	c.dropInboxForForgotten(childID, "spawn aborted")
	// The per-child MCP secret is minted BEFORE the initial insert (buildEnv /
	// scriptRunner run before writeRecord), and forgetMCPToken normally runs
	// from handleChildExit — which never runs for a refused spawn, so the
	// credential would stay live until a later mint triggered sweepMCPTokens.
	c.forgetMCPToken(childID)
	// The script-output hook registers per-spawn state at spec-build time and
	// creates the coalescer lazily on the child's first line; both live until
	// handleChildExit takes them. A refused spawn has an output line's worth of
	// window (child.Spawn runs before the insert), so release them here too:
	// take drops the state entry, and Close stops the coalescer's goroutine.
	if co := c.takeScriptOutputCoalescer(childID); co != nil {
		co.Close()
	}
}

// Close finalizes an exited child: it leaves the in-memory store and its
// conversations.child row, and can never be resumed, reattached or continued
// again. Its TRANSCRIPT is not deleted — no foreign key references
// conversations.child, so conversation_message, event_log and conversation_turn
// all survive and stay readable through `rafiki history`.
//
// The verb is spelled Forget
// protocol is frozen (see docs/plans/2026-09-01-model-catalog-and-close-design.md
// §3.0), and its old clients must keep working regardless.
func (c *Controller) Close(childID string) error {
	// Wait for handleChildExit (running on monitorChild's goroutine) to finish
	// before we touch on-disk state. Two races are possible when forget arrives
	// immediately after a kill:
	//
	//  1. MarkExited hasn't run yet — snap.Status is still streaming/idle, not
	//     "exited".  cm still holds the child.
	//  2. MarkExited has run (status=exited) but cm.Remove hasn't yet — the
	//     child is still in cm.  Without this wait, our delete can race with
	//     writeRecord's atomic-rename: writeRecord's .tmp is in-progress when
	//     Close runs os.Remove(.json) — finds nothing — then writeRecord
	//     completes the rename, leaving an orphan .json that rafiki ls picks
	//     up on the next daemon restart via loadOrphans.
	//
	// Both resolve when cm.Remove(childID) runs (the final step of
	// handleChildExit), so we spin on that.  While we wait, re-read the store
	// snapshot: the initial read may have preceded MarkExited (race 1).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := c.cm.Get(childID); !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap, ok := c.st.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrNotFound, Message: "child not found: " + childID}
	}
	if snap.Status != protocol.StatusExited {
		return &connectapi.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"}
	}

	// A synthetic thread child exists only in the in-memory store: no durable
	// row (EnsureThreadChild never calls writeRecord), no inbox, no daraja, no
	// executor binding, no log dump. Everything below would be a round trip
	// asking a database to forget something it was never told, so the store
	// delete IS the close.
	if snap.Native {
		c.st.Delete(childID)
		return nil
	}

	// Persist the child's current snapshot BEFORE forgetting it: the upsert
	// carries its session_id/conversation_id to conversations.child (and clears
	// closed_at), and the tombstone below stamps closed_at again. A child whose
	// lineage could not be recorded must not silently drop out of its
	// ancestors' spend, so a persist failure FAILS CLOSED: the child stays in
	// the live store, is not tombstoned, and the error is returned. This runs
	// before the daraja revocation so a failed persist leaves nothing torn.
	//
	// Only for a row THIS daemon owns: a recovered row belonging to another
	// daemon is not ours to write (the upsert would stamp our daemon label and
	// our exited status onto a live child's row), and we do not tombstone it
	// below for the same reason.
	owns := c.ownsChildRow(snap)
	if owns {
		if err := c.writeRecord(childID); err != nil {
			// A persist that keeps failing would otherwise leave no trace once the
			// error is discarded: the caller sees one refusal, and a sweep's
			// best-effort Close sees nothing at all. Log the child id and the error
			// TYPE only — never the message, which can name infrastructure — then
			// still return the wrapped error.
			slog.Warn("close: child not persisted", "childId", childID, "errorType", fmt.Sprintf("%T", err))
			return fmt.Errorf("close %s: %w", childID, err)
		}
	}

	// Revoke the daraja's ability to reconnect — the row is going away.
	// Must run before st.Delete; once the row is gone the OnDisconnect handler
	// (fired if the daraja was still connected) has no child to label. The
	// belt goes with the row: a forgotten child can never be settled from it,
	// and a later Watch must not revive its frames.
	if c.darajaReg != nil {
		c.darajaReg.Forget(childID)
	}
	if c.darajaPool != nil {
		c.darajaPool.DropReplay(childID)
	}

	c.st.Delete(childID)
	// A forgotten child is never coming back: nothing must resurrect it, so
	// its rate-limit watch goes with the row. CloseAllExited, the other
	// forget site, drops the same way.
	c.rateWatch.drop(childID)
	// A pending recovery resume must not fire for a child that was closed
	// while it waited for its executor.
	c.dropPendingResume(childID)
	// Its synthetic thread children go with it: they carry a parent label, so
	// leaving them would strand them at the top of the rail pointing at a
	// session that no longer exists. Their transcripts survive, exactly as this
	// child's does.
	c.closeNativeChildrenOf(childID)
	// Its watcher entry goes with it: nothing else will poll a binding whose
	// child cannot receive the news.
	c.forgetBoundExecutor(childID)
	// After the store delete, so a message accepted concurrently cannot slip
	// in behind the drop: validateSendTarget refuses an unknown child.
	//
	// Gated on ownsChildRow, the SAME authority the row-delete below already
	// uses — not holdsLease, which is false for every legitimately forgotten
	// child (its lease was already released on exit) and would disable this
	// drop entirely. loadChildren inserts every daemon's children into this
	// daemon's local store as exited, including ones genuinely alive on
	// another daemon right now; recoverOne's placeholder StatusExited write
	// (before its async resume goroutine even runs) means a Close landing in
	// that window read this daemon's own local snapshot and, unguarded, would
	// Drop — a full, terminal delete, worse than the reset-to-pending
	// recoverOne's own resume path can cause — another daemon's live child's
	// queue.
	if owns {
		c.dropInboxForForgotten(childID, "child forgotten")
	}
	if c.children != nil && owns {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.children.Delete(ctx, childID); err != nil {
			slog.Warn("delete child row", "childId", childID, "error", err)
		}
		if err := c.stampConversationsClosed(ctx, childID); err != nil {
			slog.Warn("stamp conversation closed", "childId", childID, "error", err)
		}
		// A conversation just became stopped: its tail windows may now be
		// sealed and embedded and its summary chain advanced. Nudge the indexer
		// so that happens now rather than on the next tick. recall and the
		// durable child store share the same nil condition (both need the
		// pool), but the guard stays independent so a future wiring change
		// cannot silently drop the nudge.
		if c.recall != nil {
			c.recall.indexer.Nudge()
		}
		cancel()
	}
	if err := c.deleteLogDump(childID); err != nil {
		slog.Warn("delete log dump", "childId", childID, "error", err)
	}
	if snap.Kind == protocol.KindFundi {
		if err := c.deleteSpillDir(childID); err != nil {
			slog.Warn("delete spill dir", "childId", childID, "error", err)
		}
	}
	if snap.Kind == protocol.KindScript {
		// The script host directory held the child's materialized pymodule
		// and its per-child socket; the socket was unlinked at exit
		// (scriptRunner.Wait). Removing the directory finishes removing the
		// child's footprint, the same way the spill dir goes for fundi.
		if err := c.deleteScriptHostDir(childID); err != nil {
			slog.Warn("delete script host dir", "childId", childID, "error", err)
		}
	}

	// A spawn-block sandbox exists only for its child. The child's row is
	// tombstoned above (or never was ours to write), so tear the sandbox down
	// now rather than waiting for the reaper. Best-effort and bounded: a failed
	// removal is logged (removeSandboxesOwnedBy) and must NEVER fail the close —
	// the reaper finishes any container left behind.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Minute)
	c.removeSandboxesOwnedBy(closeCtx, childID)
	closeCancel()
	return nil
}

// deleteLogDump removes the per-child log dump directory at ~/.pi/run/logs/<childID>.
// Close calls this so finalizing a child fully removes its footprint rather
// than leaving orphan dumps to accumulate forever.  Missing directory is not
// an error (no dump was written, e.g. for a child that crashed pre-Idle).
func (c *Controller) deleteLogDump(childID string) error {
	if c.logsDir == "" {
		return nil
	}
	path := filepath.Join(c.logsDir, childID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// stampConversationsClosed stamps closed_at on every conversation linked to a
// closed child. A conversation is linked when it is the child's own
// conversation (fundi children set child.conversation_id), or when its
// external_ref is the child id (claude root threads) or child_id + ":" +
// thread (claude subagent threads). _ and % are escaped in the LIKE because
// child ids are c_<ulid> and _ is a single-character wildcard — an unescaped
// pattern for c_1 would also stamp cX1:t1.
//
// Only conversations whose closed_at is still NULL are stamped, so a re-close
// or a Close racing another closer cannot overwrite an existing close time.
// Kill paths must not call this: a killed child is not a closed conversation.
func (c *Controller) stampConversationsClosed(ctx context.Context, childID string) error {
	if c.pool == nil {
		return nil
	}
	const q = `
UPDATE conversations.conversation c SET closed_at = now()
 WHERE c.closed_at IS NULL
   AND (c.id = (SELECT conversation_id FROM conversations.child WHERE child_id = $1)
        OR c.external_ref = $1
        OR c.external_ref LIKE replace(replace($1,'_','\_'),'%','\%') || ':%' ESCAPE '\')`
	if _, err := c.pool.Exec(ctx, q, childID); err != nil {
		return fmt.Errorf("stamp conversations closed %s: %w", childID, err)
	}
	return nil
}

// deleteScriptHostDir removes a script child's materialization directory
// (scriptHostDir: the pymodule tree plus its per-child socket path). Close
// and CloseAllExited call it so 'rafiki forget' fully removes the child's
// footprint, mirroring deleteSpillDir. Missing directory is not an error.
func (c *Controller) deleteScriptHostDir(childID string) error {
	path := scriptHostDir(c.stateDir, childID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// deleteSpillDir removes an agent-kind child's clipped-tool-output spill
// directory (see buildAgentArgv/agentSpillDir). Close/CloseAllExited call
// this for "fundi" kind children so 'rafiki forget' fully removes the child's
// footprint, mirroring deleteLogDump. Missing directory is not an error (the
// child may have exited before writing any spilled output).
func (c *Controller) deleteSpillDir(childID string) error {
	path := agentSpillDir(c.stateDir, childID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *Controller) CloseAllExited(olderThan time.Duration) ([]string, error) {
	snaps := c.st.FindByStatus(protocol.StatusExited)
	now := time.Now()
	var closed []string
	failed := 0
	for _, s := range snaps {
		if olderThan > 0 && !s.ExitedAt.IsZero() {
			age := now.Sub(s.ExitedAt)
			if age < olderThan {
				continue
			}
		}
		// snaps was read before the loop, and an earlier iteration's cascade may
		// already have taken this row: an exited native child appears in
		// FindByStatus in its own right AND is deleted with its parent, so
		// without this it would be reported closed twice.
		if _, still := c.st.Get(s.ChildID); !still {
			continue
		}
		// A synthetic thread child has no durable footprint at all, so the
		// store delete is its whole close. Same reasoning as Close: nothing to
		// persist, no failure path.
		if s.Native {
			c.st.Delete(s.ChildID)
			// A cascaded child is closed and belongs in the answer; the guard above
			// is what keeps it from being named a second time on its own iteration.
			closed = append(closed, c.closeNativeChildrenOf(s.ChildID)...)
			closed = append(closed, s.ChildID)
			continue
		}
		// Gated on ownsChildRow, same reasoning as Forget above: sweepExpired
		// calls ForgetAllExited on the daemon's own grace-window tick, and a
		// recovered record — including one belonging to a still-live OTHER
		// daemon's child — carries its old ExitedAt, so this is not merely a
		// hypothetical race.
		owns := c.ownsChildRow(s)
		// Persist before forgetting: the upsert carries the child's session_id
		// to conversations.child before the tombstone stamps closed_at. A child
		// whose lineage could not be recorded must not silently drop out of its
		// ancestors' spend, so skip it (leave it for the next tick), log the
		// child id, and count the failure to surface in the result. Only for a
		// row this daemon owns — another daemon's recovered row is not ours to
		// write (nor to tombstone below).
		if owns {
			if err := c.writeRecord(s.ChildID); err != nil {
				slog.Warn("close-all-exited: child not persisted", "childId", s.ChildID, "errorType", fmt.Sprintf("%T", err))
				failed++
				continue
			}
		}
		c.st.Delete(s.ChildID)
		closed = append(closed, c.closeNativeChildrenOf(s.ChildID)...)
		// The other deletion path, and the one that leaks without this: a row
		// for a child forgotten here is never pending-for-a-live-child again
		// and never terminal, so the retention sweep can never reach it.
		if owns {
			c.dropInboxForForgotten(s.ChildID, "child forgotten")
			c.rateWatch.drop(s.ChildID)
			c.dropPendingResume(s.ChildID)
		}
		if c.children != nil && owns {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := c.children.Delete(ctx, s.ChildID); err != nil {
				slog.Warn("delete child row", "childId", s.ChildID, "error", err)
			}
			cancel()
		}
		if err := c.deleteLogDump(s.ChildID); err != nil {
			slog.Warn("delete log dump", "childId", s.ChildID, "error", err)
		}
		if s.Kind == protocol.KindFundi {
			if err := c.deleteSpillDir(s.ChildID); err != nil {
				slog.Warn("delete spill dir", "childId", s.ChildID, "error", err)
			}
		}
		if s.Kind == protocol.KindScript {
			if err := c.deleteScriptHostDir(s.ChildID); err != nil {
				slog.Warn("delete script host dir", "childId", s.ChildID, "error", err)
			}
		}
		// A spawn-block sandbox exists only for its child; the child's row is
		// tombstoned above, so tear the sandbox down now rather than waiting for
		// the reaper. Best-effort: a failure is logged and never fails the sweep.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Minute)
		c.removeSandboxesOwnedBy(closeCtx, s.ChildID)
		closeCancel()
		closed = append(closed, s.ChildID)
	}
	if failed > 0 {
		return closed, fmt.Errorf("close-all-exited: %d child(ren) not persisted", failed)
	}
	return closed, nil
}

// SetLabels mutates labels on the named child. Rejects keys with the rafiki/
// prefix or invalid characters.
func (c *Controller) SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error) {
	if _, ok := c.st.Get(childID); !ok {
		return nil, &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if err := validateUserLabelKeys(set); err != nil {
		return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if err := validateUserRemoveKeys(remove); err != nil {
		return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	merged, err := c.st.SetLabels(childID, set, remove)
	if err != nil {
		return nil, &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after set_labels", "childId", childID, "error", err)
	}
	return merged, nil
}

// Send is the one place a frame is classified.
//
// A prompt, a steer or an abort is work for a turn: it is durably accepted
// before it is written, so a daemon that dies between accepting and delivering
// replays it on restart instead of stranding whoever was waiting for it. Every
// other frame — get_state, extension_ui_response, new_session — is a control
// frame with no turn semantics and goes straight down.
//
// Interception runs FIRST: a claude abort is a signal plus a resume cycle
// rather than a message, and persisting one would replay a cancellation into
// an unrelated later turn.
//
// "Accepted" now means durably queued, not written to a pipe. A child whose
// command channel is momentarily full no longer answers ErrBackpressure to the
// caller: the row is persisted, the delivery failure is logged, and the idle
// drain retries it. That is strictly better than discarding the message.
func (c *Controller) Send(childID string, frame json.RawMessage) error {
	if c.inbox == nil {
		return c.sendFrame(childID, frame)
	}
	if _, intercepted := inspect(frame); intercepted {
		return c.sendFrame(childID, frame)
	}
	if isAbortFrame(frame) && c.isClaudeAbortTarget(childID) {
		return c.sendFrame(childID, frame)
	}
	in, ok := inboundFromFrame(childID, frame)
	if !ok {
		return c.sendFrame(childID, frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.acceptAndDeliver(ctx, in)
	return err
}

// sendFrame is the raw write path to a child. Everything that reaches a child
// goes through here, INCLUDING inbox delivery — which is why it must never
// call Send, on pain of infinite recursion.
func (c *Controller) sendFrame(childID string, frame json.RawMessage) error {
	// new_session and switch_session are handled via kill+respawn rather than
	// forwarded to pi (spec §5.1).
	if decision, ok := inspect(frame); ok {
		return c.handleInterceptedSend(childID, decision)
	}

	// Claude abort: claude -p has no in-band abort frame and can only be
	// interrupted by signalling the process. Intercept abort for claude
	// children and run the interrupt+resume cycle; pi children fall through and
	// forward abort natively to --mode rpc.
	if isAbortFrame(frame) && c.isClaudeAbortTarget(childID) {
		return c.handleClaudeAbort(childID)
	}

	if err := c.validateSendTarget(childID); err != nil {
		return err
	}

	ch, ok := c.cm.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}

	// Detect extension_ui_response frames and update the SM so the blocked_ui
	// state is cleared when the last pending dialog is resolved (spec §10).
	var uiResp struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(frame, &uiResp) == nil &&
		uiResp.Type == "extension_ui_response" &&
		uiResp.ID != "" {
		ch.NotifyExtensionUIResponse(uiResp.ID)
	}

	if err := ch.Send(frame); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "backpressure") {
			return &connectapi.ControllerError{Code: protocol.ErrBackpressure, Message: msg}
		}
		if strings.Contains(msg, "shutting down") {
			return &connectapi.ControllerError{Code: protocol.ErrChildShuttingDown, Message: msg}
		}
		return err
	}
	return nil
}

// handleInterceptedSend handles new_session and switch_session by killing the
// current child process and re-spawning it with the same childId (spec §5.1).
// Per-child subscriptions are preserved across the kill+resume cycle so that
// clients observe a seamless transition. A synthesized pi-level response is
// delivered to subscribers after the new process is ready.
func (c *Controller) handleInterceptedSend(childID string, decision interceptDecision) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	// Refuse both commands outright for an agent child. Every piece of the
	// respawn below assumes a child whose conversation identity lives in a pi
	// session file that --session can point at; an agent child's conversation
	// is keyed by the daemon's child id instead. buildAgentArgv ignores
	// ResumeSession entirely, and appendDaemonRef pins --ref to the SAME
	// unchanged childID (deliberately, so a caller cannot aim one child at
	// another's history) — so a respawn here reattaches the ENTIRE prior
	// conversation and reports success. A user who asked for a fresh session
	// would get their old one back with nothing to indicate it.
	//
	// Before RespawnChild was routed through resolveSpawnPlan this produced a
	// dead child, which at least told the user something was wrong. Failing
	// loudly is the honest replacement; it stays this way until agent
	// conversations have an identity of their own, separate from the child id.
	if snap.Kind == protocol.KindFundi {
		return &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: string(decision.Type) + " is not supported for an agent child: an agent conversation is " +
				"identified by the child id itself, so a respawn would silently reattach the same conversation " +
				"rather than starting a new one. Spawn a new agent child instead.",
		}
	}

	// Refuse for a script child too, and before the kill — the fundi refusal
	// above makes the same point the other way: killing-and-then-failing
	// leaves the caller with a dead child AND an error, the worst of both. A
	// script has no session to switch to, and respawn is off for the kind (a
	// script's exit is its result): a respawn here would silently start the
	// work over, reporting success for a different run.
	if snap.Kind == protocol.KindScript {
		return &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: string(decision.Type) + " is not supported for a script child: a script has no session " +
				"and cannot be respawned — its exit is its result, so a respawn would silently start the " +
				"work over. Spawn a new script child instead.",
		}
	}

	// Gracefully shut down the current child.
	if _, err := c.Kill(context.Background(), childID, respawnShutdownGrace, respawnKillGrace); err != nil {
		var ce *connectapi.ControllerError
		if !errors.As(err, &ce) ||
			(ce.Code != protocol.ErrChildExited && ce.Code != protocol.ErrChildShuttingDown) {
			return fmt.Errorf("intercept kill: %w", err)
		}
	}

	// Spin-wait for handleChildExit (running on the monitorChild goroutine) to
	// call cm.Remove. Kill returns once c.done is closed (process reaped), but
	// monitorChild runs concurrently and calls handleChildExit shortly after.
	// We must wait for cm.Remove to complete; otherwise RespawnChild.cm.Add
	// races with handleChildExit.cm.Remove and the new entry can be deleted
	// immediately, causing the restored subscribers to be silently dropped.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := c.cm.Get(childID); !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Respawn the child with the same childId, applying the session override
	// dictated by the intercepted command (spec §5.1).
	sessionPath := "" // new_session: let pi create a fresh session
	if decision.Type == interceptSwitchSession {
		sessionPath = decision.SessionPath
	}
	if _, err := c.RespawnChild(context.Background(), childID, sessionPath); err != nil {
		return fmt.Errorf("intercept respawn: %w", err)
	}

	return nil
}

// isAbortFrame reports whether a raw frame is the normalized abort
// command ({"type":"abort"}). Used to special-case claude children, whose
// headless stream-json stdin has no abort frame (see handleClaudeAbort).
func isAbortFrame(frame []byte) bool {
	if len(frame) == 0 {
		return false
	}
	var hdr struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return false
	}
	return hdr.Type == "abort"
}

// handleClaudeAbort cancels an in-flight turn on a claude child. claude -p has
// no in-band abort frame, so we SIGINT the process (claude flushes a
// "[Request interrupted by user]" frame + result:error_during_execution, which
// the translator turns into agent_end so the TUI unblocks, and persists the
// turn), wait for exit, then re-spawn with --resume <session_id> via the
// kind-aware Resume. The childID and per-child subscribers are preserved.
func (c *Controller) handleClaudeAbort(childID string) error {
	ch, ok := c.cm.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if snap.Status == protocol.StatusShuttingDown {
		return &connectapi.ControllerError{Code: protocol.ErrChildShuttingDown, Message: "child is shutting down"}
	}
	if snap.Status == protocol.StatusExited {
		return &connectapi.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
	}

	// A daraja-routed claude child never exits on abort: the relay stream and
	// the Child object survive a Restart by design (see the daraja design
	// doc's "The relay stream survives a restart"), so the local-subprocess
	// dance below (Interrupt + wait-for-Exited + Resume) does not apply —
	// there is nothing to wait for and nothing to re-spawn from rafikid's
	// side. rafiki/executor is set only for a daraja-routed claude child
	// (claudeRunner/Task 7), never for the local-subprocess fallback.
	if snap.Labels["rafiki/executor"] != "" {
		return c.handleDarajaClaudeAbort(childID, ch, snap)
	}

	// Resume threads --resume <snap.SessionID>. The store's SessionID is synced
	// lazily by monitorChild on the first bus event, but claude's system/init
	// produces no bus frame, so snap.SessionID can lag the live sniffed value
	// (which is set right after init, at spawn). Read the live metadata and
	// persist it before Resume so the resumed child actually continues the
	// conversation rather than silently starting a fresh session. Without any
	// sniffed id there is nothing to resume — refuse rather than discard history
	// (this window only exists before claude's first system/init).
	sessionID := ch.Metadata().SessionID
	if sessionID == "" {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "cannot abort claude child before its session is established"}
	}
	if snap.SessionID != sessionID {
		_ = c.st.Update(childID, func(s *childstore.Session) { s.SessionID = sessionID })
	}

	if err := ch.Interrupt(); err != nil {
		return fmt.Errorf("claude abort interrupt: %w", err)
	}

	// Wait for the process to exit (status flips to Exited via handleChildExit).
	// Escalate to a hard Kill if SIGINT didn't take within the grace window.
	deadline := time.Now().Add(3 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		if snap, ok := c.st.Get(childID); ok && snap.Status == protocol.StatusExited {
			exited = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !exited {
		if _, err := c.Kill(context.Background(), childID, abortShutdownGrace, abortKillGrace); err != nil {
			var ce *connectapi.ControllerError
			if !errors.As(err, &ce) || (ce.Code != protocol.ErrChildExited && ce.Code != protocol.ErrChildShuttingDown) {
				return fmt.Errorf("claude abort kill: %w", err)
			}
		}
		// Wait again for Exited.
		deadline = time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if snap, ok := c.st.Get(childID); ok && snap.Status == protocol.StatusExited {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Wait for handleChildExit to call cm.Remove before calling Resume, which
	// calls cm.Add. If cm.Add races with cm.Remove, the new entry can be
	// silently deleted.
	cmDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(cmDeadline) {
		if _, alive := c.cm.Get(childID); !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Re-spawn resumed. apiKey "" is correct for claude (auth via CLAUDE_CONFIG_DIR,
	// no --api-key); Resume uses snap.SessionID for kind == "claude".
	if _, err := c.Resume(context.Background(), childID, ""); err != nil {
		return fmt.Errorf("claude abort resume: %w", err)
	}

	return nil
}

// handleDarajaClaudeAbort aborts a daraja-routed claude child's in-flight
// turn. Unlike handleClaudeAbort's local-subprocess path, this makes exactly
// ONE call: Pool.Restart asks daraja to SIGINT-wait-respawn atomically on its
// own machine (grace mirrors handleClaudeAbort's own 3s SIGINT window before
// escalating to SIGKILL — see the design doc's "Restart" section). The Child
// object, its relay stream, and its subscribers all survive untouched: there
// is no exit to wait for and no Resume to re-spawn, because daraja's Restart
// already did both halves of that in one round trip. darajapool.Runner's
// pending-reset flag (set from the SAME RelayResponse_Restarted event this
// call's Restart produces) makes readStdout reset the claude translator's
// stale turnActive/model/message state before it sees the replacement
// process's first frame — see Runner.TakeResetPending and
// claudeProvider.ResetState.
//
// rafikid supplies the ChildSpec (kind, model, resume_session, permission
// mode) explicitly rather than passing spec=nil (which would tell daraja to
// reuse whatever ChildSpec it was ORIGINALLY launched with): the original
// launch's ChildSpec has an empty resume_session (a fresh conversation has no
// session yet), so reusing it would respawn claude into a brand-new
// conversation and silently discard history. This mirrors handleClaudeAbort's
// own comment about resolving the live sniffed session id, and matches
// claudeRunner's own PermissionMode default exactly, so a restarted process
// is launched the same way the original one was.
func (c *Controller) handleDarajaClaudeAbort(childID string, ch *child.Child, snap childstore.Snapshot) error {
	if c.darajaPool == nil {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "daraja pool not wired"}
	}
	sessionID := ch.Metadata().SessionID
	if sessionID == "" {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "cannot abort claude child before its session is established"}
	}
	if snap.SessionID != sessionID {
		_ = c.st.Update(childID, func(s *childstore.Session) { s.SessionID = sessionID })
	}
	spec := buildDarajaAbortSpec(snap, sessionID)
	if _, err := c.darajaPool.Restart(context.Background(), childID, spec, 3*time.Second); err != nil {
		return fmt.Errorf("claude abort restart: %w", err)
	}
	return nil
}

// buildDarajaAbortSpec builds the ChildSpec a daraja-routed claude abort
// restarts with. Pulled out of handleDarajaClaudeAbort as a pure function so
// its one job — reconstruct an equivalent launch spec but with the FRESH
// sniffed session id — is unit-testable with no pool, no connection, and no
// child.Child at all. See handleDarajaClaudeAbort's doc comment for why the
// spec cannot be nil (which would tell daraja to reuse the ORIGINAL launch's
// spec, whose resume_session is empty).
func buildDarajaAbortSpec(snap childstore.Snapshot, sessionID string) *darajapb.ChildSpec {
	return &darajapb.ChildSpec{
		Kind: darajapb.Kind_KIND_CLAUDE,
		Claude: &darajapb.ClaudeParams{
			Model:         snap.Model,
			ResumeSession: sessionID,
			// Matches claudeRunner's own default exactly (agent_runtime.go) —
			// a daemon-managed child has no human to answer an interactive
			// permission prompt, restarted or not. The constant, not a literal:
			// the same word the two launch paths state.
			PermissionMode: claudeargv.PermissionModeBypass,
			// AppendSystemPrompt and ExtraArgs are argv-shaped RESTART fields,
			// unlike the launch-only block below: daraja rebuilds the child's
			// argv from this spec on every Restart, so dropping either here
			// silently restarts the child without the system-prompt appendix
			// and operator flags it was launched with. (Contrast the
			// launch-only block — ProxyUrl/ProxyToken/PassthroughAuth/
			// AutoCompactWindow/RecordRequests — which this function
			// deliberately omits: those are launch-only (see the proto comment
			// on ClaudeParams) — daraja's environment is fixed at ITS OWN
			// process startup and a Restart never rebuilds it, only argv. The
			// Restart RPC's spec is used purely for argv.)
			AppendSystemPrompt: snap.AppendSystemPrompt,
			ExtraArgs:          snap.ExtraArgs,
		},
	}
}

// ─── monitorChild ─────────────────────────────────────────────────────────────

// monitorChild runs as a goroutine for each live child. It consumes the
// child's bus frames, delivers status transitions, handles rename detection,
// model-label updates, and child exit.
//
// Status transitions are DRAINED from the child, never sampled off Status().
// The state machine transitions on the child's readStdout goroutine, which runs
// far ahead of this loop's consumption of the bus (a JSON header decode per
// frame versus a store lookup and three subscriber fan-outs), so a turn whose
// frames arrive in one burst used to complete its whole idle→streaming→idle
// round trip between two samples and lose BOTH ends of it — a subscriber
// watching a fast turn saw nothing at all. Draining is loss-free whatever the
// relative speed of the two goroutines.
func (c *Controller) monitorChild(childID string, ch *child.Child) {
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	// drainChildStatus (below) is the ONLY path from a child-side transition to
	// handleStatusChange, and once this goroutine is running it is the only
	// caller, which is what keeps delivery exactly-once.
	drainStatus := func() { c.drainChildStatus(childID, ch) }

	// Initialise last-known name from the store so we can detect renames.
	// Initialise last-known model so we can detect model changes (set_model/cycle_model).
	lastKnownName := ""
	lastKnownModel := ""
	lastKnownSessionID := ""
	lastKnownSessionFile := ""
	slashSynced := false
	if snap, ok := c.st.Get(childID); ok {
		lastKnownName = snap.Name
		lastKnownModel = joinModel(snap.Provider, snap.Model)
		lastKnownSessionID = snap.SessionID
		lastKnownSessionFile = snap.SessionFile
	}

	for {
		select {
		case _, ok := <-busCh:
			if !ok {
				// Bus was closed (shouldn't happen in normal operation).
				// Drain first: a transition recorded just before the bus went
				// away is still owed to the store, and handleChildExit reports
				// the store's status as last_status.
				drainStatus()
				c.handleChildExit(childID, ch)
				return
			}
			// Emit any status transitions this frame (or an earlier one) caused,
			// keeping the store in sync. Draining here rather than only on the
			// StatusChanged wake keeps a status change behind the frames that
			// produced it in the common case: handleFrame records a transition
			// only after publishing the frame that caused it.
			drainStatus()

			// Detect session name changes produced by the sniffer. The sniffer
			// updates Metadata().SessionName when set_session_name completes.
			// Polling on each bus event is the right moment because the sniffer
			// update happens in the same readStdout goroutine that feeds the bus
			// (spec §7.5).
			md := ch.Metadata()
			if md.SessionName != "" && md.SessionName != lastKnownName {
				c.handleChildRenamed(childID, md.SessionName, lastKnownName)
				lastKnownName = md.SessionName
			}

			// Detect model changes from set_model / cycle_model responses.
			// Update the store and persist.
			if md.Model != "" && md.Model != lastKnownModel {
				c.handleModelChange(childID, md.Model)
				lastKnownModel = md.Model
			}

			// Sync session id / file once they appear. For ReadyOnSpawn children
			// (claude) the process is silent until prompted, so these are unknown
			// at spawn and only surface on the first turn's init; without this the
			// store would keep the empty session id captured at activate time and
			// resume could not re-attach.
			//
			// A claude child already holding an id is a different case: the id
			// it reports now naming another session means the process started a
			// new conversation under this child. That ends the child rather
			// than overwriting the id, so the row keeps pointing at the
			// original conversation and a resume goes back to it.
			if md.SessionID != "" && md.SessionID != lastKnownSessionID && c.refuseClaudeSessionIDChange(childID, lastKnownSessionID, md.SessionID) {
				// Neither id nor file is synced from a process being ended.
			} else if (md.SessionID != "" && md.SessionID != lastKnownSessionID) ||
				(md.SessionFile != "" && md.SessionFile != lastKnownSessionFile) {
				c.handleSessionMetaChange(childID, md.SessionID, md.SessionFile)
				lastKnownSessionID = md.SessionID
				lastKnownSessionFile = md.SessionFile
			}

			// Capture claude's advertised slash commands once they appear
			// (claude emits them in the init frame; static for the session).
			if len(md.SlashCommands) > 0 && !slashSynced {
				sc := md.SlashCommands
				_ = c.st.Update(childID, func(s *childstore.Session) { s.SlashCommands = sc })
				slashSynced = true
			}

		case <-ch.StatusChanged():
			// A transition was recorded without a bus frame following it — the
			// last one of a turn, or one made off the readStdout goroutine
			// (NotifyExtensionUIResponse). Without this wake it would sit in the
			// queue until the next frame arrived, which for a child that has gone
			// quiet is never.
			//
			// Guarded by TestMonitorChild_UIResponseTransition_NeedsNoBusFrame,
			// which is the shape that bites: the burst tests do NOT cover this
			// case and pass with this branch deleted, because readStdout records
			// before monitorChild consumes the frame that caused it.
			drainStatus()

		case <-ch.Done():
			// Then the transitions the frames caused, BEFORE handleChildExit:
			// after handleChildExit a transition would be unreachable
			// (cm.Remove drops the child). Draining first also means the exit
			// event's last_status reflects the child's real final status.
			//
			// This drain is DEFENSIVE, and deliberately kept as such: no test
			// covers it, and deleting it breaks nothing (30 runs of the
			// burst-then-exit test and the full suite stay green). The window it
			// closes is real but narrow — the wake token is set strictly before
			// done closes, so a parked monitorChild takes the StatusChanged case
			// and drains before the reap even completes. Losing a transition here
			// needs monitorChild to be busy across the whole record→reap interval
			// AND the select to pick Done over an equally-ready StatusChanged.
			// Cheap insurance against a uniformly-random select; do not read the
			// absence of a failing test as evidence it is unnecessary.
			drainStatus()
			c.handleChildExit(childID, ch)
			return
		}
	}
}

// drainChildStatus emits every status transition the child has queued, oldest
// first. It is used by activateLiveChild to flush the startup transitions
// synchronously, before monitorChild takes over draining for the rest of the
// child's life. Only one goroutine drains a given child at a time: this call
// completes before `go c.monitorChild` starts.
func (c *Controller) drainChildStatus(childID string, ch *child.Child) {
	for _, t := range ch.DrainTransitions() {
		c.handleStatusChange(childID, t.To, t.From)
	}
}

func (c *Controller) handleStatusChange(childID string, newStatus, prev protocol.Status) {
	// now feeds only the transition announcement's timestamp; it
	// is NOT threaded into startWorking/heartbeats, which track elapsed time
	// entirely on sweepHeartbeats' own caller-supplied clock (see
	// heartbeatState.due's doc) — hoisted here only because that is where
	// every other status-change timestamp in this function is captured.
	now := time.Now()
	storePrev, ok := c.st.SetStatus(childID, newStatus)
	// Publish onto the native/rafiki-v1 event stream so the TUI rail can
	// render a working spinner and an accurate glyph. This is the ONLY
	// producer of agent_status in the whole daemon, and it is deliberately
	// here rather than in a per-kind path: handleStatusChange already fires
	// for every kind (pi/claude via ch.DrainTransitions, fundi via the same
	// StateMachine driven by its own pi-shaped frames -- see child.Child's
	// handleFrame), so one call site covers all of them. Before this, the
	// rail's Status was frozen at whatever ListChildren reported when the TUI
	// last (re)seeded -- the working spinner and glyph never moved again for
	// the rest of the attachment, no matter which kind of child it was.
	if ok && storePrev != newStatus {
		c.publishEvent(childID, &rafikiv1.Event{
			ChildId: childID,
			Ts:      timestamppb.New(now),
			Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: string(newStatus)}},
		})
	}
	// An LLM child settles every turn, so a result set by an EARLIER turn would
	// otherwise ride a later settle fragment (notifySubagentSettled carries
	// whatever Result is stored at settle time). Clear it when a new turn
	// starts — the transition out of a non-working status into a working one.
	// Script children are exempt: their result is the work product of the whole
	// run, not of one turn. The check lives inside the Update closure, not in a
	// preceding Get: SetResult runs on a Connect handler goroutine, and a result
	// landing between a Get and the Update would otherwise be wiped. The guard
	// means the clear mutates only when a result was actually present, so the
	// ordinary per-turn status churn costs no extra row write — persistence is
	// this function's unconditional tail writeRecord, not a second one here.
	if ok && !isWorkingStatus(storePrev) && isWorkingStatus(newStatus) {
		if err := c.st.Update(childID, func(s *childstore.Session) {
			if s.Kind != protocol.KindScript && s.Result != "" {
				s.Result = ""
			}
		}); err != nil {
			slog.Warn("clear turn result", "childId", childID, "error", err)
		}
	}
	// Release any event batches deferred while this child was mid-turn.
	// This is rafiki's turn-end drain; it is why no busy-poller is needed.
	if ok && newStatus == protocol.StatusIdle && storePrev != protocol.StatusIdle {
		if c.evbuf != nil {
			if isWorkingStatus(storePrev) {
				c.notifySubagentSettled(childID, c.settleReason(childID), "", "")
			}
			c.evbuf.DrainIdle(childID)
		}
		// Retry anything a failed immediate delivery left pending. Outside the
		// evbuf guard: the inbox is the durable queue whether or not an event
		// buffer is configured. On its own goroutine, because this runs on
		// monitorChild's status path and must not be held up by a database
		// round trip.
		go c.drainInbox(c.inbox, childID)
		c.heartbeats.stopWorking(childID)
		// The turn just settled: if the upstream's latest verdict for this
		// child was a 429, that verdict is what killed it — schedule the
		// auto-resume (claude children only; the gate is inside). In-memory
		// work only, so it stays on the status path.
		c.maybeRateLimitResume(childID)
	}
	if ok && isWorkingStatus(newStatus) {
		c.heartbeats.startWorking(childID)
	}
	// A graceful shutdown overwrites EVERY live child's status with
	// shutting_down — idle and mid-turn alike — and status is recovery's only
	// resume signal. Stamp what the child was doing BEFORE the overwrite so a
	// restarted daemon can still tell "was working, cycle lost" from "was
	// idle"; the claude continuation prompt keys off it. It gates a nudge,
	// never a resume (status gates that), so the last_status-never-gates rule
	// is untouched. The tail writeRecord below persists the label with the
	// shutting_down status in one upsert.
	if ok && newStatus == protocol.StatusShuttingDown && isWorkingStatus(prev) {
		if err := c.st.Update(childID, func(s *childstore.Session) {
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			s.Labels[preShutdownStatusLabel] = string(prev)
		}); err != nil {
			slog.Warn("stamp pre-shutdown status", "childId", childID, "error", err)
		}
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after status change", "childId", childID, "error", err)
	}
}

// handleModelChange updates the store's Provider/Model fields and the
// rafiki/model + rafiki/provider auto-labels when the sniffer detects a model change
// via set_model or cycle_model responses.
func (c *Controller) handleModelChange(childID, modelStr string) {
	provider, model := splitModel(modelStr)
	_ = c.st.Update(childID, func(s *childstore.Session) {
		s.Provider = provider
		s.Model = model
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		if provider != "" {
			s.Labels["rafiki/provider"] = provider
		} else {
			delete(s.Labels, "rafiki/provider")
		}
		if model != "" {
			s.Labels["rafiki/model"] = model
		} else {
			delete(s.Labels, "rafiki/model")
		}
	})
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after model change", "childId", childID, "error", err)
	}
}

// refuseClaudeSessionIDChange reports whether reported is a CHANGE of the
// session id a claude child already holds, and if so ends the child (once per
// launch). A pi child's session id legitimately changes (new_session,
// switch_session), every other kind has none, and a child holding no id yet
// simply adopts its first one — so only a claude child with a held id can
// trip it. Callers must not store a refused id.
func (c *Controller) refuseClaudeSessionIDChange(childID, held, reported string) bool {
	if held == "" || reported == "" || held == reported {
		return false
	}
	snap, ok := c.st.Get(childID)
	if !ok || snap.Kind != protocol.KindClaude {
		return false
	}
	if _, already := c.sessionIDEnded.LoadOrStore(childID, struct{}{}); !already {
		go c.endChildOnSessionIDChange(childID, held, reported)
	}
	return true
}

// endChildOnSessionIDChange ends a claude child whose process reported a
// session id other than the one it holds. The stored id is left alone on
// purpose, and the reason goes on the row, so the failure is visible and the
// original conversation is still the one a resume returns to. A broken child
// now is better than a conversation silently split across sessions: capture
// keys on the child, so a second session would write over the first's history.
//
// It runs on its own goroutine: Kill waits for monitorChild's handleChildExit
// to remove the child, and the caller IS monitorChild.
func (c *Controller) endChildOnSessionIDChange(childID, was, now string) {
	reason := fmt.Sprintf("claude session id changed from %s to %s; the child was ended instead of continuing in a different conversation", was, now)
	slog.Error("claude child reported a different session id; ending it", "childId", childID, "was", was, "now", now)
	_ = c.st.Update(childID, func(s *childstore.Session) {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		s.Labels["rafiki/session-error"] = reason
	})
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after session id change", "childId", childID, "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Kill(ctx, childID, 5*time.Second, 5*time.Second); err != nil {
		slog.Error("end child after session id change", "childId", childID, "error", err)
	}
}

// handleSessionMetaChange syncs the child's sniffed session id / file into the
// store and persists the record. For ReadyOnSpawn children (claude) these are
// unknown at spawn — the process is silent until prompted — and only appear on
// the first turn's init; without this sync the store keeps the empty session id
// captured at activate time and resume cannot re-attach.
func (c *Controller) handleSessionMetaChange(childID, sessionID, sessionFile string) {
	_ = c.st.Update(childID, func(s *childstore.Session) {
		if sessionID != "" {
			s.SessionID = sessionID
		}
		if sessionFile != "" {
			s.SessionFile = sessionFile
		}
	})
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after session meta change", "childId", childID, "error", err)
	}
}

// handleChildRenamed updates the store when the sniffer detects that the
// child's process changed its session name.
func (c *Controller) handleChildRenamed(childID, newName, previous string) {
	_ = c.st.Rename(childID, newName)
	_ = previous // kept in the signature: the previous name is caller context
}

func (c *Controller) handleChildExit(childID string, ch *child.Child) {
	res := ch.ExitResult()
	now := time.Now()

	// A spontaneous exit KEEPS its rate-limit watch: the fire path relaunches
	// an exited child via Resume, which is the whole point of keeping it. The
	// exits that must NOT be undone — an operator's Kill, the daemon's own
	// shutdown — drop the watch at their own call sites (Kill,
	// ShutdownAllChildren), where the intent is known, rather than here,
	// where a passive stdin close makes an operator's kill read as the
	// child's own death (fake-pi and claude alike exit 0 on EOF).

	// Determine last known status before marking exited. Recorded in the
	// row's last_status column and the log dump's ExitInfo as observability —
	// what the child was doing when it ended. The recovery predicate reads
	// rec.Status, never this.
	snap, _ := c.st.Get(childID)
	lastStatus := string(snap.Status)
	// When the daemon gracefully shut this child down, the store status is
	// "shutting_down"; record the child's real pre-shutdown state instead.
	if lastStatus == string(protocol.StatusShuttingDown) {
		if ps := ch.PreShutdownStatus(); ps != "" {
			lastStatus = string(ps)
		}
	}

	// Snapshot the ring before removing the child so GetRecent continues
	// to work after the child is gone (spec §11.4).
	ringSnapshot := ch.Ring().Recent(ring.Query{})
	// RenderRecent has returned nil since B4 removed the render ring; the call
	// is kept so MarkExited's signature stays honest about what it stores.
	// Rendered reads for an exited child are served from conversation_message.
	renderEvents := ch.RenderRecent(ring.Query{})

	// Flush the script coalescer BEFORE MarkExited: nothing between here and
	// the final durable appends depends on the coalescer still being open, and
	// MarkExited is what makes status "exited" visible to a polling client —
	// which must not see "exited" until the child's last output is in the log.
	// The flushed appends are durable, so they precede the child_exited event
	// below in ordinal order.
	if co := c.takeScriptOutputCoalescer(childID); co != nil {
		co.Close()
	}

	// MarkExited sets Status, ExitedAt, ExitCode, ExitSignal, and ExitedRing
	// atomically under one sess.mu hold so a concurrent Snapshot() cannot
	// observe Status=Exited with ExitedRing still nil.
	c.st.MarkExited(childID, now, res.ExitCode, res.Signal, ringSnapshot, renderEvents)

	// A dead child's per-child MCP secret stops resolving (the proxy face
	// consults it on every request). handleChildExit is already the one place
	// an exit is recorded, so no new exit hook is added.
	c.forgetMCPToken(childID)

	// Persist AFTER MarkExited so the row records the exit itself: rec.Status
	// comes off the snapshot as "exited", which is the one durable fact the
	// recovery predicate reads — "this child ended while a daemon was alive to
	// record it" — and lastStatus rides along as observability. Skipped during
	// the daemon's own shutdown (see c.stopping): the rows must keep saying
	// idle/streaming so the next daemon resumes them.
	if !c.stopping.Load() {
		if err := c.writeRecordLastStatus(childID, lastStatus); err != nil {
			slog.Warn("write state record on exit", "childId", childID, "error", err)
		}
	}

	// Dump logs before removing from the manager so per-child subscribers are
	// still reachable for the exit event delivery below.
	if c.dumper != nil {
		dumpSnap, _ := c.st.Get(childID)
		meta := persist.Meta{
			ChildID:     childID,
			Name:        dumpSnap.Name,
			Cwd:         dumpSnap.Cwd,
			Model:       joinModel(dumpSnap.Provider, dumpSnap.Model),
			SessionFile: dumpSnap.SessionFile,
			SpawnedAt:   dumpSnap.StartedAt.Unix(),
			ExitedAt:    now.Unix(),
			ExitCode:    res.ExitCode,
			ExitSignal:  res.Signal,
			Argv:        dumpSnap.ExtraArgs,
		}
		exitInfo := persist.ExitInfo{
			ExitCode:   res.ExitCode,
			Signal:     res.Signal,
			LastStatus: lastStatus,
		}
		renderBytes := make([][]byte, len(renderEvents))
		for i, e := range renderEvents {
			renderBytes[i] = e.Bytes
		}
		if err := c.dumper.Dump(childID, ch.InSnapshot(), ch.RingSnapshot(), renderBytes, ch.StderrSnapshot(), meta, exitInfo); err != nil {
			slog.Warn("log dump failed", "child", childID, "error", err)
		}
	}

	// A Task subagent runs inside this process, so it dies with it. Before the
	// exit event below purely for ordering legibility: a subscriber that sees
	// the parent gone has already been told about its threads.
	c.exitNativeChildrenOf(childID)

	var exitCode *int
	if res.Signal == "" {
		code := res.ExitCode
		exitCode = &code
	}

	var exitCodePtr *int32
	if exitCode != nil {
		c32 := int32(*exitCode)
		exitCodePtr = &c32
	}
	// Durable, so it precedes the exit event below in ordinal order: the
	// child's last output was flushed to the log at the top of this handler,
	// before MarkExited made "exited" visible.
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{
			ChildId:  childID,
			ExitCode: exitCodePtr,
			Signal:   res.Signal,
		}},
	})

	// Tell the parent its worker is gone. This runs before Forget (which
	// clears batches aimed AT this child, not at its parent) and before
	// cm.Remove, which is the observable "teardown complete" signal.
	//
	// A kill the caller initiated itself via agent_kill suppresses the notice
	// the audience that called it already has an answer for — that call blocks
	// until cm.Remove, so its tool result already confirmed termination. But
	// only when the death was the kill's own doing (exitCausedByShutdown): a
	// daraja-hosted claude exits 143 from the SIGTERM the ladder sent, which
	// is the kill's doing, while a fundi panic's ExitCode=2 or a foreign
	// SIGKILL landing during the passive stdin-close wait is news even when a
	// kill was in flight. And only for the audience that acted: a coordinator
	// suppresses its parent fragment; an MCP caller's own user is excluded
	// from the fan-out while the parent (a coordinator that did not act) is
	// still told. checkTaskResidue runs either way: killing a subagent with
	// unresolved tasks is itself worth surfacing, self-initiated or not.
	mark, selfKilled := c.selfKilled.take(childID)
	d := selfKillDispositionFor(mark, selfKilled && exitCausedByShutdown(res))
	switch {
	case d.suppressParent:
		c.checkTaskResidue(childID)
	default:
		// A script child's settle is its exit, with the semantics the kind
		// implies: exit 0 settles done, anything else failed, and the
		// fragment carries the stored SetResult — or, when the script never
		// called SetResult, the last 4 KiB of its stderr. Every other kind
		// keeps the plain "exited" reason with no tail.
		reason, tail := scriptSettleFor(snap.Kind, res, ch.StderrSnapshot())
		c.notifySubagentSettled(childID, reason, tail, d.excludeMCPUser)
	}

	// Drop any buffered events aimed at this child. It will never transition
	// to idle again, so DrainIdle can never clear them.
	if c.evbuf != nil {
		c.evbuf.Forget(childID)
	}

	// And its retained boundExecutor, for the same reason: exited is terminal
	// (pkg/protocol/types.go), so nothing will poll a binding whose child can
	// no longer be told anything. Close does this too, but Close is optional —
	// a child that exits and is never closed would otherwise hold its binding,
	// and the executor client it references, for the daemon's lifetime.
	c.forgetBoundExecutor(childID)

	// Return this child's unconfirmed inbox rows to pending. A RESET, not a
	// drop: the child can be resumed and its queue is exactly what a resume
	// should run. Before cm.Remove for the same reason as the task sweep
	// below — cm.Remove is the observable "teardown complete" signal, so a
	// caller that has seen it must not still find rows marked sent to a
	// process that no longer exists.
	//
	// Skipped for a fundi child this daemon never actually held the lease
	// for. recoverOne's resume path can spawn a competing in-process engine
	// for a conversation another daemon already owns; that engine's lease
	// acquisition is refused, but the failure surfaces asynchronously (see
	// holdsLease's doc comment), so THIS exit — not resumeWithAutoRecovery's
	// return — is where "did this daemon ever really own the child" first
	// becomes checkable on this path. Resetting the OWNING daemon's still-
	// live child's inbox here is the other half of the bug replayInbox's own
	// holdsLease gate closes: verified live, a doomed competing build's exit
	// reached this call and reset the row to 'pending' with nobody left to
	// redeliver it, stranding it exactly like an unguarded replay would —
	// gating replayInbox alone was not sufficient. A claude/pi child never
	// acquires a lease at all (agentRunner returns a nil Runner for anything
	// but fundi), so it is exempt rather than silently skipped by an
	// always-false holdsLease.
	//
	// c.leases == nil is checked FIRST and short-circuits the rest: without a
	// database pool (inboxStore(nil), the default dev configuration)
	// OnConversationResolved's own short-circuit means NO fundi child ever
	// calls trackLease, so holdsLease is permanently false and every exit
	// would otherwise skip this reset — silently stranding rows at 'sent'
	// (invisible to the pending-only idle drain) and logging a false WARN on
	// every single fundi exit. A prior version of this condition copied only
	// half of OnConversationResolved's own bypass test
	// (`c.leases == nil || c.daemonID == ""`); this restores the other half.
	if c.leases == nil || snap.Kind != protocol.KindFundi || c.daemonID == "" || c.holdsLease(childID) {
		c.releaseInboxOnExit(childID)
	} else {
		slog.Warn("child exited without this daemon ever holding its lease; not resetting its inbox",
			"childId", childID)
	}

	c.nudgedMu.Lock()
	delete(c.nudgedOnce, childID)
	c.nudgedMu.Unlock()

	// Clear tracked heartbeat/turn-outcome state too, or both leak forever
	// for a child that exits without going through a clean idle transition
	// first (an engine fatal, an operator kill) — handleStatusChange's own
	// idle-transition cleanup never runs for those.
	c.heartbeats.stopWorking(childID)
	c.turnOutcomes.take(childID)

	// Sweep this child's unfinished work to orphaned BEFORE cm.Remove.
	// cm.Remove is the observable "kill complete" signal (waitForChildRemoval
	// blocks on it), so sweeping after it would let a caller see a finished
	// kill while the tasks still read in_progress.
	//
	// Best-effort under a short deadline: a database outage must not wedge
	// child teardown. The cost of failure is that rows stay in_progress
	// behind a dead child, which is recoverable.
	if c.tasks != nil {
		sweepCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if n, err := c.tasks.OrphanAssigned(sweepCtx, childID); err != nil {
			slog.Warn("orphan task sweep failed; tasks remain in_progress",
				"childId", childID, "error", err)
		} else if n > 0 {
			slog.Info("orphaned tasks for exited child", "childId", childID, "count", n)
		}
		cancel()
	}

	// Release workspace. Best-effort under a short deadline: a docker hiccup
	// must not wedge child teardown, exactly like the orphan sweep above.
	if snap, ok := c.st.Get(childID); ok {
		if wID := snap.Labels["rafiki/workspace"]; wID != "" {
			eID := snap.Labels["rafiki/executor"]
			go func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				c.releaseWorkspace(rctx, eID, wID)
			}()
		}
	}

	c.releaseLease(childID)
	var relaunch *childstore.ChildRecord
	if _, lost := c.darajaLost.LoadAndDelete(childID); lost {
		if s, ok := c.st.Get(childID); ok {
			rec := childstore.RecordFromSnapshot(s)
			rec.Status = lastStatus
			rec.DaemonID = c.daemonID
			relaunch = &rec
		}
	}
	c.cm.Remove(childID)
	if relaunch != nil {
		c.relaunchLostDaraja(*relaunch)
	}
}

// ─── persistence ─────────────────────────────────────────────────────────────

func (c *Controller) writeRecord(childID string) error {
	return c.writeRecordLastStatus(childID, "")
}

// writeRecordLastStatus persists the child's child-state record. lastStatus,
// when non-empty, is the pre-exit state recorded by handleChildExit rather
// than the store's Status at the time of an ordinary write. It is written on
// that path only, and is observability — the recovery predicate reads the
// row's status column. The upsert COALESCEs an empty value so an ordinary
// write cannot blank what the last real exit recorded.
//
// A failed upsert is RETURNED as well as logged, so a caller that must not let
// a child drop out of the database (Close, CloseAllExited, the spawn-fatal
// insert) can fail closed; the ordinary best-effort callers log and carry on.
// A child absent from the in-memory store leaves nothing to write and is not an
// error.
//
// noteConversationID records a child's resolved conversation id on its store
// entry so the next writeRecord carries it to conversations.child.
//
// This is the first moment the daemon knows the id. Before it, conversation_id
// was filled only from a snapshot's SessionID, which for a fundi child is not
// set until its first turn completes.
func (c *Controller) noteConversationID(childID, conversationID string) {
	if err := c.st.Update(childID, func(s *childstore.Session) {
		s.SessionID = conversationID
	}); err != nil {
		slog.Warn("record conversation id", "childId", childID, "error", err)
		return
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write child row after conversation id", "childId", childID, "error", err)
	}
}

func (c *Controller) writeRecordLastStatus(childID string, lastStatus string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil
	}

	if c.children != nil {
		// Stamp the owning daemon label so Forgets can check ownership.
		// Ordering matters: this must run before RecordFromSnapshot so
		// the label reaches the row.
		if c.daemonID != "" {
			if err := c.st.Update(childID, func(s *childstore.Session) {
				if s.Labels == nil {
					s.Labels = map[string]string{}
				}
				s.Labels["rafiki/daemon"] = c.daemonID
			}); err != nil {
				slog.Warn("stamp owning daemon label", "childId", childID, "error", err)
			}
			snap, _ = c.st.Get(childID)
		}

		rec := childstore.RecordFromSnapshot(snap)
		rec.LastStatus = lastStatus
		rec.DaemonID = c.daemonID
		rec.NSToken = c.nsToken
		if snap.Kind == protocol.KindFundi {
			rec.ConversationID = snap.SessionID
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.children.Upsert(ctx, rec)
		cancel()
		if err != nil {
			// Returned, not just logged: the callers that must fail CLOSED on a
			// lost row (Close/CloseAllExited/abortSpawn) decide their own fate
			// from it, while the best-effort writers log and carry on.
			return fmt.Errorf("write child row %s: %w", childID, err)
		}
	}
	return nil
}

// computeLineageLabels resolves the rafiki/parent and rafiki/root label
// values for a child being spawned under parentID. Both are empty when
// parentID is empty (a top-level child).
//
// root is taken from the parent's own root label when it has one, and is
// otherwise the parent's id — the parent is then top-level. This never walks
// the chain: the parent's labels are correct by induction, which is what
// keeps spawn O(1) regardless of tree depth.
func computeLineageLabels(st *childstore.Store, parentID string) (parent, root string, err error) {
	if parentID == "" {
		return "", "", nil
	}
	if _, ok := st.Get(parentID); !ok {
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "parentChildId: no such child: " + parentID,
		}
	}
	return parentID, st.RootOf(parentID), nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func newChildID() string {
	return "c_" + ulid.Make().String()
}

// resolveClaudeBinary resolves the Claude Code CLI binary path. Precedence:
// explicit override → CLAUDE_BINARY env → ~/.local/bin/claude (the path the
// user's claudew/claudep wrappers exec) → PATH lookup of "claude".
func resolveClaudeBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("CLAUDE_BINARY"); env != "" {
		return env, nil
	}
	if u, err := user.Current(); err == nil {
		cand := filepath.Join(u.HomeDir, ".local", "bin", "claude")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return exec.LookPath("claude")
}

// resolveClaudeBinaryIfNeeded resolves the local claude binary path, but only
// when it will actually be used. runner is agentRunner's result: non-nil
// means claudeRunner already built a daraja-backed Runner, and child.Spawn
// never reads spec.PiBinary in that case (every caller clears it right after
// this — "if runner != nil { spec.PiBinary = "" ... }"). Called AFTER
// agentRunner, never inside resolveSpawnPlan, which runs before agentRunner
// even decides whether this spawn routes through daraja — resolving it there
// aborted every claude spawn on a daemon with no local claude install before
// daraja got a chance to route it elsewhere.
func resolveClaudeBinaryIfNeeded(req protocol.SpawnRequest, runner child.Runner) (string, error) {
	if req.Kind != protocol.KindClaude || runner != nil {
		return "", nil
	}
	return resolveClaudeBinary(req.PiBinary)
}

// buildClaudeArgv converts a SpawnRequest into the claude CLI argument list
// (excluding the binary itself) for stream-json bidirectional driving.
// buildClaudeArgv is a thin wrapper over claudeargv.Build — the ONE claude
// argv builder shared with the executor's AdminService and daraja's own
// Restart/respawn path (see that package's doc comment). This is the pure
// form: the exec path STAGES the launch files in resolveSpawnPlan (moving the
// system-prompt appendix and MCP config off argv where `ps` shows them) and
// this function is what tests build against directly.
//
// The req→Params mapping itself lives in claudeargv.ParamsFromSpawnRequest,
// NOT here: it must be reachable from test/integration, whose
// TestClaudeArgvIdenticalAcrossPaths drives this path and the daraja path
// (daraja.ClaudeParamsForRequest → SpecFromProto → ChildSpec.Argv) against
// each other. Keep this a delegation — any field mapping added here instead
// of there is mapping the cross-path test cannot see.
//
// vals carries the proxy's argv decisions (proxyChildEnv → buildEnv →
// resolveSpawnPlan): MCPConfig becomes the single --mcp-config element and
// ModelArgs REPLACES the plain --model pair req.Model would otherwise emit,
// so a proxied child carries exactly one --model.
func buildClaudeArgv(req protocol.SpawnRequest, vals proxyenv.Values) []string {
	return claudeargv.Build(claudeargv.ParamsFromSpawnRequest(req, vals))
}

// resolveSpawnPlan picks the binary, argv, and ProtocolProvider for a spawn
// request based on its Kind. Empty Kind defaults to "fundi". Shared by Spawn and
// Resume so the two paths can never diverge on protocol selection.
//
// childID and stateDir are only used by the "fundi" kind, which needs both to
// pin --spill-dir to a location Forget can find deterministically later (see
// buildAgentArgv/agentSpillDir). claude ignores them but consumes vals, the
// proxy's argv decisions (see buildClaudeArgv); the other kinds ignore it.
func resolveSpawnPlan(req protocol.SpawnRequest, childID, stateDir string, vals proxyenv.Values) (bin string, argv []string, prov child.ProtocolProvider, err error) {
	kind := req.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	switch kind {
	case protocol.KindClaude:
		// The binary path is resolved LAZILY, by resolveClaudeBinaryIfNeeded
		// after agentRunner decides whether this spawn routes through daraja
		// — never here. Resolving it unconditionally made every claude spawn
		// fail outright on a daemon with no local claude install (the exact
		// production topology daraja exists for: a remote/k8s daemon with an
		// executor pool doing all the hosting), because this function's error
		// aborts the spawn before agentRunner/claudeRunner ever gets a chance
		// to route it through daraja instead — bin/argv from here are
		// discarded anyway once a non-nil Runner is returned (see the
		// "if runner != nil" clearing at each call site).
		//
		// The launch files (system-prompt appendix, MCP config) are staged here,
		// immediately before Build, so neither text rides argv where `ps` shows
		// it. A respawn restages from the same held text (Stage is keyed by
		// content hash, so it converges).
		p, err := claudeargv.Stage(claudeargv.ParamsFromSpawnRequest(req, vals), promptfile.Dir())
		if err != nil {
			return "", nil, nil, fmt.Errorf("claude launch files: %w", err)
		}
		return "", claudeargv.Build(p), child.ClaudeProvider{}, nil
	case protocol.KindFundi:
		// The fundi runtime is `rafikid fundi ...`: the daemon re-execs itself
		// rather than shelling out to a separate binary. It speaks pi's rpc
		// protocol natively (pkg/fundi/frontend.go), so no translator is
		// needed — the identity provider is correct for fundi's stdout.
		//
		// --model is a required flag for `rafikid fundi` (parseAgentFlags):
		// reject an unresolvable model here, at spawn time, rather than
		// exec'ing a child that immediately dies on the flag-parse error.
		if !agentSpawnHasModel(req) {
			return "", nil, nil, errors.New(`fundi kind requires a model: set SpawnRequest.Model (provider-qualified, e.g. "anthropic/sonnet-latest") or pass --model via ExtraArgs`)
		}
		// Unlike pi/claude, the fundi kind carries its provider inside the
		// model id itself (e.g. "anthropic/sonnet-latest" - see
		// pkg/fundi/config.go's senderOptions); there is no separate
		// --provider flag for `rafikid agent` to consume. A caller-supplied
		// req.Provider here would silently be dropped were it not for this
		// check, or worse, get double-prefixed onto the reported model - so
		// reject it explicitly rather than exec'ing a child whose model
		// doesn't match what the caller asked for.
		if req.Provider != "" {
			return "", nil, nil, errors.New(`fundi kind does not accept a separate Provider: fold it into a provider-qualified Model (e.g. "anthropic/sonnet-latest") instead`)
		}
		self, selfErr := os.Executable()
		if selfErr != nil {
			return "", nil, nil, fmt.Errorf("resolving own binary for fundi kind: %w", selfErr)
		}
		return self, buildAgentArgv(req, childID, stateDir), child.IdentityProvider{}, nil
	case protocol.KindScript:
		// The Runner (scriptRunner) execs the interpreter and carries the
		// process; only the provider is decided here, so stdout lines get the
		// script provider's liveness semantics (and nothing is ever written
		// to the script's stdin). bin/argv stay empty — a non-nil runner
		// discards them, and a script has no binary of its own to resolve.
		return "", nil, child.ScriptProvider{}, nil
	default:
		return "", nil, nil, fmt.Errorf("unknown kind: %s", kind)
	}
}

// agentSpillDir returns the deterministic spill directory for an agent-kind
// child's clipped tool output: <stateDir>/spill/<childID>. Shared by
// buildAgentArgv (which pins the child's --spill-dir here, overriding Task
// 14's own os.TempDir()-based default) and Close/CloseAllExited (which
// remove it), so the two can never diverge on the path.
func agentSpillDir(stateDir, childID string) string {
	return filepath.Join(stateDir, "spill", childID)
}

// agentSpawnHasModel reports whether a "fundi" kind SpawnRequest resolves to
// a non-empty --model: either req.Model itself, or a "--model VALUE"/
// "--model=VALUE" pair supplied through the ExtraArgs escape hatch
// (buildAgentArgv appends ExtraArgs last, so an ExtraArgs --model can stand
// in for req.Model even though req.Model itself is required by
// parseAgentFlags). Checked by resolveSpawnPlan before ever building the
// argv/exec'ing the child - `rafikid agent` treats a missing --model as a hard
// flag-parse error, and a spawn-time rejection here is a far cleaner failure
// than a child that execs and immediately dies.
//
// A bare "--model" token only counts when it is followed by a value that
// isn't itself another flag: "--model" as the last ExtraArgs element, or
// immediately followed by a "-"-prefixed token, is exactly the shape that
// leaves parseAgentFlags with no value - the same failure as --model being
// absent entirely - so it must not satisfy this guard.
func agentSpawnHasModel(req protocol.SpawnRequest) bool {
	if req.Model != "" {
		return true
	}
	for i, a := range req.ExtraArgs {
		if strings.HasPrefix(a, "--model=") {
			return true
		}
		if a == "--model" && i+1 < len(req.ExtraArgs) && !strings.HasPrefix(req.ExtraArgs[i+1], "-") {
			return true
		}
	}
	return false
}

// buildAgentArgv converts a SpawnRequest into the `rafikid fundi` CLI argument
// list (excluding the binary itself), mirroring Task 14's flag contract
// (cmd/rafikid/agent.go's parseAgentFlags). The leading "fundi" token is
// required so main.go's `os.Args[1] == protocol.KindFundi` dispatch fires on
// re-exec.
//
// --spill-dir is always pinned to agentSpillDir(stateDir, childID) so the
// daemon and the agent child agree on where clipped tool output lives,
// letting Forget clean it up deterministically - this intentionally
// overrides parseAgentFlags' own os.TempDir()-based default. It is emitted
// before req.ExtraArgs so the existing "extra args win last" escape hatch
// (see buildArgv/buildClaudeArgv) still lets a caller override it.
func buildAgentArgv(req protocol.SpawnRequest, childID, stateDir string) []string {
	argv := []string{protocol.KindFundi}

	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if req.Thinking != "" {
		argv = append(argv, "--thinking", req.Thinking)
	}
	if req.SystemPrompt != "" {
		argv = append(argv, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", req.AppendSystemPrompt)
	}
	if len(req.Prefill) > 0 {
		// JSON.Marshal of []PrefillRead (plain string/int fields) cannot fail;
		// the branch exists so a future field that could fail degrades to a
		// child with no pre-fill instead of one that dies on the flag parse.
		b, err := json.Marshal(req.Prefill)
		if err != nil {
			slog.Error("agent spawn: marshal prefill; omitting --prefill",
				"childId", childID, "error", err)
		} else {
			argv = append(argv, "--prefill", string(b))
		}
	}
	if len(req.Skills) > 0 {
		argv = append(argv, "--skills", strings.Join(req.Skills, ","))
	}
	if req.NoSkills {
		argv = append(argv, "--no-skills")
	}
	if req.Tools != "" {
		argv = append(argv, "--tools", req.Tools)
	}
	if req.NoBuiltinTools {
		argv = append(argv, "--no-builtin-tools")
	}
	// The resolved routing spec, as the child's llm.Client's WithRouting. The
	// daemon resolved it once; the child never re-resolves.
	if req.Routing != "" {
		argv = append(argv, "--routing", req.Routing)
	}
	if req.NoContextFiles {
		argv = append(argv, "--no-context-files")
	}
	if req.Name != "" {
		argv = append(argv, "--name", req.Name)
	}
	for _, d := range req.SkillsDirs {
		argv = append(argv, "--skills-dir", d)
	}
	if req.MCPConfig != "" {
		argv = append(argv, "--mcp-config", req.MCPConfig)
	}
	if len(req.MCPServers) > 0 {
		argv = append(argv, "--mcp-servers", strings.Join(req.MCPServers, ","))
	}
	if req.NoMCP {
		argv = append(argv, "--no-mcp")
	}
	if req.RecordRequests {
		argv = append(argv, "--record-requests")
	}
	argv = append(argv, "--spill-dir", agentSpillDir(stateDir, childID))

	// Extra args are appended last (last-flag-wins override), same convention
	// as buildArgv/buildClaudeArgv.
	argv = append(argv, req.ExtraArgs...)
	return argv
}

// spawnKindLabel normalizes a SpawnRequest/snapshot Kind into the value used for
// the rafiki/kind auto-label. Empty defaults to "fundi",
// matching resolveSpawnPlan's kind handling.
func spawnKindLabel(kind string) string {
	if kind == "" {
		return protocol.KindFundi
	}
	return kind
}

// claudeEnv returns the extra env entries a claude child needs. Currently just
// CLAUDE_CONFIG_DIR; returns nil when configDir is empty so the child inherits
// the controller's default config dir.
func claudeEnv(configDir string) []string {
	if configDir == "" {
		return nil
	}
	return []string{"CLAUDE_CONFIG_DIR=" + configDir}
}

// buildEnv assembles the per-process env var additions for a child process.
// The slice is passed to SpawnSpec.Env. Whether these additions replace or
// extend the parent environment is controlled by SpawnSpec.EnvOverride
// (honoured in child.Spawn, not here). The second return value carries the
// proxy's argv decisions as data (proxyenv.Values, empty when no proxy
// applies); the caller threads them into resolveSpawnPlan, whose
// buildClaudeArgv turns them into the child's argv — there is no post-hoc
// argv append any more, so the flags land in claudeargv.Build's canonical
// positions and --mcp-config/--model are emitted exactly once. They take
// effect only on the local-subprocess path (a non-nil runner discards argv
// entirely; the daraja path rebuilds argv from ClaudeParams instead).
//
// The two reserved controller vars are always injected regardless of mode.
//
// Note on API key propagation for the "fundi" kind: unlike pi/claude, `rafikid
// fundi` has no --api-key flag (pkg/fundi.Config.AnthropicAPIKey /
// OpenRouterAPIKey are read from the environment by cmd/rafikid/agent.go's
// runAgent, deliberately, so tests can exercise the missing-key path without
// mutating the process env - see pkg/fundi/config.go's Config doc
// comment). Two paths feed the child those vars: (1) when EnvOverride is
// false (the default), child.Spawn merges os.Environ() - the daemon's own
// inherited env - into the child's env for free, so an ANTHROPIC_API_KEY /
// OPENROUTER_API_KEY already present in the daemon's environment reaches the
// child with no code here; (2) when the caller supplies req.APIKey
// explicitly (the same field pi/claude thread via --api-key), this function
// translates it into the correctly-named var below so it isn't silently
// dropped for agent kind. Which var name to use is decided the same way
// `rafikid agent` itself decides routing (pkg/fundi/config.go's
// senderOptions): an "anthropic/" prefixed model needs ANTHROPIC_API_KEY,
// anything else needs OPENROUTER_API_KEY - there is no separate --provider
// concept any more.
func (c *Controller) buildEnv(req protocol.SpawnRequest, childID, socketPath string) (env []string, vals proxyenv.Values) {
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	env = append(env,
		paths.ChildID+"="+childID,
		paths.Socket+"="+socketPath,
	)
	if req.Kind == protocol.KindFundi && req.APIKey != "" {
		envVar := "OPENROUTER_API_KEY"
		if strings.HasPrefix(req.Model, "anthropic/") {
			envVar = "ANTHROPIC_API_KEY"
		}
		env = append(env, envVar+"="+req.APIKey)
	}
	proxyEnv, vals := c.proxyChildEnv(req, childID)
	env = append(env, proxyEnv...)
	return env, vals
}

// proxyChildEnv returns the environment variables and argv decisions that
// point a child at the rafiki proxy, or nothing when no proxy is configured
// or this kind is not routed. The env additions are appended to SpawnSpec.Env;
// the argv decisions travel as proxyenv.Values and reach the child's argv only
// through buildClaudeArgv (via resolveSpawnPlan) on the local-subprocess path
// — never appended after the fact, which is what used to emit a second,
// duplicate --model.
//
// The agent kind is never routed: it reaches rafiki in-process through pkg/llm
// and pkg/routing, so there is no HTTP face to point it at, and doing so would
// put a network hop in front of a library call.
// proxyEndpoint resolves the URL/token a child should be pointed at: the
// embedded proxy face, unless an explicit RAFIKI_URL names an external rafiki
// instead (useful for aiming a whole machine at a shared capture server, whose
// token then comes from the environment file rather than this daemon's own).
// Shared by proxyChildEnv (the local-subprocess path) and claudeRunner's
// daraja path so the two cannot resolve a different endpoint for the same
// daemon.
func (c *Controller) proxyEndpoint() (url, token string) {
	url, token = c.proxyURL, c.proxyToken
	if v := paths.Get(paths.URL); v != "" {
		url, token = v, paths.Get(paths.Token)
	}
	return url, token
}

func (c *Controller) proxyChildEnv(req protocol.SpawnRequest, childID string) (env []string, vals proxyenv.Values) {
	url, token := c.proxyEndpoint()
	if url == "" || req.Kind == protocol.KindFundi || !proxyRoutesKind(req.Kind) {
		return nil, proxyenv.Values{}
	}

	// childID rather than the session id: it exists before the child does, is
	// stable for the child's whole life, and already identifies it everywhere
	// else in the daemon. The proxy stores it as external_ref, so a captured
	// conversation traces back to the child that produced it.
	headers := map[string]string{
		"X-Rafiki-Session": childID,
		"X-Rafiki-Source":  req.Kind,
	}

	if req.RecordRequests {
		headers["X-Rafiki-Record-Requests"] = "1"
	}

	switch req.Kind {
	case protocol.KindClaude:
		// Built by the same code as `rafiki claude`, so the two cannot drift.
		// Passing a nil environ yields only the additions, which is what is
		// wanted: the child inherits os.Environ and this is appended to it,
		// where the last assignment wins.
		//
		// PassthroughAuth is deliberately NOT set here and must not be: this
		// path passes nil for the environ and receives only ADDITIONS, which
		// buildEnv appends to the daemon's own os.Environ(). Passthrough is
		// defined by the absence of ANTHROPIC_AUTH_TOKEN and
		// ANTHROPIC_API_KEY, and appending cannot un-set anything — the
		// daemon's own ANTHROPIC_API_KEY would reach the child and it would
		// quietly use API-key auth. Supporting children means converting this
		// path to a full-environment contract first. Note the same asymmetry
		// already makes proxyenv.Credentials inert here: children do inherit
		// the daemon's ANTHROPIC_API_KEY today, and only Claude Code's own
		// precedence (ANTHROPIC_AUTH_TOKEN outranks it) keeps that harmless.
		//
		// vals (MCPConfig, ModelArgs) is consumed by buildClaudeArgv through
		// resolveSpawnPlan, not appended here: ModelArgs replaces the plain
		// --model pair instead of duplicating it, and MCPConfig lands in
		// claudeargv.Build's canonical position rather than the end of argv.
		// MCPToken is the per-child MCP secret, distinct from the proxy bearer
		// above (which stays the per-boot secret for billing attribution). It
		// is mint-or-reused per child so a resume/respawn of the same child
		// keeps the credential it already holds, and proxyenv renders it into
		// RAFIKI_MCP_TOKEN plus the --mcp-config placeholder — never argv.
		env, vals = proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{
			URL: url, Token: token, Model: req.Model, Headers: headers,
			MCPToken: c.mintMCPToken(childID),
		})
		// A mint grew the credential map; the sweep that bounds it hangs off
		// the mints that grew it (see sweepMCPTokensIfDue — forget only runs
		// from handleChildExit, so failed spawns and sibling-closed rows leak).
		c.sweepMCPTokensIfDue()
		return env, vals
	default:
		// Only claude is proxied. Fundi runs in-process (no HTTP face to point
		// it at), and anything else is not a routeable kind.
		return nil, proxyenv.Values{}
	}
}

// proxyRoutesKind reports whether kind is listed in RAFIKI_PROXY_KINDS, which
// defaults to "claude".
func proxyRoutesKind(kind string) bool {
	kinds := splitComma(paths.Get(paths.ProxyKinds))
	if len(kinds) == 0 {
		kinds = []string{protocol.KindClaude}
	}
	return slices.Contains(kinds, kind)
}

// splitModel splits "provider/model" into provider and model. If no slash is
// present the entire string is the model and provider is empty.
func splitModel(s string) (provider, model string) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// joinModel returns "provider/model" if both are non-empty, otherwise just model.
func joinModel(provider, model string) string {
	if provider != "" && model != "" {
		return provider + "/" + model
	}
	return model
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durOrDefault(d time.Duration, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// The kill ladder's production grace periods. Each is a named time.Duration so
// a bare millisecond literal can never silently become nanoseconds at a
// Controller.Kill call site (the wave that made the timeouts Durations).
const (
	respawnShutdownGrace   = 3 * time.Second
	respawnKillGrace       = 500 * time.Millisecond
	abortShutdownGrace     = time.Second
	abortKillGrace         = 500 * time.Millisecond
	leaseLossShutdownGrace = 5 * time.Second
	leaseLossKillGrace     = 5 * time.Second
	failChildShutdownGrace = 5 * time.Second
	failChildKillGrace     = time.Second
)

func framePassesTypeFilter(frame []byte, include, exclude []string) bool {
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}
	var hdr struct {
		Type string `json:"type"`
	}
	if err := parseEventType(frame, &hdr); err != nil {
		return true
	}
	for _, ex := range exclude {
		if ex == hdr.Type {
			return false
		}
	}
	if len(include) > 0 {
		for _, inc := range include {
			if inc == hdr.Type {
				return true
			}
		}
		return false
	}
	return true
}

func matchesSessionFilter(snap childstore.Snapshot, f protocol.SearchSessionFilter) bool {
	if f.CwdContains != "" && !strings.Contains(snap.Cwd, f.CwdContains) {
		return false
	}
	if f.NameContains != "" && !strings.Contains(snap.Name, f.NameContains) {
		return false
	}
	if !f.Since.IsZero() && snap.StartedAt.Before(f.Since) {
		return false
	}
	if !matchesLabelFilter(snap.Labels, f.Labels, f.HasLabel) {
		return false
	}
	return true
}

// matchesLabelFilter returns true when labels satisfies both the AND-match
// required map and the key-presence hasLabels list.
func matchesLabelFilter(labels, required map[string]string, hasLabels []string) bool {
	for k, v := range required {
		if labels[k] != v {
			return false
		}
	}
	for _, k := range hasLabels {
		if _, ok := labels[k]; !ok {
			return false
		}
	}
	return true
}

// parseEventType partially decodes a JSON frame into hdr. Using a shared helper
// avoids duplicating json.Unmarshal calls in hot paths.
func parseEventType(frame []byte, hdr any) error {
	return json.Unmarshal(frame, hdr)
}

// TaskList queries the task ledger for the ListTasks RPC.
//
// No conversation scope: this verb answers "what is every agent doing",
// which is a cross-conversation question. The dispatcher clamps Limit before
// calling, and the store applies it after sorting, which is what keeps the
// response inside protocol.MaxFrameBytes.
func (c *Controller) TaskList(ctx context.Context, req protocol.TaskListRequest) ([]tasks.Task, error) {
	if c.tasks == nil {
		return nil, &connectapi.ControllerError{
			Code:    protocol.ErrNoAgentDB,
			Message: "task ledger unavailable: no database configured",
		}
	}

	f := tasks.ListFilter{
		ConversationID: req.ConversationID,
		Assignee:       req.ChildID,
		Status:         tasks.Status(req.Status),
		IncludeDropped: req.All,
		Limit:          req.Limit,
	}
	return c.tasks.List(ctx, f)
}

// ─── Executor management ────────────────────────────────────────────────────

// requireExecutorStore returns a ControllerError when the executor store is
// nil — controller methods call this so the error message names one condition
// instead of five places that must all agree on wording.
func (c *Controller) requireExecutorStore() error {
	if c.execStore == nil {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "no executor store configured (requires RAFIKI_DB; also requires RAFIKI_EXECUTORS_ENABLED=1 when RAFIKI_CONTROL_LISTEN is set)",
		}
	}
	return nil
}

// ExecutorEnroll mints a one-time enrollment token.
// translateExecutorErr promotes the executor store's domain sentinels into
// ControllerErrors, so their text — which this codebase wrote and which tells
// an operator something actionable ("enrollment token already consumed") —
// survives mapErr's allowlist. Anything else is returned unchanged and is
// therefore treated as internal: unexpected store failures carry the store's
// own text, and a pgx connection failure names the database host, user and
// database. That belongs in the daemon log, not in a response.
func translateExecutorErr(err error) error {
	if err == nil {
		return nil
	}
	var code string
	switch {
	case errors.Is(err, executors.ErrNotFound), errors.Is(err, executors.ErrTokenUnknown):
		code = protocol.ErrNotFound
	case errors.Is(err, executors.ErrTokenConsumed),
		errors.Is(err, executors.ErrTokenExpired),
		errors.Is(err, executors.ErrDisabled):
		// The argument is real but no longer usable — a client error, not ours.
		code = protocol.ErrInvalidArgs
	case errors.Is(err, executors.ErrMachineNameTaken):
		// A collision on (owner, machine) is the operator naming a machine
		// twice, not a daemon fault. Left as the store's raw text it reaches
		// the client as ERR_INTERNAL / 503, which reads as "the daemon is
		// broken" for a mistake only the operator can fix.
		//
		// Its own message rather than err.Error(): the sentinel's text is
		// written for the EXECUTOR, which learns of the collision when it
		// redeems its token and can only be told to get a different token. An
		// operator holding a control connection has the row in reach instead.
		//
		// Phrased to be true on BOTH control paths. It reaches an operator
		// naming a new executor (CreateExecutor) and one renaming an
		// existing one (LabelExecutor), so it must not say "--name",
		// which the label verb has no flag for, nor "relabel the existing
		// executor", which on the label path is the thing they just tried.
		// What both need is the same: which executor is holding the name.
		return &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "that executor name is already taken for this owner — (owner, " +
				"machine) names exactly one executor. Choose a different name, or " +
				"free this one by relabelling or deleting whichever executor holds " +
				"it: `rafiki executor list --selector machine=<name>`",
		}
	default:
		return err
	}
	return &connectapi.ControllerError{Code: code, Message: err.Error()}
}

// executorTrustLabels merges the operator's own labels with the two the DAEMON
// owns, and refuses a request that tries to write either itself.
//
// owner and machine are stamped HERE, from the connection and from a validated
// --name — never from req.Labels. Both gate access: owner is what an executor's
// admits selector matches, and machine decides which durable executor an
// interactive client on that box binds its children to. A client that could
// name either would be granting itself access to another operator's machine.
//
// Refusing is deliberate rather than silently overwriting: a caller who wrote
// `--label owner=x` needs to learn that their selector will not mean what they
// wrote.
func executorTrustLabels(id users.Identity, name string, given map[string]string) (map[string]string, error) {
	owner, err := sessionOwner(id)
	if err != nil {
		return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	return executorTrustLabelsFor(owner, name, given)
}

// executorTrustLabelsFor is executorTrustLabels with the owner NAME supplied by
// the caller instead of derived from the connection. The sandbox provisioner
// uses it: every child-provenance identity carries an EMPTY Username, so
// sessionOwner would stamp the DAEMON's OS user on a child's sandbox — the bug
// attestOwner exists to avoid for the child's own label.
func executorTrustLabelsFor(owner, name string, given map[string]string) (map[string]string, error) {
	if _, ok := given["owner"]; ok {
		return nil, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "owner is derived from the connection and cannot be set with --label",
		}
	}
	if _, ok := given["machine"]; ok {
		return nil, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "machine is set with --name, not with --label",
		}
	}
	labels := make(map[string]string, len(given)+2)
	for k, v := range given {
		labels[k] = v
	}
	labels["owner"] = owner
	if name != "" {
		// Validated daemon-side as well as in the CLI: a name lands in a
		// comma-separated selector, so a comma or an equals sign silently
		// reparses into a different selector.
		if err := paths.ValidateMachineName(name); err != nil {
			return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
		}
		labels["machine"] = name
	}
	return labels, nil
}

func (c *Controller) ExecutorCreate(id users.Identity, req protocol.ExecutorCreateRequest) (protocol.ExecutorCreateResponseData, error) {
	if err := c.requireExecutorStore(); err != nil {
		return protocol.ExecutorCreateResponseData{}, err
	}
	labels, err := executorTrustLabels(id, req.Name, req.Labels)
	if err != nil {
		return protocol.ExecutorCreateResponseData{}, err
	}
	e, credential, err := c.execStore.Create(context.Background(), executors.NewToken{
		Labels:        labels,
		Roots:         req.Roots,
		Isolation:     req.Isolation,
		WorkspaceMode: req.WorkspaceMode,
		Admits:        req.Admits,
		OwnerUserID:   id.UserID,
	})
	if err != nil {
		return protocol.ExecutorCreateResponseData{}, translateExecutorErr(fmt.Errorf("create executor: %w", err))
	}
	return protocol.ExecutorCreateResponseData{ExecutorID: e.ID, Credential: credential}, nil
}

func (c *Controller) ExecutorEnroll(id users.Identity, req protocol.ExecutorEnrollRequest) (protocol.ExecutorEnrollResponseData, error) {
	if err := c.requireExecutorStore(); err != nil {
		return protocol.ExecutorEnrollResponseData{}, err
	}
	labels, err := executorTrustLabels(id, req.Name, req.Labels)
	if err != nil {
		return protocol.ExecutorEnrollResponseData{}, err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 72 * time.Hour
	}
	token, err := c.execStore.MintToken(context.Background(), executors.NewToken{
		Labels:        labels,
		Roots:         req.Roots,
		Isolation:     req.Isolation,
		WorkspaceMode: req.WorkspaceMode,
		Admits:        req.Admits,
		OwnerUserID:   id.UserID,
		ExpiresAt:     time.Now().Add(ttl),
	})
	if err != nil {
		return protocol.ExecutorEnrollResponseData{}, translateExecutorErr(fmt.Errorf("mint token: %w", err))
	}
	return protocol.ExecutorEnrollResponseData{Token: token}, nil
}

// ExecutorList returns enrolled executors, optionally filtered.
func (c *Controller) ExecutorList(req protocol.ExecutorListRequest) ([]executors.Executor, error) {
	if err := c.requireExecutorStore(); err != nil {
		return nil, err
	}
	execs, err := c.execStore.List(context.Background())
	if err != nil {
		return nil, translateExecutorErr(err)
	}
	// Connected/ConnectedAt are a view over the live pool, not the store: the
	// row cannot tell a client whether an executor is currently up.
	//
	// The pool is also a SOURCE here, not only a decoration. A transient
	// executor has no row at all, and `waitExecutorLive` polls this verb to
	// learn its own session executor connected — a list built from the store
	// alone can never answer, so the client times out and tears down a healthy
	// executor.
	if c.execPool != nil {
		seen := make(map[string]int, len(execs))
		for i := range execs {
			seen[execs[i].ID] = i
		}
		for _, le := range c.execPool.Live() {
			t := le.ConnectedAt
			if i, ok := seen[le.Executor.ID]; ok {
				execs[i].Connected = true
				execs[i].ConnectedAt = &t
				continue
			}
			e := le.Executor
			e.Connected = true
			e.ConnectedAt = &t
			execs = append(execs, e)
		}
	}
	if req.Selector != "" {
		sel, pErr := executors.ParseSelector(req.Selector)
		if pErr != nil {
			return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: pErr.Error()}
		}
		var filtered []executors.Executor
		for _, e := range execs {
			if sel.Matches(e.Labels) {
				filtered = append(filtered, e)
			}
		}
		execs = filtered
	}
	if req.Limit > 0 && len(execs) > req.Limit {
		execs = execs[:req.Limit]
	}
	return execs, nil
}

// ListExecutorRows enumerates the LIVE executors this owner could spawn onto,
// with kind-scoped eligibility. It mirrors chooseExecutor/chooseLaunchExecutor
// exactly — via the shared executorReason helper — for a HYPOTHETICAL
// top-level spawn (no parent), which is what a human deciding "which executor
// should I use" is actually asking.
//
// Deliberately live-only, not merged with persisted-but-offline rows the way
// ExecutorList is: an offline durable executor cannot serve a fresh spawn
// either, so this answers exactly what a spawn attempt would see. `rafiki
// executor list` remains the place to see the full management-table view.
func (c *Controller) ListExecutorRows(ctx context.Context, kind, ownerName, ownerUserID string) ([]connectapi.ExecutorRow, error) {
	if c.execPool == nil {
		return nil, errors.New("no executor pool is configured (requires RAFIKI_DB; also requires RAFIKI_EXECUTORS_ENABLED=1 when RAFIKI_CONTROL_LISTEN is set)")
	}
	req := protocol.SpawnRequest{}
	launchKind := kind
	if kind == "" || kind == protocol.KindFundi {
		launchKind = ""
	}
	// ownerName is the display label Admits selectors match against (only
	// ever matched, never compared for identity) and ownerUserID the durable
	// id the ownership rule compares. Both come from the caller's connection
	// identity — the same pair Controller.Spawn would carry into a real
	// top-level spawn, so the preview cannot disagree with one. A nil
	// identity (the unix socket) arrives as ""/"": an unowned caller, whose
	// spawns are unowned, sees exactly the unowned executors — the same
	// match-both-empty rule selection applies.
	_, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, executorOwner{Name: ownerName, UserID: ownerUserID})
	if err != nil {
		return nil, err
	}
	launchable := launchKindSet(c.execPool.Live(), launchKind)

	live := c.execPool.Live()
	out := make([]connectapi.ExecutorRow, 0, len(live))
	for _, le := range live {
		e := le.Executor
		reason := executorReason(e, req, launchable, launchKind, sel, childLabels, parentSet, ownerUserID)
		out = append(out, connectapi.ExecutorRow{
			ID:            e.ID,
			Machine:       e.Labels["machine"],
			Labels:        e.Labels,
			Isolation:     e.Isolation,
			WorkspaceMode: e.WorkspaceMode,
			Roots:         e.Roots,
			Admits:        e.Admits,
			Enabled:       e.Enabled,
			Connected:     true,
			LaunchKinds:   le.Describe.GetLaunchKinds(),
			Eligible:      reason == "",
			Reason:        reason,
		})
	}
	return out, nil
}

// ExecutorLabel sets or removes labels on an executor row.
func (c *Controller) ExecutorLabel(req protocol.ExecutorLabelRequest) (executors.Executor, error) {
	if err := c.requireExecutorStore(); err != nil {
		return executors.Executor{}, err
	}
	e, err := c.resolveExecutorRef(context.Background(), req.ExecutorID)
	if err != nil {
		return executors.Executor{}, err
	}
	e, err = c.execStore.SetLabels(context.Background(), e.ID, req.Set, req.Remove)
	return e, translateExecutorErr(err)
}

// ExecutorDisable disables an executor.
func (c *Controller) ExecutorDisable(req protocol.ExecutorDisableRequest) error {
	if err := c.requireExecutorStore(); err != nil {
		return err
	}
	e, err := c.resolveExecutorRef(context.Background(), req.ExecutorID)
	if err != nil {
		return err
	}
	return translateExecutorErr(c.execStore.SetEnabled(context.Background(), e.ID, false))
}

// ExecutorEnable re-enables a disabled executor.
func (c *Controller) ExecutorEnable(req protocol.ExecutorEnableRequest) error {
	if err := c.requireExecutorStore(); err != nil {
		return err
	}
	e, err := c.resolveExecutorRef(context.Background(), req.ExecutorID)
	if err != nil {
		return err
	}
	return translateExecutorErr(c.execStore.SetEnabled(context.Background(), e.ID, true))
}

// ExecutorDelete permanently removes an executor row. Unlike disable, this
// cannot be undone — there is no tombstone for executors.
func (c *Controller) ExecutorDelete(req protocol.ExecutorDeleteRequest) error {
	if err := c.requireExecutorStore(); err != nil {
		return err
	}
	e, err := c.resolveExecutorRef(context.Background(), req.ExecutorID)
	if err != nil {
		return err
	}
	// A sandbox's executor row backs a container that restarts forever
	// (`--restart unless-stopped`). Deleting the row alone would evict nothing
	// and orphan that container, so the sandbox must be removed through the
	// path that stops it first. Presence of the label, not value: a row written
	// with an empty value is still a sandbox.
	//
	// The refusal holds only while a LIVE sandbox row backs this executor. A row
	// whose sandbox row is absent or already tombstoned (crash-F1: a create
	// rollback that failed to delete the executor, or a launcher that vanished)
	// would otherwise be UNDELETABLE by any CLI — `rafiki sandbox rm` cannot
	// find a tombstoned row — and would block its (owner, machine) name forever.
	if _, isSandbox := e.Labels[sandbox.RowLabelSandbox]; isSandbox {
		live, err := c.sandboxRowLive(context.Background(), e.Labels[sandbox.RowLabelID])
		if err != nil {
			return err
		}
		if live {
			sbx := e.Labels[sandbox.RowLabelID]
			if sbx == "" {
				sbx = shortID(e.ID)
			}
			return &connectapi.ControllerError{
				Code: protocol.ErrInvalidArgs,
				Message: fmt.Sprintf(
					"executor %s is sandbox %q: deleting its row would orphan a container that restarts forever — remove it with `rafiki sandbox rm`",
					shortID(e.ID), sbx),
			}
		}
	}
	return translateExecutorErr(c.execStore.Delete(context.Background(), e.ID))
}

// executorRefMinLen is the shortest trailing fragment resolveExecutorRef will
// look for. Four characters is long enough that accidental suffix collisions
// stay rare while still forgiving to type; anything shorter answers not-found
// rather than guessing.
const executorRefMinLen = 4

// maxAmbiguousRefs caps how many matching ids an ambiguity error spells out.
const maxAmbiguousRefs = 5

// resolveExecutorRef maps a possibly-truncated executor id onto exactly one row.
//
// An exact row id always wins. Anything else is matched by SUFFIX, never by
// prefix: executor ids are UUIDv7s whose leading bits are a millisecond
// timestamp, so every row minted in the same window shares its front and only
// the tail carries distinguishing entropy. Matching by suffix is what makes the
// fragment the list command displays usable verbatim as the <executor-id>
// argument of the label/enable/disable verbs.
//
// A fragment that matches no row is not-found; one that matches several rows is
// an invalid-args error naming them, never a silent pick.
func (c *Controller) resolveExecutorRef(ctx context.Context, ref string) (executors.Executor, error) {
	notFound := &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("executor %q: no such row", ref),
	}
	if ref == "" {
		return executors.Executor{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "executor id required",
		}
	}
	e, exactErr := c.execStore.Get(ctx, ref)
	switch {
	case exactErr == nil:
		return e, nil
	case !errors.Is(exactErr, executors.ErrNotFound):
		return executors.Executor{}, translateExecutorErr(exactErr)
	}
	if len(ref) < executorRefMinLen {
		return executors.Executor{}, notFound
	}
	all, listErr := c.execStore.List(ctx)
	if listErr != nil {
		return executors.Executor{}, translateExecutorErr(listErr)
	}
	var matches []executors.Executor
	for _, cand := range all {
		if strings.HasSuffix(cand.ID, ref) {
			matches = append(matches, cand)
		}
	}
	switch len(matches) {
	case 0:
		return executors.Executor{}, notFound
	case 1:
		return matches[0], nil
	}
	slices.SortFunc(matches, func(a, b executors.Executor) int {
		return strings.Compare(a.ID, b.ID)
	})
	listed, more := matches, 0
	if len(listed) > maxAmbiguousRefs {
		listed, more = listed[:maxAmbiguousRefs], len(listed)-maxAmbiguousRefs
	}
	ids := make([]string, len(listed))
	for i, m := range listed {
		ids[i] = m.ID
	}
	msg := fmt.Sprintf("executor id %q is ambiguous — it matches %d rows: %s",
		ref, len(matches), strings.Join(ids, ", "))
	if more > 0 {
		msg += fmt.Sprintf(", and %d more", more)
	}
	return executors.Executor{}, &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: msg,
	}
}

// executorForOwnerCheck resolves ref to a row for connect_executoradmin.go's
// RPC-scoping check ahead of a mutation: the durable store first, through the
// same exact-or-suffix resolution ExecutorLabel/Disable/Enable/Delete
// themselves use (resolveExecutorRef), then the live pool for a transient
// session executor, which carries no row at all. ok is false when ref
// resolves to nothing — the caller lets the request through in that case, so
// the downstream Controller call reports its own not-found rather than this
// helper inventing one.
func (c *Controller) executorForOwnerCheck(ctx context.Context, ref string) (executors.Executor, bool) {
	if c.execStore != nil {
		if e, err := c.resolveExecutorRef(ctx, ref); err == nil {
			return e, true
		}
	}
	if c.execPool != nil {
		for _, le := range c.execPool.Live() {
			if le.Executor.ID == ref {
				return le.Executor, true
			}
		}
	}
	return executors.Executor{}, false
}

// ─── Identity ──────────────────────────────────────────────────────────────

// errNoUserStore is returned when identity commands are used on a daemon with
// no database. Every user verb needs a row; there is nothing to degrade to.
var errNoUserStore = &connectapi.ControllerError{
	Code:    protocol.ErrNoAgentDB,
	Message: "no database configured (RAFIKI_DB unset); user identity requires one",
}

func (c *Controller) UserCreate(ctx context.Context, username string) (protocol.UserCreateResponseData, error) {
	return c.createUser(ctx, username, false)
}

// createUser is the one place a users row is minted. isAdmin is never
// inferred: `rafikid user create --admin` passes true because an operator
// must be able to review every owner's conversations, and every
// authenticated Connect create passes false. Nothing else may set the bit.
func (c *Controller) createUser(ctx context.Context, username string, isAdmin bool) (protocol.UserCreateResponseData, error) {
	if c.users == nil {
		return protocol.UserCreateResponseData{}, errNoUserStore
	}
	u, token, err := c.users.Create(ctx, users.NewUser{Username: username, IsAdmin: isAdmin, MintToken: true})
	if err != nil {
		return protocol.UserCreateResponseData{}, err
	}
	slog.Info("user created", "username", u.Username, "id", u.ID, "is_admin", u.IsAdmin)
	return protocol.UserCreateResponseData{
		ID: u.ID, Username: u.Username, Token: token,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

func (c *Controller) UserList(ctx context.Context, includeDeleted bool, limit int) ([]users.User, error) {
	if c.users == nil {
		return nil, errNoUserStore
	}
	return c.users.List(ctx, includeDeleted, limit)
}

func (c *Controller) UserRm(ctx context.Context, username string) error {
	if c.users == nil {
		return errNoUserStore
	}
	// The user id is resolved BEFORE the tombstone: after Delete the row is
	// gone and LookupUsername would answer ErrNotFound for a user that
	// existed a moment ago. Both calls return ErrNotFound for the same
	// unknown-or-tombstoned shapes, so the error the caller sees is unchanged.
	userID, err := c.users.LookupUsername(ctx, username)
	if err != nil {
		return err
	}
	if err := c.users.Delete(ctx, username); err != nil {
		return err
	}
	// The cut: every stream any of the user's tokens held open ends, and
	// every executor serving that user's children is disconnected — revocation
	// is a security act, so it cuts rather than waiting for the connections
	// to notice. Stream counts are logged by the registry; the executor count
	// rides this line. The pool is nil when no executor listener is
	// configured; the registry is inert on a nil receiver, so only the
	// interface value needs the guard.
	streams := c.streamRevoke.revokeUser(userID)
	var disconnected int
	if c.execPool != nil {
		disconnected = c.execPool.DisconnectOwner(userID)
	}
	slog.Info("user removed", "username", username, "streams_cancelled", streams, "executors_disconnected", disconnected)
	return nil
}
