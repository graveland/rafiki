// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

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
	// modeErr, when set, fails every SetMode: a store that cannot record the
	// transport an install fell back to.
	modeErr error
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
	if s.modeErr != nil {
		return s.modeErr
	}
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

// fakeLaunch is one RemoteLaunch the daemon asked a launcher to perform. The
// ticket is recorded HERE, never in the start frame: the fake keeps the two
// apart so a test can prove the secret only ever travelled as the ticket.
type fakeLaunch struct {
	start  *executorpb.RemoteLaunchStart
	ticket string
}

// fakeLauncher records every Remote* call and answers from scripted results.
type fakeLauncher struct {
	mu           sync.Mutex
	probeResults []*executorpb.RemoteProbeResponse
	probes       []*executorpb.RemoteProbeRequest
	pushes       []fakePush
	serves       []*executorpb.RemoteServeStart
	streams      []*fakeServeStream
	launches     []fakeLaunch
	tails        []*executorpb.RemoteLogTailRequest
	cleans       []*executorpb.RemoteCleanRequest

	probeErr  error
	pushErr   error
	serveErr  error
	launchErr error
	tailErr   error
	cleanErr  error
	// cleanResult, when set, is what Clean answers: a target whose stop was not
	// achieved removes nothing and prints neither word, and that outcome has to
	// be reachable from a test.
	cleanResult *executorpb.RemoteCleanResponse
	// serveExit, when set, makes the remote exit immediately after the stream
	// opens — the shape of a target whose ssh works and whose binary does not.
	serveExit   bool
	serveStderr string
	// launchResp is what Launch answers. Unset means `started pid=4242`.
	launchResp *executorpb.RemoteLaunchResponse
	// tailText is what LogTail answers, and onLaunch runs INSIDE Launch so a
	// test can announce the connection a direct launch is waiting for.
	tailText string
	onLaunch func(*executorpb.RemoteLaunchStart)
	// launchCtxErr is the context error Launch was HANDED, recorded on every
	// call: a launch bound to the caller's lifetime shows up here as a cancelled
	// context, which is what the daemon must never pass.
	launchCtxErr error
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

func (f *fakeLauncher) Launch(
	ctx context.Context, start *executorpb.RemoteLaunchStart, ticket string,
) (*executorpb.RemoteLaunchResponse, error) {
	f.mu.Lock()
	f.launches = append(f.launches, fakeLaunch{start: start, ticket: ticket})
	f.launchCtxErr = ctx.Err()
	resp, launchErr, hook := f.launchResp, f.launchErr, f.onLaunch
	f.mu.Unlock()
	if launchErr != nil {
		return nil, launchErr
	}
	if hook != nil {
		hook(start)
	}
	if resp != nil {
		return resp, nil
	}
	return &executorpb.RemoteLaunchResponse{State: "started", Pid: 4242}, nil
}

func (f *fakeLauncher) LogTail(
	_ context.Context, target *executorpb.RemoteTarget, lines int32,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tails = append(f.tails, &executorpb.RemoteLogTailRequest{Target: target, Lines: lines})
	if f.tailErr != nil {
		return "", f.tailErr
	}
	return f.tailText, nil
}

func (f *fakeLauncher) Clean(
	_ context.Context, target *executorpb.RemoteTarget, installID string, stop bool,
) (removed, stopped bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleans = append(f.cleans, &executorpb.RemoteCleanRequest{
		Target: target, InstallId: installID, StopProcess: stop,
	})
	if f.cleanErr != nil {
		return false, false, f.cleanErr
	}
	if f.cleanResult != nil {
		return f.cleanResult.GetRemoved(), f.cleanResult.GetStopped(), nil
	}
	return true, stop, nil
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

// launchCalls returns how many direct launches were asked for.
func (f *fakeLauncher) launchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches)
}

// launchAt returns the i'th recorded direct launch, if it happened.
func (f *fakeLauncher) launchAt(i int) (fakeLaunch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.launches) {
		return fakeLaunch{}, false
	}
	return f.launches[i], true
}

// launchContextErr returns the context error the last Launch was handed.
func (f *fakeLauncher) launchContextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launchCtxErr
}

// tailCalls returns how many log tails were read.
func (f *fakeLauncher) tailCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tails)
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

// rpcFields renders every string a launcher RPC carried, for the tests that
// prove the ticket is in none of them. A launch's ticket is deliberately NOT
// here: it is kept apart precisely so the scan can show it never became a field.
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
	for _, l := range f.launches {
		out = append(out, l.start.GetBinaryPath(), l.start.GetRoot(), l.start.GetInstallId(),
			l.start.GetTarget().GetDestination(), l.start.GetTarget().GetInstance())
	}
	for _, t := range f.tails {
		out = append(out, t.GetTarget().GetDestination(), t.GetTarget().GetInstance())
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

// installReqMode is installReq with the transport the caller asked for.
func installReqMode(via, mode string) *rafikiv1.InstallExecutorRequest {
	return &rafikiv1.InstallExecutorRequest{Target: "brent@ublk-s2", Via: via, Mode: mode}
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
	f.seedInstalledExecutorMode("")
}

// seedInstalledExecutorMode is seedInstalledExecutor with a recorded transport:
// "" is a recipe written before the column existed, which reads as tunnel.
func (f *installFixture) seedInstalledExecutorMode(mode string) {
	f.execs.execs["exec-1"] = executors.Executor{
		ID: "exec-1", Labels: map[string]string{"machine": "ublk-s2"},
		OwnerUserID: installTestOwner, Enabled: true, Isolation: "none",
		WorkspaceMode: "pinned", Roots: []string{"~"},
	}
	if err := f.installs.Insert(context.Background(), executorinstall.Row{
		ID: "recipe-1", OwnerUserID: installTestOwner, ExecutorID: "exec-1",
		LauncherExecutorID: installTestLauncher, Target: "brent@ublk-s2",
		Instance: "work", Platform: "linux/amd64", Mode: mode,
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
	c.StrContains(ce.Message, "executor ublk-s2 on brent@ublk-s2 (via greyshift) did not connect within 25ms; ssh said:",
		"the timeout names the executor being launched, the target and the launcher")

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

// directConsequence is the sentence a DIRECT install's uninstall must say when
// it could not stop the remote process. Written out here rather than reusing the
// code's own constant: a test that imported it would pass whatever that constant
// happened to say, which is no guard on the promise made to an operator.
const directConsequence = "the remote executor is still running; it stops itself the next time it reconnects, " +
	"because its executor row is deleted and the daemon answers 401"

// logLineContaining returns the first captured log line containing needle, or ""
// when no line does. captureLogs installs the text handler, so a line carries
// its own level as `level=WARN`.
func logLineContaining(logs, needle string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

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
//
// This row is a TUNNEL one, whose remote process is the launcher's own ssh
// child and died with the session above, so the line is still Debug and says
// nothing about a process left running.
func TestUninstallSucceedsWhenLauncherDown(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutor()
	f.pool.live = nil
	logs := captureLogs(t)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")
	c.Eq(0, f.launcher.cleanCalls(), "a down launcher cannot clean the marker")
	c.Eq(0, len(f.installs.liveRows()), "the recipe is still tombstoned")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is still deleted")

	line := logLineContaining(logs.String(), "uninstall: launcher is not connected")
	c.Require().NotEq("", line, "the skipped cleanup is still recorded; log:\n"+logs.String())
	c.StrContains(line, "level=DEBUG", "a tunnel row's leftover marker stays a debug line: "+line)
	c.NotStrContains(logs.String(), directConsequence,
		"a tunnel row's process is the launcher's ssh child; nothing is left running to name")
}

// A DIRECT install's process is detached, so uninstall must ask the launcher to
// stop it — and for THIS install's id, since the launcher stops a process only
// when the target's lock records the install that owns it.
func TestUninstallDirectAsksTheLauncherToStopTheProcess(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")

	c.Eq(1, f.launcher.cleanCalls(), "the target is cleaned once")
	call := f.launcher.cleans[0]
	c.True(call.GetStopProcess(), "a direct install must ask the launcher to stop the detached process")
	c.Eq("recipe-1", call.GetInstallId(), "the stop is matched to THIS install's id")
	c.Eq("brent@ublk-s2", call.GetTarget().GetDestination(), "the right target")
	c.Eq("work", call.GetTarget().GetInstance(), "the right namespace")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the row is still deleted")
}

// installEvictPool is the fixture's fake pool with the real pool's one rule this
// test needs to observe — an evicted executor leaves the live list at once, so
// "still live after an uninstall" is a fact rather than an inference — plus a
// record of how many executor rows were already deleted when each cut happened,
// which is what pins the ORDER.
type installEvictPool struct {
	*fakePool
	store *installExecStore
	mu    sync.Mutex
	gone  []string
	rows  []int
}

func (p *installEvictPool) Evict(id string) {
	p.mu.Lock()
	p.gone = append(p.gone, id)
	p.rows = append(p.rows, len(p.store.deleted))
	p.mu.Unlock()
	p.fakePool.Evict(id)
}

// evictedIDs returns the ids Evict was called for, in order.
func (p *installEvictPool) evictedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.gone...)
}

// deletedBeforeCuts returns, per cut, how many rows the executor store had
// already deleted: 1 means every cut happened after its row went.
func (p *installEvictPool) deletedBeforeCuts() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.rows...)
}

// liveIDs is the pool's live list minus what was evicted.
func (p *installEvictPool) liveIDs() []string {
	gone := map[string]bool{}
	for _, id := range p.evictedIDs() {
		gone[id] = true
	}
	var out []string
	for _, le := range p.Live() {
		if !gone[le.Executor.ID] {
			out = append(out, le.Executor.ID)
		}
	}
	return out
}

// An uninstall cuts the executor's connection ITSELF. Waiting for the pool's next
// row refresh (about 30s) leaves the daemon accepting work for a machine the
// caller has just uninstalled, on a row that no longer exists.
func TestUninstallEvictsTheExecutorImmediately(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	pool := &installEvictPool{fakePool: f.pool, store: f.execs}
	f.ctrl.execPool = pool

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")

	c.EqDeep([]string{"exec-1"}, pool.evictedIDs(), "the deleted executor's connection is cut at once")
	c.EqDeep([]string{installTestLauncher}, pool.liveIDs(),
		"the uninstalled executor is gone from the pool and the launcher it went through stays")
	c.EqDeep([]int{1}, pool.deletedBeforeCuts(),
		"the row is deleted BEFORE the cut: a reconnect meets a 401, not an admission")
}

// A controller with no executor listener has no pool, and an uninstall must not
// panic on it — there is nothing connected to cut.
func TestUninstallWithoutAPoolStillRemovesTheRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.ctrl.execPool = nil
	f.ctrl.execPoolConn = nil

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the row is deleted with no pool to cut")
	c.Eq(0, len(f.installs.liveRows()), "the recipe is still tombstoned")
}

// A stop the target could not ACHIEVE removes nothing there, the marker
// included (remoteStopScript), and the row is still deleted — that deletion is
// what ends the process. The response carries no warning field, so the daemon's
// Warn is the only place that says the process may still be running; see the
// RULING above cleanInstall.
func TestUninstallDirectUnachievedStopWarnsAndRemovesTheRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.launcher.cleanResult = &executorpb.RemoteCleanResponse{}
	logs := captureLogs(t)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"),
		"an unachieved stop must not fail the uninstall")
	c.Eq(1, f.launcher.cleanCalls(), "the launcher was asked to stop the process")
	tomb, ok := f.installs.rowByID("recipe-1")
	c.Require().True(ok, "the recipe row is kept")
	c.NotNil(tomb.RemovedAt, "the recipe is still tombstoned")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is still deleted")

	line := logLineContaining(logs.String(), "could not confirm the detached executor stopped")
	c.Require().NotEq("", line, "the unachieved stop must be reported; log:\n"+logs.String())
	c.StrContains(line, "level=WARN", "a direct row's unachieved stop is a warning: "+line)
	c.StrContains(line, "brent@ublk-s2", "the warning names the target: "+line)
	c.StrContains(line, directConsequence, "the warning must name the consequence: "+line)
	c.Eq(1, strings.Count(logs.String(), directConsequence),
		"the consequence is said once per uninstall, not once per line; log:\n"+logs.String())
	c.NotStrContains(logs.String(), "uninstall: cleaned the target",
		"a stop that was not achieved is not a clean uninstall; log:\n"+logs.String())
}

// an uninstall drops the entry: otherwise a daemon that installs and uninstalls
// all day holds one dead entry per install, and a listing's decoration map grows
// with a history nothing can render.
func TestUninstallForgetsTheLaunchOutcome(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.ctrl.install.setOutcome("exec-1", "ok: direct launch started in 40ms")

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")

	c.Eq("", f.ctrl.install.outcomeOf("exec-1"),
		"the deleted executor's outcome must be dropped, not left for a row that no longer exists")
}

// A TUNNEL install has nothing to stop: its remote process is the launcher's own
// ssh child, ended with the session, so the uninstall asks for the marker only.
func TestUninstallTunnelDoesNotAskToStop(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeTunnel)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")

	c.Eq(1, f.launcher.cleanCalls(), "the marker is removed through the launcher")
	c.False(f.launcher.cleans[0].GetStopProcess(),
		"a tunnel install must not ask the launcher to stop a process it does not own")
}

// A launcher the daemon cannot reach does not block a direct uninstall — the
// row is deleted and the recipe tombstoned regardless — but it must say what it
// could not do: the remote process keeps running until its next connection is
// refused.
func TestUninstallDirectWithUnreachableLauncherStillRemovesTheRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.pool.live = nil
	logs := captureLogs(t)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"),
		"an unreachable launcher must not fail the uninstall")
	c.Eq(0, f.launcher.cleanCalls(), "a down launcher cannot stop the remote process")
	tomb, ok := f.installs.rowByID("recipe-1")
	c.Require().True(ok, "the recipe row is kept")
	c.NotNil(tomb.RemovedAt, "the recipe is still tombstoned")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is still deleted")

	line := logLineContaining(logs.String(), "uninstall: launcher is not connected")
	c.Require().NotEq("", line, "the uninstall must say what it could not do; log:\n"+logs.String())
	c.StrContains(line, "level=WARN", "a direct row's leftover process is a warning, not debug: "+line)
	c.StrContains(line, directConsequence, "the warning must name the consequence: "+line)
	c.Eq(1, strings.Count(logs.String(), directConsequence),
		"the consequence is said once per uninstall, not once per line; log:\n"+logs.String())
}

// A Clean that fails is the same shape: the launcher was reached but could not
// do the work, so uninstall still removes the row and the warning still names
// the process left running.
func TestUninstallDirectCleanErrorStillRemovesTheRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.launcher.cleanErr = errors.New("ssh: connect to host ublk-s2 port 22: connection refused")
	logs := captureLogs(t)

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"),
		"a failed stop must not fail the uninstall")
	c.Eq(1, f.launcher.cleanCalls(), "the launcher was asked")
	tomb, ok := f.installs.rowByID("recipe-1")
	c.Require().True(ok, "the recipe row is kept")
	c.NotNil(tomb.RemovedAt, "the recipe is still tombstoned")
	c.EqDeep([]string{"exec-1"}, f.execs.deleted, "the executor row is still deleted")

	line := logLineContaining(logs.String(), "uninstall: could not remove the target's install marker")
	c.Require().NotEq("", line, "the failed cleanup must be reported; log:\n"+logs.String())
	c.StrContains(line, "level=WARN", "a direct row's failed stop is a warning: "+line)
	c.StrContains(line, directConsequence, "the warning must name the consequence: "+line)
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

// ─── Mode choice and the direct launch ──────────────────────────────────────

// TestInstallChooseInstallMode runs the pinned mode-choice table under a name the
// install tests' verify pattern reaches; the table itself is
// TestChooseInstallMode.
func TestInstallChooseInstallMode(t *testing.T) {
	t.Run("TestChooseInstallMode", TestChooseInstallMode)
}

// The mode is one decision, made once, from what the operator asked for and what
// the target proved it can do. Every cell of that table is pinned here, because
// the refusal text, the logged reason and the STORED mode all come from this one
// function.
func TestChooseInstallMode(t *testing.T) {
	c := assert.NewAborting(t)

	const reachWhy = "target reaches the daemon (curl: connected to 10.0.0.5:8035)"
	decided := []struct {
		requested string
		reach     string
		detail    string
		mode      string
		why       string
	}{
		{installModeAuto, "yes", "curl: connected to 10.0.0.5:8035", executorinstall.ModeDirect, reachWhy},
		{"", "yes", "curl: connected to 10.0.0.5:8035", executorinstall.ModeDirect, reachWhy},
		{installModeAuto, "no", "curl: connection refused", executorinstall.ModeTunnel,
			"target cannot reach the daemon: no (curl: connection refused)"},
		{installModeAuto, "unavailable", "", executorinstall.ModeTunnel,
			"launcher has no dialable address"},
		{"", "", "", executorinstall.ModeTunnel, "launcher has no dialable address"},
		{executorinstall.ModeTunnel, "yes", "curl: connected to 10.0.0.5:8035", executorinstall.ModeTunnel,
			"tunnel requested"},
		{executorinstall.ModeTunnel, "no", "curl: connection refused", executorinstall.ModeTunnel,
			"tunnel requested"},
		{executorinstall.ModeTunnel, "unavailable", "", executorinstall.ModeTunnel, "tunnel requested"},
		{executorinstall.ModeDirect, "yes", "curl: connected to 10.0.0.5:8035", executorinstall.ModeDirect,
			reachWhy},
	}
	for _, tc := range decided {
		mode, why, err := chooseInstallMode(tc.requested, tc.reach, tc.detail)
		c.NoError(err, "chooseInstallMode(%q, %q)", tc.requested, tc.reach)
		c.Eq(tc.mode, mode, "mode for requested=%q reach=%q", tc.requested, tc.reach)
		c.Eq(tc.why, why, "why for requested=%q reach=%q", tc.requested, tc.reach)
	}

	// Forcing direct onto a target that cannot reach the daemon is refused with
	// the probe's own words, so the operator learns what to fix.
	for _, reach := range []string{"no", "unavailable", ""} {
		_, _, err := chooseInstallMode(executorinstall.ModeDirect, reach, "curl: connection refused")
		expectControllerError(t, err, protocol.ErrFailedPrecondition,
			fmt.Sprintf("direct install requires the target to reach the daemon, and it cannot "+
				"(reach=%s: curl: connection refused); use mode=tunnel, or fix the route and retry", reach))
	}

	// An unknown spelling is a caller error, never a silent fallback.
	_, _, err := chooseInstallMode("sideways", "yes", "")
	expectControllerError(t, err, protocol.ErrInvalidArgs, "mode must be auto, direct or tunnel")
}

// An unknown mode is refused BEFORE any ssh round trip: a typo must not look like
// a broken target.
func TestInstallRejectsUnknownModeBeforeAnySSH(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "sideways"))
	expectControllerError(t, err, protocol.ErrInvalidArgs, "mode must be auto, direct or tunnel")
	c.Eq(0, f.launcher.probeCalls(), "an unknown mode runs no ssh")
	c.Eq(0, len(f.execs.creates), "and writes no row")
}

// directConnect is the fixture's "the launched process dialled the daemon"
// announcement: without it a direct launch waits for its timeout, which is what
// the timeout tests want and every success test does not.
func (f *installFixture) directConnect(executorID string) {
	f.launcher.onLaunch = func(*executorpb.RemoteLaunchStart) {
		f.ctrl.installConnected(executorID)
	}
}

// reachableTarget primes both probes with a target that reached the daemon.
func (f *installFixture) reachableTarget() {
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{
		{Uname: "Linux x86_64", Reach: "yes", ReachDetail: "curl: connected to 10.0.0.5:8035"},
		{Uname: "Linux x86_64", Reach: "yes", ReachDetail: "curl: connected to 10.0.0.5:8035"},
	}
}

// probeReach primes BOTH probes with one reach answer: the mode is decided from
// the second (the one that carries the depot's binary path), so a fixture that
// set only one would have the pair disagree about the target.
func (f *installFixture) probeReach(reach, detail string) {
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{
		{Uname: "Linux x86_64", Reach: reach, ReachDetail: detail},
		{Uname: "Linux x86_64", Reach: reach, ReachDetail: detail},
	}
}

// Auto with a target that proved it can dial the daemon installs the
// SELF-DIALLING form and records it: the relay is not opened at all.
func TestInstallAutoChoosesDirectWhenReachYes(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()
	f.directConnect("exec-1")

	row, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "install")

	live := f.installs.liveRows()
	c.Require().Len(live, 1, "the recipe")
	c.Eq(executorinstall.ModeDirect, live[0].Mode, "the recipe records the self-dialling transport")
	c.Eq(1, f.launcher.launchCalls(), "one direct launch")
	c.Eq(0, f.launcher.serveCalls(), "the relay must not be opened as well")
	c.Eq(executorinstall.ModeDirect, row.GetInstallMode(), "the answer names the transport")
	c.StrContains(f.state.outcomeOf("exec-1"), "ok: direct launch started in",
		"the launch outcome is recorded for the listing")
}

// A target that reports it cannot reach the daemon keeps today's relay.
func TestInstallAutoFallsBackToTunnelWhenReachNo(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.probeReach("no", "curl: connection refused")
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "install")

	c.Eq(executorinstall.ModeTunnel, f.installs.liveRows()[0].Mode, "an unreachable target installs the relay")
	c.Eq(1, f.launcher.serveCalls(), "the relay is opened")
	c.Eq(0, f.launcher.launchCalls(), "nothing self-dialling is started")
}

// The same fallback when the launcher has no address to hand a target at all —
// a unix-socket or relay-only launcher.
func TestInstallAutoFallsBackToTunnelWhenReachUnavailable(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.probeReach("unavailable", "")
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "install")
	c.Eq(executorinstall.ModeTunnel, f.installs.liveRows()[0].Mode, "no dialable address means the relay")
	c.Eq(0, f.launcher.launchCalls(), "nothing self-dialling is started")
}

// Forcing direct onto a target that cannot reach the daemon refuses, and says
// what the probe saw.
func TestInstallForcedDirectRefusedWhenUnreachable(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.probeReach("no", "curl: connection refused")

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(),
		installReqMode("", executorinstall.ModeDirect))
	expectControllerError(t, err, protocol.ErrFailedPrecondition,
		"direct install requires the target to reach the daemon, and it cannot "+
			"(reach=no: curl: connection refused); use mode=tunnel, or fix the route and retry")
	c.Eq(0, len(f.execs.creates), "a refused mode writes nothing")
	c.Eq(0, f.launcher.launchCalls(), "and starts nothing")
}

// Forcing tunnel is obeyed even when the target COULD reach the daemon: the
// override is a decision, not a preference.
func TestInstallForcedTunnelIgnoresReach(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(),
		installReqMode("", executorinstall.ModeTunnel))
	c.Require().NoError(err, "install")
	c.Eq(executorinstall.ModeTunnel, f.installs.liveRows()[0].Mode, "tunnel requested, tunnel recorded")
	c.Eq(1, f.launcher.serveCalls(), "the relay is opened")
	c.Eq(0, f.launcher.launchCalls(), "even though the target could have dialled")
}

// A launcher older than the direct-dial RPCs answers CodeUnimplemented to the
// launch. Under auto the install falls back to the relay and records the
// transport that actually worked — the install succeeds, once, as a tunnel.
func TestInstallAutoFallsBackToTunnelOnAnUnimplementedLauncher(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()
	f.launcher.launchErr = connect.NewError(connect.CodeUnimplemented,
		errors.New("unknown service rafiki.executor.v1.ExecutorService"))
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "an old launcher must not fail an auto install")

	c.Eq(1, f.launcher.launchCalls(), "the direct launch was tried")
	c.Eq(1, f.launcher.serveCalls(), "and the install fell back to the relay")
	c.Eq(executorinstall.ModeTunnel, f.installs.liveRows()[0].Mode,
		"the recipe records the transport that worked, not the one that was chosen")
	c.Eq(1, f.installs.modes, "the recipe's mode is REWRITTEN once")
}

// A REQUIRED direct install on the same launcher refuses, naming the machine
// that has to be upgraded: silently relaying would be the very thing the
// operator asked not to happen.
func TestInstallForcedDirectOnAnUnimplementedLauncherNamesTheLauncher(t *testing.T) {
	const refused = "launcher greyshift is too old to start a self-dialling executor; " +
		"upgrade it or use mode=tunnel"

	t.Run("at launch", func(t *testing.T) {
		f := newInstallFixture(t)
		f.reachableTarget()
		f.launcher.launchErr = connect.NewError(connect.CodeUnimplemented,
			errors.New("unknown service rafiki.executor.v1.ExecutorService"))

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(),
			installReqMode("", executorinstall.ModeDirect))
		expectControllerError(t, err, protocol.ErrFailedPrecondition, refused)
		assert.NewAborting(t).Eq(0, f.launcher.serveCalls(),
			"a forced direct install must not quietly relay")
	})

	t.Run("at probe", func(t *testing.T) {
		f := newInstallFixture(t)
		f.launcher.probeErr = connect.NewError(connect.CodeUnimplemented,
			errors.New("unknown service rafiki.executor.v1.ExecutorService"))

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(),
			installReqMode("", executorinstall.ModeDirect))
		expectControllerError(t, err, protocol.ErrFailedPrecondition, refused)
		c := assert.NewAborting(t)
		c.Eq(0, f.launcher.serveCalls(), "a probe that never answered relays nothing")
		c.Eq(0, f.launcher.launchCalls(), "and launches nothing")
		c.Eq(0, len(f.execs.creates), "no row is written without a platform to record")
	})
}

// RULING: under AUTO an unimplemented probe stays the launcher's own failure
// rather than quietly choosing the relay. The probe is what reports the target's
// uname and platform, and nothing can be installed onto a target without them —
// there is no platform to record and no transport that installs anything on its
// own — so "choose tunnel" would only move the same failure later with less said
// about it. The cost is that an old launcher is refused under auto with the
// probe's own words; the operator upgrades it or passes mode=tunnel, which still
// needs this probe, so the refusal is honest rather than cautious.
func TestInstallAutoOnAnUnimplementedProbeIsTheLaunchersFailure(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.launcher.probeErr = connect.NewError(connect.CodeUnimplemented,
		errors.New("unknown service rafiki.executor.v1.ExecutorService"))

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	expectControllerError(t, err, protocol.ErrUnavailable,
		"probe brent@ublk-s2 failed: unknown service rafiki.executor.v1.ExecutorService")
	c.Eq(0, f.launcher.serveCalls(), "a probe that never answered relays nothing")
	c.Eq(0, f.launcher.launchCalls(), "and launches nothing")
	c.Eq(0, len(f.execs.creates), "no row is written without a platform to record")
}

// The ticket of a direct launch is the stream's stdin frame and NOTHING else: it
// appears in no RPC field, it is bound to the executor's row, and redeeming it
// mints the credential the detached process reconnects with.
func TestDirectLaunchTicketOnlyOnStdinAndMintsCredential(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()
	f.directConnect("exec-1")

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "install")

	launch, ok := f.launcher.launchAt(0)
	c.Require().True(ok, "one direct launch")
	c.NotEq("", launch.ticket, "the launch carried a ticket")

	// The start frame is daemon-written and carries the recipe's own facts.
	wantBin := "$HOME/.cache/rafiki/bin/rafiki-" + installTestTag + "-" + f.binSHA[:12]
	c.Eq(wantBin, launch.start.GetBinaryPath(), "launch binary path")
	c.Eq("brent@ublk-s2", launch.start.GetTarget().GetDestination(), "launch destination")
	c.Eq("work", launch.start.GetTarget().GetInstance(), "launch instance")
	c.Eq(f.installs.liveRows()[0].ID, launch.start.GetInstallId(), "the remote records the recipe id")

	for _, field := range f.launcher.rpcFields() {
		c.NotStrContains(field, launch.ticket, "the ticket must not appear in any RPC field")
	}

	// A real one-shot grant, bound to the row, that MINTS a credential: a
	// detached process has no other way to hold one.
	grant, ok := f.pool.Tickets().Redeem(launch.ticket)
	c.Require().True(ok, "the launch carried a minted ticket")
	c.True(grant.MintCredential, "a direct install redeems into a durable credential")
	c.Eq("exec-1", grant.RowID, "the ticket is bound to the executor's row")
	if _, ok := f.pool.Tickets().Redeem(launch.ticket); ok {
		t.Fatal("a ticket redeemed twice")
	}
}

// `running` means the instance was already held: nothing redeemed the ticket, so
// it must be revoked rather than left for a process that will never read it.
func TestDirectLaunchRunningSpendsNoTicket(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()
	f.launcher.launchResp = &executorpb.RemoteLaunchResponse{State: "running", Pid: 77}
	f.directConnect("exec-1")

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "install")

	launch, ok := f.launcher.launchAt(0)
	c.Require().True(ok, "one direct launch")
	if _, ok := f.pool.Tickets().Redeem(launch.ticket); ok {
		t.Fatal("a `running` launch left its ticket redeemable; a late dial could still spend it")
	}
}

// Every direct-launch exit that is not a connection revokes the ticket: one
// minted for a session nobody is awaiting is a credential an ssh that finally
// starts could still redeem. Only the `running` arm was pinned before, so any one
// of these three discards could be deleted with every test still green.
func TestDirectLaunchRevokesTheTicketOnEveryFailedExit(t *testing.T) {
	// revoked asserts the launch's ticket can no longer be spent.
	revoked := func(t *testing.T, f *installFixture) {
		t.Helper()
		c := assert.NewAborting(t)
		launch, ok := f.launcher.launchAt(0)
		c.Require().True(ok, "the direct launch ran")
		if _, ok := f.pool.Tickets().Redeem(launch.ticket); ok {
			t.Fatal("a failed launch left its ticket redeemable; a late dial could still spend it")
		}
	}

	t.Run("the launch RPC failed", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newInstallFixture(t)
		f.reachableTarget()
		f.launcher.launchErr = connect.NewError(connect.CodeUnavailable,
			errors.New("ssh: connect to host brent@ublk-s2 port 22: Connection refused"))

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		c.Require().Error(err, "the launch failed")
		c.Eq(1, f.launcher.launchCalls(), "the launch was attempted")
		revoked(t, f)
	})

	t.Run("the target never connected", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newInstallFixture(t)
		f.reachableTarget()
		f.state.connectTimeout = 25 * time.Millisecond

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		c.Require().Error(err, "the target never dialled back")
		revoked(t, f)
	})

	t.Run("the caller gave up", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newInstallFixture(t)
		f.reachableTarget()
		caller, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		// Cancelled from INSIDE the launch, so the ticket is minted and spent on the
		// wire before the caller gives up.
		f.launcher.onLaunch = func(*executorpb.RemoteLaunchStart) { cancel() }

		_, err := f.ctrl.InstallExecutor(caller, installUserCaller(), installReqMode("", "auto"))
		c.ErrorIs(err, context.Canceled, "the caller's own give-up is what it reads")
		revoked(t, f)
	})
}

// The launch RPC rides the DAEMON's lifetime, never the caller's: a caller that
// gave up did not start this process, and cancelling the ssh mid-flight would
// leave a target whose executor may or may not exist.
func TestDirectLaunchRunsOnTheDaemonsContext(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.reachableTarget()

	caller, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.ctrl.InstallExecutor(caller, installUserCaller(), installReqMode("", "auto"))
	c.ErrorIs(err, context.Canceled, "the caller's own give-up is what it reads, so the launch itself succeeded")
	c.Eq(1, f.launcher.launchCalls(), "the launch still ran")
	c.Nil(f.launcher.launchContextErr(),
		"the launch RPC must be bound to the daemon's context, not the cancelled caller's")
}

// A target that never dials back is diagnosed by its OWN log, and the error names
// the executor and the target — not the launcher alone, which is the machine that
// was working.
func TestDirectLaunchTimeoutErrorNamesTargetAndCarriesLogTail(t *testing.T) {
	const tail = "executor: dial tcp 10.0.0.5:8035: connect: connection refused"

	newTimeout := func(t *testing.T) *installFixture {
		f := newInstallFixture(t)
		f.reachableTarget()
		f.state.connectTimeout = 25 * time.Millisecond
		return f
	}

	t.Run("with a log tail", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newTimeout(t)
		f.launcher.tailText = tail

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		var ce *connectapi.ControllerError
		c.Require().True(errors.As(err, &ce), "a launch failure is a ControllerError, got %v", err)
		c.Eq(protocol.ErrUnavailable, ce.Code, "timeout code")
		c.StrContains(ce.Message,
			"executor ublk-s2 on brent@ublk-s2 (via greyshift) did not connect within 25ms; remote log:",
			"the timeout names the executor, its target and the launcher")
		c.StrContains(ce.Message, tail, "the remote executor's own words ride the error")
		c.Eq(1, f.launcher.tailCalls(), "the log is read exactly once")
		c.Eq(int32(40), f.launcher.tails[0].GetLines(), "40 lines of it")
	})

	t.Run("with no readable log", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newTimeout(t)
		f.launcher.tailErr = connect.NewError(connect.CodeUnavailable,
			errors.New("ssh: connect to host brent@ublk-s2 port 22: refused"))

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		var ce *connectapi.ControllerError
		c.Require().True(errors.As(err, &ce), "a launch failure is a ControllerError, got %v", err)
		c.StrContains(ce.Message, "(log unavailable:", "an unreadable tail is said out loud")
	})
}

// The stored outcome of a failed launch is its FIRST line. A launch error carries
// the remote log tail, and a listing is one row per executor, so the rest must not
// reach it.
func TestLastLaunchRecordsFailureFirstLine(t *testing.T) {
	t.Run("a remote log tail", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newInstallFixture(t)
		f.reachableTarget()
		f.state.connectTimeout = 25 * time.Millisecond
		f.launcher.tailText = "executor: dial tcp 10.0.0.5:8035: connect: connection refused\n" +
			"executor: retrying in 1s\nexecutor: giving up"

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		c.Require().Error(err, "the target never connected")

		outcome := f.state.outcomeOf("exec-1")
		c.Eq("failed: executor ublk-s2 on brent@ublk-s2 (via greyshift) did not connect within 25ms; "+
			"remote log:", outcome, "the outcome is the error's first line, whole")
		c.NotStrContains(outcome, "\n", "a listing row holds one line")
		c.NotStrContains(outcome, "giving up", "the log tail is not in the row")
	})

	t.Run("a launcher's own multi-line refusal", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newInstallFixture(t)
		f.reachableTarget()
		f.launcher.launchErr = connect.NewError(connect.CodePermissionDenied,
			errors.New("Permission denied (publickey).\nHost key verification failed."))

		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
		c.Require().Error(err, "the launcher refused")

		outcome := f.state.outcomeOf("exec-1")
		c.Eq("failed: launch brent@ublk-s2 failed: Permission denied (publickey).",
			outcome, "the outcome is the first line of the launcher's own words")
		c.NotStrContains(outcome, "\n", "a listing row holds one line")
		c.NotStrContains(outcome, "Host key", "the rest of the message stays out of the row")
	})
}

// TestRelaunchRecordsFailureFirstLine runs the pinned first-line table under a
// name the relaunch tests' verify pattern reaches; the table itself is
// TestLastLaunchRecordsFailureFirstLine.
func TestRelaunchRecordsFailureFirstLine(t *testing.T) {
	t.Run("TestLastLaunchRecordsFailureFirstLine", TestLastLaunchRecordsFailureFirstLine)
}

// The relay's timeout names the executor being launched FIRST and the launcher
// only as the machine it went through: naming the launcher told an operator to
// debug the wrong machine.
func TestTunnelTimeoutNamesTheExecutorNotTheLauncher(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeTunnel)
	f.state.connectTimeout = 25 * time.Millisecond

	err := f.ctrl.launchInstalled(context.Background(), "exec-1")
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "a launch failure is a ControllerError, got %v", err)
	c.True(strings.HasPrefix(ce.Message,
		"executor ublk-s2 on brent@ublk-s2 (via greyshift) did not connect within 25ms; ssh said:"),
		"the launched executor is named first and the launcher only as `via`: %s", ce.Message)
}

// A reinstall onto THIS row's own credential is not a collision: the executor.cred
// beside the marker was put there by the launch this install is reinstalling.
func TestReinstallOwnDirectCredentialIsNotAClobber(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.launcher.probeResults = []*executorpb.RemoteProbeResponse{
		{Uname: "Linux x86_64", CredentialPresent: true, Marker: "recipe-1", BinaryPresent: true},
		{Uname: "Linux x86_64", CredentialPresent: true, Marker: "recipe-1", BinaryPresent: true, Reach: "yes"},
	}
	f.directConnect("exec-1")

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "a reinstall over its own credential must proceed")
	c.Eq(0, len(f.execs.creates), "the same executor row is reused")
	c.Eq(1, f.launcher.launchCalls(), "and the direct install runs again")
}

// A credential that is NOT this install's is still a collision — on a first
// install, and on a reinstall whose marker names another install.
func TestForeignCredentialStillRefused(t *testing.T) {
	const refused = "ublk-s2 already has a durable executor " +
		"(${XDG_DATA_HOME:-$HOME/.local/share}/rafiki/profiles/work/executor.cred); " +
		"use it, or remove that file first"

	t.Run("first install", func(t *testing.T) {
		f := newInstallFixture(t)
		f.launcher.probeResults = []*executorpb.RemoteProbeResponse{{
			Uname: "Linux x86_64", CredentialPresent: true,
		}}
		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
		expectControllerError(t, err, protocol.ErrAlreadyExists, refused)
	})

	t.Run("reinstall beside another install's marker", func(t *testing.T) {
		f := newInstallFixture(t)
		f.seedInstalledExecutorMode(executorinstall.ModeTunnel)
		f.launcher.probeResults = []*executorpb.RemoteProbeResponse{
			{Uname: "Linux x86_64", CredentialPresent: true, Marker: "0193-other-install"},
			{Uname: "Linux x86_64", CredentialPresent: true, Marker: "0193-other-install"},
		}
		_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReq(""))
		expectControllerError(t, err, protocol.ErrAlreadyExists, refused)
		assert.NewAborting(t).Eq(0, len(f.execs.creates), "nothing is written over a foreign credential")
	})
}

// A reinstall that changes the mode REWRITES the recipe, so the next relaunch —
// possibly after a daemon restart, with no memory of this call — drives the new
// transport.
func TestReinstallChangingModeUpdatesTheRecipe(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.probeReach("no", "curl: connection refused")
	f.connectAs = "exec-1"

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(),
		installReqMode("", executorinstall.ModeTunnel))
	c.Require().NoError(err, "reinstall")

	c.Eq(executorinstall.ModeTunnel, f.installs.liveRows()[0].Mode, "the recipe now records the relay")
	c.Eq(1, f.installs.modes, "SetMode is called once, not on every reinstall")
	c.Eq(1, f.launcher.serveCalls(), "and the relay is what ran")
}

// A reinstall whose fallback cannot be RECORDED still falls back. The launch is
// what the caller asked for; recording it is what the next relaunch reads, so an
// unwritable mode is a warning naming the consequence, never a refusal of the
// relay that just worked.
func TestReinstallFallbackSurvivesAnUnrecordableMode(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	f.reachableTarget()
	f.launcher.launchErr = connect.NewError(connect.CodeUnimplemented,
		errors.New("unknown service rafiki.executor.v1.ExecutorService"))
	f.installs.modeErr = errors.New("conversations.executor_install: i/o timeout")
	f.connectAs = "exec-1"

	prev := slog.Default()
	logs := &capturingHandler{}
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, err := f.ctrl.InstallExecutor(context.Background(), installUserCaller(), installReqMode("", "auto"))
	c.Require().NoError(err, "an unrecordable fallback must not deny the launch the caller asked for")
	c.Eq(1, f.launcher.launchCalls(), "the direct launch was tried")
	c.Eq(1, f.launcher.serveCalls(), "and the relay ran anyway")
	c.Eq(executorinstall.ModeDirect, f.installs.liveRows()[0].Mode,
		"the recipe still says direct, which is exactly what the warning is about")
	c.StrContains(logs.String(), "could not record the relay transport",
		"the consequence must be said out loud")
}

// A recipe written before the mode column existed carries no mode, and that reads
// as tunnel — which is exactly how it behaved.
func TestRecipeWithoutModeReadsAsTunnel(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode("")
	f.connectAs = "exec-1"

	c.Require().NoError(f.ctrl.launchInstalled(context.Background(), "exec-1"), "relaunch")
	c.Eq(1, f.launcher.serveCalls(), "an empty mode relaunches through the relay")
	c.Eq(0, f.launcher.launchCalls(), "and never starts a detached process")
}

// A relaunch reads the STORED mode: a direct recipe dials again, and never opens
// the relay.
func TestRelaunchDirectUsesLaunchNotServe(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	launched := make(chan struct{})
	var once sync.Once
	f.launcher.onLaunch = func(*executorpb.RemoteLaunchStart) {
		once.Do(func() { close(launched) })
	}

	done := make(chan error, 1)
	go func() { done <- f.ctrl.launchInstalled(context.Background(), "exec-1") }()
	select {
	case <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("the direct launch never ran")
	}
	c.Eq(0, f.launcher.serveCalls(), "a direct relaunch opens no ssh relay")
	f.ctrl.installConnected("exec-1")
	select {
	case err := <-done:
		c.NoError(err, "the connect announcement completes the launch")
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not complete on the connect announcement")
	}
	c.Eq(1, f.launcher.launchCalls(), "one direct launch")
	c.StrContains(f.state.outcomeOf("exec-1"), "ok: direct launch started in",
		"the outcome names the transport and the state")
}

// Two concurrent relaunches of a direct executor cost ONE launch, exactly as they
// cost one ssh session in the relay: the flight is keyed by executor id, not by
// transport.
//
// The followers have LIVE contexts and must come back with the LEADER's outcome.
// That is what makes the coalescing observable: a caller that gave up on its own
// context would return before the leader finished and could never see it started a
// second launch. Every wait here is bounded, so dropping the flight fails the test
// rather than hanging it.
func TestRelaunchCoalescesDirectLaunches(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	f.seedInstalledExecutorMode(executorinstall.ModeDirect)
	// The leader FAILS rather than connecting, so it HAS an outcome to share.
	f.state.connectTimeout = 25 * time.Millisecond

	// The leader is held INSIDE the launch until the followers have all arrived,
	// so "they joined" is a fact rather than a scheduling hope.
	launched := make(chan struct{})
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	f.launcher.onLaunch = func(*executorpb.RemoteLaunchStart) {
		once.Do(func() { close(launched) })
		<-release
	}

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- f.ctrl.launchInstalled(context.Background(), "exec-1") }()
	select {
	case <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never ran its launch")
	}

	followerErrs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered <- struct{}{}
			followerErrs <- f.ctrl.launchInstalled(context.Background(), "exec-1")
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("a follower never started")
		}
	}

	// Released only once every follower is running, and collected through a
	// bounded wait: a follower that ignored the flight would block inside the fake
	// launch and be reported here instead of hanging the test.
	close(release)
	followersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(followersDone)
	}()
	select {
	case <-followersDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the followers never returned: the launch flight published no outcome")
	}
	close(followerErrs)

	var leaderErr error
	select {
	case leaderErr = <-leaderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never returned")
	}
	c.Require().Error(leaderErr, "the leader's launch never connected, so it failed")

	c.Eq(1, f.launcher.launchCalls(), "five callers must cost ONE direct launch")
	for err := range followerErrs {
		// The outcome INSTANCE, not its text: a follower that led a launch of its
		// own would carry an identical-looking error of its own making.
		c.True(err == leaderErr, "a coalesced caller returns the leader's own outcome, got %v", err)
	}
}
