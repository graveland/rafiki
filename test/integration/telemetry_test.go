// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof that a fundi turn reaches an OTLP endpoint, and that with
// no OTLP endpoint configured nothing is exported at all. The receiver is a
// real HTTP server (otlp_fixture_test.go), so the daemon's exporter config,
// protobuf encoding and wire path are exercised, not stubbed.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// telemetryFakeTurns writes a --fake-turns script with one plain end_turn
// reply, so a single prompt drives exactly one turn through the real engine.
func telemetryFakeTurns(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	const endTurn = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"telemetry fixture reply"}],"usage":{"input_tokens":4,"output_tokens":2}}`
	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(endTurn+"\n"), 0o600), "write fake-turns script")
	return path
}

// telemetryConfigEnv isolates the daemon from the developer's real config. The
// test suite's daemons do not set XDG_CONFIG_HOME, so an ambient rafikid.toml
// (or RAFIKI_CONFIG) would otherwise be read; both are pinned at a scratch
// directory whose config file does not exist, which means "defaults".
func telemetryConfigEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{
		"XDG_CONFIG_HOME=" + dir,
		"RAFIKI_CONFIG=" + filepath.Join(dir, "rafikid.toml"),
	}
}

// spawnTelemetryFundi spawns a real in-process fundi child driven by the fake
// sender. It does not prompt: callers open their event stream first, then send.
func spawnTelemetryFundi(t *testing.T, d *daemon) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind:      protocol.KindFundi,
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-x",
		NoSession: true,
		ExtraArgs: []string{"--fake-turns", telemetryFakeTurns(t)},
	}))
	c.NoError(err, "spawn fundi child")
	childID := resp.Msg.GetChildId()
	c.NotEq("", childID, "spawn returned empty childId")
	return childID
}

// TestTelemetryFundiTurnExported boots a daemon pointed at the fixture
// collector, drives one fundi turn, and asserts the turn arrived as a root
// invoke_agent span with the child's id and — content defaulting off — no
// prompt text.
func TestTelemetryFundiTurnExported(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	fixture := newOTLPFixture(t)

	// Endpoint and protocol are set on BOTH the generic and the traces-specific
	// variables: the traces-specific pair wins, so an inherited value there
	// would silently redirect this test. RAFIKI_CONFIG/XDG_CONFIG_HOME keep a
	// developer's rafikid.toml out of the boot.
	// The traces-specific endpoint is used verbatim (OTel spec): with no path it
	// means "/", not "/v1/traces". The generic endpoint is a base URL and the
	// exporter appends /v1/traces itself, so the traces-specific value carries
	// the path.
	env := append(telemetryConfigEnv(t),
		"OTEL_EXPORTER_OTLP_ENDPOINT="+fixture.srv.URL,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+fixture.srv.URL+"/v1/traces",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
		// The batch processor's default flush interval is 5 s; shorten it so
		// the test waits on the wire, not on a timer.
		"OTEL_BSP_SCHEDULE_DELAY=100",
	)
	d := bootDaemonDB(t, nextDaemonID(), env...)
	childID := spawnTelemetryFundi(t, d)
	sendPrompt(t, d.control(t), childID, "one telemetry turn")

	deadline := time.After(30 * time.Second)
	var turnSpan *tracepb.Span
	for turnSpan == nil {
		select {
		case sp := <-fixture.spans:
			// Match the child id as well as the name: the disposable database
			// is shared across runs, so a boot can adopt and resume a stale
			// child from an earlier test and export its turn too.
			if id, _ := spanStringAttr(sp, "gen_ai.agent.id"); id == childID &&
				strings.HasPrefix(sp.GetName(), "invoke_agent ") {
				turnSpan = sp
			}
		case <-deadline:
			t.Fatalf("no invoke_agent span exported within 30s (daemon stderr:\n%s)", d.stderr.tail(4000))
		}
	}

	c.Eq(childID, mustSpanAttr(t, turnSpan, "gen_ai.agent.id"), "turn span gen_ai.agent.id")
	c.Eq("invoke_agent", mustSpanAttr(t, turnSpan, "gen_ai.operation.name"), "turn span gen_ai.operation.name")
	c.Eq(0, len(turnSpan.GetParentSpanId()), "turn span must be a root span")
	_, present := spanStringAttr(turnSpan, "rafiki.user_message")
	c.False(present, "content defaults to off, so the turn must carry no rafiki.user_message")
}

// TestTelemetryOffByDefault boots a daemon with no OTLP endpoint anywhere (the
// environment is blanked, the config is a missing scratch file), completes a
// turn, and asserts the collector received nothing.
func TestTelemetryOffByDefault(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	fixture := newOTLPFixture(t)

	// Blank every endpoint/protocol variable explicitly: an inherited one would
	// silently turn tracing on and defeat the assertion.
	env := append(telemetryConfigEnv(t),
		"OTEL_EXPORTER_OTLP_ENDPOINT=",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=",
		"OTEL_EXPORTER_OTLP_PROTOCOL=",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=",
	)
	d := bootDaemonDB(t, nextDaemonID(), env...)

	client := d.control(t)
	childID := spawnTelemetryFundi(t, d)
	// Replay the child's whole log (cursor -1): the child is unique per run, so
	// nothing stale can exist for it, and opening before the prompt cannot miss
	// the turn's own status events.
	es := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: -1}, 0)
	sendPrompt(t, client, childID, "one telemetry turn")
	es.waitEvent(t, turnSettled(childID), 20*time.Second)

	select {
	case <-fixture.exports:
		t.Fatal("the fixture received a trace export although tracing is off")
	case <-time.After(2 * time.Second):
	}
	c.Eq(int64(0), fixture.requests.Load(), "trace export requests received with tracing off")
}

// mustSpanAttr reads a required string attribute, failing the test when it is
// absent.
func mustSpanAttr(t *testing.T, sp *tracepb.Span, key string) string {
	t.Helper()
	v, ok := spanStringAttr(sp, key)
	assert.NewAborting(t).True(ok, "span %q has no %s attribute", sp.GetName(), key)
	return v
}
