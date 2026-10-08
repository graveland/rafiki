package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/toolmeta"
)

// fakePool stands in for *execpool.Pool so selection is testable without a
// listener, a database or a dialling executor.
type fakePool struct {
	live    []execpool.LiveExecutor
	tickets *execpool.TicketRegistry
	// evicted records each id Evict was called for, so a test can assert a
	// second session request releases the first.
	evicted map[string]bool
}

func (f *fakePool) Live() []execpool.LiveExecutor { return f.live }
func (f *fakePool) ClientFor(id string) (tools.ExecutorClient, error) {
	return &stubExecutorClient{}, nil
}

func (f *fakePool) Tickets() *execpool.TicketRegistry {
	if f.tickets == nil {
		f.tickets = execpool.NewTicketRegistry()
	}
	return f.tickets
}

func (f *fakePool) Evict(id string) {
	if f.evicted != nil {
		f.evicted[id] = true
	}
}

// DisconnectOwner mirrors the real pool's rule — cut what the user owns, on
// the SAME Live() list the selection tests populate — so the selection tests
// keep compiling and the user-rm test drives the whole cut through one fake.
func (f *fakePool) DisconnectOwner(userID string) int {
	cut := 0
	for _, le := range f.live {
		if le.Executor.OwnerUserID == userID {
			f.Evict(le.Executor.ID)
			cut++
		}
	}
	return cut
}

// stubExecutorClient satisfies tools.ExecutorClient for selection tests, which
// never dispatch a tool call.
type stubExecutorClient struct{}

func (stubExecutorClient) Execute(context.Context, string, json.RawMessage) (toolmeta.Result, error) {
	return toolmeta.Result{}, nil
}
func (stubExecutorClient) StartJob(context.Context, string) (string, error) { return "", nil }
func (stubExecutorClient) JobOutput(context.Context, string, int64) (tools.JobSnapshot, error) {
	return tools.JobSnapshot{}, nil
}
func (stubExecutorClient) KillJob(context.Context, string) error { return nil }
func (stubExecutorClient) Ping(context.Context) error            { return nil }

func ex(id string, labels map[string]string, admits string) execpool.LiveExecutor {
	return execpool.LiveExecutor{Executor: executors.Executor{
		ID: id, Labels: labels, Admits: admits, Enabled: true,
	}}
}

// exOwned is ex plus an OwnerUserID, for the ownership-selection tests.
func exOwned(id string, labels map[string]string, admits, ownerUserID string) execpool.LiveExecutor {
	le := ex(id, labels, admits)
	le.Executor.OwnerUserID = ownerUserID
	return le
}

// selectFixture: a coordinator that landed on the `env=home` set, and a child
// beneath it.
func selectFixture(t *testing.T, parentSelector string, live ...execpool.LiveExecutor) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager(), execPool: &fakePool{live: live}, native: nativebus.New()}
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: parentSelector, MaxDepth: 1, MaxChildren: 8,
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi,
		Labels: map[string]string{
			childstore.LabelParent: "c_parent", childstore.LabelRoot: "c_parent",
			"rafiki/kind": "fundi",
		},
	})
	return c
}

// THE property. A child asking for something outside its parent's set gets
// nothing — not because its selector was proved to imply its parent's (that is
// a logic puzzle the moment notin appears, and it fails OPEN), but because the
// sets are intersected.
func TestChildCannotReachAnExecutorItsParentCouldNot(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=work",
	}, executorOwner{})
	ck.Require().Error(err, "a child reached outside its parent's effective set")
	ck.False(!strings.Contains(err.Error(), "parent") && !strings.Contains(err.Error(), "PARENT"), "the refusal must say the parent's set is why: %v", err)
}

func TestChildInheritsTheParentsSetWhenItNamesNoSelector(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("exec-work", map[string]string{"env": "work"}, ""),
		ex("exec-home", map[string]string{"env": "home"}, ""),
	)
	set, err := c.effectiveExecutorSet("c_child")
	ck.NoError(err)
	ck.False(len(set) != 1 || set[0].ID != "exec-home", "want only exec-home, got %+v", set)
}

// Narrowing is transitive: a grandchild is bounded by its grandparent.
func TestNarrowingIsTransitiveUpTheWholeChain(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		ex("a", map[string]string{"env": "home", "os": "linux"}, ""),
		ex("b", map[string]string{"env": "home", "os": "darwin"}, ""),
		ex("z", map[string]string{"env": "work", "os": "linux"}, ""),
	)
	_ = c.st.Update("c_child", func(s *childstore.Session) { s.ExecutorSelector = "os=linux" })
	c.st.Insert(&childstore.Session{
		ChildID: "c_grandchild", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_child", childstore.LabelRoot: "c_parent",
		},
	})

	set, err := c.effectiveExecutorSet("c_grandchild")
	ck.NoError(err)
	ck.False(len(set) != 1 || set[0].ID != "a", "grandchild must be bounded by env=home AND os=linux; got %+v", set)
}

// A top-level agent's set is everything live, subject to executor admission.
func TestTopLevelAgentSeesEveryAdmittingExecutor(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		ex("a", map[string]string{"env": "home"}, ""),
		ex("b", map[string]string{"env": "work"}, ""),
	)
	set, err := c.effectiveExecutorSet("c_parent")
	ck.NoError(err)
	ck.Len(set, 2, "want both, got")
}

// Scheduling failure is fast and legible. The current message says "labels do
// not match selector" for every candidate regardless of the real reason, which
// is a diagnostic that cannot distinguish a typo from a missing label from an
// executor that refused the child.
func TestNoMatchNamesTheExcludingPredicatePerExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "",
		ex("exec-home", map[string]string{"env": "home", "os": "linux"}, ""),
		ex("exec-mac", map[string]string{"env": "work", "os": "darwin"}, ""),
		ex("exec-picky", map[string]string{"env": "work", "os": "linux"}, "rafiki/kind=claude"),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=work,os=linux",
	}, executorOwner{})
	ck.Require().Error(err, "want a refusal")
	msg := err.Error()
	for _, want := range []string{
		"env=work,os=linux",     // what was required
		"exec-home", "env=home", // wrong label value, named
		"exec-mac", "os=darwin",
		"exec-picky", "admission", "rafiki/kind", // the executor refused the child
	} {
		ck.StrContains(msg, want, "refusal missing")
	}
}

// Queueing is opt-in only. A spawn that matches nothing fails NOW — silent
// queueing turns a structural mistake into a hang nobody can diagnose.
func TestNoMatchFailsImmediatelyRatherThanQueueing(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "")
	start := time.Now()
	_, err := c.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "env=nowhere"}, executorOwner{})
	ck.Error(err, "want a refusal")
	ck.LessOrEqual(time.Second, time.Since(start), "selection took")
}

func TestSortCandidatesPrefersDurableThenID(t *testing.T) {
	in := []executors.Executor{
		{ID: "zzz-durable"},
		{ID: "bbb-session", Labels: map[string]string{"kind": "session"}},
		{ID: "aaa-durable"},
		{ID: "aaa-session", Labels: map[string]string{"kind": "session"}},
	}
	sortCandidates(in)

	want := []string{"aaa-durable", "zzz-durable", "aaa-session", "bbb-session"}
	for i, w := range want {
		assert.NewAborting(t).Eq(w, in[i].ID, "position %d: want %s, got %s (full order: %v)", i, w, in[i].ID, ids(in))
	}
}

func TestSortCandidatesIsStableAcrossCalls(t *testing.T) {
	// chooseExecutor returns candidates[0] and Live() ranges a Go map, so
	// without an explicit sort a child lands on an arbitrary executor and a
	// DIFFERENT arbitrary one after a restart.
	first := []executors.Executor{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	second := []executors.Executor{{ID: "c"}, {ID: "b"}, {ID: "a"}}
	sortCandidates(first)
	sortCandidates(second)
	for i := range first {
		assert.NewAborting(t).Eq(second[i].ID, first[i].ID, "ordering is not deterministic: %v vs %v", ids(first), ids(second))
	}
}

func ids(in []executors.Executor) []string {
	out := make([]string, len(in))
	for i, e := range in {
		out[i] = e.ID
	}
	return out
}

// The session executor's admits: owner=<user> must actually match a child — the
// whole point of the daemon-attested owner label. Before it existed, every
// spawn was refused because children carried no owner label at all.
func TestAdmissionMatchesDaemonAttestedOwner(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "", ex("laptop", map[string]string{"kind": "client"}, "owner=brent"))
	c.st.Insert(&childstore.Session{
		ChildID: "c_owned", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind:   protocol.KindFundi,
		Labels: map[string]string{"owner": "brent", "rafiki/kind": "fundi"},
	})
	set, err := c.effectiveExecutorSet("c_owned")
	ck.NoError(err)
	ck.False(len(set) != 1 || set[0].ID != "laptop", "owner=brent admitted the wrong set: %+v", set)
}

// The actual spawn path, not just an already-stored child: a TOP-LEVEL spawn
// (no ParentChildID) has no childstore entry of its own to read an owner
// label back from — Spawn only inserts one after selection succeeds — so
// chooseExecutor must evaluate admission against the owner the caller
// attests for the child about to be created, not an empty label set. Every
// session executor is minted with exactly this admits selector (see
// ExecutorSession), so getting this wrong means `rafiki create` can never
// place a single top-level agent on the operator's own machine.
func TestTopLevelSpawnIsAdmittedByItsAttestedOwner(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "", ex("laptop", map[string]string{"kind": "client", "owner": "brent"}, "owner=brent"))
	req := protocol.SpawnRequest{ExecutorSelector: "owner=brent,kind=client"}

	chosen, err := c.chooseExecutor(req, executorOwner{Name: "brent"})
	ck.NoError(err, "a top-level spawn with its owner attested must reach the laptop executor")
	ck.Eq("laptop", chosen.ID, "chose")

	// And the failure mode this guards against: an unattested (or wrong)
	// owner must still be refused, not silently admitted some other way —
	// proving the fix checks the right thing rather than always succeeding.
	if _, err := c.chooseExecutor(req, executorOwner{}); err == nil {
		t.Fatal("a top-level spawn with no attested owner reached an owner-scoped executor")
	}
}

// The ownership rule (verbatim from the design): "Selection requires
// executor.owner_user_id = child.owner_user_id IN ADDITION to Admits ...
// No admin exception." These four tests pin the pieces that rule needs to
// hold, independent of Admits, which the tests above already cover.

// TestSelectRefusesOtherUsersExecutor: A's executor, empty Admits, B's
// top-level child — no match. Admits alone would admit everyone; ownership
// must still refuse.
func TestSelectRefusesOtherUsersExecutor(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "", exOwned("a-exec", map[string]string{"env": "home"}, "", "u_A"))
	_, err := c.chooseExecutor(protocol.SpawnRequest{ExecutorSelector: "env=home"}, executorOwner{UserID: "u_B", Name: "bob"})
	ck.Error(err, "B must not reach A's executor even though Admits is empty")
	// The refusal must name the ownership rule, so the reason is legible —
	// and must never name the other user.
	ck.StrContains(err.Error(), "belongs to another user", "the refusal does not name the owner mismatch: %v", err)
	ck.False(strings.Contains(err.Error(), "u_A"), "the refusal names the other user's id: %v", err)
}

// TestSelectUnownedServesOnlyUnowned: an unowned executor serves an unowned
// child, and never a user's child — with no admin exception. executorOwner
// carries no IsAdmin bit at all, which is what makes an admin exception
// structurally impossible here; "admin" is just another non-empty UserID.
func TestSelectUnownedServesOnlyUnowned(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "", exOwned("unowned-exec", map[string]string{"env": "home"}, "", ""))

	chosen, err := c.chooseExecutor(protocol.SpawnRequest{ExecutorSelector: "env=home"}, executorOwner{})
	ck.Require().NoError(err, "an unowned executor must serve an unowned child")
	ck.Eq("unowned-exec", chosen.ID, "chose")

	_, err = c.chooseExecutor(protocol.SpawnRequest{ExecutorSelector: "env=home"}, executorOwner{UserID: "u_admin", Name: "admin"})
	ck.Error(err, "an admin user's child must not reach an unowned executor — no admin exception")
}

// TestExecutorSelectionUnownedServesOnlyUnowned is a -run shim, not a second
// test: the brief's verify command matches the UNANCHORED substrings
// Executor|Owner|Session|Ticket, and "TestSelectUnownedServesOnlyUnowned"
// contains none of them (no "Executor", and "Unowned" does not contain
// "Owner"), so without this wrapper the pinned body silently never ran under
// that command. The subtest calls the pinned body by name, exactly as the
// repo's other pattern shims do.
func TestExecutorSelectionUnownedServesOnlyUnowned(t *testing.T) {
	t.Run("unowned-serves-only-unowned", TestSelectUnownedServesOnlyUnowned)
}

// TestSelectChildInheritsParentOwner: a sub-agent of A's child can use A's
// executor; B's cannot. Mirrors how Controller.Spawn actually threads
// ownership for a subagent (controllerSpawner.Spawn passes the PARENT's own
// OwnerUserID, not a re-derivation) by supplying owner.UserID directly at
// the ParentChildID call site, the same way that real path does.
func TestSelectChildInheritsParentOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "", exOwned("a-exec", map[string]string{"env": "home"}, "", "u_A"))

	chosen, err := c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home",
	}, executorOwner{UserID: "u_A"})
	ck.Require().NoError(err, "a sub-agent of A's child must reach A's executor")
	ck.Eq("a-exec", chosen.ID, "chose")

	_, err = c.chooseExecutor(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "env=home",
	}, executorOwner{UserID: "u_B"})
	ck.Error(err, "B's sub-agent must not reach A's executor")
}

// TestSelectOwnerCheckBeforeAdmits: an Admits selector that would match B's
// labels still refuses B — the owner check is not something Admits can route
// around. B's own labels ("rafiki/kind=fundi") satisfy a-exec's Admits
// selector exactly, so if the owner check ran after Admits (or not at all),
// this spawn would wrongly succeed.
func TestSelectOwnerCheckBeforeAdmits(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "", exOwned("a-exec", nil, "rafiki/kind=fundi", "u_A"))
	req := protocol.SpawnRequest{Labels: map[string]string{"rafiki/kind": "fundi"}}

	_, err := c.chooseExecutor(req, executorOwner{UserID: "u_B", Name: "bob"})
	ck.Error(err, "B's labels satisfy a-exec's Admits selector, but the owner mismatch must still refuse it")
}

// TestAgentRunnerScopesTheResumedFundiBinderToItsOwner pins the owner
// threading the resume call sites depend on: agentRunner builds the fundi
// binder from the owner it is HANDED (both call sites pass snap.Labels["owner"]
// and resumeOwnerUserID), and selection consults that owner. A resumed user
// child therefore re-binds only to an executor that user owns — never to an
// unowned one, never to another user's. The regression shape is any call site
// that stops threading resumeOwnerUserID's result into agentRunner: an empty
// owner lands here as a resumed child whose binder selects the UNOWNED
// executor instead of its own, silently widening the child's machines.
func TestAgentRunnerScopesTheResumedFundiBinderToItsOwner(t *testing.T) {
	newResumed := func(live ...execpool.LiveExecutor) (*Controller, *childstore.Snapshot) {
		t.Helper()
		ctrl := &Controller{
			st: childstore.New(), cm: newChildManager(), native: nativebus.New(),
			execPool: &fakePool{live: live}, stateDir: t.TempDir(),
		}
		stored := &childstore.Session{
			ChildID:          "c_resumed",
			Status:           protocol.StatusExited,
			StartedAt:        time.Now(),
			Kind:             protocol.KindFundi,
			Cwd:              t.TempDir(),
			Model:            "anthropic/sonnet-latest",
			ExecutorSelector: "env=home",
			WorkspaceMode:    "pinned",
			OwnerUserID:      "u-bob",
			MaxDepth:         1,
			MaxChildren:      8,
			Labels:           map[string]string{"owner": "bob"},
		}
		ctrl.st.Insert(stored)
		snap, ok := ctrl.st.Get("c_resumed")
		if !ok {
			t.Fatal("the inserted session did not read back")
		}
		return ctrl, &snap
	}

	// The resume request is built exactly as the resume paths build it, and
	// agentRunner receives exactly the arguments the call sites pass.
	drive := func(ctrl *Controller, snap *childstore.Snapshot) *boundExecutor {
		t.Helper()
		req := resumeRequestFromSnapshot(*snap, "")
		req.Kind = protocol.KindFundi
		runner, err := ctrl.agentRunner(req, "c_resumed", true, snap.Labels["owner"], snap.OwnerUserID, snap)
		if err != nil {
			t.Fatalf("agentRunner for a resumed fundi child: %v", err)
		}
		if runner == nil {
			t.Fatal("a fundi resume returns an in-process runner")
		}
		ctrl.boundMu.Lock()
		be := ctrl.bound["c_resumed"]
		ctrl.boundMu.Unlock()
		if be == nil {
			t.Fatal("the resumed child's binder must be retained")
		}
		return be
	}

	t.Run("owned executor live re-binds to it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		ctrl, snap := newResumed(
			exOwned("exec-owned", map[string]string{"env": "home"}, "", "u-bob"),
			ex("exec-free", map[string]string{"env": "home"}, ""), // unowned
			exOwned("exec-other", map[string]string{"env": "home"}, "", "u-alice"),
		)
		be := drive(ctrl, snap)
		binder, ok := be.binder.(*controllerBinder)
		ck.Require().True(ok, "the fundi binder is the controller's own")
		ck.Eq("u-bob", binder.owner.UserID, "the binder must carry the owner agentRunner was handed")
		got, err := binder.ChooseFor("c_resumed")
		ck.Require().NoError(err, "an owned executor is live and admits the child")
		ck.Eq("exec-owned", got, "the resumed child must bind its OWN executor, got %q", got)
	})

	t.Run("only a foreign-owned executor live is refused", func(t *testing.T) {
		ck := assert.NewAborting(t)
		ctrl, snap := newResumed(
			exOwned("exec-other", map[string]string{"env": "home"}, "", "u-alice"),
		)
		be := drive(ctrl, snap)
		binder, ok := be.binder.(*controllerBinder)
		ck.Require().True(ok, "the fundi binder is the controller's own")
		ck.Eq("u-bob", binder.owner.UserID, "the binder must carry the owner agentRunner was handed")
		_, err := binder.ChooseFor("c_resumed")
		ck.Require().Error(err, "another user's executor must never serve the resumed child")
		ck.StrContains(err.Error(), "exec-other", "the refusal should explain itself: %v", err)
	})
}
