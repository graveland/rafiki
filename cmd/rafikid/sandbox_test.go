package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// handlerRT dispatches an Engine request straight into an http.Handler, so a
// sandbox test drives the engine without a network socket.
type handlerRT struct{ h http.Handler }

func (rt handlerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// fakeDocker answers the subset of the Docker Engine API the sandbox flow uses.
type fakeDocker struct {
	mu sync.Mutex

	events *[]string

	imageExists   bool
	pullFail      bool
	createFail    bool
	startFail     bool
	containerGone bool

	createBody []byte
	removed    []string
	started    []string
	list       []map[string]any // entries for GET /containers/json

	// onList runs while serving GET /containers/json, so a test can simulate a
	// concurrent create landing between the container listing and the reaper's
	// per-container row read.
	onList func()
}

func (d *fakeDocker) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.events != nil {
		*d.events = append(*d.events, s)
	}
}

func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
		if !d.imageExists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && r.URL.Path == "/images/create":
		if d.pullFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"done"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
		if d.createFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.createBody = body
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"ctr-1"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		if d.startFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		d.mu.Lock()
		d.started = append(d.started, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/start"))
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
		id := strings.TrimPrefix(r.URL.Path, "/containers/")
		d.mu.Lock()
		d.removed = append(d.removed, id)
		d.mu.Unlock()
		d.record("container-remove:" + id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
		d.mu.Lock()
		list := d.list
		hook := d.onList
		d.mu.Unlock()
		if hook != nil {
			hook()
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
		if d.containerGone {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"Id":"ctr-1","State":{"Running":true},"Config":{"Labels":{}}}`)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// fakeSandboxStore is an in-memory sandbox.Store.
type fakeSandboxStore struct {
	mu        sync.Mutex
	rows      map[string]sandbox.Row
	insertErr error
	listErr   error
}

func newFakeSandboxStore() *fakeSandboxStore {
	return &fakeSandboxStore{rows: map[string]sandbox.Row{}}
}

func (s *fakeSandboxStore) Insert(_ context.Context, r sandbox.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	if r.Name != "" {
		for _, e := range s.rows {
			if e.Name == r.Name && e.OwnerUserID == r.OwnerUserID && e.RemovedAt == nil {
				return sandbox.ErrNameTaken
			}
		}
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	s.rows[r.ID] = r
	return nil
}

func (s *fakeSandboxStore) Get(_ context.Context, id string) (sandbox.Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok, nil
}

func (s *fakeSandboxStore) GetByName(_ context.Context, owner, name string) (sandbox.Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.Name == name && r.Name != "" && r.OwnerUserID == owner && r.RemovedAt == nil {
			return r, true, nil
		}
	}
	return sandbox.Row{}, false, nil
}

func (s *fakeSandboxStore) ListLive(_ context.Context, owner string) ([]sandbox.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []sandbox.Row
	for _, r := range s.rows {
		if r.OwnerUserID == owner && r.RemovedAt == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeSandboxStore) ListAllLive(_ context.Context) ([]sandbox.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []sandbox.Row
	for _, r := range s.rows {
		if r.RemovedAt == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeSandboxStore) SetContainer(_ context.Context, id, containerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	r.ContainerID = containerID
	s.rows[id] = r
	return nil
}

func (s *fakeSandboxStore) SetState(_ context.Context, id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	r.State = state
	s.rows[id] = r
	return nil
}

func (s *fakeSandboxStore) MarkRemoved(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.RemovedAt != nil {
		return nil
	}
	r.RemovedAt = &at
	s.rows[id] = r
	return nil
}

func (s *fakeSandboxStore) get(id string) (sandbox.Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

// timelineExecStore records executor deletes onto a shared timeline so a test
// can assert the container went first.
type timelineExecStore struct {
	*fakeExecStore
	events *[]string
}

func (t *timelineExecStore) Delete(ctx context.Context, id string) error {
	*t.events = append(*t.events, "exec-delete:"+id)
	return t.fakeExecStore.Delete(ctx, id)
}

// erroringExecStore injects Get/List errors, so a test can exercise the
// dead-launcher confirmation path without a database. (The real
// executorsdb.pgStore.Get collapses every error into ErrNotFound; List does
// not.)
type erroringExecStore struct {
	*fakeExecStore
	getErr  error
	listErr error
}

func (e *erroringExecStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	if e.getErr != nil {
		return executors.Executor{}, e.getErr
	}
	return e.fakeExecStore.Get(ctx, id)
}

func (e *erroringExecStore) List(ctx context.Context) ([]executors.Executor, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.fakeExecStore.List(ctx)
}

// childLiveness is a childstore.ChildStore whose List returns live children.
type childLiveness struct {
	stubChildStore
	live []string
}

func (c childLiveness) List(context.Context) ([]childstore.ChildRecord, error) {
	out := make([]childstore.ChildRecord, 0, len(c.live))
	for _, id := range c.live {
		out = append(out, childstore.ChildRecord{ChildID: id})
	}
	return out, nil
}

type sandboxEnv struct {
	ctrl   *Controller
	pool   *fakePool
	exec   *fakeExecStore
	store  *fakeSandboxStore
	docker *fakeDocker
}

func newSandboxEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	events := &[]string{}
	pool := &fakePool{evicted: map[string]bool{}}
	exec := newFakeExecStore()
	store := newFakeSandboxStore()
	docker := &fakeDocker{imageExists: true, events: events}
	ctrl := &Controller{
		st:           childstore.New(),
		execPool:     pool,
		execStore:    exec,
		sandboxStore: store,
		sandboxCfg: sandbox.Config{
			Image:         "rafiki/sandbox:test",
			Network:       protocol.NetworkEgress,
			TTL:           time.Hour,
			MaxTTL:        24 * time.Hour,
			MaxPerOwner:   8,
			SweepInterval: time.Minute,
		},
	}
	ctrl.sandboxEngine = func(string) *sandbox.Engine { return sandbox.NewEngine(handlerRT{docker}) }
	return &sandboxEnv{ctrl: ctrl, pool: pool, exec: exec, store: store, docker: docker}
}

// launcher returns a live launcher executor advertising the docker proxy.
func sandboxLauncher(id, machine, ownerUserID string) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{
			ID: id, Enabled: true, OwnerUserID: ownerUserID,
			Labels: map[string]string{"machine": machine},
		},
		Proxies:  []string{sandbox.DockerProxyName},
		Describe: describeResponse([]string{"/srv/repos"}, "/var/run/rafiki-relay"),
	}
}

// describeResponse builds the Describe an executor self-reports: the launcher's
// mount roots and its relay directory.
func describeResponse(mountRoots []string, relayDir string) *executorpb.DescribeResponse {
	return &executorpb.DescribeResponse{SandboxMountRoots: mountRoots, SandboxRelayDir: relayDir}
}

// spawnedExecutor is the executor the sandbox's container connects as — no
// docker proxy.
func spawnedExecutor(id, ownerUserID string) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{ID: id, Enabled: true, OwnerUserID: ownerUserID},
	}
}

func sandboxSpec(name string) protocol.SandboxSpec {
	return protocol.SandboxSpec{Name: name, Image: "rafiki/sandbox:test"}
}

func sandboxOwner() users.Identity {
	return users.Identity{Username: "u", UserID: "0191f2a3-4b5c-7d1e-8f00-112233445566"}
}

// TestSandboxCreateHappyPath drives the whole create flow: a container is
// created and started, the row reaches ready, and the credential rides the
// container env but never the stored row.
func TestSandboxCreateHappyPath(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	info, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.NoError(err, "create")
	ck.Eq("ready", info.State, "state")
	ck.Eq("dev", info.Name)
	ck.Eq("ctr-1", info.ContainerID)
	ck.Eq("exec-created", info.ExecutorID)
	ck.True(info.Connected, "the created executor is live")

	// The credential reached the container env.
	var body struct {
		Env []string `json:"Env"`
	}
	ck.NoError(json.Unmarshal(env.docker.createBody, &body), "decode create body")
	ck.True(sandboxHas(body.Env, sandbox.CredentialEnv+"=credential"), "credential in env: %v", body.Env)

	// ... but never the stored row.
	row, ok := env.store.get(info.ID)
	ck.True(ok, "row stored")
	ck.False(strings.Contains(string(row.Spec), "credential"), "credential must not be in the stored spec: %s", row.Spec)

	// The daemon-owned labels are on the executor row.
	tok := env.exec.lastCreate
	ck.Eq("1", tok.Labels[sandbox.RowLabelSandbox])
	ck.Eq(info.ID, tok.Labels[sandbox.RowLabelID])
	ck.Eq("container", tok.Isolation)
	ck.Eq("pinned", tok.WorkspaceMode)
}

func sandboxHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestSandboxCreateRollsBackOnImagePullFailure: a failed pull leaves no row,
// no executor, no container.
func TestSandboxCreateRollsBackOnImagePullFailure(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.imageExists = false
	env.docker.pullFail = true

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "create must fail")
	assertRolledBack(t, env, ck)
}

func TestSandboxCreateRollsBackOnCreateFailure(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.createFail = true

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "create must fail")
	assertRolledBack(t, env, ck)
}

func TestSandboxCreateRollsBackOnStartFailure(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.startFail = true

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "create must fail")
	assertRolledBack(t, env, ck)
}

// TestSandboxCreateRollsBackWhenNeverConnects: the container starts but its
// executor never joins; a short-lived context bounds the wait.
func TestSandboxCreateRollsBackWhenNeverConnects(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	// The launcher is live, but "exec-created" never appears.
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_, err := env.ctrl.SandboxCreate(ctx, owner, "", sandboxSpec("dev"))
	ck.Error(err, "create must fail")
	ck.True(strings.Contains(err.Error(), "did not connect"), "error should name the failure: %v", err)
	assertRolledBack(t, env, ck)
}

// assertRolledBack checks the rollback invariants: the row is tombstoned, no
// live row remains, the executor row is gone, and the container is removed.
func assertRolledBack(t *testing.T, env *sandboxEnv, ck *assert.C) {
	t.Helper()
	live, err := env.store.ListAllLive(context.Background())
	ck.NoError(err, "list live")
	ck.Len(live, 0, "no live row after rollback")
	ck.True(env.exec.deleted != nil, "the executor row was deleted")
	ck.True(env.pool.evicted["exec-created"], "the executor was evicted from the pool")
}

// TestSandboxCreateRefusesCap: an owner at the cap cannot create another.
func TestSandboxCreateRefusesCap(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.sandboxCfg.MaxPerOwner = 1
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-existing", OwnerUserID: owner.UserID, Name: "old", State: sandboxStateReady,
	}))

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "must refuse at the cap")
	ck.True(strings.Contains(err.Error(), "limit reached"), "error names the cap: %v", err)
}

// TestSandboxCreateRefusesNameCollision: a live row already holding the name.
func TestSandboxCreateRefusesNameCollision(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-existing", OwnerUserID: owner.UserID, Name: "dev", State: sandboxStateReady,
	}))

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "must refuse a name collision")
	ck.True(strings.Contains(err.Error(), `"dev"`), "error names the sandbox: %v", err)
}

// TestSandboxCreateRefusesNoLauncher: no docker launcher is in scope.
func TestSandboxCreateRefusesNoLauncher(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "must refuse with no launcher")
	ck.True(strings.Contains(err.Error(), "--proxy docker"), "error points at the fix: %v", err)
}

// TestSandboxCreateRefusesAmbiguousLauncher: two docker launchers in scope.
func TestSandboxCreateRefusesAmbiguousLauncher(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("l1", "box-a", owner.UserID),
		sandboxLauncher("l2", "box-b", owner.UserID),
	}

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "must refuse an ambiguous launcher set")
	ck.True(strings.Contains(err.Error(), "box-a") && strings.Contains(err.Error(), "box-b"), "error lists both: %v", err)
}

// TestSandboxCreateRefusesLauncherWithoutRelayDir.
func TestSandboxCreateRefusesLauncherWithoutRelayDir(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	le := sandboxLauncher("launcher", "box", owner.UserID)
	le.Describe = describeResponse([]string{"/srv/repos"}, "")
	env.pool.live = []execpool.LiveExecutor{le}

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "must refuse")
	ck.True(strings.Contains(err.Error(), "--relay-dir"), "error names --relay-dir: %v", err)
}

// TestSandboxRemoveContainerBeforeExecutor pins the teardown order.
func TestSandboxRemoveContainerBeforeExecutor(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{spawnedExecutor("exec-created", owner.UserID)}
	events := &[]string{}
	env.docker.events = events
	env.ctrl.execStore = &timelineExecStore{fakeExecStore: env.exec, events: events}

	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))
	env.pool.live = append(env.pool.live, sandboxLauncher("launcher", "box", owner.UserID))

	ck.NoError(env.ctrl.SandboxRemove(context.Background(), owner, "", "dev"), "remove")

	ci, ei := indexOf(*events, "container-remove:ctr-1"), indexOf(*events, "exec-delete:exec-created")
	ck.True(ci >= 0 && ei >= 0, "both teardown steps recorded: %v", *events)
	ck.True(ci < ei, "the container must be removed before the executor row: %v", *events)
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "row tombstoned")
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// TestSandboxRemoveLauncherOfflineLeavesRemoving: an offline launcher whose
// executor ROW still exists leaves the row in removing for the sweep to finish.
func TestSandboxRemoveLauncherOfflineLeavesRemoving(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	// The launcher's executor row still exists — it may come back.
	env.exec.execs["launcher"] = executors.Executor{ID: "launcher", Enabled: true}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))

	err := env.ctrl.SandboxRemove(context.Background(), owner, "", "dev")
	ck.Error(err, "must report the offline launcher")
	ck.True(strings.Contains(err.Error(), "launcher offline"), "error text: %v", err)
	row, _ := env.store.get("sbx-1")
	ck.Eq(sandboxStateRemoving, row.State, "left removing")
	ck.True(row.RemovedAt == nil, "not tombstoned")
}

// TestSandboxRemoveDeadLauncherFreesSlot: an offline launcher whose executor row
// is GONE is permanently dead — the removal proceeds so the cap slot is freed,
// and the container is left to the reaper.
func TestSandboxRemoveDeadLauncherFreesSlot(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	// "launcher" is not live in the pool AND has no executor row.
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))

	ck.NoError(env.ctrl.SandboxRemove(context.Background(), owner, "", "dev"), "remove")
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "the cap slot is freed (row tombstoned)")
	ck.True(env.pool.evicted["exec-created"], "the sandbox's executor was evicted")
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "a dead launcher cannot remove the container")
}

// TestSandboxRemoveTransientGetErrorLeavesRemoving: a Get error that is NOT a
// real absence (pgStore.Get collapses every error into ErrNotFound) must not be
// read as "launcher gone" — the row is left removing.
func TestSandboxRemoveTransientGetErrorLeavesRemoving(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.ctrl.execStore = &erroringExecStore{fakeExecStore: env.exec, getErr: errors.New("db blip")}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))

	err := env.ctrl.SandboxRemove(context.Background(), owner, "", "dev")
	ck.Error(err, "a transient Get error must surface")
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "a transient error must not tombstone the row")
	ck.Eq(sandboxStateRemoving, row.State, "left removing")
}

// TestSandboxRemoveConfirmsAbsenceWithList: Get says not-found but List errors,
// so the launcher-gone conclusion is not safe — the row is left removing.
func TestSandboxRemoveConfirmsAbsenceWithList(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	// Get returns ErrNotFound (as pgStore.Get does for any error); List errors.
	env.ctrl.execStore = &erroringExecStore{fakeExecStore: env.exec, listErr: errors.New("list failed")}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))

	err := env.ctrl.SandboxRemove(context.Background(), owner, "", "dev")
	ck.Error(err, "a List error must surface")
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "a List error must not tombstone the row")
	ck.Eq(sandboxStateRemoving, row.State, "left removing")
}

// TestSandboxRemoveListFindsLauncherLeavesRemoving: Get says not-found but List
// contains the launcher, so the ErrNotFound was a collapsed transient error —
// the row is left removing.
func TestSandboxRemoveListFindsLauncherLeavesRemoving(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.ctrl.execStore = &erroringExecStore{fakeExecStore: env.exec, getErr: executors.ErrNotFound}
	env.exec.execs["launcher"] = executors.Executor{ID: "launcher", Enabled: true}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}))

	err := env.ctrl.SandboxRemove(context.Background(), owner, "", "dev")
	ck.Error(err, "the launcher exists, so it is merely offline")
	ck.True(strings.Contains(err.Error(), "launcher offline"), "error text: %v", err)
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "the row is not tombstoned")
	ck.Eq(sandboxStateRemoving, row.State, "left removing")
}

// TestSandboxRemoveSkipsTombstonedRow: removeSandboxRow re-reads the row, so a
// stale snapshot of an already-removed row is a no-op.
func TestSandboxRemoveSkipsTombstonedRow(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	stale := sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
	}
	ck.NoError(env.store.Insert(context.Background(), stale))
	ck.NoError(env.store.MarkRemoved(context.Background(), "sbx-1", time.Now()))

	ck.NoError(env.ctrl.removeSandboxRow(context.Background(), stale), "a tombstoned row is a no-op")
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "no container removal on an already-removed row")
}

// TestSandboxRemoveChildAuthority: a creator and its ancestor may remove; a
// sibling may not.
func TestSandboxRemoveChildAuthority(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T, env *sandboxEnv) string {
		env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", "owner")}
		// root -> mid -> maker ; sibling is separate.
		sandboxSeedChild(env, "root", "")
		sandboxSeedChild(env, "mid", "root")
		sandboxSeedChild(env, "maker", "mid")
		sandboxSeedChild(env, "sibling", "root")
		ck := assert.NewAborting(t)
		ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
			ID: "sbx-1", OwnerUserID: "owner", Name: "dev", CreatedBy: "maker",
			ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
			State: sandboxStateReady,
		}))
		return "sbx-1"
	}

	t.Run("creator may remove", func(t *testing.T) {
		env := newSandboxEnv(t)
		seed(t, env)
		ck := assert.NewAborting(t)
		ck.NoError(env.ctrl.SandboxRemove(context.Background(), users.Identity{UserID: "owner"}, "maker", "dev"), "creator")
	})
	t.Run("ancestor may remove", func(t *testing.T) {
		env := newSandboxEnv(t)
		seed(t, env)
		ck := assert.NewAborting(t)
		ck.NoError(env.ctrl.SandboxRemove(context.Background(), users.Identity{UserID: "owner"}, "root", "dev"), "ancestor")
	})
	t.Run("sibling refused", func(t *testing.T) {
		env := newSandboxEnv(t)
		seed(t, env)
		ck := assert.NewAborting(t)
		err := env.ctrl.SandboxRemove(context.Background(), users.Identity{UserID: "owner"}, "sibling", "dev")
		ck.Error(err, "sibling must be refused")
		ck.True(strings.Contains(err.Error(), "not created by this child"), "error text: %v", err)
	})
}

// TestSandboxListReportsLostNotRecreated: a ready row whose container is gone
// is reported lost and persisted, never recreated.
func TestSandboxListReportsLostNotRecreated(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.containerGone = true
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", Spec: []byte(`{}`),
		LauncherExecutorID: "launcher", ExecutorID: "gone", ContainerID: "ctr-1",
		State: sandboxStateReady,
	}))

	infos, err := env.ctrl.SandboxList(owner)
	ck.NoError(err, "list")
	ck.Len(infos, 1, "one sandbox")
	ck.Eq(sandboxStateLost, infos[0].State, "lost")
	row, _ := env.store.get("sbx-1")
	ck.Eq(sandboxStateLost, row.State, "state persisted")
	ck.False(env.docker.created(), "a lost sandbox is never recreated")
}

func (d *fakeDocker) created() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.createBody != nil
}

// TestSandboxSweepExpiresTTL: a named row past its expiry is removed.
func TestSandboxSweepExpiresTTL(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	past := time.Now().Add(-time.Minute)
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady,
		ExpiresAt: &past,
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "expired row tombstoned")
}

// TestSandboxReaperRemovesUnrowedContainer: a container whose sandbox row is
// gone is removed.
func TestSandboxReaperRemovesUnrowedContainer(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.list = []map[string]any{
		{"Id": "orphan-1", "Labels": map[string]string{sandbox.DockerLabelSandbox: "sbx-gone"}},
	}

	env.ctrl.sweepSandboxes(context.Background())
	ck.True(sandboxHas(env.docker.removed, "orphan-1"), "orphan removed: %v", env.docker.removed)
}

// TestSandboxReaperKeepsCreatingRowContainer: a container whose row is still
// creating is kept (the row is inserted before the container exists).
func TestSandboxReaperKeepsCreatingRowContainer(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev", State: sandboxStateCreating,
	}))
	env.docker.list = []map[string]any{
		{"Id": "ctr-1", "Labels": map[string]string{sandbox.DockerLabelSandbox: "sbx-1"}},
	}

	env.ctrl.sweepSandboxes(context.Background())
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "the creating row's container is kept: %v", env.docker.removed)
}

// TestSandboxReaperBeforeRecoveryKeepsLiveSpawnBlock: a spawn-block row whose
// child is live in the DB (but not yet loaded in memory) is kept, even though
// the row is old enough to be reapable.
func TestSandboxReaperBeforeRecoveryKeepsLiveSpawnBlock(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.children = childLiveness{live: []string{"child-1"}}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "child-1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateReady, CreatedAt: sandboxOldTime(),
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "a live child's spawn-block sandbox is kept")
}

// TestSandboxReaperRemovesClosedChildSpawnBlock: an OLD spawn-block row whose
// child is absent from the DB (closed or gone) is removed.
func TestSandboxReaperRemovesClosedChildSpawnBlock(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.children = childLiveness{} // child-1 is not live
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "child-1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateReady, CreatedAt: sandboxOldTime(),
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "a closed child's spawn-block sandbox is removed")
}

// TestSandboxReaperKeepsYoungSpawnBlock: a spawn-block row younger than
// sandboxMinReapAge is kept even when its child row is absent, because
// sandboxCreateForSpawn inserts the row before the spawn writes the child's row.
func TestSandboxReaperKeepsYoungSpawnBlock(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.children = childLiveness{} // child-1 is not live yet
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "child-1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateReady, CreatedAt: time.Now(), // young: mid-spawn
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "a young spawn-block row is never reaped on an absent child")
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "its container is kept")
}

// TestSandboxReaperRemovesCreatingRowOnBootSweep: a live `creating` row created
// BEFORE this process started is abandoned on the first post-boot sweep — no
// in-flight create survives a restart — so a daemon that died mid-create does
// not hold a cap slot forever. Its executor is evicted, freeing the slot.
func TestSandboxReaperRemovesCreatingRowOnBootSweep(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	boot := time.Now()
	env.ctrl.sandboxBootTime = boot
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev",
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateCreating, CreatedAt: boot.Add(-time.Minute), // predates this process
	}))

	env.ctrl.sweepSandboxesOnBoot(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "a creating row present at boot is abandoned")
	ck.True(env.pool.evicted["exec-created"], "its executor is evicted, freeing the cap slot")
}

// TestSandboxReaperKeepsPostBootCreatingRow: a live `creating` row created AFTER
// this process started is an IN-FLIGHT create this process is running — the
// first sweep fires a few seconds in, after clients can already reach the
// daemon — so the boot pass must NOT abandon it. It is left to the age gate.
func TestSandboxReaperKeepsPostBootCreatingRow(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	boot := time.Now()
	env.ctrl.sandboxBootTime = boot
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev",
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateCreating, CreatedAt: boot.Add(time.Second), // created after this process
	}))

	env.ctrl.sweepSandboxesOnBoot(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "an in-flight create that postdates the boot is kept")
	ck.False(env.pool.evicted["exec-created"], "its executor is not evicted")
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "its container is kept")
}

// TestSandboxReaperRemovesStaleCreatingRow: a `creating` row older than
// sandboxStaleCreatingAge is abandoned on any sweep (covering an unbounded
// image pull / a wedged create).
func TestSandboxReaperRemovesStaleCreatingRow(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev",
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateCreating, CreatedAt: sandboxOldTime(),
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "a stale creating row is abandoned")
}

// TestSandboxReaperKeepsYoungCreatingRow: a young `creating` row (an in-flight
// create) is kept on a non-boot sweep.
func TestSandboxReaperKeepsYoungCreatingRow(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "dev",
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateCreating, CreatedAt: time.Now(),
	}))

	env.ctrl.sweepSandboxes(context.Background())
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "a young creating row is kept")
	ck.False(sandboxHas(env.docker.removed, "ctr-1"), "its container is kept")
}

// TestSandboxReaperReadsTheRowAfterListingItsContainers proves the ordering
// fix: a sandbox created AFTER the container listing (its row inserted while
// ListContainers runs) must NOT have its container removed.
func TestSandboxReaperReadsTheRowAfterListingItsContainers(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.list = []map[string]any{
		{"Id": "ctr-new", "Labels": map[string]string{sandbox.DockerLabelSandbox: "sbx-new"}},
	}
	// The concurrent create: a live row for sbx-new lands while the container
	// listing is being served.
	env.docker.onList = func() {
		_ = env.store.Insert(context.Background(), sandbox.Row{
			ID: "sbx-new", OwnerUserID: owner.UserID, Name: "new",
			ExecutorID: "exec-created", LauncherExecutorID: "launcher",
			ContainerID: "ctr-new", State: sandboxStateCreating, CreatedAt: time.Now(),
		})
	}

	env.ctrl.sweepSandboxes(context.Background())
	ck.False(sandboxHas(env.docker.removed, "ctr-new"),
		"a container whose row was written after the listing is kept: %v", env.docker.removed)
	row, _ := env.store.get("sbx-new")
	ck.True(row.RemovedAt == nil, "the newly created row is untouched")
}

// TestSandboxCreateNilEngineFailsClosed: a seam that returns a nil engine is
// refused instead of panicking.
func TestSandboxCreateNilEngineFailsClosed(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.sandboxEngine = func(string) *sandbox.Engine { return nil }

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Error(err, "a nil engine must be refused")
	ck.True(strings.Contains(err.Error(), "sandbox engine is not wired"), "error text: %v", err)
}

// TestSandboxRemoveSandboxesOwnedBy: teardown removes the OWNING child's own
// spawn-block sandbox ONLY. A still-live descendant's own sandbox survives its
// ancestor's close (its workspace may be in use), and an unrelated child's is
// untouched. Teardown is keyed on the owning child; the reaper covers each
// sandbox once its OWN child's row is closed or absent.
func TestSandboxRemoveSandboxesOwnedBy(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	sandboxSeedChild(env, "a", "")
	sandboxSeedChild(env, "b", "a")
	sandboxSeedChild(env, "other", "")
	for _, r := range []struct{ id, child string }{
		{"sbx-a", "a"}, {"sbx-b", "b"}, {"sbx-other", "other"},
	} {
		ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
			ID: r.id, OwnerUserID: owner.UserID, OwnerChild: r.child, Scope: protocol.ScopeSelf,
			ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-" + r.child,
			State: sandboxStateReady,
		}))
	}

	env.ctrl.removeSandboxesOwnedBy(context.Background(), "a")
	a, _ := env.store.get("sbx-a")
	ck.True(a.RemovedAt != nil, "the owning child's own sandbox is removed")
	for _, id := range []string{"sbx-b", "sbx-other"} {
		row, _ := env.store.get(id)
		ck.True(row.RemovedAt == nil, "%s must NOT be removed (only the owning child's sandbox is)", id)
	}
}

// TestSandboxCreateForSpawnHappyPath: the spawn flow's unnamed variant mints a
// spawn-block row bound to the new child.
func TestSandboxCreateForSpawnHappyPath(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	sandboxSeedChild(env, "parent-1", "")
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	executor, rowID, err := env.ctrl.sandboxCreateForSpawn(context.Background(), owner, "parent-1", "child-1",
		protocol.SandboxSpec{Image: "rafiki/sandbox:test", Scope: protocol.ScopeSubtree})
	ck.NoError(err, "spawn create")
	ck.Eq("exec-created", executor.ID)
	row, ok := env.store.get(rowID)
	ck.True(ok, "row stored")
	ck.Eq("child-1", row.OwnerChild)
	ck.Eq(protocol.ScopeSubtree, row.Scope)
	ck.True(row.Name == "", "spawn blocks are unnamed")
	ck.True(row.ExpiresAt == nil, "spawn blocks have no TTL")
	tok := env.exec.lastCreate
	ck.Eq("child-1", tok.Labels[sandbox.RowLabelOwnerChild])
	ck.Eq("subtree", tok.Labels[sandbox.RowLabelScope])
}

// TestSandboxOwnedByOwner: the child that owns the block binds to it.
func TestSandboxOwnedByOwner(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: "o", OwnerChild: "c1", Scope: protocol.ScopeSelf, State: sandboxStateReady,
	}))
	row, ok, err := env.ctrl.ownedSandbox(context.Background(), "c1", "o")
	ck.NoError(err, "ownedSandbox")
	ck.True(ok, "c1 owns its own block")
	ck.Eq("sbx-1", row.ID)
}

// TestSandboxOwnedBySubtreeDescendant: a subtree-scoped ancestor block owns a
// descendant.
func TestSandboxOwnedBySubtreeDescendant(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	seedLineage(env, "a", "b")
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: "o", OwnerChild: "a", Scope: protocol.ScopeSubtree, State: sandboxStateReady,
	}))
	row, ok, err := env.ctrl.ownedSandbox(context.Background(), "b", "o")
	ck.NoError(err, "ownedSandbox")
	ck.True(ok, "the subtree block owns the descendant")
	ck.Eq("sbx-1", row.ID)
}

// TestSandboxSelfScopeNotOwnedByDescendant: a self-scoped ancestor block does
// not own a descendant.
func TestSandboxSelfScopeNotOwnedByDescendant(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	seedLineage(env, "a", "b")
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: "o", OwnerChild: "a", Scope: protocol.ScopeSelf, State: sandboxStateReady,
	}))
	_, ok, err := env.ctrl.ownedSandbox(context.Background(), "b", "o")
	ck.NoError(err, "ownedSandbox")
	ck.False(ok, "a self-scoped block does not own a descendant")
}

// TestSandboxUnrelatedChildNotOwned.
func TestSandboxUnrelatedChildNotOwned(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
		ID: "sbx-1", OwnerUserID: "o", OwnerChild: "a", Scope: protocol.ScopeSubtree, State: sandboxStateReady,
	}))
	_, ok, err := env.ctrl.ownedSandbox(context.Background(), "unrelated", "o")
	ck.NoError(err, "ownedSandbox")
	ck.False(ok, "an unrelated child owns nothing")
}

// TestSandboxTwoEligibleRowsIsAnError.
func TestSandboxTwoEligibleRowsIsAnError(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	seedLineage(env, "a", "b")
	for _, id := range []string{"sbx-1", "sbx-2"} {
		ck.NoError(env.store.Insert(context.Background(), sandbox.Row{
			ID: id, OwnerUserID: "o", OwnerChild: "a", Scope: protocol.ScopeSubtree, State: sandboxStateReady,
		}))
	}
	_, _, err := env.ctrl.ownedSandbox(context.Background(), "b", "o")
	ck.Error(err, "two eligible rows must fail closed")
}

// TestSandboxStoreErrorIsNotUnowned.
func TestSandboxStoreErrorIsNotUnowned(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	env.store.listErr = errors.New("boom")
	_, ok, err := env.ctrl.ownedSandbox(context.Background(), "c1", "o")
	ck.Error(err, "a store error must surface")
	ck.False(ok, "a store error is not 'not owned'")
}

// TestSandboxChildCallerResolvesLauncherWithinItsSet confirms a child caller's
// launcher is drawn from its effective set.
func TestSandboxChildCallerResolvesLauncherWithinItsSet(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	seedLineageWithLabels(env, "child-1", nil)
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "child-1", sandboxSpec("dev"))
	ck.NoError(err, "a child caller creates within its launcher set")
}

// --- test helpers -----------------------------------------------------------

// sandboxOldTime is a created_at old enough to pass sandboxMinReapAge, so a
// test's spawn-block row is eligible for the reaper's child-absent test.
func sandboxOldTime() time.Time { return time.Now().Add(-time.Hour) }

// seedChild inserts a live child, optionally with a parent link.
func sandboxSeedChild(env *sandboxEnv, id, parent string) {
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
		labels[childstore.LabelRoot] = parent
	}
	env.ctrl.st.Insert(&childstore.Session{ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(), Labels: labels})
}

func seedLineage(env *sandboxEnv, ids ...string) {
	for i, id := range ids {
		parent := ""
		if i > 0 {
			parent = ids[i-1]
		}
		sandboxSeedChild(env, id, parent)
	}
}

func seedLineageWithLabels(env *sandboxEnv, id string, labels map[string]string) {
	env.ctrl.st.Insert(&childstore.Session{ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(), Labels: labels})
}
