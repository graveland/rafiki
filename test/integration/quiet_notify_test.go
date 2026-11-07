// SPDX-License-Identifier: Apache-2.0

package integration_test

// A child spawned with notify=result wakes its spawner only when something
// needs the spawner. The observation is the spawner's durable inbox
// (conversations.agent_inbox): every settle a child pushes at its parent is
// persisted there before the debounce is armed, so the rows are the complete
// record of what the parent was told. The daemon runs with a debounce far
// longer than any test, so no row is ever delivered and the parent never runs
// a turn of its own.
//
// A dropped settle leaves no trace, so each absence is asserted behind a
// barrier that is itself a later settle from the same child: killing the child
// pushes its exit settle, and a row for it proves every earlier push has
// already landed.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

const (
	settledIdleReason = "settled (idle)"
	exitedReason      = "exited"
)

type inboxRow struct {
	source string
	key    string
	body   string
}

type quietRun struct {
	d      *daemon
	dsn    string
	parent string
}

func quietNotifyEnv() []string {
	return []string{
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=3600000",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=3600000",
	}
}

func bootQuietRun(t *testing.T) (*quietRun, string) {
	t.Helper()
	dsn := requireExecutorDB(t)
	id := nextDaemonID()
	d := bootDaemonDB(t, id, quietNotifyEnv()...)
	parent := d.spawnChild(t)
	dropAutoBgInboxRows(t, dsn, parent)
	return &quietRun{d: d, dsn: dsn, parent: parent}, id
}

func quietEndTurn(n int) string {
	return fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"turn %d"}],"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, n, n)
}

func quietResultTurn(n int, result string) string {
	return fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu_%d","name":"agent_result","input":{"result":%q}}],"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, n, n, result)
}

func writeQuietTurns(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600), "write fake-turns script")
	return path
}

func (r *quietRun) spawnChild(t *testing.T, notify, script string) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := r.d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:           t.TempDir(),
		NoSession:     true,
		Kind:          protocol.KindFundi,
		Model:         "anthropic/claude-x",
		ParentChildId: r.parent,
		Name:          "quiet-" + uniqueSuffix(),
		Notify:        notify,
		ExtraArgs:     []string{"--fake-turns", script},
	}))
	c.NoError(err, "spawn under %s with notify=%q", r.parent, notify)
	return resp.Msg.GetChildId()
}

func runQuietTurn(t *testing.T, d *daemon, es *eventStream, idx int, childID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := d.control(t).SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: `{"type":"prompt","message":"go"}`,
	}))
	assert.NewAborting(t).NoError(err, "SendFrame prompt to %s", childID)
	_, idx = es.waitEventAfter(t, idx, turnStarted(childID), 10*time.Second)
	_, idx = es.waitEventAfter(t, idx, turnSettled(childID), 10*time.Second)
	return idx
}

func (r *quietRun) openStream(t *testing.T, childID string) (*eventStream, int) {
	t.Helper()
	w := eventWatermark(t, r.d.control(t), childID)
	return r.d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: w - 1}, w), 0
}

func (r *quietRun) kill(t *testing.T, childID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.d.control(t).Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: childID}))
	assert.NewAborting(t).NoError(err, "Kill %s", childID)
}

func (r *quietRun) inbox(t *testing.T) []inboxRow {
	t.Helper()
	c := assert.NewAborting(t)
	pool := openPool(t, r.dsn)
	rows, err := pool.Query(context.Background(), `
		SELECT source, coalesce_key, body
		  FROM conversations.agent_inbox
		 WHERE child_id = $1
		 ORDER BY accepted_at, id`, r.parent)
	c.NoError(err, "query the parent's inbox")
	defer rows.Close()
	var out []inboxRow
	for rows.Next() {
		var row inboxRow
		c.NoError(rows.Scan(&row.source, &row.key, &row.body), "scan inbox row")
		out = append(out, row)
	}
	c.NoError(rows.Err(), "iterate inbox rows")
	return out
}

func bodiesContaining(rows []inboxRow, sub string) []string {
	var out []string
	for _, r := range rows {
		if strings.Contains(r.body, sub) {
			out = append(out, r.body)
		}
	}
	return out
}

func TestQuietNotifyPlainTurnEndWakesNobody(t *testing.T) {
	c := assert.NewCollecting(t)
	r, _ := bootQuietRun(t)
	child := r.spawnChild(t, protocol.NotifyResult, writeQuietTurns(t, quietEndTurn(1), quietEndTurn(2)))
	es, idx := r.openStream(t, child)

	idx = runQuietTurn(t, r.d, es, idx, child)
	runQuietTurn(t, r.d, es, idx, child)
	r.kill(t, child)

	rows := r.inbox(t)
	c.Require().Eq(1, len(rows), "only the exit settle reaches the parent, got %+v", rows)
	c.Eq(child, rows[0].key, "the one row is keyed by the child")
	c.StrContains(rows[0].body, exitedReason, "the one row is the exit settle")
	c.Eq(0, len(bodiesContaining(rows, settledIdleReason)), "no turn-end settle was queued: %+v", rows)
	for _, row := range rows {
		c.False(strings.Contains(row.source, "heartbeat"), "no heartbeat was queued: %+v", row)
	}
}

func TestQuietNotifyRecordedResultWakesParent(t *testing.T) {
	c := assert.NewCollecting(t)
	r, _ := bootQuietRun(t)
	child := r.spawnChild(t, protocol.NotifyResult, writeQuietTurns(t,
		quietResultTurn(1, "DONE all green"), quietEndTurn(2), quietEndTurn(3)))
	es, idx := r.openStream(t, child)

	idx = runQuietTurn(t, r.d, es, idx, child)
	runQuietTurn(t, r.d, es, idx, child)
	r.kill(t, child)

	rows := r.inbox(t)
	settles := bodiesContaining(rows, settledIdleReason)
	c.Require().Eq(1, len(settles), "exactly one turn-end settle, for the turn that recorded a result: %+v", rows)
	c.StrContains(settles[0], "final result of "+child+": DONE all green", "the settle carries the result")
	c.Eq(1, len(bodiesContaining(rows, exitedReason)), "the exit settle follows: %+v", rows)
}

func TestQuietNotifyExitAlwaysWakes(t *testing.T) {
	c := assert.NewCollecting(t)
	r, _ := bootQuietRun(t)
	child := r.spawnChild(t, protocol.NotifyResult, writeQuietTurns(t, quietEndTurn(1)))
	es, idx := r.openStream(t, child)

	runQuietTurn(t, r.d, es, idx, child)
	r.kill(t, child)

	rows := r.inbox(t)
	c.Require().Eq(1, len(rows), "the exit settle is the only row: %+v", rows)
	c.StrContains(rows[0].body, exitedReason, "a quiet child's exit still reaches its parent")
	c.Eq(child, rows[0].key, "the row is keyed by the child")
}

func TestQuietNotifyDefaultUnchanged(t *testing.T) {
	c := assert.NewCollecting(t)
	r, _ := bootQuietRun(t)
	child := r.spawnChild(t, "", writeQuietTurns(t, quietEndTurn(1), quietEndTurn(2)))
	es, idx := r.openStream(t, child)

	idx = runQuietTurn(t, r.d, es, idx, child)
	runQuietTurn(t, r.d, es, idx, child)
	r.kill(t, child)

	rows := r.inbox(t)
	c.Eq(2, len(bodiesContaining(rows, settledIdleReason)), "a default child settles once per turn: %+v", rows)
	c.Eq(1, len(bodiesContaining(rows, exitedReason)), "and once more on exit: %+v", rows)
}

func TestQuietNotifyInvalidValueRefusedByCLI(t *testing.T) {
	c := assert.NewCollecting(t)
	d := bootDaemon(t)

	var stderr bytes.Buffer
	cmd := cliCmd(t, d, "create", "quiet-"+uniqueSuffix(),
		"--kind", "fundi",
		"--cwd", t.TempDir(),
		"--model", "anthropic/claude-x",
		"--no-session",
		"--no-local-executor",
		"--detached",
		"--notify", "none",
	)
	cmd.Stderr = &stderr
	err := cmd.Run()
	c.Require().Error(err, "create --notify none must fail; stderr: %s", stderr.String())
	c.StrContains(stderr.String(), "notify must be", "the refusal names the accepted values")
	c.Eq(0, len(listChildren(t, d.control(t))), "no child was created")
}

func TestQuietNotifySurvivesDaemonRestart(t *testing.T) {
	c := assert.NewCollecting(t)
	r, id := bootQuietRun(t)
	child := r.spawnChild(t, protocol.NotifyResult, writeQuietTurns(t, quietEndTurn(1), quietEndTurn(2), quietEndTurn(3)))
	es, idx := r.openStream(t, child)
	runQuietTurn(t, r.d, es, idx, child)

	policy := func() string {
		var p string
		c.Require().NoError(openPool(t, r.dsn).QueryRow(context.Background(),
			`SELECT notify_policy FROM conversations.child WHERE child_id = $1`, child).Scan(&p), "read notify_policy")
		return p
	}
	c.Eq(protocol.NotifyResult, policy(), "the policy is stored on the row")

	_ = r.d.proc.Process.Signal(syscall.SIGTERM)
	_ = r.d.proc.Wait()
	r.d = restartDaemon(t, r.d, id, quietNotifyEnv()...)
	waitForStatus(t, r.d, child, "idle", 30*time.Second)
	c.Eq(protocol.NotifyResult, policy(), "the policy survives the restart")

	es2, idx2 := r.openStream(t, child)
	runQuietTurn(t, r.d, es2, idx2, child)
	r.kill(t, child)

	rows := r.inbox(t)
	c.Eq(0, len(bodiesContaining(rows, settledIdleReason)), "plain turn ends stay dropped after the restart: %+v", rows)
	c.Eq(1, len(bodiesContaining(rows, exitedReason)), "the exit settle is the barrier: %+v", rows)
}
