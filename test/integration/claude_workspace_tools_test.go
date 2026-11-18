// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof that a claude spawn's workspace-tools mode survives the whole
// trip the brief's unit tests cannot see: the Connect request, the daemon's
// resolution, the Session literal Spawn writes, the childstore row, and the
// ChildSummary the control plane reports back. The unit tests pin the resolution
// and the resume rebuild; only this covers the persistence link between them —
// drop the `WorkspaceTools:` line from Spawn's Session literal and a resumed
// rafiki-mode child comes back native with nothing else failing.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestWorkspaceToolsRafikiClaudeChildReportsTheModeItSpawnedWith spawns a claude
// child with workspace_tools=rafiki onto a live executor and reads the mode back
// off GetChild.
//
// The executor resolves its hosted claude binary with exec.LookPath("claude"),
// so a fake `claude` earlier on PATH is what keeps the launch off a real
// install; the child only has to LAUNCH for the row to be written, which is all
// this asserts. The daemon's own proxy face is up (bootGrantDaemon waits for its
// URL), which is what makes rafiki mode admissible at all.
func TestWorkspaceToolsRafikiClaudeChildReportsTheModeItSpawnedWith(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := requireExecutorDB(t)

	binDir := t.TempDir()
	c.NoError(os.Symlink(buildFakeClaude(t), filepath.Join(binDir, "claude")),
		"symlink the fake claude fixture onto PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	g := bootGrantDaemon(t, dsn)
	// --launch claude is opt-in: without it the executor advertises no claude
	// launch support and darajaLaunchExecutor refuses the spawn.
	g.enrollExecutorExtra(t, map[string]string{"env": "home"}, "--launch", "claude")
	g.waitForLiveExecutors(t, 1)

	spawn := func(mode string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := g.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd:              t.TempDir(),
			Kind:             protocol.KindClaude,
			Model:            "anthropic/claude-x",
			ExecutorSelector: "env=home",
			WorkspaceTools:   mode,
		}))
		c.Require().NoError(err, "claude spawn (workspace_tools=%q) failed", mode)
		id := resp.Msg.GetChildId()
		t.Cleanup(func() { killChildren(t, g.control(t), id) })
		return id
	}

	rafikiID := spawn(protocol.WorkspaceToolsRafiki)
	c.Eq(protocol.WorkspaceToolsRafiki, getChild(t, g.control(t), rafikiID).GetWorkspaceTools(),
		"GetChild must report the mode the spawn resolved")

	// A spawn that names NO mode is the DEFAULT, and this daemon can serve
	// rafiki mode (executor pool, proxy face, claude proxied) — so the default
	// IS rafiki, decided by the daemon because nothing on the request said so.
	defaultID := spawn("")
	c.Eq(protocol.WorkspaceToolsRafiki, getChild(t, g.control(t), defaultID).GetWorkspaceTools(),
		"a claude spawn that named no mode must resolve to rafiki where the daemon can serve it")

	// A native PIN is the control: the report follows the request rather than
	// being a constant, and the default never overrules a declaration.
	nativeID := spawn(protocol.WorkspaceToolsNative)
	c.Eq("", getChild(t, g.control(t), nativeID).GetWorkspaceTools(),
		"a pinned native claude child must report no workspace-tools mode")

	// RESUME: relaunched from the row, never re-resolved. This daemon can
	// still serve the mode, so the resume is allowed (the refused half is
	// TestWorkspaceToolsResumeRefusedWhenTheDaemonCannotServe, cmd/rafikid) and
	// the new row must still carry it — the Session activateLiveChild inserts
	// copies snap.WorkspaceTools, and dropping that copy would bring the child
	// back native while its MCP replacements stayed served.
	killChildren(t, g.control(t), rafikiID)
	waitForChildStatus(t, g.control(t), rafikiID, string(protocol.StatusExited), 30*time.Second)
	rc, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer rcancel()
	resumed, err := g.control(t).Resume(rc, connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: rafikiID}))
	c.Require().NoError(err, "resume a servable rafiki-mode child")
	c.Eq(rafikiID, resumed.Msg.GetChildId(), "resume keeps the child id")
	c.Eq(protocol.WorkspaceToolsRafiki, getChild(t, g.control(t), rafikiID).GetWorkspaceTools(),
		"a resumed rafiki-mode child must keep the mode its row records")
}
