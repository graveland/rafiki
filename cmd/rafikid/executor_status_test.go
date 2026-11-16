// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorStatusFixture is newInstallFixture with the row set a status listing
// has to sort out: one live launcher, one down machine of the same owner, one
// machine of ANOTHER owner, a machine this test's child is bound to, and two
// machines installed over ssh — one by the child's own lineage, one by a
// sibling's.
type executorStatusFixture struct {
	*installFixture
}

const (
	statusHuman   = installTestOwner // "u_brent"
	statusForeign = "u_other"
	statusChild   = "c_kid"
	statusSibling = "c_sib"
)

func newExecutorStatusFixture(t *testing.T) *executorStatusFixture {
	t.Helper()
	f := newInstallFixture(t)
	c := f.ctrl

	// The launcher is pool-only (no store row): a listing must still show a
	// machine that is live, which is what ExecutorList's pool merge is for.
	f.pool.live[0].Describe.LaunchKinds = []string{"fundi", "claude"}

	addRow := func(id, machine, owner string, seen time.Time) {
		f.execs.execs[id] = executors.Executor{
			ID:            id,
			Labels:        map[string]string{"machine": machine, "owner": owner},
			Isolation:     "none",
			WorkspaceMode: "pinned",
			Roots:         []string{"/srv/" + machine},
			OwnerUserID:   owner,
			Enabled:       true,
			LastSeenAt:    seen,
		}
	}
	// The launcher also has a durable row, as a machine enrolled by an operator
	// would: it is what makes an install's VIA resolvable while the launcher is
	// live, rather than only from the fallback label the install wrote.
	addRow(installTestLauncher, installTestLauncherN, statusHuman, time.Now())
	// Annotations live on the ROW (executor_annotate writes there), so the merge
	// with the live pool entry — which supplies only Connected/ConnectedAt —
	// keeps them.
	launcher := f.execs.execs[installTestLauncher]
	launcher.Annotations = map[string]string{"built": "2026-11-15"}
	f.execs.execs[installTestLauncher] = launcher
	addRow("exec-down", "bluebox", statusHuman, time.Time{})
	addRow("exec-foreign", "notmine", statusForeign, time.Now())
	addRow("exec-home", "homebox", statusHuman, time.Now())
	addRow("exec-kid-install", "kidbox", statusHuman, time.Now())
	addRow("exec-sibling", "sibbox", statusHuman, time.Now())

	seedInstall(t, f, "inst-kid", "exec-kid-install", installTestLauncher, "brent@kidbox", statusChild)
	seedInstall(t, f, "inst-sib", "exec-sibling", installTestLauncher, "brent@sibbox", statusSibling)

	// The child this test speaks for: stored, owned, and bound to exec-home.
	seedInstallChild(t, c, statusChild, "", installTestOwnerName, protocol.KindFundi)
	if _, err := c.st.SetLabels(statusChild, map[string]string{executorBindingLabel: "exec-home"}, nil); err != nil {
		t.Fatalf("bind the child's executor: %v", err)
	}
	return &executorStatusFixture{installFixture: f}
}

// seedInstall writes one live install recipe, the row an install would leave
// behind: it names the launcher, the ssh destination and the child that asked.
func seedInstall(t *testing.T, f *installFixture, id, executorID, launcher, target, byChild string) {
	t.Helper()
	err := f.installs.Insert(context.Background(), executorinstall.Row{
		ID:                 id,
		ExecutorID:         executorID,
		LauncherExecutorID: launcher,
		InstalledByChild:   byChild,
		Target:             target,
	})
	if err != nil {
		t.Fatalf("insert install %s: %v", id, err)
	}
}

func statusNames(rows []protocol.ExecutorStatus) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func statusByID(rows []protocol.ExecutorStatus) map[string]protocol.ExecutorStatus {
	out := make(map[string]protocol.ExecutorStatus, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// A human caller sees its owner's fleet and nothing else — the rows `rafiki
// executor list` shows that credential (executorAuthority), ownership compared
// by durable id, never by the display label.
func TestExecutorStatusesScopeForAHuman(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)

	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{
		OwnerUserID: statusHuman, OwnerName: installTestOwnerName,
	})
	c.NoError(err, "ExecutorStatuses for a human caller")
	c.EqDiff([]string{"bluebox", "greyshift", "homebox", "kidbox", "sibbox"}, statusNames(rows),
		"the owner's fleet, by machine name")

	byID := statusByID(rows)
	live := byID["grey-1"]
	c.True(live.Connected, "the live launcher must be marked connected")
	c.NotNil(live.ConnectedAt, "a connected executor carries its join time")
	c.EqDiff([]string{"fundi", "claude"}, live.LaunchKinds, "the live pool's launch kinds")
	c.Eq("2026-11-15", live.Annotations["built"], "annotations ride the listing")

	down := byID["exec-down"]
	c.False(down.Connected, "a store row with no pool connection is down")
	c.Nil(down.LastSeen, "never seen must be absent, not the zero time")
	c.Eq("", down.InstalledVia, "an enrolled machine was not installed")

	kid := byID["exec-kid-install"]
	c.Eq(installTestLauncherN, kid.InstalledVia, "an install names its live launcher's machine")
	c.Eq("brent@kidbox", kid.InstalledTarget, "an install names its ssh destination")
}

// A child caller sees only what its own provisioning can touch: the machine it
// is bound to, and the machines its LINEAGE installed. Its owner's wider fleet
// is invisible — the borrowed-identity rule Connect's userOnly ListExecutors
// enforces, kept narrower here rather than widened.
func TestExecutorStatusesScopeForAChild(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)

	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{
		OwnerUserID: statusHuman, OwnerName: installTestOwnerName, ChildID: statusChild,
	})
	c.NoError(err, "ExecutorStatuses for a child caller")
	c.EqDiff([]string{"homebox", "kidbox"}, statusNames(rows),
		"the bound machine plus what this child's lineage installed")
}

// A sibling's install is not the caller's to see, even though the same owner
// owns both rows: lineage, not owner, decides the child half of the scope.
func TestExecutorStatusesChildDoesNotSeeASiblingsInstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)

	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{
		OwnerUserID: statusHuman, OwnerName: installTestOwnerName, ChildID: statusSibling,
	})
	c.NoError(err, "ExecutorStatuses for the sibling")
	c.EqDiff([]string{"sibbox"}, statusNames(rows),
		"a sibling sees its own install only")
}

// An owner with no executors of its own gets an empty listing, not another
// owner's rows: ownership is compared on the durable id.
func TestExecutorStatusesIsEmptyForAnUnknownOwner(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)

	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{OwnerUserID: "u_nobody"})
	c.NoError(err, "ExecutorStatuses for an unknown owner")
	c.Len(rows, 0, "no rows belong to an unknown owner")
}

// executor_list reaches a child on the MCP face, bound to that child's own
// credential: the tool is wired, and the scope the daemon enforces is the one
// the child's listing shows.
func TestExecutorListOnTheMCPFaceScopesToTheChild(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	face, _ := mcpFaceFixture(t)
	face.SetController(f.ctrl)

	cs := mcpConnect(t, face.getServer(mcpChildRequest(statusHuman, statusChild, false)))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "executor_list", Arguments: map[string]any{},
	})
	c.NoError(err, "executor_list over MCP is a tool result, not a transport error")
	c.False(res.IsError, "the child's own machines are listable: %s", mcpToolResultText(t, res))
	text := mcpToolResultText(t, res)
	c.True(strings.Contains(text, "homebox"), "the bound machine must be listed: %s", text)
	c.True(strings.Contains(text, "kidbox"), "the lineage's install must be listed: %s", text)
	c.False(strings.Contains(text, "bluebox"), "the owner's other machines must not be: %s", text)
	c.False(strings.Contains(text, "notmine"), "another owner's machine must not be: %s", text)
}
