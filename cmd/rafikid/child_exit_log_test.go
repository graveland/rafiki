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
