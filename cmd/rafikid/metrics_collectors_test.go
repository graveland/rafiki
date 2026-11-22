// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/execpool"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/recall"
)

// metricValue returns the value of the series name{labels...} in reg, and
// whether the series exists. labels must name every label of the series.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	assert.NewAborting(t).NoError(err, "gather")
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, lp := range m.GetLabel() {
				if labels[lp.GetName()] != lp.GetValue() {
					continue series
				}
			}
			switch {
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

func mustValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	v, ok := metricValue(t, reg, name, labels)
	assert.NewAborting(t).True(ok, "series %s%v missing", name, labels)
	return v
}

func lintClean(t *testing.T, c prometheus.Collector) {
	t.Helper()
	problems, err := testutil.CollectAndLint(c)
	c2 := assert.NewAborting(t)
	c2.NoError(err, "lint")
	c2.Empty(problems, "metric naming problems")
}

func TestConvCacheCollectorReportsBytesLookupsAndEvictions(t *testing.T) {
	c := assert.NewAborting(t)
	b := conv.NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)
	s := conv.NewStore(b, conv.Options{CacheBytes: 2048})
	h := s.Open(convID, conv.AsOwner())
	for range 8 {
		_, err := h.Append(context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{{
			Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: strings.Repeat("x", 600)}}},
		}}})
		c.NoError(err, "append")
	}
	_, err := h.Working(context.Background())
	c.NoError(err, "read")

	col := newConvCacheCollector(s)
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)

	c.Eq(2048.0, mustValue(t, reg, "rafiki_conv_cache_budget_bytes", nil), "budget")
	charged := mustValue(t, reg, "rafiki_conv_cache_bytes", map[string]string{"kind": "charged"})
	decoded := mustValue(t, reg, "rafiki_conv_cache_bytes", map[string]string{"kind": "decoded"})
	c.True(charged > 0 && charged <= 2048, "charged stays inside the budget: %v", charged)
	c.True(decoded > 0, "decoded estimate reported")
	c.True(mustValue(t, reg, "rafiki_conv_cache_evictions_total", nil) > 0, "a 2 KiB budget evicts")
	c.True(mustValue(t, reg, "rafiki_conv_cache_lookups_total", map[string]string{"kind": "path", "result": "miss"}) > 0,
		"the evicted history is walked back through the backend")
	for _, kind := range []string{"node", "path"} {
		for _, result := range []string{"hit", "miss"} {
			_, ok := metricValue(t, reg, "rafiki_conv_cache_lookups_total", map[string]string{"kind": kind, "result": result})
			c.True(ok, "lookups %s/%s series exists", kind, result)
		}
	}
}

func TestPgxPoolCollectorReportsPoolShape(t *testing.T) {
	c := assert.NewAborting(t)
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:1/db?pool_max_conns=7")
	c.NoError(err, "parse")
	// Never acquired from, so nothing dials: Stat() works on a lazy pool.
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	c.NoError(err, "pool")
	defer p.Close()

	col := newPgxPoolCollector(p)
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	c.Eq(7.0, mustValue(t, reg, "rafiki_pgx_pool_max_conns", nil), "max conns")
	for _, state := range []string{"acquired", "idle", "constructing"} {
		_, ok := metricValue(t, reg, "rafiki_pgx_pool_conns", map[string]string{"state": state})
		c.True(ok, "conns state %s", state)
	}
	_, ok := metricValue(t, reg, "rafiki_pgx_pool_empty_acquires_total", nil)
	c.True(ok, "empty acquires counter")
}

func TestExecutorCollectorSurvivesSharedMachineNamesAndKeepsSessionIDsOutOfLabels(t *testing.T) {
	c := assert.NewAborting(t)
	connected := time.Unix(5_000, 0)
	col := newExecutorCollectorFrom(func() execpool.PoolStats {
		return execpool.PoolStats{
			Parked: 2,
			Executors: []execpool.ExecutorStat{
				// Two enrolled executors sharing a machine name, as two owners'
				// "laptop" can: the scrape must still gather.
				{ID: "id-a", Name: "laptop", InFlight: 3, ConnectedAt: connected},
				{ID: "id-b", Name: "laptop", InFlight: 9, Draining: true},
				{ID: "sess-01X", Name: "sess-01X", Transient: true, InFlight: 100},
			},
			JobExitedEvents: 4,
		}
	})
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	_, err := reg.Gather()
	c.NoError(err, "two executors with one machine name must not fail the whole scrape")

	c.Eq(2.0, mustValue(t, reg, "rafiki_executors_live", map[string]string{"kind": "enrolled"}), "enrolled")
	c.Eq(1.0, mustValue(t, reg, "rafiki_executors_live", map[string]string{"kind": "transient"}), "transient")
	c.Eq(2.0, mustValue(t, reg, "rafiki_executors_parked", nil), "parked")
	c.Eq(3.0, mustValue(t, reg, "rafiki_executor_in_flight_calls", map[string]string{"executor": "id-a", "machine": "laptop"}), "a in flight")
	c.Eq(9.0, mustValue(t, reg, "rafiki_executor_in_flight_calls", map[string]string{"executor": "id-b", "machine": "laptop"}), "b in flight")
	c.Eq(1.0, mustValue(t, reg, "rafiki_executor_draining", map[string]string{"executor": "id-b", "machine": "laptop"}), "b draining")
	c.Eq(0.0, mustValue(t, reg, "rafiki_executor_draining", map[string]string{"executor": "id-a", "machine": "laptop"}), "a not draining")
	c.Eq(float64(connected.Unix()), mustValue(t, reg, "rafiki_executor_connected_timestamp_seconds", map[string]string{"executor": "id-a", "machine": "laptop"}), "connected at")
	_, transientLabelled := metricValue(t, reg, "rafiki_executor_in_flight_calls", map[string]string{"executor": "sess-01X", "machine": "sess-01X"})
	c.False(transientLabelled, "a per-session executor id is unbounded cardinality and must not become a label")
	c.Eq(4.0, mustValue(t, reg, "rafiki_executor_job_events_total", map[string]string{"kind": "exited"}), "job events")
}

func TestEventBusCollectorReportsBacklogAndDrops(t *testing.T) {
	c := assert.NewAborting(t)
	r := nativebus.New()
	r.Subscribe("c_stuck")
	r.Publish("c_stuck", &rafikiv1.Event{})

	col := newEventBusCollector(r)
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	c.Eq(1.0, mustValue(t, reg, "rafiki_event_buses", nil), "one per-child bus")
	c.Eq(1.0, mustValue(t, reg, "rafiki_event_subscribers", nil), "one subscriber")
	c.Eq(1.0, mustValue(t, reg, "rafiki_events_published_total", nil), "one publish")
	c.Eq(1.0, mustValue(t, reg, "rafiki_event_subscriber_max_backlog", nil), "the undrained event is the backlog")
	c.True(mustValue(t, reg, "rafiki_event_subscriber_buffer", nil) > 1, "capacity reported")
}

func TestRecallCollectorReportsStagesAndLastSuccess(t *testing.T) {
	c := assert.NewAborting(t)
	ix := recall.NewIndexer(recall.IndexerOptions{Store: nil})
	ix.Mark("c1")

	col := newRecallCollector(ix)
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	c.Eq(1.0, mustValue(t, reg, "rafiki_recall_dirty_conversations", nil), "one marked")
	c.Eq(1.0, mustValue(t, reg, "rafiki_recall_marks_total", nil), "one mark")
	for _, stage := range []string{"extract", "embed", "summarize"} {
		c.Eq(0.0, mustValue(t, reg, "rafiki_recall_stage_last_success_timestamp_seconds", map[string]string{"stage": stage}),
			"stage %s has not succeeded: 0, not the Unix epoch's negative", stage)
	}
}

func TestRegisterRecallMetricsIgnoresNilRegistryAndIndexer(t *testing.T) {
	registerRecallMetrics(nil, recall.NewIndexer(recall.IndexerOptions{}))
	registerRecallMetrics(prometheus.NewRegistry(), nil)
}

func TestFleetCountsUseThePublishedDisplayStateWithSafeBaselines(t *testing.T) {
	c := assert.NewAborting(t)
	st := childstore.New()
	add := func(id, kind string, status protocol.Status) {
		st.Insert(&childstore.Session{ChildID: id, Kind: kind, Status: status, StartedAt: time.Now()})
	}
	add("published-waiting", protocol.KindClaude, protocol.StatusIdle)
	add("unpublished-idle", protocol.KindClaude, protocol.StatusIdle)
	add("unpublished-busy", protocol.KindFundi, "streaming")
	add("gone", protocol.KindFundi, protocol.StatusExited)
	add("gone-but-last-published-working", protocol.KindFundi, protocol.StatusExited)
	add("legacy-empty-kind", "", protocol.StatusIdle)
	ctrl := &Controller{st: st, displayLast: map[string]displayValue{
		"published-waiting":               {state: protocol.DisplayWaiting, on: protocol.WaitingOnJob},
		"gone-but-last-published-working": {state: protocol.DisplayWorking},
	}}

	got := ctrl.fleetCounts()
	c.Eq(1, got[fleetKey{protocol.KindClaude, protocol.DisplayWaiting, protocol.WaitingOnJob}], "waiting on job")
	c.Eq(1, got[fleetKey{protocol.KindClaude, protocol.DisplayIdle, ""}], "an unpublished idle child is idle")
	c.Eq(1, got[fleetKey{protocol.KindFundi, protocol.DisplayIdle, ""}], "a row written before the kind default is a fundi child, not kind \"\"")
	c.Eq(1, got[fleetKey{protocol.KindFundi, protocol.DisplayWorking, ""}], "an unpublished non-idle child is working")
	c.Eq(2, got[fleetKey{protocol.KindFundi, protocol.DisplayExited, ""}], "an exited child is exited whatever was last published")

	col := newFleetCollector(ctrl)
	lintClean(t, col)
	reg := prometheus.NewRegistry()
	reg.MustRegister(col)
	c.Eq(1.0, mustValue(t, reg, "rafiki_children", map[string]string{"kind": protocol.KindClaude, "state": "waiting", "waiting_on": "job"}), "gauge")
	c.Eq(2.0, mustValue(t, reg, "rafiki_children", map[string]string{"kind": protocol.KindFundi, "state": "exited", "waiting_on": ""}), "exited gauge")
}

func TestFleetMetricsCountLaunchesExitsAndDeferralsAndAreNilSafe(t *testing.T) {
	c := assert.NewAborting(t)
	var none *fleetMetrics
	none.launch("x")
	none.exit("x", 1, "")
	none.deferred("budget")

	reg := prometheus.NewRegistry()
	f := newFleetMetrics(reg)
	f.launch(protocol.KindClaude)
	f.launch(protocol.KindClaude)
	f.exit(protocol.KindClaude, 0, "")
	f.exit(protocol.KindClaude, 1, "")
	f.exit(protocol.KindFundi, 0, "SIGKILL")
	f.deferred("budget")

	c.Eq(2.0, mustValue(t, reg, "rafiki_child_launches_total", map[string]string{"kind": "claude"}), "launches")
	c.Eq(1.0, mustValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "claude", "outcome": "clean"}), "clean exit")
	c.Eq(1.0, mustValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "claude", "outcome": "error"}), "non-zero exit")
	c.Eq(1.0, mustValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "fundi", "outcome": "error"}), "signalled exit is an error even with code 0")
	c.Eq(1.0, mustValue(t, reg, "rafiki_inbox_delivery_deferrals_total", map[string]string{"reason": "budget"}), "deferred")
}
