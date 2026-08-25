package main

import (
	"context"
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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/ring"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/tasksdb"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/version"
)

// Controller wires together the store, child lifecycle, persistence and the
// control.Controller interface. It is safe for concurrent use.
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

	// children is the durable child-state store. Nil when pool is nil, which
	// means children live in memory only and do not survive a restart.
	children childstore.ChildStore

	// leases gates who may WRITE to a conversation. Nil under the same
	// condition as children.
	leases *store.LeaseStore

	// daemonID is this daemon's stable identity: what a lease records as its
	// holder, and what says whose pid namespace the pid column belongs to.
	daemonID string

	// nsToken identifies this daemon's PID namespace so a recorded pid can
	// be proven to belong to the same namespace before it is signalled.
	nsToken string

	// heldLeases maps childID to the conversation lease this daemon holds for
	// it. Guarded by heldLeasesMu.
	heldLeasesMu sync.Mutex
	heldLeases   map[string]store.Lease

	// rawTrace, when non-nil, enables raw LLM API request/response capture to
	// the debug raw_http_request hypertable. Created at daemon startup when
	// RAFIKI_RECORD_REQUESTS=1. Handed to agent children via
	// fundi.RuntimeOptions.RawTrace.
	rawTrace *rawtrace.RawTraceStore

	// insights answers the ctrl_conversation_* RPCs. Always constructed —
	// agentcli/local.New is nil-pool-safe, so a nil pool just means every
	// read method below returns local.ErrNoPool instead of panicking.
	insights agentcli.Backend

	// proxyURL and proxyToken address the daemon's own proxy face, which pi
	// and claude children are pointed at so their turns are captured and
	// routed by the same code the agent kind uses in-process. Set once at
	// startup via SetProxy; empty means no face was started.
	proxyURL   string
	proxyToken string

	// catalog answers ContextWindow (ctrl_get/ctrl_list's ContextWindow/
	// MaxCompletionTokens fields). Set once at startup via SetCatalog, from
	// the SAME *routing.ModelCatalog instance the proxy face's llm.Client
	// uses (main.go builds one and hands it to both) — a nil catalog here
	// just means ContextWindow always returns ok=false, matching "the proxy
	// face failed to start" or any other reason main.go has none to give.
	catalog *routing.ModelCatalog

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

	// native fans rafiki-native events out per child, for the Connect
	// control plane's StreamEvents.
	native *nativebus.Registry

	// coster resolves what an agent subtree has spent. An interface rather
	// than *insights.Insights so the admission logic is testable without a
	// database — the number's correctness is insights' problem, what the
	// controller does with it is this package's.
	coster subtreeCoster

	// breaches bounds the budget sweep to one steer per breach.
	breaches budgetBreaches

	// nudgedOnce bounds prompting.md's enforcement ladder to one nudge per
	// child. Guarded by nudgedMu.
	nudgedMu   sync.Mutex
	nudgedOnce map[string]bool

	// execPool is the live executor connection registry. Nil when the
	// executor listener is not configured.
	execPool executorPool

	// execPoolConn is the SAME pool as execPool, at its concrete type.
	// relayTransport needs execpool.NewProxyTransport, which only the
	// concrete *execpool.Pool can build (it reaches a private method,
	// connectClientFor, that the narrow executorPool interface does not
	// expose); everything else in this file deliberately goes through the
	// interface so selection stays testable without a listener. Nil under
	// exactly the same condition as execPool.
	execPoolConn *execpool.Pool

	// execStore is the durable executor registry. Nil when the executor
	// listener is not configured (require the pool to mint tokens).
	execStore executors.Store

	// users is the identity store backing ctrl_user_*. Nil when RAFIKI_DB is
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

	sessionExecMu sync.Mutex
	sessionExecs  map[control.Connection]sessionExecutor
}

type workspaceLabels struct {
	workspaceID   string
	executorID    string
	mode          string // "ephemeral" or "pinned"
	executorState string // "unbound" until the first successful NoteBinding
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
	}
}

func NewController(st *childstore.Store, stateDir, logsDir, socketPath string, dumper *persist.LogDumper, pool *pgxpool.Pool, rawTrace *rawtrace.RawTraceStore, baseCtx context.Context, execStore executors.Store, userStore users.Store, prov *providers.Set) *Controller {
	gw := 7 * 24 * time.Hour
	if h := paths.Get(paths.GraceHours); h != "" {
		if n, err := strconv.ParseFloat(h, 64); err == nil && n > 0 {
			gw = time.Duration(n * float64(time.Hour))
		}
	}
	c := &Controller{
		st:          st,
		cm:          newChildManager(),
		dumper:      dumper,
		startedAt:   time.Now(),
		socketPath:  socketPath,
		logsDir:     logsDir,
		stateDir:    stateDir,
		graceWindow: gw,
		pool:        pool,
		rawTrace:    rawTrace,
		insights:    local.New(local.Options{Pool: pool}),
		baseCtx:     baseCtx,
		tasks:       taskStore(pool),
		evbuf:       newEventBuffer(),
		execStore:   execStore,
		users:       userStore,
		providers:   prov,
		heldLeases:  make(map[string]store.Lease),
		native:      nativebus.New(),
	}

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
	// coster is nil), while unbudgeted ones are unaffected.
	if pool != nil {
		c.coster = insights.New(pool)
		c.children = childstoredb.New(pool)
		c.leases = store.NewLeases(pool)
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

// startSweeper launches a background goroutine that periodically forgets
// exited children whose age exceeds the configured grace window. It stops
// when ctx is cancelled. Call Stop() to wait for the goroutine to exit.
func (c *Controller) startSweeper(ctx context.Context) {
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.sweepExpired()
				// Budget breaches are checked on the same tick as expiry: both are
				// periodic reconciliations of stored state, and a second ticker would be
				// a second thing to reason about at shutdown.
				sweepCtx, cancel := context.WithTimeout(ctx, budgetSweepTimeout)
				c.sweepBudgets(sweepCtx)
				cancel()
			}
		}
	}()
}

// Stop waits for background goroutines (currently the sweeper) to exit.
// The caller is responsible for cancelling the context passed to startSweeper
// before calling Stop.
func (c *Controller) Stop() {
	c.sweeperWg.Wait()
}

// sweepExpired forgets all exited children whose ExitedAt is older than
// graceWindow. Called periodically by the sweeper goroutine.
func (c *Controller) sweepExpired() {
	cutoff := time.Now().Add(-c.graceWindow)
	var toForget []string
	for _, s := range c.st.FindByStatus(protocol.StatusExited) {
		if !s.ExitedAt.IsZero() && s.ExitedAt.Before(cutoff) {
			toForget = append(toForget, s.ChildID)
		}
	}
	for _, id := range toForget {
		_ = c.Forget(id)
	}
	if len(toForget) > 0 {
		slog.Info("sweep: forgot expired children", "count", len(toForget))
	}
}

// ─── control.Controller implementation ────────────────────────────────────────

func (c *Controller) List(filter protocol.ListFilter) []childstore.Snapshot {
	snaps := c.st.List()
	if filter.Status == "" && filter.Name == "" && filter.NameContains == "" &&
		filter.CwdContains == "" && filter.Since == 0 &&
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
		if filter.Since > 0 && s.StartedAt.UnixMilli() < filter.Since {
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

// ConversationID satisfies connectapi.ConversationResolver: it maps a child
// id to the fundi conversation UUID that owns its persisted message history.
// Only fundi children have a conversation as their session id (see
// pkg/fundi/engine.go, which sets SessionID: conv.ID) — a pi or claude
// child's SessionID means something else entirely (a session file path/id),
// so this deliberately excludes non-fundi kinds rather than handing their
// SessionID to a query expecting a UUID.
func (c *Controller) ConversationID(childID string) (string, bool) {
	snap, ok := c.st.Get(childID)
	if !ok || snap.Kind != protocol.KindFundi || snap.SessionID == "" {
		return "", false
	}
	return snap.SessionID, true
}

func (c *Controller) GetRecent(childID string, q control.RecentQuery) (control.RecentResult, error) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return control.RecentResult{}, &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	ch, alive := c.cm.Get(childID)

	// Select the source event slice based on kind and liveness.
	// Fundi children read from the database (canonical store); pi and
	// claude children keep the ring-buffer + on-disk-dump path.
	var events []ring.Event
	var total int
	var oldestTS int64

	if snap.Kind == protocol.KindFundi {
		events = c.dbRecentForFundi(snap.SessionID, q)
		total = len(events)
		if len(events) > 0 {
			oldestTS = events[0].Timestamp
		}
	} else if alive {
		if q.Rendered && ch.Normalizes() {
			events = ch.RenderRecent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, oldestTS = ch.RenderStats()
		} else {
			r := ch.Ring()
			events = r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, _, oldestTS = r.Stats()
		}
	} else {
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
			out = append(out, json.RawMessage(ev.Bytes))
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

	return control.RecentResult{
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

func (c *Controller) GetStreams(childID string, which string) (control.GetStreamsResult, error) {
	if _, ok := c.st.Get(childID); !ok {
		return control.GetStreamsResult{}, &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	ch, alive := c.cm.Get(childID)
	if !alive {
		return control.GetStreamsResult{Alive: false}, nil
	}
	res := control.GetStreamsResult{Alive: true}
	if which == "" || which == "all" || which == "in" {
		res.In = ch.InSnapshot()
	}
	// Live stderr is intentionally omitted: errBuf is an unguarded bytes.Buffer
	// written by the readStderr goroutine, so StderrSnapshot races until Done()
	// is closed. Stderr for a live child is therefore left nil; callers fall
	// back to the on-disk dump, which becomes available after the child exits.
	return res, nil
}

func (c *Controller) Search(q control.SearchQuery) control.SearchResult {
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
				Timestamp:   ev.Timestamp,
				Snippet:     snippet,
				MatchStart:  idx,
				MatchEnd:    idx + len(q.Query),
			})
			if len(hits) >= limit {
				return control.SearchResult{
					Hits:      hits,
					TotalHits: len(hits),
					Scanned:   scanned,
					Elapsed:   time.Since(start).Milliseconds(),
				}
			}
		}
	}
	return control.SearchResult{
		Hits:      hits,
		TotalHits: len(hits),
		Scanned:   scanned,
		Elapsed:   time.Since(start).Milliseconds(),
	}
}

func (c *Controller) Status() control.ControllerStatus {
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
	return control.ControllerStatus{
		Version:     version.String(),
		StartedAt:   c.startedAt.UnixMilli(),
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
		return &control.ControllerError{
			Code:    protocol.ErrNoAgentDB,
			Message: "no agent database configured (RAFIKI_DB unset); set it and run `rafiki service install`",
		}
	}
	if errors.Is(err, insights.ErrNotFound) {
		return &control.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: err.Error(),
		}
	}
	return err
}

func (c *Controller) ConversationStats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error) {
	st, err := c.insights.Stats(ctx, f)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error) {
	st, err := c.insights.ConversationStats(ctx, id)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationSearch(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
	rows, err := c.insights.Search(ctx, f)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return rows, nil
}

func (c *Controller) ConversationExport(ctx context.Context, id string) (*insights.Transcript, error) {
	tr, err := c.insights.Export(ctx, id)
	if err != nil {
		return nil, translateInsightsErr(err)
	}
	return tr, nil
}

func (c *Controller) Spawn(ctx context.Context, req protocol.SpawnRequest, owner users.Identity) (control.SpawnResult, error) {
	// Validate cwd exists on THIS machine (dispatch already checks it's
	// absolute) — but only for kinds the daemon itself forks a subprocess
	// for (pi, claude; cmd.Dir = req.Cwd in pkg/child/runner.go). A fundi
	// child never touches the daemon's own filesystem: its tools, if any,
	// run against whichever executor gets bound after this point, and that
	// executor validates its own root independently. Stat-ing req.Cwd here
	// for fundi checks the wrong machine — wrongly rejecting a valid path
	// that exists only on a remote executor, or wrongly accepting a
	// coincidentally-existing but unrelated path on the daemon host.
	if req.Kind != protocol.KindFundi {
		if _, err := os.Stat(req.Cwd); err != nil {
			return control.SpawnResult{}, &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: "cwd: " + err.Error(),
			}
		}
	}

	// Validate user-supplied labels: no invalid keys, no rafiki/ prefix.
	if err := validateUserLabelKeys(req.Labels); err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	// Resolve lineage before spawning: a bad parentChildId must fail without
	// leaving a started process behind.
	parentLabel, rootLabel, err := computeLineageLabels(c.st, req.ParentChildID)
	if err != nil {
		return control.SpawnResult{}, err
	}

	// Before the grant is inherited, not after: this asks what the PARENT was
	// confined to, and inheritExecutorGrant would copy that grant onto a child
	// whose kind cannot honour it, making the two indistinguishable.
	if err := checkKindNarrowing(c.st, req); err != nil {
		return control.SpawnResult{}, err
	}

	// A silent executor grant INHERITS the spawner's. Done here, before
	// anything reads req, so every path gets it: the runtime's
	// resolveExecutor, the workspace provisioning check, and the selector
	// stored on the new session.
	req = c.inheritExecutorGrant(req)

	// Resource admission. Deliberately before the childID is minted and long
	// before anything is registered: a refusal must leave no process, no
	// store entry, no record and — with phase 04's ordering — no task
	// assignment to roll back.
	if err := c.checkSpawnLimits(req); err != nil {
		return control.SpawnResult{}, err
	}

	// childID is minted before resolveSpawnPlan (rather than after, as
	// before) because the "fundi" kind needs it to pin --spill-dir
	// (see buildAgentArgv/agentSpillDir).
	childID := newChildID()
	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	env := c.buildEnv(req, childID, c.socketPath)
	if req.Kind == protocol.KindClaude {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	// Computed here, before agentRunner, rather than alongside the rest of
	// initLabels below: agentRunner resolves this child's OWN executor
	// (resolveExecutor -> chooseExecutor), and for a top-level spawn that
	// admission check runs against labels this child does not have a
	// childstore entry to carry yet (see admissionLabels). The owner must
	// already be in hand at that point or a session executor's
	// "admits: owner=<user>" (every one — see ExecutorSession) refuses every
	// top-level spawn outright.
	ownerName := attestOwner(c.st, req, owner)

	runner, err := c.agentRunner(req, childID, false, ownerName)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
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
	if runner != nil {
		// The agent kind's argv is parsed into RuntimeOptions above, not
		// executed; leave PiBinary/Argv empty so nothing accidentally execs it.
		spec.PiBinary = ""
		spec.Argv = nil
	}

	now := time.Now()

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Register the child in the ChildManager immediately after spawn so that any
	// global subscriber that calls ctrl_subscribe in response to
	// ctrl_child_spawned (below) will find the child ready in the manager.
	// monitorChild is still started after Idle(), but the Subscribe endpoint
	// must be non-racy from the moment the spawn event is visible.
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
	if wl, ok := c.takeWorkspaceLabels(childID); ok {
		initLabels["rafiki/workspace"] = wl.workspaceID
		initLabels["rafiki/executor"] = wl.executorID
		initLabels["rafiki/workspace-mode"] = wl.mode
		if wl.executorState != "" {
			initLabels["rafiki/executor-state"] = wl.executorState
		}
	}

	// FIX 5: Insert a minimal record at StatusSpawning immediately after the
	// process is confirmed running. A crash between exec and Idle() would
	// otherwise leave an orphan pi process with no persisted record.
	sess := &childstore.Session{
		ChildID:      childID,
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
		Verbose:            req.Verbose,
		PiBinary:           bin,
		ExtraArgs:          req.ExtraArgs,
		RecordRequests:     req.RecordRequests,
		ExecutorSelector:   req.ExecutorSelector,
		WorkspaceMode:      req.WorkspaceMode,
		MaxDepth:           grantedDepth(req, childDepthFor(c.st, req.ParentChildID), resolveAbsoluteDepthCeiling()),
		MaxCost:            grantedCost(req),
		MaxChildren:        grantedChildren(req),
	}
	c.st.Insert(sess)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record (spawning)", "childId", childID, "error", err)
	}

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

	// Emit ctrl_child_spawned immediately after the process is running and the
	// state record is persisted (spec §6.3.3, §7.2). Delivered to global,
	// per-child, and label-filtered subscribers.
	spawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    req.Name,
		Cwd:     req.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(req.Provider, req.Model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(spawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}

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
// StatusSpawning session, called cm.Add, and emitted ctrl_child_spawned.
// This method performs name reconciliation (if req.Name is set), updates the
// existing session with post-Idle metadata, persists a record, emits the
// spawning→idle status transition, and starts monitorChild.
//
// Resume / RespawnChild (baseSnap != nil): the caller has already deleted the
// old exited session. noSession/resumeSession/forkSession are the session-
// continuity fields that differ between the two callers. This method builds
// the full Session from baseSnap with those overrides, inserts it, calls
// cm.Add, persists a record, emits ctrl_child_spawned, and starts monitorChild.
func (c *Controller) activateLiveChild(
	childID string,
	ch *child.Child,
	piBin string,
	req protocol.SpawnRequest,
	baseSnap *childstore.Snapshot,
	noSession bool,
	resumeSession string,
	forkSession string,
) (control.SpawnResult, error) {
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
		if !stalled && req.Kind != protocol.KindClaude && req.Name != "" && meta.SessionName != req.Name {
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

		return control.SpawnResult{
			ChildID:     childID,
			SessionID:   meta.SessionID,
			SessionFile: meta.SessionFile,
			Model:       joinModel(provider, model),
			Stalled:     stalled,
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
	// spawning→idle after the ctrl_child_spawned below would describe a
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

	sess := &childstore.Session{
		ChildID:            childID,
		PID:                ch.PID(),
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
		Provider:           provider,
		Model:              model,
		Thinking:           snap.Thinking,
		SessionID:          meta.SessionID,
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
		Verbose:            snap.Verbose,
		PiBinary:           piBin,
		ExtraArgs:          snap.ExtraArgs,
		RecordRequests:     snap.RecordRequests,
		ExecutorSelector:   snap.ExecutorSelector,
		WorkspaceMode:      snap.WorkspaceMode,
		MaxDepth:           snap.MaxDepth,
		MaxCost:            snap.MaxCost,
		MaxChildren:        snap.MaxChildren,
	}
	c.st.Insert(sess)
	c.cm.Add(childID, ch)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned for the resumed/respawned child (spec §7.2).
	spawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    snap.Name,
		Cwd:     snap.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(provider, model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(spawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if spawnSnap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, spawnSnap.Labels, b)
		}
	}

	go c.monitorChild(childID, ch)

	return control.SpawnResult{
		ChildID:     childID,
		SessionID:   meta.SessionID,
		SessionFile: meta.SessionFile,
		Model:       joinModel(provider, model),
		Stalled:     stalled,
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
		Verbose:            snap.Verbose,
		PiBinary:           snap.PiBinary,
		ExtraArgs:          snap.ExtraArgs,
		ExecutorSelector:   snap.ExecutorSelector,
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

func (c *Controller) Resume(ctx context.Context, childID string, apiKey string) (control.SpawnResult, error) {
	return c.resumeInternal(ctx, childID, apiKey, false)
}

// resumeWithAutoRecovery is the auto-recovery version of Resume: it sets
// AutoResume on the engine so the worker calls agentloop.Resume on startup
// (finalising any incomplete previous turn) before accepting inbound prompts.
func (c *Controller) resumeWithAutoRecovery(ctx context.Context, childID string) (control.SpawnResult, error) {
	return c.resumeInternal(ctx, childID, "", true)
}

// resumeInternal is the shared implementation of Resume and auto-recovery resume.
func (c *Controller) resumeInternal(ctx context.Context, childID string, apiKey string, autoResume bool) (control.SpawnResult, error) {
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
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "resume already in progress for child: " + childID,
		}
	}
	defer c.spawnClaims.release(childID)

	snap, ok := c.st.Get(childID)
	if !ok {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "child is not exited (status: " + string(snap.Status) + ")",
		}
	}

	kind := snap.Kind
	if kind == "" {
		kind = protocol.KindPi
	}

	// Verify the session file exists for pi children that have one. Claude does
	// not track a session file (it manages its own ~/.claude store keyed by
	// session id), so there is nothing to stat.
	if kind == protocol.KindPi && !snap.NoSession && snap.SessionFile != "" {
		if _, err := os.Stat(snap.SessionFile); err != nil {
			return control.SpawnResult{}, &control.ControllerError{
				Code:    protocol.ErrSessionFileMissing,
				Message: "session file not found: " + snap.SessionFile,
			}
		}
	}

	req := resumeRequestFromSnapshot(snap, apiKey)

	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	env := c.buildEnv(req, childID, c.socketPath)
	if kind == protocol.KindClaude {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	// The owner was already attested at the child's original spawn (see
	// Spawn's ownerName computation) and lives on in snap.Labels; a resume
	// re-derives nothing, it reuses that value so chooseExecutor's admission
	// check (for an ExecutorSelector carried over from snap) sees the same
	// owner the executor's row was minted to admit.
	runner, err := c.agentRunner(req, childID, autoResume, snap.Labels["owner"])
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
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
	if runner != nil {
		spec.PiBinary = ""
		spec.Argv = nil
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Spawn succeeded — only now remove the old exited entry. Deleting before
	// spawn would lose the session if spawn fails (e.g. bad pi binary path);
	// the entry would only be recoverable on controller restart via state scan.
	c.st.Delete(childID)

	// activateLiveChild (baseSnap != nil path): waits for Idle, builds the full
	// Session from snap with Resume's session-continuity values, inserts it,
	// adds to cm, persists, emits ctrl_child_spawned, starts monitorChild.
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
// delete-and-replace the record), reached via a concurrent ctrl_send
// {new_session|switch_session} on the same childID (see
// handleInterceptedSend), and a shared claim set also blocks the cross-path
// case of a resume racing an intercepted respawn for the same exited
// childID. See childClaimSet's doc comment for the full rationale.
func (c *Controller) RespawnChild(ctx context.Context, childID, sessionPath string) (control.SpawnResult, error) {
	if !c.spawnClaims.tryClaim(childID) {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "respawn already in progress for child: " + childID,
		}
	}
	defer c.spawnClaims.release(childID)

	snap, ok := c.st.Get(childID)
	if !ok {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return control.SpawnResult{}, &control.ControllerError{
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
		kind = protocol.KindPi
	}

	bin, argv, prov, err := resolveSpawnPlan(req, childID, c.stateDir)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	env := c.buildEnv(req, childID, c.socketPath)
	if kind == protocol.KindClaude {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	// See Resume's identical call: the owner was attested at the child's
	// original spawn and lives on in snap.Labels.
	runner, err := c.agentRunner(req, childID, false, snap.Labels["owner"])
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "agent runner: " + err.Error(),
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
	if runner != nil {
		spec.PiBinary = ""
		spec.Argv = nil
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return control.SpawnResult{}, &control.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Spawn succeeded — remove the old exited entry only after the new process
	// is confirmed running (so a failed spawn doesn't lose the record).
	c.st.Delete(childID)

	// activateLiveChild (baseSnap != nil path): waits for Idle, builds the full
	// Session from snap with RespawnChild's session-continuity values (fresh
	// start: noSession=false, resumeSession=sessionPath, forkSession=""),
	// inserts, adds to cm, persists, emits ctrl_child_spawned, starts monitorChild.
	return c.activateLiveChild(childID, ch, bin, protocol.SpawnRequest{}, &snap,
		false, sessionPath, "")
}

// exitPersistDeadline bounds the wait for handleChildExit to finish after a
// child process has been reaped. It is a backstop, not a timeout anyone
// should hit: that goroutine only has to run MarkExited and cm.Remove.
const exitPersistDeadline = 2 * time.Second

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

func (c *Controller) Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int64) (control.KillResult, error) {
	ch, ok := c.cm.Get(childID)
	if !ok {
		if snap, ok2 := c.st.Get(childID); ok2 && snap.Status == protocol.StatusExited {
			return control.KillResult{}, &control.ControllerError{
				Code:    protocol.ErrChildExited,
				Message: "child has already exited",
			}
		}
		return control.KillResult{}, &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	// Drive the SM to shutting_down so ctrl_child_status subscribers see the
	// transition before the graceful-shutdown sequence begins (spec §6.5).
	if changed, prev := ch.BeginShutdown(); changed {
		c.handleStatusChange(childID, protocol.StatusShuttingDown, prev)
	}

	shutdownTimeout := durOrDefault(shutdownTimeoutMs, 180*time.Second)
	killTimeout := durOrDefault(killTimeoutMs, 30*time.Second)

	res, err := ch.Shutdown(shutdownTimeout, killTimeout)
	if err != nil {
		return control.KillResult{}, fmt.Errorf("shutdown: %w", err)
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
	return control.KillResult{
		ExitCode:   exitCode,
		Signal:     res.Signal,
		DurationMs: res.Duration.Milliseconds(),
		Escalated:  res.Escalated,
		Abandoned:  res.Abandoned,
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
	ids := c.cm.LiveIDs()
	if len(ids) == 0 {
		return nil
	}

	slog.Info("shutting down children", "count", len(ids))

	type result struct {
		id        string
		err       error
		abandoned bool
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
		// Drive SM to shutting_down and emit ctrl_child_status to subscribers
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
			done <- result{id: id, err: err, abandoned: res.Abandoned}
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
				slog.Warn("child shutdown error", "childId", r.id, "error", r.err)
				errs = append(errs, fmt.Errorf("child %s: %w", r.id, r.err))
			case r.abandoned:
				// Not an error — Shutdown did everything it could — but "shut
				// down" would be a lie: the goroutine is still in there.
				slog.Error("child abandoned rather than reaped; its execution context is leaked", "childId", r.id)
			default:
				slog.Info("child shut down", "childId", r.id)
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

func (c *Controller) Forget(childID string) error {
	// Wait for handleChildExit (running on monitorChild's goroutine) to finish
	// before we touch on-disk state. Two races are possible when forget arrives
	// immediately after a kill:
	//
	//  1. MarkExited hasn't run yet — snap.Status is still streaming/idle, not
	//     "exited".  cm still holds the child.
	//  2. MarkExited has run (status=exited) but cm.Remove hasn't yet — the
	//     child is still in cm.  Without this wait, our delete can race with
	//     writeRecord's atomic-rename: writeRecord's .tmp is in-progress when
	//     Forget runs os.Remove(.json) — finds nothing — then writeRecord
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
		return &control.ControllerError{Code: protocol.ErrNotFound, Message: "child not found: " + childID}
	}
	if snap.Status != protocol.StatusExited {
		return &control.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"}
	}

	c.st.Delete(childID)
	if c.children != nil && c.ownsChildRow(snap) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.children.Delete(ctx, childID); err != nil {
			slog.Warn("delete child row", "childId", childID, "error", err)
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
	return nil
}

// deleteLogDump removes the per-child log dump directory at ~/.pi/run/logs/<childID>.
// Forget calls this so 'rafiki forget' fully removes the child's footprint rather
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

// deleteSpillDir removes an agent-kind child's clipped-tool-output spill
// directory (see buildAgentArgv/agentSpillDir). Forget/ForgetAllExited call
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

func (c *Controller) ForgetAllExited(olderThanMs int64) (int, error) {
	snaps := c.st.FindByStatus(protocol.StatusExited)
	now := time.Now().UnixMilli()
	count := 0
	for _, s := range snaps {
		if olderThanMs > 0 && !s.ExitedAt.IsZero() {
			age := now - s.ExitedAt.UnixMilli()
			if age < olderThanMs {
				continue
			}
		}
		c.st.Delete(s.ChildID)
		if c.children != nil && c.ownsChildRow(s) {
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
		count++
	}
	return count, nil
}

// SetLabels mutates labels on the named child. Rejects keys with the rafiki/
// prefix or invalid characters. Emits ctrl_child_labeled to subscribers.
func (c *Controller) SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error) {
	if _, ok := c.st.Get(childID); !ok {
		return nil, &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if err := validateUserLabelKeys(set); err != nil {
		return nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if err := validateUserRemoveKeys(remove); err != nil {
		return nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	merged, err := c.st.SetLabels(childID, set, remove)
	if err != nil {
		return nil, &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after set_labels", "childId", childID, "error", err)
	}
	c.emitChildLabeled(childID, merged)
	return merged, nil
}

// emitChildLabeled broadcasts a ctrl_child_labeled event carrying the full
// post-mutation label map to global, per-child, and label-filtered subscribers.
//
// Label-filtered delivery uses the NEW (post-mutation) labels for matching.
// Subscribers that matched the old labels but not the new simply stop
// receiving future events — no synthetic "left filter" event is emitted (v1).
func (c *Controller) emitChildLabeled(childID string, labels map[string]string) {
	evt := protocol.CtrlChildLabeled{
		Type:    protocol.TypeCtrlChildLabeled,
		ChildID: childID,
		Labels:  labels,
	}
	if b, err := json.Marshal(evt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		// Use new labels for dynamic filter evaluation.
		c.cm.DeliverToMatching(childID, labels, b)
	}
}

func (c *Controller) Send(childID string, frame json.RawMessage) error {
	// new_session and switch_session are handled via kill+respawn rather than
	// forwarded to pi (spec §5.1).
	if decision, ok := inspect(frame); ok {
		return c.handleInterceptedSend(childID, decision)
	}

	// Claude abort: claude -p has no in-band abort frame and can only be
	// interrupted by signalling the process. Intercept abort for claude
	// children and run the interrupt+resume cycle; pi children fall through and
	// forward abort natively to --mode rpc.
	if isAbortFrame(frame) {
		if snap, ok := c.st.Get(childID); ok && snap.Kind == protocol.KindClaude {
			return c.handleClaudeAbort(childID)
		}
	}

	snap, ok := c.st.Get(childID)
	if !ok {
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if snap.Status == protocol.StatusShuttingDown {
		return &control.ControllerError{Code: protocol.ErrChildShuttingDown, Message: "child is shutting down"}
	}
	if snap.Status == protocol.StatusExited {
		return &control.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
	}

	ch, ok := c.cm.Get(childID)
	if !ok {
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
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
			return &control.ControllerError{Code: protocol.ErrBackpressure, Message: msg}
		}
		if strings.Contains(msg, "shutting down") {
			return &control.ControllerError{Code: protocol.ErrChildShuttingDown, Message: msg}
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
		return &control.ControllerError{
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
		return &control.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: string(decision.Type) + " is not supported for an agent child: an agent conversation is " +
				"identified by the child id itself, so a respawn would silently reattach the same conversation " +
				"rather than starting a new one. Spawn a new agent child instead.",
		}
	}

	// Save per-child subscribers before Kill. monitorChild.Remove (called by
	// handleChildExit) will clear the list when the old process exits.
	savedSubs := c.cm.GetSubscribers(childID)

	// Gracefully shut down the current child.
	if _, err := c.Kill(context.Background(), childID, 3000, 500); err != nil {
		var ce *control.ControllerError
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

	// Restore preserved subscriptions on the new child instance and deliver
	// the synthetic pi-level response so subscribers observe the transition.
	// Wrap in ctrl_event so subscribers see the correct envelope shape (§7.1).
	c.cm.RestoreSubscribers(childID, savedSubs)
	synthFrame := synthesizeResponse(string(decision.Type), decision.PiRequestID)
	c.cm.DeliverToChild(childID, wrapCtrlEvent(childID, synthFrame))

	return nil
}

// isAbortFrame reports whether a ctrl_send frame is the normalized abort
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
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if snap.Status == protocol.StatusShuttingDown {
		return &control.ControllerError{Code: protocol.ErrChildShuttingDown, Message: "child is shutting down"}
	}
	if snap.Status == protocol.StatusExited {
		return &control.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
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
		return &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: "cannot abort claude child before its session is established"}
	}
	if snap.SessionID != sessionID {
		_ = c.st.Update(childID, func(s *childstore.Session) { s.SessionID = sessionID })
	}

	// Save subscribers before exit: handleChildExit clears them on process exit.
	savedSubs := c.cm.GetSubscribers(childID)

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
		if _, err := c.Kill(context.Background(), childID, 1000, 500); err != nil {
			var ce *control.ControllerError
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
	// silently deleted, causing the restored subscribers to be dropped.
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

	c.cm.RestoreSubscribers(childID, savedSubs)
	return nil
}

func (c *Controller) Subscribe(childID string, conn control.Connection, filter protocol.SubscribeFilter) error {
	if _, ok := c.st.Get(childID); !ok {
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	c.cm.Subscribe(childID, conn, filter)
	return nil
}

func (c *Controller) Unsubscribe(childID string, conn control.Connection) error {
	c.cm.Unsubscribe(childID, conn)
	return nil
}

func (c *Controller) GlobalSubscribe(conn control.Connection) error {
	c.cm.GlobalSubscribe(conn)
	return nil
}

func (c *Controller) GlobalUnsubscribe(conn control.Connection) error {
	c.cm.GlobalUnsubscribe(conn)
	return nil
}

// SubscribeLabeled registers conn as a label-filtered subscriber. Events are
// delivered from every child whose labels match the filter, evaluated
// dynamically on each event. A nil filter means "pass everything".
// Cleanup occurs automatically in OnConnectionClose.
func (c *Controller) SubscribeLabeled(conn control.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
	var fp *protocol.SubscribeFilter
	if filter.Profile != "" || len(filter.Include) > 0 || len(filter.Exclude) > 0 {
		f := filter
		fp = &f
	}
	c.cm.RegisterLabeled(conn, labels, hasLabel, fp)
	return nil
}

// OnConnectionClose is called by the server when a client connection closes.
// It removes global and label-filtered subscriptions held by this connection.
//
// Note: per-child subscribers for this connection are not cleaned up here;
// they accumulate until the child is removed from the ChildManager on exit.
// This is a known limitation — per-child sub sets are bounded by the child
// lifetime and the subscriber count is small in practice.
func (c *Controller) OnConnectionClose(conn control.Connection) {
	c.releaseSessionExecutor(conn)
	c.cm.GlobalUnsubscribe(conn)
	c.cm.RemoveLabeledSubsForConn(conn)
}

// ─── monitorChild ─────────────────────────────────────────────────────────────

// monitorChild runs as a goroutine for each live child. It forwards bus events
// to per-child subscribers (wrapped in ctrl_event envelopes per §7.1), delivers
// status transitions, handles rename detection, model-label updates, and child exit.
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
		case frame, ok := <-busCh:
			if !ok {
				// Bus was closed (shouldn't happen in normal operation).
				// Drain first: a transition recorded just before the bus went
				// away is still owed to subscribers, and handleChildExit reports
				// the store's status as last_status.
				drainStatus()
				c.handleChildExit(childID, ch)
				return
			}
			// Wrap in ctrl_event envelope so subscribers can correlate events to
			// their source child (spec §7.1).
			wrapped := wrapCtrlEvent(childID, frame)
			c.cm.DeliverToChild(childID, wrapped)
			// Deliver to label-filtered subscribers using the child's current labels.
			if snap, ok := c.st.Get(childID); ok {
				c.cm.DeliverToMatching(childID, snap.Labels, wrapped)
			}

			// Emit any status transitions this frame (or an earlier one) caused,
			// keeping store + subscribers in sync. Draining here rather than only
			// on the StatusChanged wake keeps a status change behind the frames
			// that produced it in the common case: handleFrame records a
			// transition only after publishing the frame that caused it.
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
			// Update the store, persist, and emit ctrl_child_labeled.
			if md.Model != "" && md.Model != lastKnownModel {
				c.handleModelChange(childID, md.Model)
				lastKnownModel = md.Model
			}

			// Sync session id / file once they appear. For ReadyOnSpawn children
			// (claude) the process is silent until prompted, so these are unknown
			// at spawn and only surface on the first turn's init; without this the
			// store would keep the empty session id captured at activate time and
			// resume could not re-attach.
			if (md.SessionID != "" && md.SessionID != lastKnownSessionID) ||
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
			// Drain any frames that arrived before the done signal.
			drained := false
			for !drained {
				select {
				case frame, ok := <-busCh:
					if !ok {
						drained = true
					} else {
						wrapped := wrapCtrlEvent(childID, frame)
						c.cm.DeliverToChild(childID, wrapped)
						if snap, ok := c.st.Get(childID); ok {
							c.cm.DeliverToMatching(childID, snap.Labels, wrapped)
						}
					}
				default:
					drained = true
				}
			}
			// Then the transitions those frames caused, BEFORE handleChildExit:
			// a transition recorded just before exit is still owed to
			// subscribers, and after handleChildExit it would be unreachable
			// (cm.Remove clears the per-child subscriber list) as well as
			// announced after ctrl_child_exited. Draining first also means the
			// exit event's last_status reflects the child's real final status.
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

// wrapCtrlEvent wraps a raw pi event in a ctrl_event envelope (spec §7.1).
// This lets per-child subscribers correlate events to their source child and
// filter by inner event type.
func wrapCtrlEvent(childID string, raw []byte) []byte {
	env := protocol.CtrlEvent{
		Type:    protocol.TypeCtrlEvent,
		ChildID: childID,
		Event:   json.RawMessage(raw),
	}
	b, _ := json.Marshal(env)
	return b
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
	storePrev, ok := c.st.SetStatus(childID, newStatus)
	// Release any event batches deferred while this child was mid-turn.
	// This is rafiki's turn-end drain; it is why no busy-poller is needed.
	if ok && newStatus == protocol.StatusIdle && storePrev != protocol.StatusIdle && c.evbuf != nil {
		if isWorkingStatus(storePrev) {
			c.notifySubagentSettled(childID, "settled (idle)")
		}
		c.evbuf.DrainIdle(childID)
	}
	now := time.Now()
	evt := protocol.CtrlChildStatus{
		Type:     protocol.TypeCtrlChildStatus,
		ChildID:  childID,
		Status:   string(newStatus),
		Previous: string(prev),
		At:       now.UnixMilli(),
	}
	if b, err := json.Marshal(evt); err == nil {
		// Deliver to global, per-child, and label-filtered subscribers (spec §7.4).
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}
}

// handleModelChange updates the store's Provider/Model fields and the
// rafiki/model + rafiki/provider auto-labels when the sniffer detects a model change
// via set_model or cycle_model responses. Emits ctrl_child_labeled.
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
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after model change", "childId", childID, "error", err)
	}
	c.emitChildLabeled(childID, snap.Labels)
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

// handleChildRenamed updates the store and emits ctrl_child_renamed when the
// sniffer detects that pi changed the session name (spec §7.5).
func (c *Controller) handleChildRenamed(childID, newName, previous string) {
	_ = c.st.Rename(childID, newName)
	evt := protocol.CtrlChildRenamed{
		Type:     protocol.TypeCtrlChildRenamed,
		ChildID:  childID,
		Name:     newName,
		Previous: previous,
		At:       time.Now().UnixMilli(),
	}
	if b, err := json.Marshal(evt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}
}

func (c *Controller) handleChildExit(childID string, ch *child.Child) {
	res := ch.ExitResult()
	now := time.Now()

	// Determine last known status before marking exited.
	snap, _ := c.st.Get(childID)
	lastStatus := string(snap.Status)
	// When the daemon gracefully shut this child down, the store status is
	// "shutting_down" — but the record's LastStatus must reflect the child's
	// real pre-exit state (idle, streaming, etc.) so loadOrphans can decide
	// whether it should be auto-recovered on daemon restart.
	if lastStatus == string(protocol.StatusShuttingDown) {
		if ps := ch.PreShutdownStatus(); ps != "" {
			lastStatus = string(ps)
		}
	}

	// Snapshot the ring before removing the child so ctrl_get_recent continues
	// to work after the child is gone (spec §11.4).
	ringSnapshot := ch.Ring().Recent(ring.Query{})
	// RenderRecent returns the render-ring events WITH real timestamps (nil for
	// pi children that have no render-ring), so Since-filtering on the in-memory
	// ExitedRenderRing stays consistent with the live render path.
	renderEvents := ch.RenderRecent(ring.Query{})

	// Persist the record BEFORE MarkExited so LastStatus reflects the pre-exit
	// state (idle, streaming, etc.) rather than just "exited". The persisted
	// LastStatus is what loadOrphans reads to decide whether a fundi child
	// should be auto-resumed on daemon restart.
	if err := c.writeRecordLastStatus(childID, lastStatus); err != nil {
		slog.Warn("write state record on exit", "childId", childID, "error", err)
	}

	// MarkExited sets Status, ExitedAt, ExitCode, ExitSignal, and ExitedRing
	// atomically under one sess.mu hold so a concurrent Snapshot() cannot
	// observe Status=Exited with ExitedRing still nil.
	c.st.MarkExited(childID, now, res.ExitCode, res.Signal, ringSnapshot, renderEvents)

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

	var exitCode *int
	if res.Signal == "" {
		code := res.ExitCode
		exitCode = &code
	}
	exitEvt := protocol.CtrlChildExited{
		Type:       protocol.TypeCtrlChildExited,
		ChildID:    childID,
		ExitCode:   exitCode,
		Signal:     res.Signal,
		LastStatus: lastStatus,
		Duration:   res.Duration.Seconds(),
		At:         now.UnixMilli(),
	}
	if b, err := json.Marshal(exitEvt); err == nil {
		// Deliver to per-child and global subscribers BEFORE Remove so the
		// subscriber list is still reachable (spec §7.3).
		c.cm.DeliverToChild(childID, b)
		c.cm.DeliverToGlobal(b)
		// Deliver to label-filtered subscribers; read snap (already marked exited).
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}

	// Tell the parent its worker is gone. This runs before Forget (which
	// clears batches aimed AT this child, not at its parent) and before
	// cm.Remove, which is the observable "teardown complete" signal.
	c.notifySubagentSettled(childID, "exited")

	// Drop any buffered events aimed at this child. It will never transition
	// to idle again, so DrainIdle can never clear them.
	if c.evbuf != nil {
		c.evbuf.Forget(childID)
	}

	c.nudgedMu.Lock()
	delete(c.nudgedOnce, childID)
	c.nudgedMu.Unlock()

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
	c.cm.Remove(childID)
}

// ─── persistence ─────────────────────────────────────────────────────────────

func (c *Controller) writeRecord(childID string) error {
	return c.writeRecordLastStatus(childID, "")
}

// writeRecordLastStatus persists the child's record. lastStatus, when non-empty,
// is the pre-exit state recorded by handleChildExit rather than the store's
// current Status (already "exited" after MarkExited). It is written only on
// that path; the upsert COALESCEs an empty value so an ordinary status write
// cannot blank the column the recovery predicate reads.
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
			slog.Warn("write child row", "childId", childID, "error", err)
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
		return "", "", &control.ControllerError{
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

func resolvePiBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := paths.Get(paths.PiBinary); env != "" {
		return env, nil
	}
	return exec.LookPath("pi")
}

// buildArgv converts a SpawnRequest into the pi CLI argument list (excluding
// the binary itself). Always starts with --mode rpc.
func buildArgv(req protocol.SpawnRequest) []string {
	var argv []string
	argv = append(argv, "--mode", "rpc")

	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if req.Provider != "" {
		argv = append(argv, "--provider", req.Provider)
	}
	if req.Thinking != "" {
		argv = append(argv, "--thinking", req.Thinking)
	}
	if req.APIKey != "" {
		argv = append(argv, "--api-key", req.APIKey)
	}

	// Session flags.
	if req.NoSession {
		argv = append(argv, "--no-session")
	}
	if req.SessionDir != "" {
		argv = append(argv, "--session-dir", req.SessionDir)
	}
	if req.ResumeSession != "" {
		argv = append(argv, "--session", req.ResumeSession)
	}
	if req.ForkSession != "" {
		argv = append(argv, "--fork", req.ForkSession)
	}

	// Tool / extension / skill scoping.
	if req.Tools != "" {
		argv = append(argv, "--tools", req.Tools)
	}
	if req.NoTools {
		argv = append(argv, "--no-tools")
	}
	if req.NoBuiltinTools {
		argv = append(argv, "--no-builtin-tools")
	}
	for _, ext := range req.Extensions {
		argv = append(argv, "--extensions", ext)
	}
	if req.NoExtensions {
		argv = append(argv, "--no-extensions")
	}
	for _, sk := range req.Skills {
		argv = append(argv, "--skills", sk)
	}
	if req.NoSkills {
		argv = append(argv, "--no-skills")
	}
	for _, pt := range req.PromptTemplates {
		argv = append(argv, "--prompt-templates", pt)
	}
	if req.NoPromptTemplates {
		argv = append(argv, "--no-prompt-templates")
	}
	for _, th := range req.Themes {
		argv = append(argv, "--themes", th)
	}
	if req.NoThemes {
		argv = append(argv, "--no-themes")
	}
	if req.NoContextFiles {
		argv = append(argv, "--no-context-files")
	}

	// System prompt.
	if req.SystemPrompt != "" {
		argv = append(argv, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", req.AppendSystemPrompt)
	}

	if req.Verbose {
		argv = append(argv, "--verbose")
	}

	// Extra args are appended last (last-flag-wins override).
	argv = append(argv, req.ExtraArgs...)
	return argv
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

// buildClaudeArgv converts a SpawnRequest into the claude CLI argument list
// (excluding the binary itself) for stream-json bidirectional driving.
func buildClaudeArgv(req protocol.SpawnRequest) []string {
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		// AskUserQuestion has no interactive renderer in headless -p mode: claude
		// self-resolves it with an error ("Answer questions?") in the same turn,
		// then the agent falls back to asking in prose — which round-trips fine
		// over the prompt/steer channel. Disallow the dead tool so it never wastes
		// a turn attempting it. Variadic flag stops at the next --option below.
		"--disallowedTools", "AskUserQuestion",
	}
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if req.ResumeSession != "" {
		argv = append(argv, "--resume", req.ResumeSession)
	}
	if req.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", req.AppendSystemPrompt)
	}
	argv = append(argv, req.ExtraArgs...)
	return argv
}

// resolveSpawnPlan picks the binary, argv, and ProtocolProvider for a spawn
// request based on its Kind. Empty Kind defaults to "pi". Shared by Spawn and
// Resume so the two paths can never diverge on protocol selection.
//
// childID and stateDir are only used by the "fundi" kind, which needs both to
// pin --spill-dir to a location Forget can find deterministically later (see
// buildAgentArgv/agentSpillDir). claude/pi ignore them.
func resolveSpawnPlan(req protocol.SpawnRequest, childID, stateDir string) (bin string, argv []string, prov child.ProtocolProvider, err error) {
	kind := req.Kind
	if kind == "" {
		kind = protocol.KindPi
	}
	switch kind {
	case protocol.KindClaude:
		bin, err = resolveClaudeBinary(req.PiBinary)
		return bin, buildClaudeArgv(req), child.ClaudeProvider{}, err
	case protocol.KindPi:
		bin, err = resolvePiBinary(req.PiBinary)
		return bin, buildArgv(req), child.PiProvider{}, err
	case protocol.KindFundi:
		// The fundi runtime is `rafikid fundi ...`: the daemon re-execs itself
		// rather than shelling out to a separate binary. It speaks pi's rpc
		// protocol natively (pkg/fundi/frontend.go), so no translator is
		// needed - child.PiProvider{} is the correct identity, same as the
		// "pi" case above.
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
		return self, buildAgentArgv(req, childID, stateDir), child.PiProvider{}, nil
	default:
		return "", nil, nil, fmt.Errorf("unknown kind: %s", kind)
	}
}

// agentSpillDir returns the deterministic spill directory for an agent-kind
// child's clipped tool output: <stateDir>/spill/<childID>. Shared by
// buildAgentArgv (which pins the child's --spill-dir here, overriding Task
// 14's own os.TempDir()-based default) and Forget/ForgetAllExited (which
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
	if len(req.Skills) > 0 {
		argv = append(argv, "--skills", strings.Join(req.Skills, ","))
	}
	if req.NoSkills {
		argv = append(argv, "--no-skills")
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
// the rafiki/kind auto-label. Empty defaults to "pi" (the implicit default kind),
// matching resolveSpawnPlan's kind handling.
func spawnKindLabel(kind string) string {
	if kind == "" {
		return protocol.KindPi
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
// (honoured in child.Spawn, not here).
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
func (c *Controller) buildEnv(req protocol.SpawnRequest, childID, socketPath string) []string {
	var env []string
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
	env = append(env, c.proxyChildEnv(req, childID)...)
	return env
}

// proxyChildEnv returns the variables that point a child at the rafiki proxy,
// or nothing when no proxy is configured or this kind is not routed.
//
// The agent kind is never routed: it reaches rafiki in-process through pkg/llm
// and pkg/routing, so there is no HTTP face to point it at, and doing so would
// put a network hop in front of a library call.
func (c *Controller) proxyChildEnv(req protocol.SpawnRequest, childID string) []string {
	url, token := c.proxyURL, c.proxyToken
	// An explicit RAFIKI_URL points children at an external rafiki
	// instead of the embedded face — useful for aiming a whole machine at a
	// shared capture server. Its token comes from the environment file.
	if v := paths.Get(paths.URL); v != "" {
		url, token = v, paths.Get(paths.Token)
	}
	if url == "" || req.Kind == protocol.KindFundi || !proxyRoutesKind(req.Kind) {
		return nil
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
		additions, _ := proxyenv.Claude(nil, proxyenv.ClaudeOptions{
			URL: url, Token: token, Model: req.Model, Headers: headers,
		})
		return additions
	default:
		// pi has no ANTHROPIC_BASE_URL equivalent. It reads these in the
		// rafiki-helpers extension, which is where its provider override is
		// registered.
		return []string{
			paths.URL + "=" + url,
			paths.Token + "=" + token,
			"RAFIKI_SESSION_REF=" + childID,
		}
	}
}

// proxyRoutesKind reports whether kind is listed in RAFIKI_PROXY_KINDS, which
// defaults to "pi,claude".
func proxyRoutesKind(kind string) bool {
	kinds := splitComma(paths.Get(paths.ProxyKinds))
	if len(kinds) == 0 {
		kinds = []string{protocol.KindPi, protocol.KindClaude}
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

func durOrDefault(ms int64, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

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
	if f.Since > 0 && snap.StartedAt.UnixMilli() < f.Since {
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

// TaskList queries the task ledger for the ctrl_task_list verb.
//
// No conversation scope: this verb answers "what is every agent doing",
// which is a cross-conversation question. The dispatcher clamps Limit before
// calling, and the store applies it after sorting, which is what keeps the
// response inside protocol.MaxFrameBytes.
func (c *Controller) TaskList(ctx context.Context, req protocol.TaskListRequest) ([]tasks.Task, error) {
	if c.tasks == nil {
		return nil, &control.ControllerError{
			Code:    protocol.ErrNoAgentDB,
			Message: "task ledger unavailable: no database configured",
		}
	}

	f := tasks.ListFilter{
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
		return &control.ControllerError{
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
		// naming a new executor (ctrl_executor_create) and one renaming an
		// existing one (ctrl_executor_label), so it must not say "--name",
		// which the label verb has no flag for, nor "relabel the existing
		// executor", which on the label path is the thing they just tried.
		// What both need is the same: which executor is holding the name.
		return &control.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "that executor name is already taken for this owner — (owner, " +
				"machine) names exactly one executor. Choose a different name, or " +
				"free this one by relabelling or deleting whichever executor holds " +
				"it: `rafiki executor list --selector machine=<name>`",
		}
	default:
		return err
	}
	return &control.ControllerError{Code: code, Message: err.Error()}
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
	if _, ok := given["owner"]; ok {
		return nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "owner is derived from the connection and cannot be set with --label",
		}
	}
	if _, ok := given["machine"]; ok {
		return nil, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "machine is set with --name, not with --label",
		}
	}
	owner, err := sessionOwner(id)
	if err != nil {
		return nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
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
			return nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
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
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 72 * time.Hour
	}
	token, err := c.execStore.MintToken(context.Background(), executors.NewToken{
		Labels:        labels,
		Roots:         req.Roots,
		Isolation:     req.Isolation,
		WorkspaceMode: req.WorkspaceMode,
		Admits:        req.Admits,
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
			return nil, &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: pErr.Error()}
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
	notFound := &control.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("executor %q: no such row", ref),
	}
	if ref == "" {
		return executors.Executor{}, &control.ControllerError{
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
	return executors.Executor{}, &control.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: msg,
	}
}

// ─── Identity ──────────────────────────────────────────────────────────────

// errNoUserStore is returned when identity commands are used on a daemon with
// no database. Every user verb needs a row; there is nothing to degrade to.
var errNoUserStore = &control.ControllerError{
	Code:    protocol.ErrNoAgentDB,
	Message: "no database configured (RAFIKI_DB unset); user identity requires one",
}

func (c *Controller) UserCreate(ctx context.Context, username string) (protocol.UserCreateResponseData, error) {
	if c.users == nil {
		return protocol.UserCreateResponseData{}, errNoUserStore
	}
	u, token, err := c.users.Create(ctx, username)
	if err != nil {
		return protocol.UserCreateResponseData{}, err
	}
	slog.Info("user created", "username", u.Username, "id", u.ID)
	return protocol.UserCreateResponseData{
		ID: u.ID, Username: u.Username, Token: token,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// errBootstrapClosed is the answer to an unauthenticated user_create once a
// user exists. It is ErrAuthRequired rather than ErrInvalidArgs because that
// is what the caller must do about it: present a token.
var errBootstrapClosed = &control.ControllerError{
	Code:    protocol.ErrAuthRequired,
	Message: "a user already exists; authenticate with ctrl_auth to create more users",
}

// UserCreateBootstrap serves ctrl_user_create on a connection that was
// admitted with NO credential, because no user existed when it connected.
//
// The re-check is the whole point of this method existing. Admission is
// decided once, at accept time, and is never revisited for the life of the
// connection — so without a check here a peer that connected during the
// window and simply held the socket open would keep minting users for as
// long as the daemon ran, days after the operator's first user closed the
// window for everyone else. Reading the count from the STORE, per request,
// is what makes "the first user closes the window" true.
//
// A store error is not an answer: it propagates as an internal error (the
// dispatcher strips its text before it reaches an unauthenticated peer) and
// never as "the window is closed" or as permission to proceed.
func (c *Controller) UserCreateBootstrap(ctx context.Context, username string) (protocol.UserCreateResponseData, error) {
	if c.users == nil {
		return protocol.UserCreateResponseData{}, errNoUserStore
	}
	n, err := c.users.CountActive(ctx)
	if err != nil {
		return protocol.UserCreateResponseData{}, err
	}
	if n > 0 {
		return protocol.UserCreateResponseData{}, errBootstrapClosed
	}
	return c.UserCreate(ctx, username)
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
	if err := c.users.Delete(ctx, username); err != nil {
		return err
	}
	slog.Info("user removed", "username", username)
	return nil
}
