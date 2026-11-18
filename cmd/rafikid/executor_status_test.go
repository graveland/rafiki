// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
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

func (f *executorStatusFixture) setChildSet(id string, set []string) {
	f.t.Helper()
	if err := f.ctrl.st.SetExecutorSet(id, set); err != nil {
		f.t.Fatalf("set executor set on %s: %v", id, err)
	}
}

func (f *executorStatusFixture) childRows(id string) []protocol.ExecutorStatus {
	f.t.Helper()
	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{
		OwnerUserID: statusHuman, OwnerName: installTestOwnerName, ChildID: id,
	})
	if err != nil {
		f.t.Fatalf("ExecutorStatuses for %s: %v", id, err)
	}
	return rows
}

// A child sees exactly the owner's rows its executor set names, whether or not
// it is bound to or installed any of them.
func TestExecutorStatusesChildSeesItsSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	f.setChildSet(statusChild, []string{"exec-down"})

	c.EqDiff([]string{"bluebox"}, statusNames(f.childRows(statusChild)),
		"only the named row, not the bound machine or the lineage's install")

	f.setChildSet(statusChild, []string{"exec-down", "exec-sibling"})
	c.EqDiff([]string{"bluebox", "sibbox"}, statusNames(f.childRows(statusChild)), "every named row")

	f.setChildSet(statusChild, []string{})
	c.Len(f.childRows(statusChild), 0, "an empty set addresses nothing")
}

// A set may name another owner's executor id; the owner match still hides it.
func TestExecutorStatusesChildNeverSeesAForeignRowInItsSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	f.setChildSet(statusChild, []string{"exec-foreign", "exec-down"})
	c.EqDiff([]string{"bluebox"}, statusNames(f.childRows(statusChild)), "the foreign id is dropped")
}

// "all" is every row the owner owns, the human arm's row set, and never a
// foreign owner's.
func TestExecutorStatusesAllChildSeesTheOwnersRows(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	f.setChildSet(statusChild, []string{childstore.ExecutorSetAll})

	c.EqDiff([]string{"bluebox", "greyshift", "homebox", "kidbox", "sibbox"}, statusNames(f.childRows(statusChild)),
		"the owner's whole fleet and no foreign row")
}

// A legacy row (nil set) reads through its selector: a match shows those rows,
// an empty selector is all, a malformed one shows nothing.
func TestExecutorStatusesLegacyChildSeesItsSelector(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	setSelector := func(sel string) {
		if err := f.ctrl.st.Update(statusChild, func(s *childstore.Session) { s.ExecutorSelector = sel }); err != nil {
			t.Fatalf("set selector: %v", err)
		}
	}

	setSelector("machine=bluebox")
	c.EqDiff([]string{"bluebox"}, statusNames(f.childRows(statusChild)), "the selector's match")

	setSelector("machine=notmine")
	c.Len(f.childRows(statusChild), 0, "a selector matching only a foreign row shows nothing")

	setSelector("")
	c.Len(f.childRows(statusChild), 5, "an empty legacy selector is all of the owner's rows")

	setSelector("not a selector !!")
	c.Len(f.childRows(statusChild), 0, "a malformed selector shows nothing")

	c.Len(f.childRows("c_no_row"), 0, "a caller with no row shows nothing")
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
	f.setChildSet(statusChild, []string{"exec-home", "exec-kid-install"})
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

// An installed row carries the transport it was installed with and the daemon's
// last launch outcome, so a reader can tell a direct machine (it dials, it
// survives the launcher) from a relayed one without asking anyone.
func TestExecutorStatusesShowModeAndLastLaunch(t *testing.T) {
	c := assert.NewAborting(t)
	f := newExecutorStatusFixture(t)
	f.setInstallMode("inst-kid", executorinstall.ModeDirect)
	f.state.setOutcome("exec-kid-install", "ok: direct launch started in 900ms")

	rows, err := f.ctrl.ExecutorStatuses(context.Background(), installCaller{
		OwnerUserID: statusHuman, OwnerName: installTestOwnerName,
	})
	c.Require().NoError(err, "ExecutorStatuses for a human caller")

	byID := statusByID(rows)
	kid := byID["exec-kid-install"]
	c.Eq(executorinstall.ModeDirect, kid.InstallMode, "the recipe's transport is shown")
	c.Eq("ok: direct launch started in 900ms", kid.LastLaunch, "and the last launch outcome")

	// A recipe written before the mode column existed reads as the relay, which
	// is how it behaved — never as "unknown".
	sib := byID["exec-sibling"]
	c.Eq(executorinstall.ModeTunnel, sib.InstallMode, "an empty mode reads as tunnel")
	c.Eq("", sib.LastLaunch, "a machine this daemon never launched has no outcome")

	// A machine that was not installed over ssh has neither.
	down := byID["exec-down"]
	c.Eq("", down.InstallMode, "an enrolled machine has no install transport")
	c.Eq("", down.LastLaunch, "and no launch outcome")
}

// setInstallMode records the transport on a seeded recipe, as an install would.
func (f *executorStatusFixture) setInstallMode(id, mode string) {
	f.installs.mu.Lock()
	defer f.installs.mu.Unlock()
	r, ok := f.installs.rows[id]
	if !ok {
		f.t.Fatalf("no seeded install %q", id)
	}
	r.Mode = mode
	f.installs.rows[id] = r
}
