// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
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
}

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

// recordedSyncCall is one path-sync verb that reached the backend, with the
// owner identity and caller child it was handed. The real *pathSyncer branches
// on neither the admin bit nor the child id directly, so a recording fake is
// the only way to pin what the adapter delivers.
type recordedSyncCall struct {
	owner       users.Identity
	callerChild string
}

// recordingPathSync is a pathSyncBackend that records every call. err, when
// set, is returned by both verbs on every call (and the call is still
// recorded).
type recordingPathSync struct {
	syncCalls []recordedSyncCall
	repoCalls []recordedSyncCall
	err       error
}

func (r *recordingPathSync) SyncPath(_ context.Context, owner users.Identity, callerChild string, _ protocol.SyncPathRequest) (protocol.SyncPathResult, error) {
	r.syncCalls = append(r.syncCalls, recordedSyncCall{owner: owner, callerChild: callerChild})
	if r.err != nil {
		return protocol.SyncPathResult{}, r.err
	}
	return protocol.SyncPathResult{}, nil
}

func (r *recordingPathSync) SyncRepo(_ context.Context, owner users.Identity, callerChild string, _ protocol.SyncRepoRequest) (protocol.SyncRepoResult, error) {
	r.repoCalls = append(r.repoCalls, recordedSyncCall{owner: owner, callerChild: callerChild})
	if r.err != nil {
		return protocol.SyncRepoResult{}, r.err
	}
	return protocol.SyncRepoResult{}, nil
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

// TestSandboxSyncToolBindingPassesNonAdminOwnerAndChild pins that Sync and
// SyncRepo hand the syncer the construction-time child id and the owner's
// NON-admin identity — a mutant that set IsAdmin or blanked the child id turns
// this red.
func TestSandboxSyncToolBindingPassesNonAdminOwnerAndChild(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingPathSync{}
	m := &controllerSandboxes{c: &recordingSandboxControl{}, sync: rec, childID: "c_kid", ownerUserID: "u-owner"}

	if _, err := m.Sync(context.Background(), protocol.SyncPathRequest{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.SyncRepo(context.Background(), protocol.SyncRepoRequest{}); err != nil {
		t.Fatalf("SyncRepo: %v", err)
	}

	c.Len(rec.syncCalls, 1, "Sync reached the syncer once")
	c.Len(rec.repoCalls, 1, "SyncRepo reached the syncer once")
	for _, call := range rec.syncCalls {
		c.Eq("u-owner", call.owner.UserID, "Sync owner user id")
		c.False(call.owner.IsAdmin, "Sync owner identity must never be admin")
		c.Eq("c_kid", call.callerChild, "Sync carries the closed-over child id")
	}
	for _, call := range rec.repoCalls {
		c.Eq("u-owner", call.owner.UserID, "SyncRepo owner user id")
		c.False(call.owner.IsAdmin, "SyncRepo owner identity must never be admin")
		c.Eq("c_kid", call.callerChild, "SyncRepo carries the closed-over child id")
	}
}

// TestSandboxSyncToolBindingRedactsNonCodedErrors pins the one error boundary
// the two faces share: a coded *connectapi.ControllerError passes through
// verbatim, and anything else — a raw error that may carry a DSN — is replaced
// by the generic text and never reaches the caller (and so never the model).
func TestSandboxSyncToolBindingRedactsNonCodedErrors(t *testing.T) {
	c := assert.NewAborting(t)
	coded := &connectapi.ControllerError{Code: protocol.ErrNotFound, Message: `executor "box" is not reachable by this caller`}
	raw := errors.New("failed to connect to `host=db user=rafiki`: postgres://user:secret@host/db")

	callSync := func(sync pathSyncBackend) error {
		m := &controllerSandboxes{c: &recordingSandboxControl{}, sync: sync, childID: "c_kid", ownerUserID: "u-owner"}
		_, err := m.Sync(context.Background(), protocol.SyncPathRequest{})
		return err
	}
	callRepo := func(sync pathSyncBackend) error {
		m := &controllerSandboxes{c: &recordingSandboxControl{}, sync: sync, childID: "c_kid", ownerUserID: "u-owner"}
		_, err := m.SyncRepo(context.Background(), protocol.SyncRepoRequest{})
		return err
	}

	for name, call := range map[string]func(pathSyncBackend) error{"Sync": callSync, "SyncRepo": callRepo} {
		if got := call(&recordingPathSync{err: coded}); got != error(coded) {
			t.Fatalf("%s: a coded ControllerError must pass through verbatim, got %v", name, got)
		}
		got := call(&recordingPathSync{err: raw})
		c.Eq("path sync failed: internal error", got.Error(), "%s: a raw error must be redacted", name)
		c.NotStrContains(got.Error(), "secret", "%s: the DSN credential must not reach the returned text", name)
		c.NotStrContains(got.Error(), "postgres://", "%s: the DSN must not reach the returned text", name)
	}
}

// TestSandboxSyncToolBindingPassesChildAndNonAdminIdentity exercises the REAL
// *pathSyncer over the fake pool: the fixture's "box" is reachable only to
// c-child and exec-src only to u-owner, so the success and the two refusals
// together pin the child id and the owner. SyncRepo is exercised the same way;
// a binding that BLANKED its child id would be treated as an operator and the
// sibling's refusal would disappear.
func TestSandboxSyncToolBindingPassesChildAndNonAdminIdentity(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newSandboxSyncFixture(t)

	own := &controllerSandboxes{c: &recordingSandboxControl{}, sync: fx.p, childID: "c-child", ownerUserID: "u-owner"}
	c.False(own.owner().IsAdmin, "the bound owner identity must never be admin")

	res, err := own.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy")},
	})
	c.NoError(err, "the child that created the sandbox may sync into it")
	c.Eq(int64(1), res.Files, "one file moved")

	// SyncRepo over the same real syncer and fake pool: both endpoints resolve
	// for the creating child, so the verb proceeds to git (the source is not a
	// repository). Blanking the child id would let the sibling below reach the
	// sandbox and this refusal would vanish.
	_, err = own.SyncRepo(context.Background(), protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "repo")},
		Dst:    protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "repo")},
		Branch: "main",
	})
	c.Error(err, "the source is not a git repository")
	c.StrContains(err.Error(), "source is not a git repository", "SyncRepo passed both resolves for the creating child")
	c.NotStrContains(err.Error(), "not reachable", "a reachable sandbox must not be refused")

	sibling := &controllerSandboxes{c: &recordingSandboxControl{}, sync: fx.p, childID: "c-other", ownerUserID: "u-owner"}
	_, err = sibling.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy2")},
	})
	c.Error(err, "a sibling must not reach the sandbox")
	c.StrContains(err.Error(), "not reachable", "sibling refusal")

	_, err = sibling.SyncRepo(context.Background(), protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "repo")},
		Dst:    protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "repo")},
		Branch: "main",
	})
	c.Error(err, "a sibling must not reach the sandbox on SyncRepo either")
	c.StrContains(err.Error(), "not reachable", "sibling SyncRepo refusal")

	stranger := &controllerSandboxes{c: &recordingSandboxControl{}, sync: fx.p, childID: "c-child", ownerUserID: "u-stranger"}
	_, err = stranger.Sync(context.Background(), protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "exec-src", Path: filepath.Join(fx.src, "hello.txt")},
		Dst: protocol.SyncEndpoint{Executor: "box", Path: filepath.Join(fx.dst, "copy3")},
	})
	c.Error(err, "a stranger owner must not reach the executors")
	c.StrContains(err.Error(), "not reachable", "owner refusal")
}
