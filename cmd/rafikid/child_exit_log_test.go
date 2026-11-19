// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// spawnExitingChild runs a real short-lived process and waits for it, so
// handleChildExit sees a genuine exit code and a genuine stderr.
func spawnExitingChild(t *testing.T, childID, script string) *child.Child {
	t.Helper()
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  childID,
		Cwd:      t.TempDir(),
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", script},
		Provider: child.ClaudeProvider{},
	})
	assert.NewAborting(t).Require().NoError(err, "Spawn")
	<-ch.Done()
	return ch
}

// A child's death is logged with what ended it. Nothing else in the daemon log
// records an exit, so an operator whose claude child died at launch had a
// child_exited event and no line to grep — the "we aren't logging enough to
// tell what went wrong" shape. The stderr tail is the whole diagnosis for a
// launch failure, and it now reaches the daemon from an executor-hosted child
// too (pkg/daraja/host.go relays it).
func TestChildExitIsLoggedWithKindAndStderr(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	c, _, _ := settleFixture(t)
	logs := captureLogs(t)

	c.st.Insert(&childstore.Session{
		ChildID: "c_claude_dead", Name: "claude-worker", Kind: protocol.KindClaude,
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	ch := spawnExitingChild(t, "c_claude_dead", "echo 'Error: invalid API key' >&2; exit 1")
	c.cm.Add("c_claude_dead", ch)

	c.handleChildExit("c_claude_dead", ch)

	got := logs.String()
	ck.StrContains(got, "child exited uncleanly", "an unclean exit must be logged: %s", got)
	ck.StrContains(got, "kind=claude", "the log must name the kind: %s", got)
	ck.StrContains(got, "exitCode=1", "the log must name the exit code: %s", got)
	ck.StrContains(got, "invalid API key",
		"the child's own stderr must reach the log: %s", got)
}

// A clean exit is still recorded — at INFO, without a stderr tail, so a normal
// child's death is visible in the log without every kill looking like a fault.
func TestCleanChildExitIsLoggedAtInfoWithoutStderr(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	c, _, _ := settleFixture(t)
	logs := captureLogs(t)

	c.st.Insert(&childstore.Session{
		ChildID: "c_fundi_clean", Name: "worker", Kind: protocol.KindFundi,
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	ch := spawnExitingChild(t, "c_fundi_clean", "echo chatter >&2; exit 0")
	c.cm.Add("c_fundi_clean", ch)

	c.handleChildExit("c_fundi_clean", ch)

	got := logs.String()
	ck.StrContains(got, `level=INFO msg="child exited"`, "a clean exit is INFO: %s", got)
	ck.False(strings.Contains(got, "stderr="),
		"a clean exit carries no stderr tail: %s", got)
}

// exitedRingAfterExit runs a child that prints one frame through
// handleChildExit with a dumper in the given mode and returns the controller.
func exitedRingAfterExit(t *testing.T, childID string, mode persist.Mode, script string) *Controller {
	t.Helper()
	c, _, _ := settleFixture(t)
	c.logsDir = t.TempDir()
	c.dumper = persist.NewLogDumper(c.logsDir, mode)
	c.st.Insert(&childstore.Session{
		ChildID: childID, Name: childID, Kind: protocol.KindClaude,
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	ch := spawnExitingChild(t, childID, script)
	c.cm.Add(childID, ch)
	c.handleChildExit(childID, ch)
	return c
}

// Once an exit's output is on disk the daemon stops holding it in memory:
// keeping every exited child's ring grew the heap with the number of children
// ever run. Reads then come from the dump, as they do after a restart.
func TestExitedRingIsReleasedOnceDumped(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	c := exitedRingAfterExit(t, "c_dumped", persist.ModeOnExit, `echo '{"type":"system"}'; exit 0`)

	snap, _ := c.st.Get("c_dumped")
	ck.Eq(protocol.StatusExited, snap.Status, "child is exited")
	ck.Eq(0, len(snap.ExitedRing), "the in-memory ring must be released after a successful dump")

	res, err := c.GetRecent("c_dumped", recentQuery{})
	ck.Require().NoError(err, "GetRecent")
	ck.Require().Eq(1, len(res.Events), "GetRecent serves the dumped frame")
	ck.StrContains(string(res.Events[0]), `"type":"system"`, "dumped frame")
}

// When nothing was written to disk the in-memory ring is the only copy, so it
// stays: ModeNever and a clean exit under ModeOnFailure both write nothing yet
// return no error from Dump.
func TestExitedRingIsKeptWhenNothingWasDumped(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	for _, mode := range []persist.Mode{persist.ModeNever, persist.ModeOnFailure} {
		id := "c_kept_" + string(mode)
		c := exitedRingAfterExit(t, id, mode, `echo '{"type":"system"}'; exit 0`)
		snap, _ := c.st.Get(id)
		ck.Eq(1, len(snap.ExitedRing), "mode %s: the ring is the only copy and must be kept", mode)
		res, err := c.GetRecent(id, recentQuery{})
		ck.Require().NoError(err, "GetRecent %s", mode)
		ck.Eq(1, len(res.Events), "mode %s: GetRecent serves the in-memory frame", mode)
	}
}
