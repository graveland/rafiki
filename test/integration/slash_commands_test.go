// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof of the daemon's slash-command interception through a REAL
// daemon subprocess and a REAL child process, over the Connect control socket.
//
// The unit tests in cmd/rafikid/slash_commands_test.go pin the controller's
// classification in isolation; only these can prove the Connect `Send` verb
// reaches it (connectAccepter.Accept), that /exit's Kill really ends the child
// (its status leaves the live set) while leaving its row resumable, that
// /clear's arm survives the round trip through a live claude process reporting
// a NEW session id, and that a refused /clear writes no inbox row.
//
// The claude children are the fake-pi.sh fixture, selected at daemon boot with
// CLAUDE_BINARY. Its `__claude_init:<id>` mode makes the fake report a claude
// system/init, and a `/clear` prompt makes it report a new `<held id>-cleared`,
// which is the id the daemon must ADOPT (not treat as a session change that
// ends the child). Executors are off, so claude spawns take the local-subprocess
// path the fake observes.
//
// -count=1 always: go test caching cannot see through to a daemon subprocess.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// fakePiBin locates the committed fake-pi.sh fixture. Absolute, so the daemon
// subprocess — which inherits the test process's cwd — always resolves it.
func fakePiBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot, "test", "integration", "fake-pi.sh")
	_, err := os.Stat(p)
	assert.NewAborting(t).NoError(err, "fake-pi.sh not found at %s", p)
	return p
}

// bootSlashClaudeDaemon boots a daemon whose claude children are fake-pi.sh.
// CLAUDE_BINARY selects the fixture for every spawn on this daemon
// (resolveClaudeBinary's override chain); executors are off so a claude spawn
// takes the local-subprocess path that observes it.
func bootSlashClaudeDaemon(t *testing.T) *daemon {
	t.Helper()
	d := bootDaemonDB(t, nextDaemonID(), append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"CLAUDE_BINARY="+fakePiBin(t),
	)...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return d
}

// waitForChildSessionID polls the live child until its row carries want,
// failing the test at the deadline with the last observed summary or GetChild
// error (a child that died mid-wait reads as NotFound, which is itself the
// symptom worth printing).
func waitForChildSessionID(t *testing.T, client rafikiv1connect.ControlClient, childID, want string, timeout time.Duration) *rafikiv1.ChildSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *rafikiv1.ChildSummary
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		cancel()
		if err != nil {
			lastErr = err
		} else {
			last = resp.Msg.GetChild()
			lastErr = nil
			if last.GetSessionId() == want {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("child %s never reported session id %q: GetChild failed: %v", childID, want, lastErr)
	}
	t.Fatalf("child %s never reported session id %q; last = session_id %q, status %q, labels %v",
		childID, want, last.GetSessionId(), last.GetStatus(), last.GetLabels())
	return nil
}

// waitForChildStatus polls the live child until its status reaches want,
// failing the test at the deadline. ListChildren enumerates exited rows too
// (an exited child stays visible so a cockpit can offer resume/close), so a
// child "leaving the live list" is observed here as its status leaving the
// live set for protocol.StatusExited.
func waitForChildStatus(t *testing.T, client rafikiv1connect.ControlClient, childID, want string, timeout time.Duration) *rafikiv1.ChildSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *rafikiv1.ChildSummary
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		cancel()
		if err != nil {
			lastErr = err
		} else {
			last = resp.Msg.GetChild()
			lastErr = nil
			if last.GetStatus() == want {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("child %s never reached status %q: GetChild failed: %v", childID, want, lastErr)
	}
	t.Fatalf("child %s never reached status %q; last = status %q, session_id %q, labels %v",
		childID, want, last.GetStatus(), last.GetSessionId(), last.GetLabels())
	return nil
}

// sendSlashPrompt sends one prompt frame over Connect and returns the raw error
// so a test can assert on the refusal instead of aborting on it.
func sendSlashPrompt(t *testing.T, client rafikiv1connect.ControlClient, childID, text string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
		}},
	}))
	return err
}

// TestSlashExitKillsTheChild: a `/exit` prompt sent through the Connect Send
// verb ends the child — its status leaves the live set — but the row is NOT
// closed: closed_at stays NULL, so the child is still resumable. Kill and
// Close are different lifecycle events, and /exit must only do the former.
func TestSlashExitKillsTheChild(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)

	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	t.Cleanup(func() { os.RemoveAll(d.homeDir) })

	child := d.spawnChild(t)
	client := d.control(t)

	ck.NoError(sendSlashPrompt(t, client, child, "/exit"), "Send(/exit)")

	waitForChildStatus(t, client, child, string(protocol.StatusExited), 15*time.Second)

	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	var closedAt *time.Time
	ck.NoError(pool.QueryRow(context.Background(),
		`SELECT closed_at FROM conversations.child WHERE child_id = $1`, child).Scan(&closedAt),
		"read the /exit child's row")
	ck.Nil(closedAt, "a /exit child must stay resumable: closed_at = %v, want NULL", closedAt)
}

// TestSlashClearAdoptsTheNewSessionID: /clear on a claude child marks the
// conversation and arms the session-id guard, then delivers the prompt. The
// child's process reports a NEW session id; the daemon ADOPTS it — the row
// moves to the new id, the child keeps running, and nothing is labelled a
// session error. Without the arm the id change would end the child.
func TestSlashClearAdoptsTheNewSessionID(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)

	d := bootSlashClaudeDaemon(t)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       "/tmp",
		Kind:      protocol.KindClaude,
		Model:     "anthropic/sonnet-latest",
		NoSession: true,
	}))
	ck.Require().NoError(err, "spawn claude child")
	child := resp.Msg.GetChildId()
	ck.Require().NotEq("", child, "spawn returned no childId")

	// Establish the first session id the daemon holds, so the /clear id is
	// derivable (fake-pi.sh reports `<held id>-cleared`).
	sendPrompt(t, client, child, "__claude_init:sess-1")
	waitForChildSessionID(t, client, child, "sess-1", 30*time.Second)

	sendPrompt(t, client, child, "/clear")
	summary := waitForChildSessionID(t, client, child, "sess-1-cleared", 30*time.Second)

	ck.Eq("sess-1-cleared", summary.GetSessionId(), "the /clear child did not adopt the new session id")
	ck.NotEq(string(protocol.StatusExited), summary.GetStatus(), "the /clear child was ended instead of adopting the new id")
	ck.Eq("", summary.GetLabels()["rafiki/session-error"],
		"adopting a /clear session id must not label the row a session error")
}

// TestSlashClearOnFundiIsRefused: /clear applies to claude children only. On a
// fundi child the Send is refused InvalidArgument (reason invalid_args) and
// NOTHING is queued — no inbox row is written, so there is nothing for the
// child's idle drain to deliver later.
func TestSlashClearOnFundiIsRefused(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)

	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	t.Cleanup(func() { os.RemoveAll(d.homeDir) })

	child := d.spawnChild(t)
	client := d.control(t)

	err := sendSlashPrompt(t, client, child, "/clear")
	ck.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "Send(/clear) on a fundi child = %v, want %v", err, connect.CodeInvalidArgument)
	ck.Eq(protocol.ErrInvalidArgs, rpcreason.Reason(err), "Send(/clear) reason = %q, want %q", rpcreason.Reason(err), protocol.ErrInvalidArgs)

	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	var n int
	ck.NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.agent_inbox WHERE child_id = $1`, child).Scan(&n),
		"count inbox rows for the fundi child")
	ck.Eq(0, n, "a refused /clear must queue no inbox row; found %d", n)
}
