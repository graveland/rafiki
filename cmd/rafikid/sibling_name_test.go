// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// newSiblingNameController wires a controller to a REAL Postgres child store in
// its own scratch database. The name rule is enforced by the
// child_sibling_name_live index, so a test of it has to run the index: the
// in-memory store is not the authority and a stub would prove only that the
// stub was called.
func newSiblingNameController(t *testing.T) *Controller {
	t.Helper()
	pool := scratchPool(t)
	ctrl := newTestController(t)
	ctrl.pool = pool
	ctrl.children = childstoredb.New(pool)
	return ctrl
}

// uniqueSiblingName keeps a run's names clear of the scratch database's own
// history. For the suffix tests it must carry no LIKE wildcard and no '-' of
// its own, so "<name>-2" stays unambiguous.
func uniqueSiblingName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("sib%d", time.Now().UnixNano())
}

// spawnNamed runs one claude child off fake-pi through Controller.Spawn. suffix
// is what the daemon's tool spawners set; every operator surface leaves it
// false.
func spawnNamed(t *testing.T, ctrl *Controller, name string, suffix bool, parentID string) (protocol.SpawnResponseData, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:                 protocol.KindClaude,
		Name:                 name,
		Cwd:                  t.TempDir(),
		ParentChildID:        parentID,
		PiBinary:             fakePiBin(t),
		NoSession:            true,
		SuffixNameOnConflict: suffix,
	}, users.Identity{})
}

// childRowsNamed returns every conversations.child row whose name is name or
// one of its -N suffixes, sorted by name.
func childNamesFor(t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT name FROM conversations.child WHERE name = $1 OR left(name, length($1) + 1) = $1 || '-' ORDER BY name`,
		name)
	assert.NewAborting(t).Require().NoError(err, "query rows named %q", name)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		assert.NewAborting(t).Require().NoError(rows.Scan(&n), "scan name")
		out = append(out, n)
	}
	assert.NewAborting(t).Require().NoError(rows.Err(), "rows")
	return out
}

// rowNameFor reads the name column of one child row.
func rowNameFor(t *testing.T, pool *pgxpool.Pool, childID string) string {
	t.Helper()
	var n string
	assert.NewAborting(t).Require().NoError(pool.QueryRow(context.Background(),
		`SELECT coalesce(name, '') FROM conversations.child WHERE child_id = $1`, childID).Scan(&n),
		"read the row name for %s", childID)
	return n
}

// controllerError unwraps a refusal as the ControllerError the RPC layer maps,
// failing the test when err is not one.
func controllerError(t *testing.T, err error) *connectapi.ControllerError {
	t.Helper()
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) is not a *connectapi.ControllerError", err, err)
	}
	return ce
}

// An OPERATOR spawn (CLI, cockpit, Connect) must be refused when a live sibling
// already holds the name, and the refusal must say who holds it so the caller
// can close that one or pick another.
func TestSiblingNameOperatorSpawnRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	name := uniqueSiblingName(t)

	first, err := spawnNamed(t, ctrl, name, false, "")
	ck.Require().NoError(err, "the first spawn of a free name")

	_, err = spawnNamed(t, ctrl, name, false, "")
	ck.Require().Error(err, "a second top-level spawn with the same name must be refused")
	ce := controllerError(t, err)
	ck.Eq(protocol.ErrAlreadyExists, ce.Code, "the refusal's reason")
	ck.StrContains(ce.Message, first.ChildID, "the refusal must name the holder: %s", ce.Message)
	ck.StrContains(ce.Message, name, "the refusal must name the name: %s", ce.Message)

	ck.EqDeep([]string{name}, childNamesFor(t, ctrl.pool, name),
		"a refused spawn must leave no second row")
}

// A TOOL spawn (agent_spawn) suffixes instead of refusing: the model asked for
// a name, so give it the next free one rather than an error it tends to retry
// unchanged.
func TestSiblingNameToolSpawnSuffixed(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	name := uniqueSiblingName(t)

	first, err := spawnNamed(t, ctrl, name, true, "")
	ck.Require().NoError(err, "the first tool spawn keeps the requested name")
	ck.Eq(name, mustSnapshot(t, ctrl, first.ChildID).Name, "the first spawn's name")

	second, err := spawnNamed(t, ctrl, name, true, "")
	ck.Require().NoError(err, "the second tool spawn must be suffixed, not refused")
	ck.Eq(name+"-2", mustSnapshot(t, ctrl, second.ChildID).Name, "the second spawn's name")

	third, err := spawnNamed(t, ctrl, name, true, "")
	ck.Require().NoError(err, "the third tool spawn must be suffixed, not refused")
	ck.Eq(name+"-3", mustSnapshot(t, ctrl, third.ChildID).Name, "the third spawn's name")

	ck.EqDeep([]string{name, name + "-2", name + "-3"}, childNamesFor(t, ctrl.pool, name),
		"one row per suffixed spawn")
}

// The sibling set is the CHILDREN OF ONE PARENT (the owner, for top-level
// agents). Two different parents may each name a child the same thing.
func TestSiblingNameCousinsAllowed(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	name := uniqueSiblingName(t)

	parentA, parentB := "c_sib_parent_a", "c_sib_parent_b"
	for _, parent := range []string{parentA, parentB} {
		insertDisplayChild(ctrl.st, parent, "", "", protocol.StatusIdle)
		// A parent must be ALLOWED to spawn: depth and the live-agent cap are
		// grants, and a planted session carries none.
		ck.Require().NoError(ctrl.st.Update(parent, func(s *childstore.Session) {
			s.MaxDepth = 1
			s.MaxChildren = 10
		}), "grant spawning to %s", parent)
	}

	_, err := spawnNamed(t, ctrl, name, false, parentA)
	ck.Require().NoError(err, "parent A's child named %q", name)
	_, err = spawnNamed(t, ctrl, name, false, parentB)
	ck.Require().NoError(err, "parent B's child may carry the same name — cousins are not siblings")

	ck.EqDeep([]string{name, name}, childNamesFor(t, ctrl.pool, name),
		"both cousins must have their own row")
}

// A KILLED child keeps its name — it can be resumed, so its name is still
// taken. CLOSING it is the tombstone, and frees the name.
func TestSiblingNameKilledHoldsClosedFrees(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	name := uniqueSiblingName(t)

	first, err := spawnNamed(t, ctrl, name, false, "")
	ck.Require().NoError(err, "the first spawn")

	killCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, first.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, first.ChildID, 5*time.Second)

	_, err = spawnNamed(t, ctrl, name, false, "")
	ck.Require().Error(err, "a killed child's name is still held — it is resumable, not closed")
	ck.Eq(protocol.ErrAlreadyExists, controllerError(t, err).Code, "the refusal's reason")

	ck.Require().NoError(ctrl.Close(first.ChildID), "close the holder")
	_, err = spawnNamed(t, ctrl, name, false, "")
	ck.Require().NoError(err, "closing the holder frees its name")
	ck.EqDeep([]string{name, name}, childNamesFor(t, ctrl.pool, name),
		"the freed name is re-used by a new row, alongside the closed one")
}

// racyNameStore is the state a spawn reaches when a sibling claims the name
// between admission and the initial write: every name reads FREE, while the
// writes still go to the real store and hit the real index.
type racyNameStore struct {
	childstore.ChildStore

	mu       sync.Mutex
	rejected []string
}

func (r *racyNameStore) NameHolder(context.Context, string, string, string) (string, string, bool, error) {
	return "", "", false, nil
}

func (r *racyNameStore) Upsert(ctx context.Context, rec childstore.ChildRecord) error {
	err := r.ChildStore.Upsert(ctx, rec)
	r.mu.Lock()
	defer r.mu.Unlock()
	if errors.Is(err, childstore.ErrNameTaken) {
		r.rejected = append(r.rejected, rec.ChildID)
	}
	return err
}

func (r *racyNameStore) rejectedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.rejected...)
}

// The pre-check is admission, never the authority: if the name is taken after
// it, the index must still refuse the spawn — and that refusal must abort the
// already-launched child, not leave a process running under a name it does not
// own. Fails if the ErrNameTaken backstop in Spawn is removed, reporting a
// generic spawn failure.
func TestSiblingNameRaceBackstop(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	racer := &racyNameStore{ChildStore: ctrl.children}
	ctrl.children = racer
	name := uniqueSiblingName(t)

	_, err := spawnNamed(t, ctrl, name, false, "")
	ck.Require().NoError(err, "the first spawn writes the name")

	_, err = spawnNamed(t, ctrl, name, false, "")
	ck.Require().Error(err, "a name taken between admission and the initial write must still refuse the spawn")
	ce := controllerError(t, err)
	ck.Eq(protocol.ErrAlreadyExists, ce.Code, "the refusal's reason")
	ck.StrContains(ce.Message, "taken concurrently", "the refusal must tell the caller to retry: %s", ce.Message)

	rejected := racer.rejectedIDs()
	ck.Require().Eq(1, len(rejected), "exactly one write must be rejected by the index")
	refused := rejected[0]
	if _, ok := ctrl.st.Get(refused); ok {
		t.Fatalf("the refused spawn left %s in the in-memory store", refused)
	}
	if _, ok := ctrl.cm.Get(refused); ok {
		t.Fatalf("the refused spawn left %s running, so a process was launched and not aborted", refused)
	}
	ck.EqDeep([]string{name}, childNamesFor(t, ctrl.pool, name), "the refused spawn must leave no row")
}

// errNameStore reports an error for every name: an UNCHECKED name must not run,
// so the spawn is refused rather than allowed through to the index.
type errNameStore struct{ stubChildStore }

func (errNameStore) NameHolder(context.Context, string, string, string) (string, string, bool, error) {
	return "", "", false, errors.New("name holder query failed")
}

func TestSiblingNameStoreErrorRefuses(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.children = errNameStore{}
	name := uniqueSiblingName(t)

	_, err := spawnNamed(t, ctrl, name, false, "")
	ck.Require().Error(err, "a spawn whose name cannot be checked must be refused")
	ck.StrContains(err.Error(), "check name", "the refusal must name the failed check: %v", err)
	var ce *connectapi.ControllerError
	ck.False(errors.As(err, &ce), "a store failure is not a name conflict; got %T", err)
	ck.Eq(0, len(ctrl.st.List()), "the refusal must come before anything is registered")
}
