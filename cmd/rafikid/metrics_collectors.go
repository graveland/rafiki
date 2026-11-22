// SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/recall"
)

// Every collector below reads a subsystem's own Stats() snapshot at scrape
// time instead of mirroring its state into metrics on the hot path: a gauge
// that is computed when asked cannot drift from the state it reports, and a
// scrape touches only memory the subsystem already holds (never the database).
// The one deliberately counted-on-the-path family is fleetMetrics, because a
// spawn or an exit is an event that no later snapshot can reconstruct.

func newDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func counter(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// unixSeconds renders a time as Prometheus' conventional timestamp gauge, 0
// for "never".
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// ─── conversation node cache ────────────────────────────────────────────────

type convCacheCollector struct {
	store *conv.Store

	bytes, budget, nodes, entries         *prometheus.Desc
	lookups, evictions, sweeps            *prometheus.Desc
	headServed, headInstalls, headRetries *prometheus.Desc
	headNotInstalled, headInvalidations   *prometheus.Desc
}

func newConvCacheCollector(s *conv.Store) *convCacheCollector {
	return &convCacheCollector{
		store: s,
		bytes: newDesc("rafiki_conv_cache_bytes",
			"Conversation node cache bytes by kind: charged is the stored JSON the budget counts, decoded estimates the decoded messages the same nodes keep alive.", "kind"),
		budget:  newDesc("rafiki_conv_cache_budget_bytes", "Configured conversation node cache budget (RAFIKI_CONV_CACHE_BYTES)."),
		nodes:   newDesc("rafiki_conv_cache_nodes", "Nodes held by the conversation node cache."),
		entries: newDesc("rafiki_conv_cache_entries", "Per-conversation entries (cached head, fork edge, write lock) held."),
		lookups: newDesc("rafiki_conv_cache_lookups_total",
			"Cache lookups by kind (node, path: a whole-chain walk) and result.", "kind", "result"),
		evictions: newDesc("rafiki_conv_cache_evictions_total", "Nodes evicted from the cold end to stay within the budget."),
		sweeps:    newDesc("rafiki_conv_cache_entry_sweeps_total", "Per-conversation entries dropped for idleness."),
		headServed: newDesc("rafiki_conv_cache_head_served_total",
			"Owner reads answered from a fresh cached head with no backend call."),
		headInstalls: newDesc("rafiki_conv_cache_head_installs_total", "Head refreshes that installed a new cached head."),
		headRetries:  newDesc("rafiki_conv_cache_head_install_retries_total", "Head installs lost to a concurrent installer and retried."),
		headNotInstalled: newDesc("rafiki_conv_cache_head_not_installed_total",
			"Head refreshes that gave up installing and answered uncached."),
		headInvalidations: newDesc("rafiki_conv_cache_head_invalidations_total", "Cached heads dropped after a write whose refresh did not install."),
	}
}

func (c *convCacheCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.bytes, c.budget, c.nodes, c.entries, c.lookups, c.evictions, c.sweeps,
		c.headServed, c.headInstalls, c.headRetries, c.headNotInstalled, c.headInvalidations} {
		ch <- d
	}
}

func (c *convCacheCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.store.CacheStats()
	gauge(ch, c.bytes, float64(s.ChargedBytes), "charged")
	gauge(ch, c.bytes, float64(s.DecodedBytes), "decoded")
	gauge(ch, c.budget, float64(s.BudgetBytes))
	gauge(ch, c.nodes, float64(s.Nodes))
	gauge(ch, c.entries, float64(s.Entries))
	counter(ch, c.lookups, float64(s.NodeHits), "node", "hit")
	counter(ch, c.lookups, float64(s.NodeMisses), "node", "miss")
	counter(ch, c.lookups, float64(s.PathHits), "path", "hit")
	counter(ch, c.lookups, float64(s.PathMisses), "path", "miss")
	counter(ch, c.evictions, float64(s.Evictions))
	counter(ch, c.sweeps, float64(s.EntrySweeps))
	counter(ch, c.headServed, float64(s.HeadServed))
	counter(ch, c.headInstalls, float64(s.HeadInstalls))
	counter(ch, c.headRetries, float64(s.HeadInstallRetries))
	counter(ch, c.headNotInstalled, float64(s.HeadNotInstalled))
	counter(ch, c.headInvalidations, float64(s.HeadInvalidations))
}

// ─── pgx pool ───────────────────────────────────────────────────────────────

type pgxPoolCollector struct {
	pool *pgxpool.Pool

	conns                            *prometheus.Desc
	maxConns                         *prometheus.Desc
	acquires, acquireWait, emptyAcq  *prometheus.Desc
	emptyWait, canceled, newConns    *prometheus.Desc
	lifetimeDestroyed, idleDestroyed *prometheus.Desc
}

func newPgxPoolCollector(p *pgxpool.Pool) *pgxPoolCollector {
	return &pgxPoolCollector{
		pool: p,
		conns: newDesc("rafiki_pgx_pool_conns",
			"Pool connections by state (acquired, idle, constructing).", "state"),
		maxConns: newDesc("rafiki_pgx_pool_max_conns", "Configured pool size."),
		acquires: newDesc("rafiki_pgx_pool_acquires_total", "Successful connection acquires."),
		acquireWait: newDesc("rafiki_pgx_pool_acquire_seconds_total",
			"Time spent in successful acquires; divide its rate by the acquires rate for the mean wait."),
		emptyAcq: newDesc("rafiki_pgx_pool_empty_acquires_total",
			"Acquires that found no idle connection and had to wait or dial."),
		emptyWait: newDesc("rafiki_pgx_pool_empty_acquire_wait_seconds_total",
			"Time acquires spent waiting because the pool had no idle connection."),
		canceled:          newDesc("rafiki_pgx_pool_canceled_acquires_total", "Acquires abandoned because the caller's context ended."),
		newConns:          newDesc("rafiki_pgx_pool_new_conns_total", "Connections dialed."),
		lifetimeDestroyed: newDesc("rafiki_pgx_pool_max_lifetime_destroyed_total", "Connections closed for exceeding the maximum lifetime."),
		idleDestroyed:     newDesc("rafiki_pgx_pool_max_idle_destroyed_total", "Connections closed for exceeding the maximum idle time."),
	}
}

func (c *pgxPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.conns, c.maxConns, c.acquires, c.acquireWait, c.emptyAcq, c.emptyWait,
		c.canceled, c.newConns, c.lifetimeDestroyed, c.idleDestroyed} {
		ch <- d
	}
}

func (c *pgxPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	gauge(ch, c.conns, float64(s.AcquiredConns()), "acquired")
	gauge(ch, c.conns, float64(s.IdleConns()), "idle")
	gauge(ch, c.conns, float64(s.ConstructingConns()), "constructing")
	gauge(ch, c.maxConns, float64(s.MaxConns()))
	counter(ch, c.acquires, float64(s.AcquireCount()))
	counter(ch, c.acquireWait, s.AcquireDuration().Seconds())
	counter(ch, c.emptyAcq, float64(s.EmptyAcquireCount()))
	counter(ch, c.emptyWait, s.EmptyAcquireWaitTime().Seconds())
	counter(ch, c.canceled, float64(s.CanceledAcquireCount()))
	counter(ch, c.newConns, float64(s.NewConnsCount()))
	counter(ch, c.lifetimeDestroyed, float64(s.MaxLifetimeDestroyCount()))
	counter(ch, c.idleDestroyed, float64(s.MaxIdleDestroyCount()))
}

// ─── executor pool ──────────────────────────────────────────────────────────

type executorCollector struct {
	stats func() execpool.PoolStats

	live, parked                                 *prometheus.Desc
	inflight, connectedAt, draining              *prometheus.Desc
	connects, displaced, healthFail, parks, lost *prometheus.Desc
	jobEvents, streamEnds, acks, ackErrors       *prometheus.Desc
}

func newExecutorCollector(p *execpool.Pool) *executorCollector {
	return newExecutorCollectorFrom(p.Stats)
}

// newExecutorCollectorFrom reads its snapshot from stats, which is the pool's
// Stats in production and a fixture in a test.
func newExecutorCollectorFrom(stats func() execpool.PoolStats) *executorCollector {
	return &executorCollector{
		stats: stats,
		live: newDesc("rafiki_executors_live",
			"Connected executors by kind: enrolled has a database row, transient is a session-ticket executor.", "kind"),
		parked: newDesc("rafiki_executors_parked", "Executors whose connection dropped and that are within their reconnect window."),
		inflight: newDesc("rafiki_executor_in_flight_calls",
			"RPCs open on the executor's single connection, measured against the HTTP/2 stream limit. Enrolled executors only.", "executor", "machine"),
		connectedAt: newDesc("rafiki_executor_connected_timestamp_seconds",
			"When the executor's current connection joined the pool. Enrolled executors only.", "executor", "machine"),
		draining: newDesc("rafiki_executor_draining", "1 when the executor has announced a graceful leave. Enrolled executors only.", "executor", "machine"),
		connects: newDesc("rafiki_executor_connects_total", "Executor connections installed."),
		displaced: newDesc("rafiki_executor_displaced_total",
			"Connections that replaced a still-registered connection of the same executor."),
		healthFail: newDesc("rafiki_executor_health_failures_total", "Connections removed after a failed health check."),
		parks:      newDesc("rafiki_executor_parks_total", "Executors parked after a dropped connection."),
		lost:       newDesc("rafiki_executor_park_expiries_total", "Parked executors whose reconnect window expired (declared lost)."),
		jobEvents:  newDesc("rafiki_executor_job_events_total", "Background-job events received on WatchJobs, by kind.", "kind"),
		streamEnds: newDesc("rafiki_executor_job_stream_ends_total", "WatchJobs subscriptions that ended and were resubscribed."),
		acks:       newDesc("rafiki_executor_job_acks_total", "AckJobExits calls."),
		ackErrors:  newDesc("rafiki_executor_job_ack_errors_total", "AckJobExits calls that failed; the executor replays the exit on its next subscribe."),
	}
}

func (c *executorCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.live, c.parked, c.inflight, c.connectedAt, c.draining, c.connects, c.displaced,
		c.healthFail, c.parks, c.lost, c.jobEvents, c.streamEnds, c.acks, c.ackErrors} {
		ch <- d
	}
}

func (c *executorCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	var enrolled, transient int
	for _, e := range s.Executors {
		if e.Transient {
			transient++
			continue
		}
		enrolled++
		// The id is the series key: a machine name is unique only per owner, so
		// two executors can share one, and a duplicate series fails the whole
		// scrape. The name rides along as a readable label.
		gauge(ch, c.inflight, float64(e.InFlight), e.ID, e.Name)
		gauge(ch, c.connectedAt, unixSeconds(e.ConnectedAt), e.ID, e.Name)
		gauge(ch, c.draining, boolFloat(e.Draining), e.ID, e.Name)
	}
	gauge(ch, c.live, float64(enrolled), "enrolled")
	gauge(ch, c.live, float64(transient), "transient")
	gauge(ch, c.parked, float64(s.Parked))
	counter(ch, c.connects, float64(s.Connects))
	counter(ch, c.displaced, float64(s.Displaced))
	counter(ch, c.healthFail, float64(s.HealthFailures))
	counter(ch, c.parks, float64(s.Parks))
	counter(ch, c.lost, float64(s.ParkExpiries))
	counter(ch, c.jobEvents, float64(s.JobRunningEvents), "running")
	counter(ch, c.jobEvents, float64(s.JobExitedEvents), "exited")
	counter(ch, c.jobEvents, float64(s.JobReplayCompleteEvents), "replay_complete")
	counter(ch, c.streamEnds, float64(s.JobStreamEnds))
	counter(ch, c.acks, float64(s.JobAcks))
	counter(ch, c.ackErrors, float64(s.JobAckErrors))
}

// ─── event bus ──────────────────────────────────────────────────────────────

type eventBusCollector struct {
	reg *nativebus.Registry

	buses, subscribers, backlog, capacity *prometheus.Desc
	published, drops                      *prometheus.Desc
}

func newEventBusCollector(r *nativebus.Registry) *eventBusCollector {
	return &eventBusCollector{
		reg:         r,
		buses:       newDesc("rafiki_event_buses", "Per-child event buses held. Only Forget lowers it."),
		subscribers: newDesc("rafiki_event_subscribers", "Live event subscriptions across every bus."),
		backlog:     newDesc("rafiki_event_subscriber_max_backlog", "Deepest subscriber queue right now, in events."),
		capacity:    newDesc("rafiki_event_subscriber_buffer", "Each subscriber's queue capacity; the backlog is measured against it."),
		published:   newDesc("rafiki_events_published_total", "Events published to the bus."),
		drops:       newDesc("rafiki_event_drops_total", "Events dropped because a subscriber's queue was full."),
	}
}

func (c *eventBusCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.buses, c.subscribers, c.backlog, c.capacity, c.published, c.drops} {
		ch <- d
	}
}

func (c *eventBusCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.reg.Stats()
	gauge(ch, c.buses, float64(s.Buses))
	gauge(ch, c.subscribers, float64(s.Subscribers))
	gauge(ch, c.backlog, float64(s.MaxBacklog))
	gauge(ch, c.capacity, float64(s.PerSubBuffer))
	counter(ch, c.published, float64(s.Published))
	counter(ch, c.drops, float64(s.Drops))
}

// ─── recall indexer ─────────────────────────────────────────────────────────

type recallCollector struct {
	ix *recall.Indexer

	dirty, overflow, backlog                  *prometheus.Desc
	marks, markOverflows, passes, lockSkips   *prometheus.Desc
	lockErrors, extracted, extractErrors      *prometheus.Desc
	embedded                                  *prometheus.Desc
	stageRuns, stageErrors, stageSecs, lastOK *prometheus.Desc
}

func newRecallCollector(ix *recall.Indexer) *recallCollector {
	return &recallCollector{
		ix:       ix,
		dirty:    newDesc("rafiki_recall_dirty_conversations", "Conversations marked by a commit and awaiting an indexer pass."),
		overflow: newDesc("rafiki_recall_dirty_overflow", "1 when the marked set overflowed and the next pass is a full one."),
		backlog: newDesc("rafiki_recall_last_full_pass_backlog",
			"Conversations with work found by the most recent full pass, capped at its cursor limit."),
		marks:         newDesc("rafiki_recall_marks_total", "Commits that marked a conversation for indexing."),
		markOverflows: newDesc("rafiki_recall_mark_overflows_total", "Marks dropped because the marked set was full."),
		passes:        newDesc("rafiki_recall_passes_total", "Indexer passes that ran, by kind (full, marked).", "kind"),
		lockSkips:     newDesc("rafiki_recall_lock_skips_total", "Passes skipped because another holder had the index lock."),
		lockErrors:    newDesc("rafiki_recall_lock_errors_total", "Index lock probes that failed."),
		extracted:     newDesc("rafiki_recall_conversations_extracted_total", "Conversations windowed."),
		extractErrors: newDesc("rafiki_recall_extract_errors_total", "Conversations whose extraction failed."),
		embedded:      newDesc("rafiki_recall_rows_embedded_total", "Window and summary rows embedded."),
		stageRuns:     newDesc("rafiki_recall_stage_runs_total", "Pass stage runs (extract, embed, summarize).", "stage"),
		stageErrors:   newDesc("rafiki_recall_stage_errors_total", "Pass stage runs that returned an error.", "stage"),
		stageSecs:     newDesc("rafiki_recall_stage_seconds_total", "Wall time spent in each pass stage.", "stage"),
		lastOK: newDesc("rafiki_recall_stage_last_success_timestamp_seconds",
			"When the stage last finished without error; 0 before the first success.", "stage"),
	}
}

func (c *recallCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.dirty, c.overflow, c.backlog, c.marks, c.markOverflows, c.passes, c.lockSkips,
		c.lockErrors, c.extracted, c.extractErrors, c.embedded, c.stageRuns, c.stageErrors, c.stageSecs, c.lastOK} {
		ch <- d
	}
}

func (c *recallCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.ix.Stats()
	gauge(ch, c.dirty, float64(s.Dirty))
	gauge(ch, c.overflow, boolFloat(s.DirtyOverflow))
	gauge(ch, c.backlog, float64(s.LastFullBacklog))
	counter(ch, c.marks, float64(s.Marks))
	counter(ch, c.markOverflows, float64(s.MarkOverflows))
	counter(ch, c.passes, float64(s.FullPasses), "full")
	counter(ch, c.passes, float64(s.MarkedPasses), "marked")
	counter(ch, c.lockSkips, float64(s.LockSkips))
	counter(ch, c.lockErrors, float64(s.LockErrors))
	counter(ch, c.extracted, float64(s.ConversationsExtracted))
	counter(ch, c.extractErrors, float64(s.ExtractErrors))
	counter(ch, c.embedded, float64(s.RowsEmbedded))
	for _, st := range []struct {
		name  string
		stats recall.StageStats
	}{{"extract", s.Extract}, {"embed", s.Embed}, {"summarize", s.Summarize}} {
		counter(ch, c.stageRuns, float64(st.stats.Runs), st.name)
		counter(ch, c.stageErrors, float64(st.stats.Errors), st.name)
		counter(ch, c.stageSecs, st.stats.Duration.Seconds(), st.name)
		gauge(ch, c.lastOK, unixSeconds(st.stats.LastSuccess), st.name)
	}
}

// registerRecallMetrics exposes the indexer's stats. A nil registry (a test
// fixture) or a nil indexer (recall search off) registers nothing.
func registerRecallMetrics(reg prometheus.Registerer, ix *recall.Indexer) {
	if reg == nil || ix == nil {
		return
	}
	reg.MustRegister(newRecallCollector(ix))
}

// ─── fleet ──────────────────────────────────────────────────────────────────

// fleetMetrics counts the lifecycle events no snapshot can reconstruct:
// launches (spawn and resume both start a process), exits, and inbox delivery
// attempts that left a batch pending. Every method is safe on a nil receiver, so a test
// Controller without metrics needs no guard at the call sites.
type fleetMetrics struct {
	launches       *prometheus.CounterVec
	exits          *prometheus.CounterVec
	inboxDeferrals *prometheus.CounterVec
}

func newFleetMetrics(reg prometheus.Registerer) *fleetMetrics {
	f := &fleetMetrics{
		launches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_child_launches_total",
			Help: "Child processes started, by kind. A resume is a launch.",
		}, []string{"kind"}),
		exits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_child_exits_total",
			Help: "Child processes that ended, by kind and outcome (clean: exit code 0; error: anything else).",
		}, []string{"kind", "outcome"}),
		inboxDeferrals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rafiki_inbox_delivery_deferrals_total",
			Help: "Inbox delivery attempts that left a batch pending, by reason (budget: the child is over its cap; script: deferred to the script's Receive stream). A batch is counted again on every retry, so this measures deferral pressure, not distinct batches.",
		}, []string{"reason"}),
	}
	reg.MustRegister(f.launches, f.exits, f.inboxDeferrals)
	return f
}

func (f *fleetMetrics) launch(kind string) {
	if f != nil {
		f.launches.WithLabelValues(kind).Inc()
	}
}

func (f *fleetMetrics) exit(kind string, code int, signal string) {
	if f == nil {
		return
	}
	outcome := "clean"
	if code != 0 || signal != "" {
		outcome = "error"
	}
	f.exits.WithLabelValues(kind, outcome).Inc()
}

func (f *fleetMetrics) deferred(reason string) {
	if f != nil {
		f.inboxDeferrals.WithLabelValues(reason).Inc()
	}
}

// fleetCollector reports the children by the state an operator sees. It reads
// the daemon's in-memory child store and the display state the worker last
// published, so it agrees with `rafiki ls` and costs no query.
type fleetCollector struct {
	c        *Controller
	children *prometheus.Desc
}

func newFleetCollector(c *Controller) *fleetCollector {
	return &fleetCollector{
		c: c,
		children: newDesc("rafiki_children",
			"Children the daemon holds by kind, display state and what a waiting child waits on.", "kind", "state", "waiting_on"),
	}
}

func (f *fleetCollector) Describe(ch chan<- *prometheus.Desc) { ch <- f.children }

func (f *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	type key struct{ kind, state, on string }
	counts := map[key]int{}
	for k, n := range f.c.fleetCounts() {
		counts[key{k.kind, string(k.state), string(k.on)}] += n
	}
	for k, n := range counts {
		gauge(ch, f.children, float64(n), k.kind, k.state, k.on)
	}
}

type fleetKey struct {
	kind  string
	state protocol.DisplayState
	on    protocol.WaitingOn
}

// fleetCounts tallies every child the store holds by its display state. The
// state is the one the display worker last published; a child it has not
// published for yet is idle by the same baseline publishOneDisplayState uses,
// except that a status that is plainly not idle is working and an exited child
// is exited whatever was last published.
func (c *Controller) fleetCounts() map[fleetKey]int {
	snaps := c.st.List()
	c.displayMu.Lock()
	defer c.displayMu.Unlock()
	out := make(map[fleetKey]int)
	for _, s := range snaps {
		k := fleetKey{kind: spawnKindLabel(s.Kind)}
		switch s.Status {
		case protocol.StatusExited:
			k.state = protocol.DisplayExited
		default:
			if v, ok := c.displayLast[s.ChildID]; ok {
				k.state, k.on = v.state, v.on
			} else if s.Status == protocol.StatusIdle {
				k.state = protocol.DisplayIdle
			} else {
				k.state = protocol.DisplayWorking
			}
		}
		out[k]++
	}
	return out
}
