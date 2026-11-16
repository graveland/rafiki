// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// The install orchestrator's seams are all fakes here: a recipe store, a
// launcher that records every RPC it is asked to run, and the tunnelled
// connection's serve loop. Nothing dials ssh, and nothing needs a database —
// which is the point, because these tests are about the DECISIONS (who may
// launch, what may be clobbered, what the caller is told) rather than about
// ssh.

// The compile-time pin for the Connect seam. A rename in either direction
// would otherwise silently leave both install RPCs answering "not yet wired".
var _ connectapi.ExecutorInstaller = connectExecutorInstaller{}

// ─── Fakes ──────────────────────────────────────────────────────────────────

// fakeInstallStore is conversations.executor_install in memory, including the
// live-row uniqueness the partial index enforces.
type fakeInstallStore struct {
	mu      sync.Mutex
	rows    map[string]executorinstall.Row
	order   []string
	inserts int
	sets    int
	modes   int
}

func newFakeInstallStore() *fakeInstallStore {
	return &fakeInstallStore{rows: map[string]executorinstall.Row{}}
}

func (s *fakeInstallStore) Insert(_ context.Context, r executorinstall.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, live := range s.rows {
		if live.RemovedAt == nil && live.ExecutorID == r.ExecutorID {
			return executorinstall.ErrExists
		}
	}
	r.CreatedAt = time.Now()
	s.rows[r.ID] = r
	s.order = append(s.order, r.ID)
	s.inserts++
	return nil
}

func (s *fakeInstallStore) ByExecutor(_ context.Context, executorID string) (executorinstall.Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if r := s.rows[id]; r.ExecutorID == executorID && r.RemovedAt == nil {
			return r, true, nil
		}
	}
	return executorinstall.Row{}, false, nil
}

func (s *fakeInstallStore) ListLive(_ context.Context, ownerUserID string) ([]executorinstall.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []executorinstall.Row
	for _, id := range s.order {
		if r := s.rows[id]; r.RemovedAt == nil && r.OwnerUserID == ownerUserID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeInstallStore) ListLiveAll(_ context.Context) ([]executorinstall.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []executorinstall.Row
	for _, id := range s.order {
		if r := s.rows[id]; r.RemovedAt == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeInstallStore) SetLauncher(_ context.Context, id, launcherExecutorID, platform string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	r.LauncherExecutorID, r.Platform = launcherExecutorID, platform
	s.rows[id] = r
	s.sets++
	return nil
}

// SetMode records the transport a live row was installed with, matching the
// Postgres store: an unknown id is a no-op.
func (s *fakeInstallStore) SetMode(_ context.Context, id, mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.RemovedAt != nil {
		return nil
	}
	r.Mode = mode
	s.rows[id] = r
	s.modes++
	return nil
}

func (s *fakeInstallStore) Tombstone(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil
	}
	if r.RemovedAt == nil {
		now := time.Now()
		r.RemovedAt = &now
	}
	s.rows[id] = r
	return nil
}

// liveRows returns the live rows, oldest first.
func (s *fakeInstallStore) liveRows() []executorinstall.Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []executorinstall.Row
	for _, id := range s.order {
		if r := s.rows[id]; r.RemovedAt == nil {
			out = append(out, r)
		}
	}
	return out
}

// rowByID returns a row whether or not it is tombstoned.
func (s *fakeInstallStore) rowByID(id string) (executorinstall.Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

// fakeServeStream is one RemoteServe bidi stream, recording every request the
// daemon sends so a test can prove where the ticket did and did not appear.
type fakeServeStream struct {
	mu     sync.Mutex
	sent   []*executorpb.RemoteServeRequest
	frames chan *executorpb.RemoteServeResponse
	closed bool
}

func newFakeServeStream() *fakeServeStream {
	return &fakeServeStream{frames: make(chan *executorpb.RemoteServeResponse, 16)}
}

func (f *fakeServeStream) Send(msg *executorpb.RemoteServeRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return io.EOF
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeServeStream) Receive() (*executorpb.RemoteServeResponse, error) {
	msg, ok := <-f.frames
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (f *fakeServeStream) CloseRequest() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeServeStream) CloseResponse() error { return nil }

// exit delivers an exit frame: the remote process ended.
func (f *fakeServeStream) exit(code int32, tail string) {
	f.frames <- &executorpb.RemoteServeResponse{
		Msg: &executorpb.RemoteServeResponse_Exit{
			Exit: &executorpb.RemoteServeExit{Code: code, StderrTail: tail},
		},
	}
}

// sentRequests returns a copy of every request the daemon sent.
func (f *fakeServeStream) sentRequests() []*executorpb.RemoteServeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*executorpb.RemoteServeRequest(nil), f.sent...)
}

// fakePush is one RemoteInstall the daemon asked a launcher to perform.
type fakePush struct {
	target     *executorpb.RemoteTarget
	binaryPath string
	sha256     string
	body       []byte
}

// fakeLauncher records every Remote* call and answers from scripted results.
type fakeLauncher struct {
	mu           sync.Mutex
	probeResults []*executorpb.RemoteProbeResponse
	probes       []*executorpb.RemoteProbeRequest
	pushes       []fakePush
	serves       []*executorpb.RemoteServeStart
	streams      []*fakeServeStream
	cleans       []*executorpb.RemoteCleanRequest

	probeErr error
	pushErr  error
	serveErr error
	cleanErr error
	// serveExit, when set, makes the remote exit immediately after the stream
	// opens — the shape of a target whose ssh works and whose binary does not.
	serveExit   bool
	serveStderr string
}

func (f *fakeLauncher) Probe(
	_ context.Context, target *executorpb.RemoteTarget, binaryPath string,
) (*executorpb.RemoteProbeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, &executorpb.RemoteProbeRequest{Target: target, BinaryPath: binaryPath})
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	if len(f.probeResults) == 0 {
		return &executorpb.RemoteProbeResponse{Uname: "Linux x86_64"}, nil
	}
	idx := len(f.probes) - 1
	if idx >= len(f.probeResults) {
		idx = len(f.probeResults) - 1
	}
	return f.probeResults[idx], nil
}

func (f *fakeLauncher) Push(
	_ context.Context, target *executorpb.RemoteTarget, binaryPath, sha256 string, src io.Reader,
) error {
	body, _ := io.ReadAll(src)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, fakePush{target: target, binaryPath: binaryPath, sha256: sha256, body: body})
	return f.pushErr
}

func (f *fakeLauncher) Serve(
	_ context.Context, start *executorpb.RemoteServeStart,
) (remoteServeStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serves = append(f.serves, start)
	if f.serveErr != nil {
		return nil, f.serveErr
	}
	stream := newFakeServeStream()
	f.streams = append(f.streams, stream)
	if f.serveExit {
		stream.exit(1, f.serveStderr)
	}
	return stream, nil
}

func (f *fakeLauncher) Clean(
	_ context.Context, target *executorpb.RemoteTarget, installID string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleans = append(f.cleans, &executorpb.RemoteCleanRequest{Target: target, InstallId: installID})
	return f.cleanErr == nil, f.cleanErr
}

// probeCalls returns how many probes ran.
func (f *fakeLauncher) probeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probes)
}

// serveCalls returns how many ssh sessions were opened.
func (f *fakeLauncher) serveCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.serves)
}

// pushCalls returns how many binaries were streamed.
func (f *fakeLauncher) pushCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pushes)
}

// cleanCalls returns how many marker removals were asked for.
func (f *fakeLauncher) cleanCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cleans)
}

func (f *fakeLauncher) starts() []*executorpb.RemoteServeStart {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*executorpb.RemoteServeStart(nil), f.serves...)
}

func (f *fakeLauncher) firstStream() *fakeServeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streams) == 0 {
		return nil
	}
	return f.streams[0]
}

// rpcFields renders every string a launcher RPC carried, for the test that
// proves the ticket is in none of them.
func (f *fakeLauncher) rpcFields() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.probes {
		out = append(out, p.GetBinaryPath(), p.GetTarget().GetDestination(), p.GetTarget().GetInstance())
	}
	for _, p := range f.pushes {
		out = append(out, p.binaryPath, p.sha256, p.target.GetDestination(), p.target.GetInstance())
	}
	for _, s := range f.serves {
		out = append(out, s.GetBinaryPath(), s.GetRoot(), s.GetInstallId(),
			s.GetTarget().GetDestination(), s.GetTarget().GetInstance())
	}
	for _, c := range f.cleans {
		out = append(out, c.GetInstallId(), c.GetTarget().GetDestination(), c.GetTarget().GetInstance())
	}
	return out
}

// installExecStore is fakeExecStore with a distinct id per Create, so a test can
// name the row one install made and a reinstall can find it again.
type installExecStore struct {
	*fakeExecStore
	next int
}

func newInstallExecStore() *installExecStore {
	return &installExecStore{fakeExecStore: newFakeExecStore()}
}

func (s *installExecStore) Create(_ context.Context, t executors.NewToken) (executors.Executor, string, error) {
	s.next++
	e := executors.Executor{
		ID:            fmt.Sprintf("exec-%d", s.next),
		Labels:        t.Labels,
		Roots:         t.Roots,
		Isolation:     t.Isolation,
		WorkspaceMode: t.WorkspaceMode,
		Admits:        t.Admits,
		OwnerUserID:   t.OwnerUserID,
		Enabled:       true,
		LastSeenAt:    time.Now(),
	}
	s.execs[e.ID] = e
	s.creates = append(s.creates, t)
	s.createCalls++
	return e, "credential-that-must-never-be-stored", nil
}

// ─── Fixture ────────────────────────────────────────────────────────────────

const (
	installTestTag       = "v9.9.9"
	installTestLauncher  = "grey-1"
	installTestLauncherN = "greyshift"
	installTestOwner     = "u_brent"
	installTestOwnerName = "brent"
)

type installFixture struct {
	t        *testing.T
	ctrl     *Controller
	pool     *fakePool
	execs    *installExecStore
	installs *fakeInstallStore
	launcher *fakeLauncher
	state    *installState
	binPath  string
	binSHA   string

	mu           sync.Mutex
	connectAs    string
	served       int
	serveEntered chan struct{}
	enterOnce    sync.Once
}

// newInstallFixture builds a Controller with the orchestrator wired to fakes. By
// default one live native launcher exists (`greyshift`, instance `work`) and the
// target answers `Linux x86_64`, has no binary, no credential and no marker.
func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	c := assert.NewAborting(t)

	dir := t.TempDir()
	c.Require().NoError(os.MkdirAll(filepath.Join(dir, "linux-amd64"), 0o755), "depot dir")
	binPath := filepath.Join(dir, "linux-amd64", "rafiki")
	body := []byte("#!/bin/sh\n# the daemon's own binary\n")
	c.Require().NoError(os.WriteFile(binPath, body, 0o755), "depot binary")
	sha, err := hashDepotFile(binPath)
	c.Require().NoError(err, "hash depot binary")

	f := &installFixture{
		t:            t,
		pool:         &fakePool{},
		execs:        newInstallExecStore(),
		installs:     newFakeInstallStore(),
		launcher:     &fakeLauncher{},
		binPath:      binPath,
		binSHA:       sha,
		serveEntered: make(chan struct{}),
	}
	f.state = newInstallState(f.installs, &executorDepot{tag: installTestTag, override: dir})
	f.state.launcher = func(string) (installLauncher, error) { return f.launcher, nil }
	f.state.serve = f.serveConn
	f.pool.live = []execpool.LiveExecutor{installLauncherLive(
		installTestLauncher, installTestLauncherN, "work", "pinned")}
	f.ctrl = &Controller{
		st:        childstore.New(),
		execStore: f.execs,
		execPool:  f.pool,
		baseCtx:   context.Background(),
		install:   f.state,
	}
	t.Cleanup(func() {
		for _, cancel := range f.cancels() {
			cancel()
		}
	})
	return f
}

// serveConn stands in for Pool.ServeConn: it reports the connection the moment
// the session starts (when the test asked for that) and otherwise holds the
// session open until it is cancelled.
func (f *installFixture) serveConn(ctx context.Context, conn net.Conn) error {
	f.mu.Lock()
	f.served++
	first := f.served == 1
	as := f.connectAs
	f.mu.Unlock()
	if first {
		f.enterOnce.Do(func() { close(f.serveEntered) })
	}
	if as != "" {
		f.ctrl.installConnected(as)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *installFixture) cancels() []context.CancelFunc {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	out := make([]context.CancelFunc, 0, len(f.state.serves))
	for _, c := range f.state.serves {
		out = append(out, c)
	}
	return out
}

// installLauncherLive is a live native launcher executor: a durable row (owner,
// machine, admits) with a Describe that says it may run ssh.
func installLauncherLive(id, machine, instance, workspaceMode string) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{
			ID:            id,
			Labels:        map[string]string{"machine": machine, "owner": installTestOwnerName},
			SelfReported:  map[string]string{"instance": instance},
			Isolation:     "none",
			WorkspaceMode: workspaceMode,
			Admits:        "owner=" + installTestOwnerName,
			OwnerUserID:   installTestOwner,
			Enabled:       true,
		},
		Describe: &executorpb.DescribeResponse{SshLaunch: true, SelfReportedLabels: map[string]string{"instance": instance}},
	}
}

// installUserCaller is the ordinary user credential's caller.
func installUserCaller() installCaller {
	return installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName}
}

// installReq is an install of `brent@ublk-s2` named `ublk-s2`.
func installReq(via string) *rafikiv1.InstallExecutorRequest {
	return &rafikiv1.InstallExecutorRequest{Target: "brent@ublk-s2", Via: via}
}

// expectControllerError asserts the error carries code and the exact message.
func expectControllerError(t *testing.T, err error, code, want string) {
	t.Helper()
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a ControllerError with code %s", err, code)
	}
	assert.NewAborting(t).Eq(code, ce.Code, "error code (message was %q)", ce.Message)
	assert.NewAborting(t).Eq(want, ce.Message, "error message")
}

// ─── Install ────────────────────────────────────────────────────────────────

// A fresh install writes the executor row, the recipe, pushes the binary and
// starts one ssh session — and the row it answers with carries the launcher and
// the target.
func TestInstallFreshCreatesRowAndLaunches(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.connectAs = "exec-1"

	row, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	c.Require().NoError(err, "install")

	// The executor row is daemon-written: name, owner, the two provenance
	// labels, and the gating facts the operator cannot choose.
	c.Require().Len(f.execs.creates, 1, "executor rows created")
	created := f.execs.creates[0]
	c.Eq("ublk-s2", created.Labels["machine"], "machine label")
	c.Eq(installTestOwnerName, created.Labels["owner"], "owner label")
	c.Eq(installTestLauncherN, created.Labels[installedViaLabel], "installed-via label")
	c.Eq("brent@ublk-s2", created.Labels[installedTargetLabel], "installed-target label")
	c.Eq("none", created.Isolation, "an installed executor is native")
	c.Eq("pinned", created.WorkspaceMode, "an installed executor is pinned")
	c.EqDeep([]string{"~"}, created.Roots, "a root-less install roots at ~")
	c.Eq(installTestOwner, created.OwnerUserID, "owner from the connection")

	// The recipe records the launcher, the target, the namespace and the
	// platform, and the child that asked (none: a human did).
	live := f.installs.liveRows()
	c.Require().Len(live, 1, "install recipes")
	recipe := live[0]
	c.Eq("exec-1", recipe.ExecutorID, "recipe executor")
	c.Eq(installTestLauncher, recipe.LauncherExecutorID, "recipe launcher")
	c.Eq("brent@ublk-s2", recipe.Target, "recipe target")
	c.Eq("work", recipe.Instance, "recipe instance comes from the launcher")
	c.Eq("linux/amd64", recipe.Platform, "recipe platform")
	c.Eq("", recipe.InstalledByChild, "a human installed it")

	// Two probes: one for the platform, one named by the content-addressed path
	// this daemon's binary produces.
	c.Eq(2, f.launcher.probeCalls(), "probe calls")
	wantBin := "$HOME/.cache/rafiki/bin/rafiki-" + installTestTag + "-" + f.binSHA[:12]
	c.Eq(wantBin, f.launcher.probes[1].GetBinaryPath(), "second probe asks about the depot binary")

	// The binary was absent, so it was pushed exactly once, byte for byte.
	c.Eq(1, f.launcher.pushCalls(), "binary pushes")
	c.Eq(wantBin, f.launcher.pushes[0].binaryPath, "pushed to the depot path")
	c.Eq(f.binSHA, f.launcher.pushes[0].sha256, "pushed the depot's sha256")
	c.EqDeep([]byte("#!/bin/sh\n# the daemon's own binary\n"), f.launcher.pushes[0].body, "pushed the depot's bytes")

	// One ssh session, started with the recipe's own install id.
	c.Eq(1, f.launcher.serveCalls(), "relay sessions")
	start := f.launcher.starts()[0]
	c.Eq(wantBin, start.GetBinaryPath(), "serve binary path")
	c.Eq("brent@ublk-s2", start.GetTarget().GetDestination(), "serve destination")
	c.Eq("work", start.GetTarget().GetInstance(), "serve instance")
	c.Eq(recipe.ID, start.GetInstallId(), "the remote records the recipe id in its marker")
	c.Eq("", start.GetRoot(), "a root-less install passes no --root")

	// The answer names the executor and where it came from.
	c.Eq("exec-1", row.GetId(), "row id")
	c.Eq("ublk-s2", row.GetMachine(), "row machine")
	c.Eq(installTestLauncherN, row.GetInstalledVia(), "row installed_via")
	c.Eq("brent@ublk-s2", row.GetInstalledTarget(), "row installed_target")
}

// An executor this daemon did not install over ssh is never taken over: the
// operator's enrolled machine keeps its identity.
func TestInstallRefusesEnrolledNameCollision(t *testing.T) {
	f := newInstallFixture(t)
	f.execs.execs["enrolled-1"] = executors.Executor{
		ID: "enrolled-1", Labels: map[string]string{"machine": "ublk-s2"},
		OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
	}

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	expectControllerError(t, err, protocol.ErrAlreadyExists,
		"executor ublk-s2 already exists and was not installed over ssh; pick another name or remove it")
	assert.NewAborting(t).Eq(0, f.launcher.probeCalls(),
		"the name collision must be decided before any ssh round trip")
}

// A machine a human set up with `rafiki executor setup` keeps its namespace: two
// executors on one machine prune each other's caches.
func TestInstallRefusesTargetCredential(t *testing.T) {
	f := newInstallFixture(t)
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{{
		Uname: "Linux x86_64", CredentialPresent: true,
	}}

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	expectControllerError(t, err, protocol.ErrAlreadyExists,
		"ublk-s2 already has a durable executor "+
			"(${XDG_DATA_HOME:-$HOME/.local/share}/rafiki/profiles/work/executor.cred); "+
			"use it, or remove that file first")
	c := assert.NewAborting(t)
	c.Eq(0, f.launcher.pushCalls(), "no binary may be pushed onto a machine with a durable executor")
	c.Eq(0, len(f.execs.creates), "no executor row may be written")
}

// A marker naming another install is another daemon's executor (or an older
// row's): refuse, and name the command that clears it.
func TestInstallRefusesForeignMarker(t *testing.T) {
	f := newInstallFixture(t)
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{{
		Uname: "Linux x86_64", Marker: "0193-other-install",
	}}

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	expectControllerError(t, err, protocol.ErrAlreadyExists,
		"ublk-s2 has executor state from another install "+
			"(${XDG_DATA_HOME:-$HOME/.local/share}/rafiki/profiles/work/installed-executor, "+
			"install 0193-other-install); remove it with: ssh brent@ublk-s2 rm -f "+
			"${XDG_DATA_HOME:-$HOME/.local/share}/rafiki/profiles/work/installed-executor")
	assert.NewAborting(t).Eq(0, len(f.execs.creates), "a foreign marker must not be overwritten")
}

// A reinstall onto THIS row's own marker proceeds, and reuses the executor row
// and the recipe rather than minting new ones.
func TestInstallReinstallWithOwnMarkerProceeds(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.execs.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "ublk-s2"},
		OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
		WorkspaceMode: "pinned", Roots: []string{"~"},
	}
	c.Require().NoError(f.installs.Insert(context.Background(), executorinstall.Row{
		ID: "recipe-1", OwnerUserID: installTestOwner, ExecutorID: "exec-1",
		LauncherExecutorID: installTestLauncher, Target: "brent@ublk-s2",
		Instance: "work", Platform: "linux/amd64",
	}), "seed the recipe")
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{{
		Uname: "Linux x86_64", BinaryPresent: true, Marker: "recipe-1",
	}}
	f.connectAs = "exec-1"

	row, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	c.Require().NoError(err, "reinstall")

	c.Eq(0, len(f.execs.creates), "a reinstall reuses the executor row")
	c.Len(f.installs.liveRows(), 1, "recipes after a reinstall")
	c.Eq("recipe-1", f.installs.liveRows()[0].ID, "the recipe row is reused")
	c.Eq(0, f.installs.sets, "nothing changed, so nothing was rewritten")
	c.Eq(1, f.launcher.serveCalls(), "a reinstall still launches")
	c.Eq("exec-1", row.GetId(), "the reinstall answers with the SAME executor id")
}

// A binary already on the target at the content-addressed path is not pushed
// again: the path carries the sha, so a present file is the same build.
func TestInstallSkipsPushWhenBinaryPresent(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{{
		Uname: "Linux x86_64", BinaryPresent: true,
	}}
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	c.Require().NoError(err, "install")
	c.Eq(0, f.launcher.pushCalls(), "the binary was already there")
	c.Eq(2, f.launcher.probeCalls(), "both probes still run")
}

// A container-isolated executor has no ssh keys of its own and is never a
// launcher: the capability an install would grant equals what `bash` on the
// launcher can already do, and a container has no such capability.
func TestInstallContainerLauncherRefused(t *testing.T) {
	f := newInstallFixture(t)
	f.pool.live[0].Executor.Isolation = "container"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(installTestLauncherN))
	expectControllerError(t, err, protocol.ErrFailedPrecondition,
		"executor greyshift is container-isolated and cannot install executors")
	assert.NewAborting(t).Eq(0, f.launcher.probeCalls(), "a refused launcher runs no ssh")
}

// --no-ssh-launch is the operator's opt-out, and it only ever narrows.
func TestInstallNoSSHLaunchRefused(t *testing.T) {
	f := newInstallFixture(t)
	f.pool.live[0].Describe = &executorpb.DescribeResponse{SshLaunch: false}

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	expectControllerError(t, err, protocol.ErrFailedPrecondition,
		"executor greyshift has ssh launch disabled (--no-ssh-launch)")
	assert.NewAborting(t).Eq(0, f.launcher.probeCalls(), "a refusing launcher runs no ssh")
}

// A child may only install through a launcher it could already call. A machine
// that does not admit it is not a launcher for it, even when it is named.
func TestInstallChildLauncherMustBeReachable(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	// A second native executor the child may NOT reach: its admits selector
	// excludes the child's owner label.
	far := installLauncherLive("grey-2", "faraway", "work", "pinned")
	far.Executor.Admits = "owner=someone-else"
	f.pool.live = append(f.pool.live, far)

	now := time.Now()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_kid", Status: protocol.StatusIdle, StartedAt: now,
		OwnerUserID: installTestOwner,
		Labels:      map[string]string{"owner": installTestOwnerName, childstore.LabelRoot: "c_kid"},
	})
	childCaller := installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: "c_kid"}

	_, err := f.ctrl.InstallExecutor(context.Background(), childCaller, installReq("faraway"))
	expectControllerError(t, err, protocol.ErrPermissionDenied,
		"executor faraway is not reachable by this agent")
	c.Eq(0, f.launcher.probeCalls(), "an unreachable launcher runs no ssh")

	// The reachable one is accepted — and the row records the INSTALLER child,
	// which is what bounds a later uninstall to this lineage.
	f.connectAs = "exec-1"
	_, err = f.ctrl.InstallExecutor(context.Background(), childCaller, installReq(installTestLauncherN))
	c.Require().NoError(err, "install through a reachable launcher")
	c.Eq("c_kid", f.installs.liveRows()[0].InstalledByChild, "the installer child is recorded")
}

// A child credential that names no stored row installs through nothing. An
// empty label set matches an executor that admits everything — which every
// installed row does — so an unguarded vanished caller would resolve the widest
// possible launcher set.
func TestInstallRefusesAVanishedChild(t *testing.T) {
	f := newInstallFixture(t)

	_, err := f.ctrl.InstallExecutor(context.Background(),
		installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: "c_ghost"},
		installReq(""))
	expectControllerError(t, err, protocol.ErrPermissionDenied,
		"agent c_ghost has no stored row, so it may not install executors")
	assert.NewAborting(t).Eq(0, f.launcher.probeCalls(), "no ssh on a vanished caller's behalf")
}

// The daemon's own label namespace is not the caller's to write: a rafiki/ key
// would let a caller forge the record of where an executor came from.
func TestInstallRefusesReservedLabels(t *testing.T) {
	f := newInstallFixture(t)
	req := installReq("")
	req.Labels = map[string]string{installedViaLabel: "somebody-else"}

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), req)
	expectControllerError(t, err, protocol.ErrInvalidArgs,
		`label "rafiki/installed-via" is reserved: labels under rafiki/ or fundi/ are written by the daemon`)
	assert.NewAborting(t).Eq(0, f.launcher.probeCalls(), "a reserved label is refused before any ssh")
	assert.NewAborting(t).Eq(0, len(f.execs.creates), "and before any row is written")
}

// A fresh install that never connects leaves NOTHING behind: no executor row to
// collide with, no live recipe to look installed. The next attempt must look
// like a first attempt.
func TestInstallFailureAfterCreateLeavesNoRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.launcher.serveExit = true
	f.launcher.serveStderr = "rafiki: no such file or directory"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	expectControllerError(t, err, protocol.ErrUnavailable,
		"relaunch failed via greyshift: rafiki: no such file or directory")

	c.Eq(0, len(f.installs.liveRows()), "a failed fresh install leaves no live recipe")
	c.Len(f.installs.order, 1, "the recipe row is kept as a tombstone, not deleted")
	tomb, ok := f.installs.rowByID(f.installs.order[0])
	c.Require().True(ok, "the recipe row exists")
	c.NotNil(tomb.RemovedAt, "a failed fresh install tombstones its recipe")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is deleted")
	_, still := f.execs.execs["exec-1"]
	c.False(still, "no half-installed executor row may survive")
}

// The remote's credential is a one-shot ticket written as the first line of the
// relayed stdin. It must appear in NO RPC field: not an argv (it would be
// visible in ps on either machine), not a request field a proxy could log.
func TestInstallTicketNeverInAnyRPCField(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
	c.Require().NoError(err, "install")

	stream := f.launcher.firstStream()
	c.Require().NotNil(stream, "one relay stream")
	var ticket string
	for _, msg := range stream.sentRequests() {
		if stdin := msg.GetStdin(); len(stdin) > 0 {
			c.Eq("", ticket, "the ticket is the FIRST stdin chunk and there is only one")
			ticket = strings.TrimSuffix(string(stdin), "\n")
		}
		start := msg.GetStart()
		c.Nil(start, "the start frame is sent by Serve, which the fake records separately")
	}
	c.NotEq("", ticket, "a ticket was written to the remote's stdin")

	for _, field := range f.launcher.rpcFields() {
		c.NotStrContains(field, ticket, "the ticket must not appear in any RPC field")
	}
	// It is a REAL ticket: minted into the registry, redeemable exactly once.
	// (The fake never connects, so nothing here has redeemed it — which is what
	// makes the redemption below observable.)
	if _, ok := f.pool.Tickets().Redeem(ticket); !ok {
		t.Fatalf("the stdin line was not a minted ticket")
	}
	if _, ok := f.pool.Tickets().Redeem(ticket); ok {
		t.Fatalf("a ticket redeemed twice")
	}
}

// ─── Relaunch ───────────────────────────────────────────────────────────────

// seedInstalledExecutor gives the fixture one installed, connected-nowhere
// executor with a live recipe, so a relaunch has something to launch.
func (f *installFixture) seedInstalledExecutor() {
	f.execs.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "ublk-s2"},
		OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
		WorkspaceMode: "pinned", Roots: []string{"~"},
	}
	if err := f.installs.Insert(context.Background(), executorinstall.Row{
		ID: "recipe-1", OwnerUserID: installTestOwner, ExecutorID: "exec-1",
		LauncherExecutorID: installTestLauncher, Target: "brent@ublk-s2",
		Instance: "work", Platform: "linux/amd64",
	}); err != nil {
		f.t.Fatalf("seed recipe: %v", err)
	}
}

// Eight callers, one ssh session: a down installed executor is relaunched once
// and everyone shares the outcome.
func TestRelaunchCoalescesEightCallers(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- f.ctrl.launchInstalled(context.Background(), "exec-1") }()
	// The leader is now blocked inside the ssh session, waiting for a connection.
	select {
	case <-f.serveEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never opened its relay session")
	}

	// Seven followers whose contexts are already cancelled: each one JOINS the
	// in-flight launch (returning context.Canceled) instead of opening a session
	// of its own. That is the coalescing under test, and it is decided entirely
	// by whether a flight exists.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 7)
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.ctrl.launchInstalled(cancelled, "exec-1")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		c.ErrorIs(err, context.Canceled, "a coalesced caller returns its own context's error")
	}

	// The leader is still waiting, so its session is the only one that exists.
	c.Eq(1, f.launcher.serveCalls(), "eight callers must cost ONE ssh session")
	f.ctrl.installConnected("exec-1")
	select {
	case err := <-leaderDone:
		c.NoError(err, "the leader completes when the executor connects")
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never returned after the executor connected")
	}
	c.Eq(1, f.launcher.serveCalls(), "still one session")
}

// The wait is on the pool's connect hook, not on a poll: nothing advances until
// the connection is announced, and the announcement alone completes the launch.
func TestRelaunchWaitsOnConnectNotPolling(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	done := make(chan error, 1)
	go func() { done <- f.ctrl.launchInstalled(context.Background(), "exec-1") }()
	select {
	case <-f.serveEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay session never started")
	}
	select {
	case err := <-done:
		t.Fatalf("the launch returned (%v) before any connection was announced", err)
	default:
	}
	// The ONE event that ends the wait.
	f.ctrl.installConnected("exec-1")
	select {
	case err := <-done:
		c.NoError(err, "the connect announcement completes the launch")
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not complete on the connect announcement")
	}
}

// A remote that never connects is given a bounded wait, and its ticket is
// revoked: a credential that outlives the session it was minted for is one
// nobody is waiting for.
func TestRelaunchTimeoutRevokesTicket(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.state.connectTimeout = 25 * time.Millisecond

	err := f.ctrl.launchInstalled(context.Background(), "exec-1")
	c.Require().Error(err, "a silent remote must fail the launch")
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "a launch failure is a ControllerError, got %v", err)
	c.Eq(protocol.ErrUnavailable, ce.Code, "timeout code")
	c.StrContains(ce.Message, "executor greyshift did not connect within 25ms; ssh said:",
		"the timeout names the launcher and the bound")

	stream := f.launcher.firstStream()
	c.Require().NotNil(stream, "the session was opened")
	var ticket string
	for _, msg := range stream.sentRequests() {
		if stdin := msg.GetStdin(); len(stdin) > 0 {
			ticket = strings.TrimSuffix(string(stdin), "\n")
		}
	}
	c.NotEq("", ticket, "a ticket was minted")
	if _, ok := f.pool.Tickets().Redeem(ticket); ok {
		t.Fatal("the timed-out launch left its ticket redeemable; a late remote could still connect")
	}
	// And the session was ended rather than abandoned.
	c.Eq(0, len(f.cancels()), "the serve context was cancelled and dropped")
}

// A caller that gives up while its launch is waiting ends the launch, and the
// one-shot ticket goes with it: a credential for a session nobody is waiting for
// is one an ssh that finally starts could still redeem.
func TestRelaunchCancelDiscardsTicket(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.state.connectTimeout = time.Minute // the cancel, not the deadline, ends this

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.ctrl.launchInstalled(ctx, "exec-1") }()
	select {
	case <-f.serveEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay session never started")
	}
	cancel()
	select {
	case err := <-done:
		c.ErrorIs(err, context.Canceled, "a cancelled caller learns its own context ended the launch")
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not return after the caller cancelled")
	}

	stream := f.launcher.firstStream()
	c.Require().NotNil(stream, "the session was opened")
	ticket := ""
	for _, msg := range stream.sentRequests() {
		if stdin := msg.GetStdin(); len(stdin) > 0 {
			ticket = strings.TrimSuffix(string(stdin), "\n")
		}
	}
	c.NotEq("", ticket, "a ticket was minted")
	if _, ok := f.pool.Tickets().Redeem(ticket); ok {
		t.Fatal("the abandoned launch left its ticket redeemable")
	}
	c.Eq(0, len(f.cancels()), "the abandoned session was cancelled rather than left running")
}

// The launcher gate runs on the RELAUNCH path too. Its facts are live: an
// operator who adds --no-ssh-launch after an install must not be routed around
// by the next relaunch.
func TestRelaunchRefusesADisabledLauncher(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.pool.live[0].Describe = &executorpb.DescribeResponse{SshLaunch: false}

	err := f.ctrl.launchInstalled(context.Background(), "exec-1")
	expectControllerError(t, err, protocol.ErrFailedPrecondition,
		"executor greyshift has ssh launch disabled (--no-ssh-launch)")
	c.Eq(0, f.launcher.serveCalls(), "a relaunch must not run ssh through a refusing launcher")
}

// ─── Uninstall ──────────────────────────────────────────────────────────────

// Uninstall tombstones the recipe, deletes the executor row and ends the ssh
// session. The recipe stays as the record; the executor row has no tombstone.
func TestUninstallTombstonesAndDeletes(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	served := make(chan struct{})
	var once sync.Once
	f.state.setServe("exec-1", func() { once.Do(func() { close(served) }) })

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")

	select {
	case <-served:
	default:
		t.Fatal("uninstall must end the executor's ssh session")
	}
	c.Eq(0, len(f.installs.liveRows()), "the recipe is tombstoned")
	tomb, ok := f.installs.rowByID("recipe-1")
	c.Require().True(ok, "the recipe row is kept")
	c.NotNil(tomb.RemovedAt, "tombstoned, never deleted")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is deleted")
}

// When the launcher is live the target's install marker is removed through it,
// so the next install of the same machine starts clean.
func TestUninstallCleansMarkerWhenLauncherLive(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "exec-1"), "uninstall")
	c.Eq(1, f.launcher.cleanCalls(), "the marker is removed through the launcher")
	call := f.launcher.cleans[0]
	c.Eq("recipe-1", call.GetInstallId(), "the marker is removed only for THIS install")
	c.Eq("brent@ublk-s2", call.GetTarget().GetDestination(), "the right target")
	c.Eq("work", call.GetTarget().GetInstance(), "the right namespace")
}

// A launcher that is not connected must not make the uninstall fail: the
// executor row is the thing the caller asked to be rid of, and the leftover
// marker is named by the next install's clobber guard.
func TestUninstallSucceedsWhenLauncherDown(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.pool.live = nil

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")
	c.Eq(0, f.launcher.cleanCalls(), "a down launcher cannot clean the marker")
	c.Eq(0, len(f.installs.liveRows()), "the recipe is still tombstoned")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is still deleted")
}

// An agent may uninstall what ITS OWN LINEAGE installed — the row it installed
// itself, or one an ANCESTOR installed. Authority flows down the tree, so the
// installer's parent and the installer's sibling are both refused, with the same
// sentence the policy layer uses for a foreign row.
func TestUninstallChildOutsideLineageRefused(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)

	// c_root → c_kid, c_root → c_sib.
	now := time.Now()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_root", Status: protocol.StatusIdle, StartedAt: now,
		OwnerUserID: installTestOwner,
		Labels:      map[string]string{"owner": installTestOwnerName, childstore.LabelRoot: "c_root"},
	})
	for _, id := range []string{"c_kid", "c_sib"} {
		f.ctrl.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, StartedAt: now,
			OwnerUserID: installTestOwner,
			Labels: map[string]string{
				"owner":                installTestOwnerName,
				childstore.LabelParent: "c_root",
				childstore.LabelRoot:   "c_root",
			},
		})
	}
	caller := func(childID string) installCaller {
		return installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: childID}
	}
	const refused = "executor exec-1 belongs to another user"

	// reseed gives the fixture one installed executor whose installer is named.
	reseed := func(installedBy string) {
		f.execs.execs["exec-1"] = executors.Executor{
			ID: "exec-1", Labels: map[string]string{"machine": "ublk-s2"},
			OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
			WorkspaceMode: "pinned", Roots: []string{"~"},
		}
		f.execs.deleted = nil
		f.installs.mu.Lock()
		f.installs.rows = map[string]executorinstall.Row{}
		f.installs.order = nil
		f.installs.mu.Unlock()
		c.Require().NoError(f.installs.Insert(context.Background(), executorinstall.Row{
			ID: "recipe-1", OwnerUserID: installTestOwner, ExecutorID: "exec-1",
			LauncherExecutorID: installTestLauncher, Target: "brent@ublk-s2",
			Instance: "work", Platform: "linux/amd64", InstalledByChild: installedBy,
		}), "seed recipe installed by %q", installedBy)
	}

	// A SIBLING's install is not the caller's to remove.
	reseed("c_sib")
	expectControllerError(t, f.ctrl.UninstallExecutor(context.Background(), caller("c_kid"), "ublk-s2"),
		protocol.ErrPermissionDenied, refused)
	c.Eq(0, len(f.execs.deleted), "a refused uninstall changes nothing")

	// The installer's PARENT may not remove it either: the walk goes downward,
	// never up.
	reseed("c_kid")
	expectControllerError(t, f.ctrl.UninstallExecutor(context.Background(), caller("c_root"), "ublk-s2"),
		protocol.ErrPermissionDenied, refused)
	c.Eq(0, len(f.execs.deleted), "the installer's parent may not uninstall it")

	// A DESCENDANT of the installer may: the row was provisioned for its subtree.
	reseed("c_root")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller("c_kid"), "ublk-s2"),
		"a descendant of the installer may uninstall what its ancestor installed")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the descendant's uninstall went through")

	// And so may the installer itself.
	reseed("c_kid")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller("c_kid"), "ublk-s2"),
		"the installer may uninstall what it installed")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the installer's uninstall went through")
}

// Uninstall takes the SAME per-(owner, machine) lock Install does, so the two
// serialise instead of interleaving into a launch for a row that is being
// deleted.
func TestUninstallTakesTheInstallLock(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()

	// A lock for a DIFFERENT name must not block it: the fix is one lock per
	// executor, not a global one.
	other := f.state.lockInstall(installTestOwner, "some-other-machine")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"),
		"an unrelated name's lock must not block this uninstall")
	other()

	// The row's own (owner, machine) lock does block it, and releasing the lock
	// lets it finish — which is only true if the uninstall takes that key.
	f.seedInstalledExecutor()
	f.execs.deleted = nil
	release := f.state.lockInstall(installTestOwner, "ublk-s2")
	done := make(chan error, 1)
	go func() { done <- f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2") }()
	select {
	case err := <-done:
		release()
		t.Fatalf("the uninstall did not wait for the (owner, machine) lock: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Still blocked, which is the answer a lock means. A negative check can
		// only be wrong in the direction of passing a buggy tree whose uninstall
		// is slower than this bound; the release below is what pins the positive.
	}
	release()
	select {
	case err := <-done:
		c.NoError(err, "the uninstall completes once the lock is released")
	case <-time.After(5 * time.Second):
		t.Fatal("the uninstall never completed after the lock was released")
	}
}

// ─── The listing ────────────────────────────────────────────────────────────

// An installed row shows its launcher and target in the management listing, and
// a row that was not installed shows neither.
func TestExecutorRowShowsInstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	// The launcher needs a durable row too, so its machine name resolves.
	f.execs.execs[installTestLauncher] = installLauncherLive(
		installTestLauncher, installTestLauncherN, "work", "pinned").Executor

	rows, err := connectExecutorAdmin{c: f.ctrl}.List(context.Background(), "", 0)
	c.Require().NoError(err, "list")

	byID := map[string]connectapi.ExecutorRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	installed, ok := byID["exec-1"]
	c.Require().True(ok, "the installed row is listed")
	c.Eq(installTestLauncherN, installed.InstalledVia, "installed_via names the launcher machine")
	c.Eq("brent@ublk-s2", installed.InstalledTarget, "installed_target names the ssh destination")

	launcher, ok := byID[installTestLauncher]
	c.Require().True(ok, "the launcher is listed too")
	c.Eq("", launcher.InstalledVia, "a row that was not installed over ssh has no launcher")
	c.Eq("", launcher.InstalledTarget, "…and no target")
}

// A child with one live launcher and one installed executor that is DOWN still
// installs through the live one by default: a down executor is a place to CALL
// (a call relaunches it), never a place to run ssh FROM.
func TestInstallLauncherDefaultIgnoresADownInstalledExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: "c_kid", Status: protocol.StatusIdle, StartedAt: time.Now(),
		OwnerUserID: installTestOwner, Labels: map[string]string{"owner": installTestOwnerName},
	})

	le, err := f.ctrl.resolveInstallLauncher(context.Background(),
		installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: "c_kid"}, "")
	c.Require().NoError(err, "the single LIVE launcher must be the default even though the installed one is reachable")
	c.Eq(installTestLauncher, le.Executor.ID, "launcher")
}
