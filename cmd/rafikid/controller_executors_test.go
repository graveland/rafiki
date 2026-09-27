package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// fakeExecStore is an in-memory executors.Store for testing the control-plane
// verbs without a database. It enforces nothing beyond recording calls — the
// real enforcement lives in the postgres store and is covered by its own
// conformance suite.
type fakeExecStore struct {
	minted      []executors.NewToken
	execs       map[string]executors.Executor
	seq         int
	createCalls int
	lastCreate  executors.NewToken
	disabled    []string
	deleted     []string

	// setLabelsErr, when set, is what SetLabels answers -- so a store
	// rejection can be exercised without a database.
	setLabelsErr error
}

func newFakeExecStore() *fakeExecStore {
	return &fakeExecStore{execs: map[string]executors.Executor{}}
}

func (f *fakeExecStore) Create(_ context.Context, t executors.NewToken) (executors.Executor, string, error) {
	f.createCalls++
	f.lastCreate = t
	e := executors.Executor{ID: "exec-created", Labels: t.Labels, Enabled: true}
	f.execs[e.ID] = e
	return e, "credential", nil
}

func (f *fakeExecStore) MintToken(_ context.Context, t executors.NewToken) (string, error) {
	f.minted = append(f.minted, t)
	return "tok-1", nil
}
func (f *fakeExecStore) Enroll(_ context.Context, _ string, _ map[string]string) (executors.Executor, string, error) {
	f.seq++
	e := executors.Executor{ID: "exec-" + string(rune('0'+f.seq)), Labels: map[string]string{}}
	f.execs[e.ID] = e
	return e, "cred", nil
}
func (f *fakeExecStore) Authenticate(_ context.Context, _ string) (executors.Executor, error) {
	return executors.Executor{}, nil
}
func (f *fakeExecStore) Get(_ context.Context, id string) (executors.Executor, error) {
	e, ok := f.execs[id]
	if !ok {
		// The postgres store answers a missing row with ErrNotFound, and
		// resolveExecutorRef branches on exactly that sentinel.
		return executors.Executor{}, executors.ErrNotFound
	}
	return e, nil
}
func (f *fakeExecStore) List(_ context.Context) ([]executors.Executor, error) {
	var out []executors.Executor
	for _, e := range f.execs {
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeExecStore) SetLabels(_ context.Context, id string, set map[string]string, remove []string) (executors.Executor, error) {
	if f.setLabelsErr != nil {
		return executors.Executor{}, f.setLabelsErr
	}
	e := f.execs[id]
	if e.Labels == nil {
		e.Labels = map[string]string{}
	}
	for k, v := range set {
		e.Labels[k] = v
	}
	for _, k := range remove {
		delete(e.Labels, k)
	}
	f.execs[id] = e
	return e, nil
}
func (f *fakeExecStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	if !enabled {
		f.disabled = append(f.disabled, id)
	}
	return nil
}
func (f *fakeExecStore) Delete(_ context.Context, id string) error {
	if _, ok := f.execs[id]; !ok {
		return executors.ErrNotFound
	}
	delete(f.execs, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeExecStore) Annotate(_ context.Context, id string, set map[string]string, remove []string) error {
	return nil
}
func (f *fakeExecStore) TouchSeen(_ context.Context, id string) error { return nil }

func TestExecutorEnrollMintsATokenWithTheGivenLabels(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}

	resp, err := c.ExecutorEnroll(users.Identity{Username: "brent"}, protocol.ExecutorEnrollRequest{
		Labels:     map[string]string{"env": "work"},
		TTLSeconds: 3600,
	})
	ck.NoError(err, "enroll")
	ck.NotEq("", resp.Token, "enroll returned an empty token")
	if len(s.minted) != 1 || s.minted[0].Labels["env"] != "work" {
		t.Fatalf("minted token did not carry the labels: %+v", s.minted)
	}
}

// owner gates which children an executor admits and which durable executor an
// interactive client binds to. It must come from the connection, never from the
// request -- a client that could name its own owner would be granting itself
// access. Nothing stamped it before, which is why ExecutorSession's durable
// branch was unreachable and every client got a transient executor.
func TestExecutorEnrollStampsOwnerFromTheConnection(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}

	_, err := c.ExecutorEnroll(users.Identity{Username: "brent"}, protocol.ExecutorEnrollRequest{
		Name:       "laptop",
		Labels:     map[string]string{"env": "dev"},
		TTLSeconds: 3600,
	})
	ck.NoError(err)
	ck.Len(s.minted, 1, "want one minted token, got %d", len(s.minted))
	got := s.minted[0].Labels
	ck.Eq("brent", got["owner"], `labels["owner"] = %q, want "brent" from the connection`, got["owner"])
	ck.Eq("laptop", got["machine"], `labels["machine"] = %q, want "laptop" from --name`, got["machine"])
	ck.Eq("dev", got["env"], "operator labels must survive alongside the stamped ones")
}

// The same rule on the stateless path: create writes a row immediately, so an
// unstamped owner there is a row no interactive client can ever match.
func TestExecutorCreateStampsOwnerFromTheConnection(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}

	_, err := c.ExecutorCreate(users.Identity{Username: "brent"}, protocol.ExecutorCreateRequest{
		Name:   "laptop",
		Labels: map[string]string{"env": "dev"},
	})
	ck.NoError(err)
	got := s.lastCreate.Labels
	ck.False(got["owner"] != "brent" || got["machine"] != "laptop" || got["env"] != "dev", "create labels = %+v, want owner=brent machine=laptop env=dev", got)
}

// The request must not carry the labels the daemon owns. Refusing rather than
// silently overwriting is the point: a caller learns their selector will not
// mean what they wrote.
func TestExecutorEnrollRefusesAClientSuppliedOwner(t *testing.T) {
	c := &Controller{execStore: newFakeExecStore()}
	for _, key := range []string{"owner", "machine"} {
		_, err := c.ExecutorEnroll(users.Identity{Username: "brent"}, protocol.ExecutorEnrollRequest{
			Labels:     map[string]string{key: "someone-else"},
			TTLSeconds: 3600,
		})
		var ce *connectapi.ControllerError
		if !errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs {
			t.Fatalf("--label %s=: got %v, want ERR_INVALID_ARGS -- %s is derived "+
				"by the daemon and a request naming it must be refused rather "+
				"than silently overwritten", key, err, key)
		}
	}
}

func TestExecutorCreateRefusesAClientSuppliedOwner(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}
	_, err := c.ExecutorCreate(users.Identity{Username: "brent"}, protocol.ExecutorCreateRequest{
		Labels: map[string]string{"owner": "someone-else"},
	})
	ck.Error(err, "create must refuse a client-supplied owner")
	ck.Eq(0, s.createCalls, "the row must not be written before the label check")
}

func TestExecutorEnrollRefusesANameASelectorCannotCarry(t *testing.T) {
	c := &Controller{execStore: newFakeExecStore()}
	_, err := c.ExecutorEnroll(users.Identity{Username: "brent"},
		protocol.ExecutorEnrollRequest{Name: "my,laptop", TTLSeconds: 3600})
	assert.NewAborting(t).Error(err, "a comma splits a selector; --name must be validated daemon-side too")
}

// A collision on (owner, machine) is an operator naming two machines the same,
// which the store reports as executors.ErrMachineNameTaken. Left untranslated
// it reaches the client as ERR_INTERNAL / 503 -- "the daemon is broken" for a
// mistake only the operator can fix.
func TestDuplicateMachineNameIsAClientError(t *testing.T) {
	c := assert.NewAborting(t)
	err := translateExecutorErr(fmt.Errorf("create executor: %w", executors.ErrMachineNameTaken))
	var ce *connectapi.ControllerError
	c.False(!errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs, "got %v, want ERR_INVALID_ARGS", err)
	c.StrContains(ce.Message, "name", "the message must tell the operator the NAME is taken")
}

// The daemon half of the same rule. LabelExecutor is the RPC the
// collision message RECOMMENDS, and it lost this mapping once already -- the
// detection moved into the store, SetLabels was not wired to it, and an
// operator following the daemon's own advice onto a taken name was told the
// daemon was broken.
func TestExecutorLabelMapsATakenMachineName(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	s.setLabelsErr = executors.ErrMachineNameTaken
	e0, _, _ := s.Enroll(context.Background(), "tok", nil)
	c := &Controller{execStore: s}

	_, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{
		ExecutorID: e0.ID, Set: map[string]string{"machine": "taken"},
	})
	var ce *connectapi.ControllerError
	ck.False(!errors.As(err, &ce) || ce.Code != protocol.ErrInvalidArgs, "got %v, want ERR_INVALID_ARGS", err)
	// The one message serves both control paths, so it must not name a flag
	// only one of them has: `executor label` has no --name.
	ck.NotStrContains(ce.Message, "--name", "this message also reaches `executor label`, which has no "+
		"--name flag: %q", ce.Message)
}

// An unclassified store error keeps the old behaviour -- returned unchanged, so
// mapErr treats it as internal and the store's text (which carries the DSN)
// stays in the daemon log.
func TestUnclassifiedStoreErrorsAreStillInternal(t *testing.T) {
	err := translateExecutorErr(fmt.Errorf("insert executor: %w",
		errors.New(`duplicate key value violates unique constraint "executors_credential_hash_key"`)))
	var ce *connectapi.ControllerError
	assert.NewAborting(t).False(errors.As(err, &ce), "an unrelated failure must not inherit the rename advice: %+v", ce)
}

func TestExecutorVerbsRefuseWithoutAStore(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{} // execStore nil
	if _, err := c.ExecutorEnroll(users.Identity{Username: "brent"}, protocol.ExecutorEnrollRequest{TTLSeconds: 1}); err == nil {
		t.Fatal("enroll must refuse without a store")
	}
	if _, err := c.ExecutorList(protocol.ExecutorListRequest{}); err == nil {
		t.Fatal("list must refuse without a store")
	}
	if _, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{}); err == nil {
		t.Fatal("label must refuse without a store")
	}
	ck.Error(c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "x"}), "disable must refuse without a store")
	ck.Error(c.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "x"}), "enable must refuse without a store")
	ck.Error(c.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: "x"}), "delete must refuse without a store")
}

func TestExecutorDeleteRemovesTheRow(t *testing.T) {
	ck := assert.NewAborting(t)
	s, c := seedSuffixFixture(t)
	idB := uuidPrefix + uuidTailB

	ck.NoError(c.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: idB}), "delete")
	if len(s.deleted) != 1 || s.deleted[0] != idB {
		t.Fatalf("delete resolved to %v, want %s", s.deleted, idB)
	}
	_, ok := s.execs[idB]
	ck.False(ok, "row %s still present after delete", idB)
}

func TestExecutorDeleteUnknownIsNotFound(t *testing.T) {
	_, c := seedSuffixFixture(t)
	err := c.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: "ffffffffffff"})
	var ce *connectapi.ControllerError
	assert.NewAborting(t).False(!errors.As(err, &ce) || ce.Code != protocol.ErrNotFound, "got %v, want ERR_NOT_FOUND", err)
}

func TestExecutorLabelRoundTrips(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}
	e0, _, _ := s.Enroll(context.Background(), "tok", nil)
	e, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{
		ExecutorID: e0.ID, Set: map[string]string{"os": "linux"},
	})
	ck.NoError(err, "label")
	ck.Eq("linux", e.Labels["os"], "label did not round-trip: %+v", e.Labels)
}

func TestExecutorListFiltersBySelector(t *testing.T) {
	ck := assert.NewAborting(t)
	s := newFakeExecStore()
	c := &Controller{execStore: s}
	s.execs["exec-work"] = executors.Executor{ID: "exec-work", Labels: map[string]string{"env": "work"}}
	s.execs["exec-home"] = executors.Executor{ID: "exec-home", Labels: map[string]string{"env": "home"}}

	execs, err := c.ExecutorList(protocol.ExecutorListRequest{Selector: "env=work"})
	ck.NoError(err, "list")
	ck.False(len(execs) != 1 || execs[0].ID != "exec-work", "selector filtered wrong: %+v", execs)
}

// Connected is a view over the live pool, not the store. A client waiting for
// its own session executor to connect has no other signal; the row alone cannot
// say whether it is up.
func TestExecutorListMarksConnectedFromTheLivePool(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := newFakeExecStore()
	s.execs["exec-live"] = executors.Executor{ID: "exec-live", Enabled: true}
	s.execs["exec-off"] = executors.Executor{ID: "exec-off", Enabled: true}
	connectedAt := time.Now().Add(-90 * time.Second)
	live := ex("exec-live", nil, "")
	live.ConnectedAt = connectedAt
	c := &Controller{execStore: s, execPool: &fakePool{live: []execpool.LiveExecutor{live}}}

	execs, err := c.ExecutorList(protocol.ExecutorListRequest{})
	ck.Require().NoError(err, "list")
	byID := map[string]executors.Executor{}
	for _, e := range execs {
		byID[e.ID] = e
	}
	ck.True(byID["exec-live"].Connected, "exec-live not marked connected though the pool has it live")
	if got := byID["exec-live"].ConnectedAt; got == nil || !got.Equal(connectedAt) {
		t.Errorf("exec-live.ConnectedAt = %v, want %v", got, connectedAt)
	}
	ck.False(byID["exec-off"].Connected, "exec-off marked connected though the pool does not have it")
	ck.Nil(byID["exec-off"].ConnectedAt, "exec-off.ConnectedAt")
}

// ─── partial-id resolution ──────────────────────────────────────────────────

// Three rows: A and B minted in the same window (UUIDv7s share their leading
// timestamp bits, so everything before the final group is identical), plus C —
// minted long before A but sharing A's tail, which is what makes a fragment
// ambiguous across time.
const (
	uuidPrefix = "0198e5f2-9c3a-7def-8a1b-"
	uuidTailA  = "4c2d9e0f1a2b"
	uuidTailB  = "998877665544"
	uuidOlderC = "77f0aabb-1122-7333-9444-" // different front, A's tail
)

func seedSuffixFixture(t *testing.T) (*fakeExecStore, *Controller) {
	t.Helper()
	s := newFakeExecStore()
	s.execs[uuidPrefix+uuidTailA] = executors.Executor{ID: uuidPrefix + uuidTailA}
	s.execs[uuidPrefix+uuidTailB] = executors.Executor{ID: uuidPrefix + uuidTailB}
	s.execs[uuidOlderC+uuidTailA] = executors.Executor{ID: uuidOlderC + uuidTailA}
	return s, &Controller{execStore: s}
}

// A unique trailing fragment names the row. This is the contract that makes
// the fragment `executor list` displays usable as an argument.
func TestExecutorVerbsAcceptUniqueTrailingFragment(t *testing.T) {
	ck := assert.NewAborting(t)
	s, c := seedSuffixFixture(t)

	ck.NoError(c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: uuidTailB}), "disable by tail fragment")
	if len(s.disabled) != 1 || s.disabled[0] != uuidPrefix+uuidTailB {
		t.Fatalf("disable resolved to %v, want %s", s.disabled, uuidPrefix+uuidTailB)
	}

	e, err := c.ExecutorLabel(protocol.ExecutorLabelRequest{
		ExecutorID: "77665544", // a shorter fragment, still unique (tail of B)
		Set:        map[string]string{"env": "work"},
	})
	ck.NoError(err, "label by short fragment")
	ck.False(e.ID != uuidPrefix+uuidTailB || e.Labels["env"] != "work", "label resolved to %+v", e)
}

// The full id keeps working.
func TestExecutorDisableAcceptsTheFullID(t *testing.T) {
	s, c := seedSuffixFixture(t)
	idA := uuidPrefix + uuidTailA
	assert.NewAborting(t).NoError(c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: idA}), "disable by full id")
	if len(s.disabled) != 1 || s.disabled[0] != idA {
		t.Fatalf("disable resolved to %v, want %s", s.disabled, idA)
	}
}

// A fragment matching several rows is an invalid-args error naming them —
// never a silent pick of one.
func TestAmbiguousFragmentFailsWithoutGuessing(t *testing.T) {
	ck := assert.NewCollecting(t)
	_, c := seedSuffixFixture(t)

	err := c.ExecutorEnable(protocol.ExecutorEnableRequest{
		ExecutorID: uuidTailA, // A and C both end in it
	})
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "expected a ControllerError, got %T: %v", err, err)
	ck.Require().Eq(protocol.ErrInvalidArgs, ce.Code, "ambiguous fragment: got code")
	for _, want := range []string{uuidPrefix + uuidTailA, uuidOlderC} {
		ck.StrContains(ce.Message, want, "ambiguity message must name")
	}
}

// No match and too-short fragments both answer not-found rather than guessing.
func TestUnknownAndShortFragmentsAreNotFound(t *testing.T) {
	_, c := seedSuffixFixture(t)

	for _, ref := range []string{"ffffffffffff", "a1b" /* shorter than executorRefMinLen */} {
		err := c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: ref})
		var ce *connectapi.ControllerError
		assert.NewCollecting(t).False(!errors.As(err, &ce) || ce.Code != protocol.ErrNotFound, "fragment %q: got %v, want ERR_NOT_FOUND", ref, err)
	}
}

// A transient executor has no row by design. waitExecutorLive polls this verb
// to learn its own session executor has connected, so a list built only from
// the store can never answer — the client times out after 20s and tears down a
// perfectly healthy executor.
func TestExecutorListIncludesRowlessTransientExecutors(t *testing.T) {
	ck := assert.NewAborting(t)
	live := ex("sess-01J0", map[string]string{"owner": "brent", "machine": "laptop", "kind": "session"}, "")
	live.ConnectedAt = time.Now()
	c := &Controller{execStore: newFakeExecStore(), execPool: &fakePool{live: []execpool.LiveExecutor{live}}}

	got, err := c.ExecutorList(protocol.ExecutorListRequest{})
	ck.NoError(err)
	ck.Len(got, 1, "want the transient executor listed, got %d rows", len(got))
	if !got[0].Connected {
		t.Fatal("a live transient executor must report Connected=true; that flag " +
			"is the only signal waitExecutorLive has")
	}
}

func TestExecutorListDoesNotDuplicateADurableExecutorThatIsAlsoLive(t *testing.T) {
	ck := assert.NewAborting(t)
	row := executors.Executor{ID: "11111111-1111-1111-1111-111111111111", Enabled: true}
	live := execpool.LiveExecutor{Executor: row, ConnectedAt: time.Now()}
	s := newFakeExecStore()
	s.execs[row.ID] = row
	c := &Controller{execStore: s, execPool: &fakePool{live: []execpool.LiveExecutor{live}}}

	got, err := c.ExecutorList(protocol.ExecutorListRequest{})
	ck.NoError(err)
	ck.Len(got, 1, "a durable executor that is also live must appear once, got %d", len(got))
}
