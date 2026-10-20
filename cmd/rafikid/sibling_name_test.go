// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// A whitespace-only name is INDEXABLE: the index's predicate is
// coalesce(name,”) <> ”, so "   " is a name like any other. The admission
// pre-check must use that same emptiness test; with a TrimSpace check it lets
// the second one through, launches a process, and is refused at the initial
// write with the misleading "taken concurrently" instead of the holder's name.
//
// A whitespace-only name cannot carry a per-run unique token, so the test owns
// its fixture outright: every live row actually named "   " is removed before
// and after, which is what a crashed earlier run would otherwise leave behind.
func TestSiblingNameWhitespaceOnlyRefusedBeforeLaunch(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	const name = "   "
	deleteWhitespaceNamed := func() {
		_, err := ctrl.pool.Exec(context.Background(), `DELETE FROM conversations.child WHERE name = $1`, name)
		ck.Require().NoError(err, "clear rows named %q", name)
	}
	deleteWhitespaceNamed()
	t.Cleanup(deleteWhitespaceNamed)

	first, err := spawnNamed(t, ctrl, name, false, "")
	ck.Require().NoError(err, "the first spawn of a whitespace-only name is admitted; it is indexable")
	ck.Eq(name, rowNameFor(t, ctrl.pool, first.ChildID), "the row keeps the name as given")

	before := len(ctrl.st.List())
	_, err = spawnNamed(t, ctrl, name, false, "")
	ck.Require().Error(err, "a second live sibling with the same whitespace-only name must be refused")
	ce := controllerError(t, err)
	ck.Eq(protocol.ErrAlreadyExists, ce.Code, "the refusal's reason")
	ck.StrContains(ce.Message, "is held by", "the refusal names the holder, not a race: %s", ce.Message)
	ck.StrContains(ce.Message, first.ChildID, "the refusal names the holder's child id: %s", ce.Message)
	ck.False(strings.Contains(ce.Message, "taken concurrently"),
		"the pre-check must catch this before a process exists: %s", ce.Message)
	ck.Eq(before, len(ctrl.st.List()), "nothing was registered for the refused spawn")

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

// seedSiblingChild writes one live child row AND the matching in-memory
// session: the row is what NameHolder queries, the session is the snapshot the
// rename path reads owner and parent from.
func seedSiblingChild(t *testing.T, ctrl *Controller, childID, owner, parent, name string) {
	t.Helper()
	ck := assert.NewAborting(t)
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	now := time.Now()
	ck.Require().NoError(childstoredb.New(ctrl.pool).Upsert(context.Background(), childstore.ChildRecord{
		ChildID:      childID,
		OwnerUserID:  owner,
		Kind:         protocol.KindFundi,
		Name:         name,
		Status:       string(protocol.StatusIdle),
		SpawnedAt:    now,
		LastActivity: now,
		Labels:       labels,
	}), "upsert child %s", childID)
	ctrl.st.Insert(&childstore.Session{
		ChildID: childID, Kind: protocol.KindFundi, Name: name, Status: protocol.StatusIdle,
		OwnerUserID: owner, StartedAt: now, LastActivity: now, Labels: labels,
	})
}

// A name the child's OWN PROCESS reports is not admitted by anything: applying
// it over a live sibling's name violates child_sibling_name_live, and every
// later writeRecord for the child then fails — it stops persisting its session
// id. The skipped rename is what keeps the row writable: the store keeps the
// old name while the process keeps displaying the name it chose.
func TestSiblingNameSniffedRenameSkipsSiblingName(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newSiblingNameController(t)
	owner := seedControllerUser(t, ctrl.pool, uniqueSiblingName(t))
	held, mine := uniqueSiblingName(t), uniqueSiblingName(t)
	const holderID, childID = "c_sib_sniff_holder", "c_sib_sniff_child"
	seedSiblingChild(t, ctrl, holderID, owner, "", held)
	seedSiblingChild(t, ctrl, childID, owner, "", mine)

	// The sniffer saw the child rename itself to the name a live sibling holds.
	ctrl.handleChildRenamed(childID, held, mine)

	ck.Eq(mine, mustSnapshot(t, ctrl, childID).Name,
		"the child keeps its own name when the detected one is a live sibling's")
	ck.Eq(held, rowNameFor(t, ctrl.pool, holderID), "the holder keeps its name")

	// The point of the skip: the row still writes. Applied, this upsert would
	// fail against child_sibling_name_live and take the child's session id with
	// it.
	ck.Require().NoError(ctrl.writeRecord(childID), "writeRecord after a skipped rename")
	ck.Eq(mine, rowNameFor(t, ctrl.pool, childID), "the row keeps the old name")
	ck.Eq(held, rowNameFor(t, ctrl.pool, holderID), "the holder's row is untouched")

	// A FREE name is still applied: the check is a sibling collision test, not
	// a licence to ignore the process.
	free := uniqueSiblingName(t)
	ctrl.handleChildRenamed(childID, free, mine)
	ck.Eq(free, mustSnapshot(t, ctrl, childID).Name, "a free name must still be applied")
	ck.Require().NoError(ctrl.writeRecord(childID), "writeRecord after a free rename")
	ck.Eq(free, rowNameFor(t, ctrl.pool, childID), "the row carries the free name")
}

// A name that cannot be CHECKED is not applied either: the sniffer's rename is
// the one name path with no operator behind it, so an unknown answer leaves the
// store's own name alone.
func TestSiblingNameSniffedRenameSkipsOnStoreError(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	const childID = "c_sib_sniff_err"
	name := uniqueSiblingName(t)
	insertDisplayChild(ctrl.st, childID, "", "", protocol.StatusIdle)
	ck.Require().NoError(ctrl.st.Rename(childID, name), "seed the child's name")
	ctrl.children = errNameStore{}

	ctrl.handleChildRenamed(childID, uniqueSiblingName(t), name)

	ck.Eq(name, mustSnapshot(t, ctrl, childID).Name,
		"a rename whose name cannot be checked must leave the stored name alone")
}
