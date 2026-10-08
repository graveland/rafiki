// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// recordedSandboxCall is one verb that reached the Controller, with the
// identity and caller child it was handed. The whole point of the child-bound
// manager is what these carry, so a recording fake observes it directly — the
// real Controller's sandbox verbs never branch on the admin bit, so behaviour
// alone cannot distinguish a non-admin identity from an admin one.
type recordedSandboxCall struct {
	verb        string
	owner       users.Identity
	callerChild string
	ref         string
}

type recordingSandboxControl struct {
	calls []recordedSandboxCall
	info  protocol.SandboxInfo
	list  []protocol.SandboxInfo
	err   error
	// sync is the path-sync backend the binding reaches through syncer(). nil
	// models a daemon whose executor pool was never wired, which is the
	// "path sync is not available" case.
	sync *pathSyncer
}

func (r *recordingSandboxControl) syncer() *pathSyncer { return r.sync }

func (r *recordingSandboxControl) SandboxCreate(_ context.Context, owner users.Identity, callerChild string, _ protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	r.calls = append(r.calls, recordedSandboxCall{verb: "create", owner: owner, callerChild: callerChild})
	return r.info, r.err
}

func (r *recordingSandboxControl) SandboxList(owner users.Identity) ([]protocol.SandboxInfo, error) {
	r.calls = append(r.calls, recordedSandboxCall{verb: "list", owner: owner})
	return r.list, r.err
}

func (r *recordingSandboxControl) SandboxRemove(_ context.Context, owner users.Identity, callerChild, ref string) error {
	r.calls = append(r.calls, recordedSandboxCall{verb: "remove", owner: owner, callerChild: callerChild, ref: ref})
	return r.err
}

// TestSandboxToolBindingPassesChildAndNonAdminIdentity pins the binding's one
// job: every verb reaches the Controller with the construction-time child id and
// the owner's NON-admin identity, and neither is ever a tool argument.
func TestSandboxToolBindingPassesChildAndNonAdminIdentity(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingSandboxControl{}
	m := &controllerSandboxes{c: rec, childID: "c_kid", ownerUserID: "u-owner"}

	_, err := m.Create(context.Background(), protocol.SandboxSpec{Name: "box"})
	c.NoError(err, "Create")
	_, err = m.List(context.Background())
	c.NoError(err, "List")
	c.NoError(m.Remove(context.Background(), "box"), "Remove")

	c.Len(rec.calls, 3, "every verb reached the Controller")
	for _, call := range rec.calls {
		c.Eq("u-owner", call.owner.UserID, "%s owner user id", call.verb)
		c.False(call.owner.IsAdmin, "%s identity must never be admin", call.verb)
	}
	// callerChild is the child id on the verbs that carry one, and absent on
	// List (which has no child dimension — SandboxList is keyed on the owner).
	c.Eq("c_kid", rec.calls[0].callerChild, "Create caller child")
	c.Eq("", rec.calls[1].callerChild, "List carries no caller child")
	c.Eq("c_kid", rec.calls[2].callerChild, "Remove caller child")
	c.Eq("box", rec.calls[2].ref, "Remove ref is passed through")
}

// TestSandboxToolBindingManagersAreIsolated pins that two managers built for two
// children never see each other's id: each carries only its own construction-time
// child id to the Controller, so neither can act as the other.
func TestSandboxToolBindingManagersAreIsolated(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingSandboxControl{}
	a := &controllerSandboxes{c: rec, childID: "c_a", ownerUserID: "u-owner"}
	b := &controllerSandboxes{c: rec, childID: "c_b", ownerUserID: "u-owner"}

	c.NoError(a.Remove(context.Background(), "r-a"), "A removes its own row")
	c.NoError(b.Remove(context.Background(), "r-b"), "B removes its own row")

	c.Len(rec.calls, 2, "each manager made exactly one call")
	c.Eq("c_a", rec.calls[0].callerChild, "A's binding carries c_a")
	c.Eq("c_b", rec.calls[1].callerChild, "B's binding carries c_b")
	c.NotEq("c_a", rec.calls[1].callerChild, "B never sees A's id")
}

// TestSandboxToolBindingEmptyChildRefusedAtConstruction pins the safety rule: a
// binding built with an empty child id is REFUSED at construction (nil), rather
// than degrading into a callerChild == "" call the Controller would treat as an
// operator's. A real child id yields a binding.
func TestSandboxToolBindingEmptyChildRefusedAtConstruction(t *testing.T) {
	c := assert.NewAborting(t)

	c.Nil(newControllerSandboxes(&Controller{}, "", "u-owner"),
		"an empty child id must not produce a binding that could act as an operator")
	c.NotNil(newControllerSandboxes(&Controller{}, "c_kid", "u-owner"),
		"a child-bound manager is built when the child id is present")
}

// TestSandboxToolBindingWiredIntoRuntimeOptions pins the daemon wiring: a fundi
// child on a daemon with a sandbox table gets the binding on its RuntimeOptions,
// and a daemon with none declines (nil) rather than advertising verbs that can
// only fail.
func TestSandboxToolBindingWiredIntoRuntimeOptions(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}

	ro, err := ctrl.agentRuntimeOptions(req, "c_kid", false, "brent", "u-owner")
	ck.Require().NoError(err, "agentRuntimeOptions without a sandbox store")
	ck.Nil(ro.Sandboxes, "a daemon with no sandbox table declines the sandbox tools")

	ctrl.sandboxStore = newFakeSandboxStore()
	ro, err = ctrl.agentRuntimeOptions(req, "c_kid", false, "brent", "u-owner")
	ck.Require().NoError(err, "agentRuntimeOptions with a sandbox store")
	ck.NotNil(ro.Sandboxes, "a daemon with a sandbox table binds the sandbox tools")
}

// TestSandboxSyncToolBindingUnavailableWithoutSyncer pins the pool-less daemon:
// a child-bound manager whose Controller has no path-sync backend answers both
// transfer verbs with the explicit unavailable error, never a nil-pointer panic.
func TestSandboxSyncToolBindingUnavailableWithoutSyncer(t *testing.T) {
	c := assert.NewAborting(t)
	m := &controllerSandboxes{c: &recordingSandboxControl{}, childID: "c_kid", ownerUserID: "u-owner"}

	_, err := m.Sync(context.Background(), protocol.SyncPathRequest{})
	ce := controllerErr(t, err)
	c.Eq(protocol.ErrInternal, ce.Code, "Sync code")
	c.Eq("path sync is not available on this daemon", ce.Message, "Sync message")

	_, err = m.SyncRepo(context.Background(), protocol.SyncRepoRequest{})
	ce = controllerErr(t, err)
	c.Eq(protocol.ErrInternal, ce.Code, "SyncRepo code")
	c.Eq("path sync is not available on this daemon", ce.Message, "SyncRepo message")
}

// TestSandboxSyncToolBindingPassesChildAndNonAdminIdentity pins that Sync and
// SyncRepo reach the syncer with the construction-time child id and the owner's
// NON-admin identity — exactly the values Create passes. The fixture's "box" is
// reachable only to c-child and exec-src only to u-owner, so the success and the
// two refusals together pin both, and neither is a tool argument.
func TestSandboxSyncToolBindingPassesChildAndNonAdminIdentity(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newSandboxSyncFixture(t)

	own := &controllerSandboxes{c: &recordingSandboxControl{sync: fx.p}, childID: "c-child", ownerUserID: "u-owner"}
	c.False(own.owner().IsAdmin, "the bound owner identity must never be admin")

	res, err := own.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy")},
	})
	c.NoError(err, "the child that created the sandbox may sync into it")
	c.Eq(int64(1), res.Files, "one file moved")

	sibling := &controllerSandboxes{c: &recordingSandboxControl{sync: fx.p}, childID: "c-other", ownerUserID: "u-owner"}
	_, err = sibling.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy2")},
	})
	c.Error(err, "a sibling must not reach the sandbox")
	c.StrContains(err.Error(), "not reachable", "sibling refusal")

	stranger := &controllerSandboxes{c: &recordingSandboxControl{sync: fx.p}, childID: "c-child", ownerUserID: "u-stranger"}
	_, err = stranger.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy3")},
	})
	c.Error(err, "a stranger owner must not reach the executors")
	c.StrContains(err.Error(), "not reachable", "owner refusal")
}
