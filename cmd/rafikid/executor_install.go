// SPDX-License-Identifier: Apache-2.0

package main

// The install orchestrator: putting an executor on an ssh target through a
// launcher executor, relaunching it, and taking it away again.
//
// Three rules govern everything here.
//
//   - The DAEMON never runs ssh. A launcher executor it already trusts does,
//     through the Remote* RPCs, and the launcher is trusted with the target's
//     traffic exactly as far as it is already trusted with the ssh keys to it.
//   - The remote runs exactly the DAEMON's version: the daemon is the epoch
//     authority (protocol.Epoch), so the binary comes from this process's own
//     depot (executor_depot.go) and nowhere else.
//   - The remote's credential is a ONE-SHOT TICKET bound to its durable row,
//     minted per launch and written as the first line of the relayed stdin. It
//     is never stored on the target, never in an argv, and in no RPC field but
//     the byte stream it rides.
//
// Nothing the target reports gates access. isolation, workspace_mode, roots,
// admits and owner are written by the daemon at Create; the probe's answer picks
// a binary and refuses a clobber, and decides nothing about who may use the
// result.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
)

const (
	// installConnectTimeout bounds the wait for an installed executor to
	// connect after its ssh session starts. Long enough for a cold machine to
	// boot the remote process, short enough that a caller learns a broken
	// target inside one tool call.
	installConnectTimeout = 30 * time.Second

	// installedViaLabel and installedTargetLabel name, for humans and
	// selectors, the launcher machine and the ssh destination an installed
	// executor came from. Daemon-written: a caller may not set them.
	installedViaLabel    = "rafiki/installed-via"
	installedTargetLabel = "rafiki/installed-target"

	// executorBindingLabel is where a child records the executor its workspace
	// lives on (controllerBinder.NoteBinding). For a daraja-hosted claude child
	// it is the launch host, which is that child's only candidate launcher.
	executorBindingLabel = "rafiki/executor"

	// installBinaryDir is the content-addressed binary cache on the target,
	// shared by every instance and every daemon that installs there.
	installBinaryDir = "$HOME/.cache/rafiki/bin"

	// installRollbackTimeout bounds the cleanup of a half-finished install,
	// which must run on its own context: the caller's is very likely the reason
	// the install failed.
	installRollbackTimeout = 15 * time.Second
)

// installCaller is WHO asked for an install or uninstall, resolved by the
// binding from the connection's identity — never from a request field.
//
// OwnerUserID is the owner every row this caller creates belongs to ("" for the
// unix socket's unattributed caller, exactly like a child's own OwnerUserID).
// OwnerName is the display name stamped into the `owner` label. ChildID names
// the child credential that asked — "" for a human — and is what bounds an
// uninstall to the installer's own lineage.
type installCaller struct {
	OwnerUserID string
	OwnerName   string
	ChildID     string
}

// installLauncher is one launcher executor's ssh capability, as the installer
// uses it. Production wires launcherRemote over the pool's generated
// ExecutorService client; a test wires a fake and records every request.
//
// The narrow shape is deliberate: the generated client is dozens of methods
// wide, and a fake of the whole thing would be a wall of stubs that says nothing
// about what the installer actually asks a launcher to do.
type installLauncher interface {
	Probe(ctx context.Context, target *executorpb.RemoteTarget, binaryPath string) (*executorpb.RemoteProbeResponse, error)
	Push(ctx context.Context, target *executorpb.RemoteTarget, binaryPath, sha256 string, src io.Reader) error
	Serve(ctx context.Context, start *executorpb.RemoteServeStart) (remoteServeStream, error)
	// Launch starts a detached, self-dialling executor. ticket is a direct
	// install's one secret, and it travels as the launch stream's stdin frame —
	// never as a field of start.
	Launch(ctx context.Context, start *executorpb.RemoteLaunchStart, ticket string) (*executorpb.RemoteLaunchResponse, error)
	// LogTail reads the last lines of the target's executor.log, which is the
	// only diagnostic a detached process leaves behind.
	LogTail(ctx context.Context, target *executorpb.RemoteTarget, lines int32) (string, error)
	Clean(ctx context.Context, target *executorpb.RemoteTarget, installID string, stop bool) (removed, stopped bool, err error)
}

// remoteServeStream is one RemoteServe bidi stream as the daemon uses it. The
// generated *connect.BidiStreamForClient satisfies it; so does a fake.
type remoteServeStream interface {
	Send(*executorpb.RemoteServeRequest) error
	Receive() (*executorpb.RemoteServeResponse, error)
	CloseRequest() error
	CloseResponse() error
}

// launcherRemote adapts one launcher's generated ExecutorService client.
type launcherRemote struct {
	cl executorpbconnect.ExecutorServiceClient
}

// streamSendErr turns a failed Send into the error that actually says why.
//
// connect's client stream answers a Send with an error wrapping io.EOF once the
// server has ended the stream, and the real status arrives only from
// CloseAndReceive. Returning the Send error as-is would turn a launcher's
// CodeUnimplemented refusal into CodeUnknown, and that code is what decides
// between falling back to the relay and refusing the install as too old — so the
// status is read off the stream the failed send came from.
func streamSendErr[Req, Res any](
	stream *connect.ClientStreamForClient[Req, Res], err error,
) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if _, cerr := stream.CloseAndReceive(); cerr != nil {
		return cerr
	}
	return err
}

// bidiSendErr is streamSendErr for a bidi stream, whose status arrives from the
// next Receive rather than from a CloseAndReceive.
func bidiSendErr[Req, Res any](stream *connect.BidiStreamForClient[Req, Res], err error) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if _, rerr := stream.Receive(); rerr != nil {
		return rerr
	}
	return err
}

func (l launcherRemote) Probe(
	ctx context.Context, target *executorpb.RemoteTarget, binaryPath string,
) (*executorpb.RemoteProbeResponse, error) {
	resp, err := l.cl.RemoteProbe(ctx, connect.NewRequest(&executorpb.RemoteProbeRequest{
		Target:     target,
		BinaryPath: binaryPath,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Push streams src to the target: the header first, then the bytes, which the
// remote verifies against sha256 before its final rename.
func (l launcherRemote) Push(
	ctx context.Context, target *executorpb.RemoteTarget, binaryPath, sha256 string, src io.Reader,
) error {
	stream := l.cl.RemoteInstall(ctx)
	if err := stream.Send(&executorpb.RemoteInstallRequest{
		Msg: &executorpb.RemoteInstallRequest_Header{Header: &executorpb.RemoteInstallHeader{
			Target: target, BinaryPath: binaryPath, Sha256: sha256,
		}},
	}); err != nil {
		return streamSendErr(stream, err)
	}
	buf := make([]byte, 64<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if err := stream.Send(&executorpb.RemoteInstallRequest{
				Msg: &executorpb.RemoteInstallRequest_Chunk{Chunk: chunk},
			}); err != nil {
				return streamSendErr(stream, err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	_, err := stream.CloseAndReceive()
	return err
}

// Serve opens the relay stream and sends its start frame. The stream is bound
// to ctx, which is the DAEMON's lifetime for a launch — never the caller's.
func (l launcherRemote) Serve(ctx context.Context, start *executorpb.RemoteServeStart) (remoteServeStream, error) {
	stream := l.cl.RemoteServe(ctx)
	if err := stream.Send(&executorpb.RemoteServeRequest{
		Msg: &executorpb.RemoteServeRequest_Start{Start: start},
	}); err != nil {
		return nil, bidiSendErr(stream, err)
	}
	return stream, nil
}

// Launch opens the client-streaming launch, sends the start frame and then the
// ticket as the stream's only stdin frame, and half-closes before reading the
// answer.
func (l launcherRemote) Launch(
	ctx context.Context, start *executorpb.RemoteLaunchStart, ticket string,
) (*executorpb.RemoteLaunchResponse, error) {
	stream := l.cl.RemoteLaunch(ctx)
	if err := stream.Send(&executorpb.RemoteLaunchRequest{
		Msg: &executorpb.RemoteLaunchRequest_Start{Start: start},
	}); err != nil {
		return nil, streamSendErr(stream, err)
	}
	if err := stream.Send(&executorpb.RemoteLaunchRequest{
		Msg: &executorpb.RemoteLaunchRequest_Stdin{Stdin: []byte(ticket + "\n")},
	}); err != nil {
		return nil, streamSendErr(stream, err)
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (l launcherRemote) LogTail(
	ctx context.Context, target *executorpb.RemoteTarget, lines int32,
) (string, error) {
	resp, err := l.cl.RemoteLogTail(ctx, connect.NewRequest(&executorpb.RemoteLogTailRequest{
		Target: target, Lines: lines,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetTail(), nil
}

func (l launcherRemote) Clean(
	ctx context.Context, target *executorpb.RemoteTarget, installID string, stop bool,
) (removed, stopped bool, err error) {
	resp, err := l.cl.RemoteClean(ctx, connect.NewRequest(&executorpb.RemoteCleanRequest{
		Target: target, InstallId: installID, StopProcess: stop,
	}))
	if err != nil {
		return false, false, err
	}
	return resp.Msg.GetRemoved(), resp.Msg.GetStopped(), nil
}

// installState is the orchestrator's state and its seams. One Controller field
// (c.install) holds it, so a test builds it with fakes and the Controller grows
// no half-dozen fields for one feature.
type installState struct {
	// store is conversations.executor_install: the launch RECIPE per executor.
	store executorinstall.Store
	// depot is this daemon's own binary, resolved once at construction.
	depot *executorDepot
	// launcher resolves one executor's ssh capability. Production is the pool's
	// ExecutorService client for that executor.
	launcher func(executorID string) (installLauncher, error)
	// serve runs ONE tunnelled executor connection through the pool's ordinary
	// upgrade handler. Production is (*execpool.Pool).ServeConn.
	serve func(ctx context.Context, conn net.Conn) error
	// connectTimeout bounds launchInstalled's wait; a test shortens it rather
	// than sleeping 30s.
	connectTimeout time.Duration

	mu       sync.Mutex
	locks    map[string]*installLock       // per (owner, machine)
	flights  map[string]*launchFlight      // per executor id, coalescing launches
	serves   map[string]context.CancelFunc // per executor id, the live ssh session
	waiters  map[string][]chan struct{}    // per executor id, one-shot connect waiters
	outcomes map[string]string             // per executor id, the last launch's outcome
}

func newInstallState(store executorinstall.Store, depot *executorDepot) *installState {
	return &installState{
		store:          store,
		depot:          depot,
		connectTimeout: installConnectTimeout,
		locks:          map[string]*installLock{},
		flights:        map[string]*launchFlight{},
		serves:         map[string]context.CancelFunc{},
		waiters:        map[string][]chan struct{}{},
		outcomes:       map[string]string{},
	}
}

// installLock is one (owner, machine) critical section. refs is what lets the
// map shrink again: a daemon lives for months, and an install name is caller
// input.
type installLock struct {
	mu   sync.Mutex
	refs int
}

// lockInstall takes the (owner, machine) lock and returns its release. Two
// concurrent installs of the same name would otherwise both see "no row" and
// both Create, and the second would lose to the (owner, machine) unique index
// after an ssh round trip.
func (s *installState) lockInstall(owner, name string) func() {
	key := owner + "\x00" + name
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &installLock{}
		s.locks[key] = l
	}
	l.refs++
	s.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
}

// launchFlight is one in-progress launch. Concurrent callers wait on done and
// share err, so eight calls to a down executor cost ONE ssh session.
type launchFlight struct {
	done chan struct{}
	err  error
}

// beginLaunch returns the flight for executorID and whether this caller is its
// leader.
func (s *installState) beginLaunch(executorID string) (*launchFlight, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.flights[executorID]; ok {
		return f, false
	}
	f := &launchFlight{done: make(chan struct{})}
	s.flights[executorID] = f
	return f, true
}

// finishLaunch publishes the leader's outcome. The flight leaves the map before
// done closes, so a caller arriving afterwards starts a fresh attempt rather
// than reading a stale answer.
func (s *installState) finishLaunch(executorID string, f *launchFlight, err error) {
	s.mu.Lock()
	if s.flights[executorID] == f {
		delete(s.flights, executorID)
	}
	f.err = err
	s.mu.Unlock()
	close(f.done)
}

// setServe records the live ssh session's cancel func, ending any previous one
// for the same executor: a relaunch must not leave two sessions competing for
// one row.
func (s *installState) setServe(executorID string, cancel context.CancelFunc) {
	s.mu.Lock()
	prev := s.serves[executorID]
	s.serves[executorID] = cancel
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
}

// cancelServe ends the ssh session for executorID, if one is open. This is what
// kills the remote process: an idle stream does not observe a cancelled context
// until a read or write, so the session is CLOSED rather than merely abandoned.
func (s *installState) cancelServe(executorID string) {
	s.mu.Lock()
	cancel := s.serves[executorID]
	delete(s.serves, executorID)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// watchConnect registers a one-shot waiter for executorID's next connection.
func (s *installState) watchConnect(executorID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.waiters[executorID] = append(s.waiters[executorID], ch)
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.waiters[executorID]
		for i, c := range list {
			if c == ch {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(s.waiters, executorID)
		} else {
			s.waiters[executorID] = list
		}
	}
}

// notifyConnected fires every waiter registered for executorID. The pool's
// on-connect hook calls it, which is what makes a launch wait for the connection
// ARRIVING instead of polling for it.
func (s *installState) notifyConnected(executorID string) {
	s.mu.Lock()
	list := s.waiters[executorID]
	delete(s.waiters, executorID)
	s.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}

// installConnected is the pool's on-connect hook's installer half. Nil-safe: a
// daemon without an installer has no waiters.
func (c *Controller) installConnected(executorID string) {
	if c.install != nil {
		c.install.notifyConnected(executorID)
	}
}

// setOutcome records the last launch outcome for executorID, which is what a
// listing shows as `last launch`. In-memory on purpose: it describes what THIS
// daemon process did, and a daemon that never launched the executor has no
// outcome to report rather than a stale one.
func (s *installState) setOutcome(executorID, outcome string) {
	s.mu.Lock()
	s.outcomes[executorID] = outcome
	s.mu.Unlock()
}

// forgetOutcome drops an executor's recorded launch outcome. Called when the
// executor's row goes away: the map is keyed by executor id, which is never
// reused, so without this a daemon that uninstalls and reinstalls all day keeps
// one dead entry per install.
func (s *installState) forgetOutcome(executorID string) {
	s.mu.Lock()
	delete(s.outcomes, executorID)
	s.mu.Unlock()
}

// outcomeOf returns the recorded last launch outcome for executorID, or "".
func (s *installState) outcomeOf(executorID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcomes[executorID]
}

// recordLaunchOutcome writes the outcome text a listing shows. The failure arm
// takes only the FIRST line: a launch error carries a remote log tail, and a
// listing is one row per executor.
func (c *Controller) recordLaunchOutcome(
	executorID, mode, state string, took time.Duration, err error,
) {
	if c.install == nil {
		return
	}
	if err != nil {
		c.install.setOutcome(executorID, "failed: "+oneLine(err.Error()))
		return
	}
	c.install.setOutcome(executorID,
		fmt.Sprintf("ok: %s launch %s in %s", mode, state, took.Round(100*time.Millisecond)))
}

// oneLine is the first line of text, which is all a one-row listing shows.
func oneLine(text string) string {
	i := strings.IndexByte(text, '\n')
	if i < 0 {
		return text
	}
	return text[:i]
}

// requireInstaller is the one place the "not available here" answer is written.
//
// The two seams are checked with the store: an orchestrator without them is one
// main.go never finished wiring, and a half-wired installer would nil-panic on
// the first launch rather than answer.
func (c *Controller) requireInstaller() error {
	if c.install == nil || c.execStore == nil || c.install.store == nil ||
		c.install.launcher == nil || c.install.serve == nil {
		return &connectapi.ControllerError{
			Code: protocol.ErrUnavailable,
			Message: "executor install is not available on this daemon " +
				"(it needs RAFIKI_DB and an executor pool)",
		}
	}
	return nil
}

// installErr is the refusal vocabulary: a ControllerError is the only error
// whose text ConnectErr forwards.
func installErr(code, format string, args ...any) error {
	return &connectapi.ControllerError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// sshText renders a launcher RPC failure for a caller. The launcher returns
// ssh's stderr VERBATIM (pkg/executor's sshFailure), which is the whole point: an
// agent reading "Host key verification failed" knows the fix, and an invented
// summary would replace that knowledge with nothing. The Connect wrapper's code
// prefix is dropped so the caller reads ssh's line alone.
func sshText(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Message() != "" {
		return ce.Message()
	}
	return err.Error()
}

// installLauncherFailure wraps a launcher RPC failure as a caller-visible
// refusal carrying ssh's own text.
func installLauncherFailure(step, target string, err error) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrUnavailable,
		Message: fmt.Sprintf("%s %s failed: %s", step, target, sshText(err)),
		Err:     err,
	}
}

// isUnimplemented reports whether a launcher answered CodeUnimplemented, which
// is how a launcher built before the direct-dial RPCs says it has them not.
func isUnimplemented(err error) bool {
	return connect.CodeOf(err) == connect.CodeUnimplemented
}

// installModeAuto is the request spelling that asks the probe instead of naming a
// transport; the empty string means the same thing.
const installModeAuto = "auto"

// validateInstallMode refuses an unknown spelling. It runs BEFORE any ssh round
// trip: an unrecognised mode is a caller error, and spending a probe on it would
// make a typo look like a broken target.
func validateInstallMode(requested string) error {
	switch requested {
	case "", installModeAuto, executorinstall.ModeDirect, executorinstall.ModeTunnel:
		return nil
	}
	return installErr(protocol.ErrInvalidArgs, "mode must be auto, direct or tunnel")
}

// chooseInstallMode resolves the requested mode against what the target actually
// reported about reaching the daemon. It is pure: the decision is one function so
// the refusal text, the reason recorded in the log and the stored mode can never
// disagree.
//
// reach is the launcher's own answer — "yes", "no", "unavailable", or "" from a
// launcher too old to check. Only "yes" can carry a direct install: everything
// else either falls back to the relay (under auto) or refuses (when direct was
// demanded).
func chooseInstallMode(requested, reach, detail string) (mode, why string, err error) {
	if err := validateInstallMode(requested); err != nil {
		return "", "", err
	}
	direct := reach == "yes"
	reachWhy := fmt.Sprintf("target reaches the daemon (%s)", detail)
	switch requested {
	case executorinstall.ModeTunnel:
		return executorinstall.ModeTunnel, "tunnel requested", nil
	case executorinstall.ModeDirect:
		if direct {
			return executorinstall.ModeDirect, reachWhy, nil
		}
		return "", "", installErr(protocol.ErrFailedPrecondition,
			"direct install requires the target to reach the daemon, and it cannot (reach=%s: %s); "+
				"use mode=tunnel, or fix the route and retry", reach, detail)
	}
	if direct {
		return executorinstall.ModeDirect, reachWhy, nil
	}
	if reach == "" || reach == "unavailable" {
		return executorinstall.ModeTunnel, "launcher has no dialable address", nil
	}
	return executorinstall.ModeTunnel,
		fmt.Sprintf("target cannot reach the daemon: %s (%s)", reach, detail), nil
}

// launcherTooOldErr is the refusal for a launcher whose binary predates the
// direct-dial RPCs: it cannot start a self-dialling executor, and the answer
// names the machine that must be upgraded.
func launcherTooOldErr(le execpool.LiveExecutor) error {
	return installErr(protocol.ErrFailedPrecondition,
		"launcher %s is too old to start a self-dialling executor; upgrade it or use mode=tunnel",
		launcherMachineName(le.Executor))
}

// probeFailure renders a probe that never ran. CodeUnimplemented from a probe
// under a REQUIRED direct install is the same old-launcher answer the launch
// path gives, so it names the machine to upgrade.
//
// RULING: under auto the same CodeUnimplemented stays a LAUNCHER FAILURE rather
// than silently choosing the relay. The probe is what reports the target's uname
// and platform, and nothing can be installed onto a target without them: there
// is no platform to record and no transport that installs anything by itself, so
// "choose tunnel" would only move the same failure later with less said about it.
// The cost is bounded and visible: an old launcher is refused under auto with the
// probe's own message, and the operator either upgrades it or passes mode=tunnel
// — which still needs this probe, so the refusal is honest rather than cautious.
func probeFailure(launcher execpool.LiveExecutor, requested, dest string, err error) error {
	slog.Warn("executor install: probe", "target", dest, "error", sshText(err))
	if isUnimplemented(err) && requested == executorinstall.ModeDirect {
		return launcherTooOldErr(launcher)
	}
	return installLauncherFailure("probe", dest, err)
}

// InstallExecutor installs an executor on an ssh target through a launcher
// executor, and returns once it has connected.
func (c *Controller) InstallExecutor(
	ctx context.Context, caller installCaller, req *rafikiv1.InstallExecutorRequest,
) (*rafikiv1.ExecutorRow, error) {
	if err := c.requireInstaller(); err != nil {
		return nil, err
	}

	// 1. The name, from the request or the destination's host part. Validated
	//    before anything is resolved: a name lands in a comma-separated
	//    selector, so a comma or an equals sign silently reparses into a
	//    different selector.
	dest := strings.TrimSpace(req.GetTarget())
	if dest == "" {
		return nil, installErr(protocol.ErrInvalidArgs, "target is required")
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		name = installNameFromTarget(dest)
	}
	if err := paths.ValidateMachineName(name); err != nil {
		return nil, installErr(protocol.ErrInvalidArgs, "%s", err.Error())
	}
	// The operator's labels are checked before any ssh round trip: a key the
	// daemon owns can never be accepted, so discovering that after a probe would
	// be a round trip spent on a refusal. executorTrustLabelsFor validates
	// --name/--label owner/--label machine and stamps the two the daemon owns;
	// installLabelsAllowed adds the rafiki//fundi/ namespace this feature writes
	// into.
	if err := installLabelsAllowed(req.GetLabels()); err != nil {
		return nil, err
	}
	labels, lerr := executorTrustLabelsFor(caller.OwnerName, name, req.GetLabels())
	if lerr != nil {
		return nil, lerr
	}
	// The requested mode is validated here, before any ssh round trip: an
	// unknown spelling is a caller error, and a probe spent on it would read as
	// a broken target.
	requested := strings.TrimSpace(req.GetMode())
	if merr := validateInstallMode(requested); merr != nil {
		return nil, merr
	}

	// 2. The launcher, and what it is allowed to do.
	launcher, err := c.resolveInstallLauncher(ctx, caller, strings.TrimSpace(req.GetVia()))
	if err != nil {
		return nil, err
	}
	if err := installLauncherGate(launcher); err != nil {
		return nil, err
	}

	// 3. Under the (owner, machine) lock: is this a fresh install, a reinstall
	//    of our own, or a collision with something we did not install?
	release := c.install.lockInstall(caller.OwnerUserID, name)
	defer release()

	existing, exists, err := c.executorRowByMachine(ctx, caller.OwnerUserID, name)
	if err != nil {
		return nil, err
	}
	var (
		installRow executorinstall.Row
		reinstall  bool
	)
	if exists {
		row, ok, rerr := c.install.store.ByExecutor(ctx, existing.ID)
		if rerr != nil {
			return nil, executorinstallErr(rerr)
		}
		if !ok {
			return nil, installErr(protocol.ErrAlreadyExists,
				"executor %s already exists and was not installed over ssh; pick another name or remove it", name)
		}
		installRow, reinstall = row, true
	}

	// 4. Probe: what is on the target, and which binary does it need?
	//
	// The namespace is the launcher's own executor instance (self-reported, and
	// it gates nothing), except on a REINSTALL: that row already lives in a
	// namespace, its install marker is keyed to it, and a launcher whose profile
	// changed since must not silently move the executor to a fresh one.
	instance := launcher.Executor.SelfReported["instance"]
	if instance == "" {
		instance = paths.DefaultInstance
	}
	if reinstall && installRow.Instance != "" {
		instance = installRow.Instance
	}
	launcherClient, cerr := c.launcherFor(launcher)
	if cerr != nil {
		return nil, cerr
	}
	target := &executorpb.RemoteTarget{Destination: dest, Instance: instance}
	probe, err := launcherClient.Probe(ctx, target, "")
	if err != nil {
		return nil, probeFailure(launcher, requested, dest, err)
	}
	goos, goarch, perr := platformFromUname(probe.GetUname())
	if perr != nil {
		return nil, installErr(protocol.ErrFailedPrecondition, "%s", perr.Error())
	}
	bin, berr := c.install.depot.Binary(ctx, goos, goarch)
	if berr != nil {
		// The depot's refusals are ours and name the command that fixes them.
		return nil, installErr(protocol.ErrFailedPrecondition, "%s", berr.Error())
	}
	binaryPath := depotRemotePath(bin)
	probe, err = launcherClient.Probe(ctx, target, binaryPath)
	if err != nil {
		return nil, probeFailure(launcher, requested, dest, err)
	}
	slog.Info("executor install: probe", "target", dest, "uname", probe.GetUname(),
		"binaryPresent", probe.GetBinaryPresent(), "reach", probe.GetReach(),
		"reachDetail", probe.GetReachDetail())

	// 5. The mode: the operator's requirement, else what the target proved it
	//    can do. It is resolved ONCE, here, and stored on the recipe — a
	//    relaunch reads it and never re-probes, because a changed network is an
	//    operator's reinstall, not a daemon's inference.
	mode, why, merr := chooseInstallMode(requested, probe.GetReach(), probe.GetReachDetail())
	if merr != nil {
		return nil, merr
	}
	slog.Info("executor install: mode chosen", "target", dest, "mode", mode,
		"requested", requested, "why", why)

	// 6. The clobber guard: an install proceeds only onto a target that is empty
	//    for its namespace, or that carries THIS row's own install. No --force:
	//    clearing it is one command an agent or a human runs deliberately.
	//
	//    A credential is the one exception a REINSTALL makes, and only when the
	//    marker beside it is this very row's: an executor.cred that this install
	//    wrote is not a collision with somebody else's executor.
	if probe.GetCredentialPresent() && (!reinstall || probe.GetMarker() != installRow.ID) {
		return nil, installErr(protocol.ErrAlreadyExists,
			"%s already has a durable executor (%s); use it, or remove that file first",
			name, remoteCredentialPath(instance))
	}
	if marker := probe.GetMarker(); marker != "" && (!reinstall || marker != installRow.ID) {
		return nil, installErr(protocol.ErrAlreadyExists,
			"%s has executor state from another install (%s, install %s); remove it with: ssh %s rm -f %s",
			name, remoteMarkerPath(instance), marker, dest, remoteMarkerPath(instance))
	}

	// 7. Push the binary only when the target does not already have THOSE bytes:
	//    the path is content-addressed, so a present file is the same build.
	staged := !probe.GetBinaryPresent()
	if staged {
		if err := c.pushInstalledBinary(ctx, launcherClient, target, binaryPath, bin); err != nil {
			return nil, err
		}
	}
	binaryAction := "reused"
	if staged {
		binaryAction = "staged"
	}
	slog.Info("executor install: binary", "target", dest, "state", binaryAction, "path", binaryPath)

	// 8. The durable side. A FRESH install creates the executor row and the
	//    recipe; a REINSTALL reuses both, so `executor: <name>` and pinned
	//    bindings keep meaning the same executor.
	platform := goos + "/" + goarch
	executorID := existing.ID
	installRowID := installRow.ID
	if !reinstall {
		executorID, installRowID, err = c.createInstalledRow(ctx, caller, req, labels, launcher, dest, instance, platform, mode)
		if err != nil {
			return nil, err
		}
	} else {
		if installRow.LauncherExecutorID != launcher.Executor.ID || installRow.Platform != platform {
			if serr := c.install.store.SetLauncher(ctx, installRow.ID, launcher.Executor.ID, platform); serr != nil {
				return nil, executorinstallErr(serr)
			}
		}
		// The recipe records the transport that is about to be used, so the NEXT
		// relaunch — possibly after a daemon restart, with no memory of this
		// call — drives the same one.
		//
		// RULING: the mode is persisted BEFORE the launch runs, so a launch that
		// then fails leaves the recipe on the NEW mode. That is the deliberate
		// choice: the recipe records what the operator asked for, the next lazy
		// relaunch retries it, and a target still holding the old process is
		// recovered by the next install, whose re-provisioned credential row
		// invalidates the process's stale one. Reverting the mode on a launch
		// failure would leave the recipe disagreeing with the attempt just made,
		// and a transient ssh error would flip a working tunnel install to direct.
		if installRow.EffectiveMode() != mode {
			if serr := c.install.store.SetMode(ctx, installRow.ID, mode); serr != nil {
				return nil, executorinstallErr(serr)
			}
		}
	}

	// 9. Launch, and refuse to leave a half-installed executor behind. A FRESH
	//    row that never connects is deleted and its recipe tombstoned: the next
	//    attempt must look like a first attempt, not like a collision with
	//    something that was never installed.
	launched := false
	if !reinstall {
		defer func() {
			if !launched {
				c.rollbackInstall(installRowID, executorID)
			}
		}()
	}
	if lerr := c.launchInstalled(ctx, executorID); lerr != nil {
		// A launcher too old to start a self-dialling executor is not a broken
		// target: under auto the install falls back to the relay and records the
		// transport that actually worked, while a REQUIRED direct install names
		// the machine that has to be upgraded.
		if !isUnimplemented(lerr) {
			return nil, lerr
		}
		if requested == executorinstall.ModeDirect {
			return nil, launcherTooOldErr(launcher)
		}
		mode = executorinstall.ModeTunnel
		if serr := c.install.store.SetMode(ctx, installRowID, mode); serr != nil {
			// The launch is what the caller asked for; recording it is what a LATER
			// relaunch reads. Failing the install here would deny the operator the
			// relay it just proved works, so say what the recipe now wrongly claims
			// and carry on: it still says direct, and every relaunch until the next
			// install retries the attempt that just failed.
			slog.Warn("executor install: could not record the relay transport; "+
				"the recipe still says direct and each relaunch will retry the direct launch",
				"target", dest, "install", installRowID, "error", serr)
		}
		slog.Info("executor install: mode chosen", "target", dest, "mode", mode,
			"requested", requested, "why", "launcher is too old to start a self-dialling executor")
		if lerr = c.launchInstalledMode(ctx, executorID, mode); lerr != nil {
			return nil, lerr
		}
	}
	launched = true
	return c.installedProtoRow(ctx, executorID)
}

// createInstalledRow writes the executor row and its launch recipe for a fresh
// install. The credential Create hands back is DISCARDED: an installed executor
// authenticates with a per-launch ticket, so there is no durable secret on the
// target to store, leak, or rotate.
func (c *Controller) createInstalledRow(
	ctx context.Context, caller installCaller, req *rafikiv1.InstallExecutorRequest,
	labels map[string]string, launcher execpool.LiveExecutor, dest, instance, platform, mode string,
) (executorID, installRowID string, err error) {
	labels[installedViaLabel] = launcherMachineName(launcher.Executor)
	labels[installedTargetLabel] = dest

	e, _, cerr := c.execStore.Create(ctx, executors.NewToken{
		Labels:        labels,
		Roots:         []string{rootOrTilde(req.GetRoot())},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		Admits:        req.GetAdmits(),
		OwnerUserID:   caller.OwnerUserID,
	})
	if cerr != nil {
		return "", "", translateExecutorErr(cerr)
	}
	installRowID = uuid.Must(uuid.NewV7()).String()
	if ierr := c.install.store.Insert(ctx, executorinstall.Row{
		ID:                 installRowID,
		OwnerUserID:        caller.OwnerUserID,
		ExecutorID:         e.ID,
		LauncherExecutorID: launcher.Executor.ID,
		Target:             dest,
		Instance:           instance,
		Root:               req.GetRoot(),
		InstalledByChild:   caller.ChildID,
		Platform:           platform,
		Mode:               mode,
	}); ierr != nil {
		// No recipe, no executor: the row we just made goes away with it.
		c.deleteExecutorRow(e.ID)
		return "", "", executorinstallErr(ierr)
	}
	return e.ID, installRowID, nil
}

// rollbackInstall undoes a fresh install that did not reach a connection.
func (c *Controller) rollbackInstall(installRowID, executorID string) {
	ctx, cancel := context.WithTimeout(context.Background(), installRollbackTimeout)
	defer cancel()
	if installRowID != "" {
		if err := c.install.store.Tombstone(ctx, installRowID); err != nil {
			slog.Warn("could not tombstone a failed executor install; the recipe will read as live",
				"install", installRowID, "executor", shortID(executorID), "error", err)
		}
	}
	c.deleteExecutorRow(executorID)
}

// deleteExecutorRow removes an executor row this daemon just created. The
// executor table has no tombstone, which is why the install RECIPE is the thing
// that keeps history.
func (c *Controller) deleteExecutorRow(executorID string) {
	ctx, cancel := context.WithTimeout(context.Background(), installRollbackTimeout)
	defer cancel()
	if err := c.execStore.Delete(ctx, executorID); err != nil {
		slog.Warn("could not delete a failed install's executor row",
			"executor", shortID(executorID), "error", err)
	}
}

// pushInstalledBinary streams the depot's binary to the target.
func (c *Controller) pushInstalledBinary(
	ctx context.Context, launcher installLauncher, target *executorpb.RemoteTarget,
	binaryPath string, bin depotBinary,
) error {
	f, err := os.Open(bin.Path)
	if err != nil {
		return installErr(protocol.ErrUnavailable, "read the executor binary: %s", err.Error())
	}
	defer f.Close()
	if err := launcher.Push(ctx, target, binaryPath, bin.SHA256, f); err != nil {
		return installLauncherFailure("install", target.GetDestination(), err)
	}
	return nil
}

// resolveInstallLauncher picks the launcher executor: the named one, or the
// caller's default.
//
// The confinement rule is the whole point of the CHILD branch: a child may only
// install through a launcher it could already call, so the capability it gains
// is exactly what `bash` on that machine could already do. A user or the local
// socket has no such bound — it owns its executors.
func (c *Controller) resolveInstallLauncher(
	ctx context.Context, caller installCaller, via string,
) (execpool.LiveExecutor, error) {
	if c.execPool == nil {
		return execpool.LiveExecutor{}, installErr(protocol.ErrUnavailable,
			"no executor pool is configured (requires RAFIKI_DB)")
	}

	// The child's reachable set is computed ONCE and used both to choose a
	// default and to bound an explicit via. An unreadable reach is no reach.
	//
	// A child credential naming no stored row is refused BEFORE that: an empty
	// label set matches an executor that admits everything (which every installed
	// row does), so a vanished caller would otherwise resolve the widest possible
	// set — the fail-open shape childScopeFor refuses for the same reason.
	var reachable []executors.Executor
	if caller.ChildID != "" {
		if c.st == nil {
			return execpool.LiveExecutor{}, installErr(protocol.ErrPermissionDenied,
				"agent %s is not known to this daemon", shortID(caller.ChildID))
		}
		if _, ok := c.st.Get(caller.ChildID); !ok {
			return execpool.LiveExecutor{}, installErr(protocol.ErrPermissionDenied,
				"agent %s has no stored row, so it may not install executors", shortID(caller.ChildID))
		}
		reach, err := c.installReachableExecutors(caller)
		if err != nil {
			return execpool.LiveExecutor{}, err
		}
		reachable = reach
	}

	var chosen execpool.LiveExecutor
	if via != "" {
		le, ok := c.liveExecutorByNameOrID(caller.OwnerUserID, via)
		if !ok {
			return execpool.LiveExecutor{}, installErr(protocol.ErrNotFound,
				"executor %s is not a connected executor you own", via)
		}
		chosen = le
	} else if caller.ChildID != "" {
		le, err := c.defaultChildLauncher(caller, reachable)
		if err != nil {
			return execpool.LiveExecutor{}, err
		}
		chosen = le
	} else {
		le, err := c.singleOwnedLauncher(caller)
		if err != nil {
			return execpool.LiveExecutor{}, err
		}
		chosen = le
	}

	if caller.ChildID != "" && !containsExecutorID(reachable, chosen.Executor.ID) {
		return execpool.LiveExecutor{}, installErr(protocol.ErrPermissionDenied,
			"executor %s is not reachable by this agent", installNameOf(chosen.Executor))
	}
	return chosen, nil
}

// defaultChildLauncher resolves a child's launcher without one being named: its
// home executor, else its launch host, else the single native executor it can
// reach. The remembered bindings come first because they are where the child
// already runs — the machine whose ssh keys and config the operator set up for
// it.
func (c *Controller) defaultChildLauncher(
	caller installCaller, reachable []executors.Executor,
) (execpool.LiveExecutor, error) {
	if c.st != nil {
		if snap, ok := c.st.Get(caller.ChildID); ok {
			for _, label := range []string{homeExecutorLabel, executorBindingLabel} {
				if id := snap.Labels[label]; id != "" {
					if le, ok := c.liveExecutor(id); ok {
						return le, nil
					}
				}
			}
		}
	}
	var native []executors.Executor
	for _, e := range reachable {
		if e.Isolation != "container" {
			native = append(native, e)
		}
	}
	if len(native) == 1 {
		if le, ok := c.liveExecutor(native[0].ID); ok {
			return le, nil
		}
	}
	return execpool.LiveExecutor{}, installViaCandidates(native)
}

// singleOwnedLauncher is a user's (or the local socket's) default: the single
// live native executor it owns.
func (c *Controller) singleOwnedLauncher(caller installCaller) (execpool.LiveExecutor, error) {
	var native []executors.Executor
	for _, le := range c.execPool.Live() {
		e := le.Executor
		if !e.Enabled || e.OwnerUserID != caller.OwnerUserID || e.Isolation == "container" {
			continue
		}
		native = append(native, e)
	}
	if len(native) == 1 {
		if le, ok := c.liveExecutor(native[0].ID); ok {
			return le, nil
		}
	}
	return execpool.LiveExecutor{}, installViaCandidates(native)
}

// installViaCandidates is the "name one" refusal. It lists the machines that
// WOULD have worked, because "ambiguous" without the options is a dead end for
// an agent.
func installViaCandidates(candidates []executors.Executor) error {
	if len(candidates) == 0 {
		return installErr(protocol.ErrFailedPrecondition,
			"no executor can install for you right now; connect a native executor, or pass via")
	}
	names := make([]string, 0, len(candidates))
	for _, e := range candidates {
		names = append(names, installNameOf(e))
	}
	return installErr(protocol.ErrInvalidArgs, "pass via: %s", strings.Join(names, ", "))
}

// installLauncherGate refuses a launcher that may not run ssh. Both facts are
// checked here and nowhere else, so no install path can skip them.
func installLauncherGate(le execpool.LiveExecutor) error {
	if !le.Executor.Enabled {
		// `rafiki executor disable` is a revocation, and a revoked executor must
		// not be able to start ssh on the daemon's behalf. Checked HERE, in the
		// one gate, because the child's home/label default does not filter it the
		// way the caller's own defaults do.
		return installErr(protocol.ErrFailedPrecondition,
			"executor %s is disabled", installNameOf(le.Executor))
	}
	if le.Executor.Isolation == "container" {
		// The capability equals what `bash` on that machine can already do; a
		// container has no ssh keys of its own and must never be a launcher.
		return installErr(protocol.ErrFailedPrecondition,
			"executor %s is container-isolated and cannot install executors", installNameOf(le.Executor))
	}
	if !le.Describe.GetSshLaunch() {
		// Self-reported, so it only ever NARROWS: an operator's
		// --no-ssh-launch takes this executor out of the install path entirely.
		return installErr(protocol.ErrFailedPrecondition,
			"executor %s has ssh launch disabled (--no-ssh-launch)", installNameOf(le.Executor))
	}
	return nil
}

// installReachableExecutors is the child's effective executor set, recomputed
// from the live pool exactly as a spawn would: an install can only go through a
// machine the child could already call.
//
// Reachable also covers installed executors that are DOWN (they are places to
// call, and a call relaunches them). A LAUNCHER must be live — it is the machine
// that will run ssh — so that half is filtered out here rather than leaking into
// the `via` choice as a candidate that can never be chosen.
//
// The request names the caller as its OWN parent on purpose: admissionLabels
// then reads the child's stored labels, and effectiveExecutorSetFor walks the
// stored selector chain from the child upward — the child's effective set. An
// empty selector adds nothing on top, and the sandbox ownership lookup is
// unchanged by the hint (its first arm already matches the child itself).
func (c *Controller) installReachableExecutors(caller installCaller) ([]executors.Executor, error) {
	binder := c.binderFor(
		protocol.SpawnRequest{ParentChildID: caller.ChildID},
		executorOwner{Name: caller.OwnerName, UserID: caller.OwnerUserID},
	)
	reach, err := binder.Reachable(caller.ChildID)
	if err != nil {
		// Fail closed: a reach that cannot be computed is not an empty set the
		// caller may install through anyway.
		return nil, installErr(protocol.ErrFailedPrecondition, "%s", err.Error())
	}
	live := make([]executors.Executor, 0, len(reach))
	for _, e := range reach {
		if _, ok := c.liveExecutor(e.ID); ok {
			live = append(live, e)
		}
	}
	return live, nil
}

// liveExecutorByNameOrID resolves a machine name or an exact executor id among
// the caller's live, enabled executors.
func (c *Controller) liveExecutorByNameOrID(ownerUserID, ref string) (execpool.LiveExecutor, bool) {
	for _, le := range c.execPool.Live() {
		e := le.Executor
		if !e.Enabled || e.OwnerUserID != ownerUserID {
			continue
		}
		if e.ID == ref || e.Labels["machine"] == ref {
			return le, true
		}
	}
	return execpool.LiveExecutor{}, false
}

// liveExecutor returns the pool's live entry for id.
func (c *Controller) liveExecutor(executorID string) (execpool.LiveExecutor, bool) {
	if c.execPool == nil || executorID == "" {
		return execpool.LiveExecutor{}, false
	}
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == executorID {
			return le, true
		}
	}
	return execpool.LiveExecutor{}, false
}

// launcherFor resolves a launcher executor's ssh capability, refusing with a
// message a caller can act on when it cannot be reached.
func (c *Controller) launcherFor(le execpool.LiveExecutor) (installLauncher, error) {
	client, err := c.install.launcher(le.Executor.ID)
	if err != nil {
		return nil, installErr(protocol.ErrUnavailable, "launcher %s is not connected (%s)",
			installNameOf(le.Executor), sshText(err))
	}
	return client, nil
}

// executorRowByMachine finds an executor row by its (owner, machine) identity —
// the pair the unique index enforces.
func (c *Controller) executorRowByMachine(
	ctx context.Context, ownerUserID, name string,
) (executors.Executor, bool, error) {
	rows, err := c.execStore.List(ctx)
	if err != nil {
		return executors.Executor{}, false, translateExecutorErr(err)
	}
	for _, e := range rows {
		if e.OwnerUserID == ownerUserID && e.Labels["machine"] == name {
			return e, true, nil
		}
	}
	return executors.Executor{}, false, nil
}

// launchInstalled starts an installed executor's ssh session and waits for it
// to connect. Coalesced per executor id: eight concurrent callers cost one ssh
// session and share its outcome.
func (c *Controller) launchInstalled(ctx context.Context, executorID string) error {
	return c.launchInstalledMode(ctx, executorID, "")
}

// launchInstalledMode is launchInstalled with the transport FORCED: a non-empty
// mode overrides the recipe for this attempt alone. That is how an install which
// could not record the relay it fell back to still launches the relay now —
// reading the recipe again would find the transport that just failed.
func (c *Controller) launchInstalledMode(ctx context.Context, executorID, mode string) error {
	if err := c.requireInstaller(); err != nil {
		return err
	}
	// Already serving: relaunching would kill a working session for nothing.
	if _, ok := c.liveExecutor(executorID); ok {
		return nil
	}

	flight, leader := c.install.beginLaunch(executorID)
	if !leader {
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := c.runLaunch(ctx, executorID, mode)
	c.install.finishLaunch(executorID, flight, err)
	return err
}

// runLaunch is launchInstalled's body, run once per flight.
//
// The outcome is recorded on EVERY failure past the recipe, not only on one that
// reached the launcher: a listing that showed the previous launch's `ok` after
// this one failed for want of a connected launcher would be a lie about the last
// thing the daemon tried.
func (c *Controller) runLaunch(ctx context.Context, executorID, modeOverride string) (err error) {
	row, ok, err := c.install.store.ByExecutor(ctx, executorID)
	if err != nil {
		return executorinstallErr(err)
	}
	if !ok {
		return installErr(protocol.ErrFailedPrecondition,
			"executor %s was not installed over ssh", shortID(executorID))
	}
	mode := row.EffectiveMode()
	if modeOverride != "" {
		mode = modeOverride
	}
	started := time.Now()
	state := ""
	defer func() {
		c.recordLaunchOutcome(executorID, mode, state, time.Since(started), err)
	}()

	launcher, live := c.liveExecutor(row.LauncherExecutorID)
	if !live {
		return installErr(protocol.ErrUnavailable,
			"launcher %s is not connected", c.launcherName(ctx, row.LauncherExecutorID))
	}
	// The gate runs on THIS path too, not only at install time: its facts are
	// live (a container label, an operator's later --no-ssh-launch), and a
	// relaunch must not be the way around a rule the install honoured.
	if err := installLauncherGate(launcher); err != nil {
		return err
	}
	goos, goarch, perr := splitInstallPlatform(row.Platform)
	if perr != nil {
		return installErr(protocol.ErrFailedPrecondition, "%s", perr.Error())
	}
	bin, berr := c.install.depot.Binary(ctx, goos, goarch)
	if berr != nil {
		return installErr(protocol.ErrFailedPrecondition, "%s", berr.Error())
	}

	// The transport is the RECIPE's: it was decided once, at install, from the
	// operator's override or the target's own reachability, and a relaunch reads
	// it rather than re-probing. A changed network is an operator's reinstall.
	if mode == executorinstall.ModeDirect {
		state, err = c.runDirectLaunch(ctx, executorID, row, launcher, bin)
	} else {
		state, err = c.runTunnelLaunch(ctx, executorID, row, launcher, bin)
	}
	return err
}

// runTunnelLaunch is the relay: the launcher opens one ssh whose stdio carries
// the executor protocol, so the target dials nothing and lives exactly as long
// as that session.
func (c *Controller) runTunnelLaunch(
	ctx context.Context, executorID string, row executorinstall.Row,
	launcher execpool.LiveExecutor, bin depotBinary,
) (string, error) {
	started := time.Now()
	// The ticket is the remote's credential: one shot, bound to the durable row,
	// and written as the first line of the relayed stdin so it is never in an
	// argv on either machine.
	ticket, terr := c.execPool.Tickets().Mint(execpool.TicketGrant{RowID: executorID})
	if terr != nil {
		return "", installErr(protocol.ErrInternal, "mint an install ticket: %s", terr.Error())
	}
	// Every way out that is not a connection discards the ticket: a credential
	// minted for a session nobody is waiting for is one an ssh that finally
	// starts could still redeem. On success the remote redeems it, which removes
	// it from the registry, so there is nothing to discard.
	discardTicket := func() { c.execPool.Tickets().Revoke(ticket) }

	// The waiter is registered BEFORE the stream opens, so a fast remote cannot
	// connect between the two and be missed.
	connected, unwatch := c.install.watchConnect(executorID)
	defer unwatch()

	// The session's lifetime is the DAEMON's, never this caller's: the executor
	// outlives the tool call that started it.
	parent := c.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	serveCtx, cancelServe := context.WithCancel(parent)

	launcherClient, lerr := c.launcherFor(launcher)
	if lerr != nil {
		cancelServe()
		discardTicket()
		return "", lerr
	}
	stream, serr := launcherClient.Serve(serveCtx, &executorpb.RemoteServeStart{
		Target:     &executorpb.RemoteTarget{Destination: row.Target, Instance: row.Instance},
		BinaryPath: depotRemotePath(bin),
		Root:       row.Root,
		InstallId:  row.ID,
	})
	if serr != nil {
		cancelServe()
		discardTicket()
		return "", installLauncherFailure("launch", row.Target, serr)
	}
	conn := newRemoteServeConn(stream, executorID, row.Target, launcherMachineName(launcher.Executor))
	if _, werr := conn.Write([]byte(ticket + "\n")); werr != nil {
		cancelServe()
		discardTicket()
		conn.Close()
		return "", installLauncherFailure("launch", row.Target, werr)
	}

	// From here the ssh session outlives this function: ServeConn runs the
	// ordinary executor upgrade on the tunnelled connection, and the executor
	// connects into the pool exactly as a dialled one does.
	slog.Info("executor install: launch", "target", row.Target,
		"mode", executorinstall.ModeTunnel, "state", "relay")
	c.install.setServe(executorID, cancelServe)
	go func() {
		if err := c.install.serve(serveCtx, conn); err != nil && serveCtx.Err() == nil {
			slog.Warn("installed executor session ended",
				"executor", shortID(executorID), "target", row.Target, "error", err)
		}
		conn.Close()
	}()

	timer := time.NewTimer(c.install.connectTimeout)
	defer timer.Stop()
	select {
	case <-connected:
		slog.Info("executor install: connected", "executor", c.launcherName(ctx, executorID),
			"target", row.Target, "mode", executorinstall.ModeTunnel,
			"took", time.Since(started).Round(100*time.Millisecond))
		return "relay", nil
	case <-conn.exited():
		// The remote process ended before it ever spoke the upgrade.
		code, signal := conn.exitCode()
		slog.Debug("installed executor exited before connecting",
			"executor", shortID(executorID), "target", row.Target,
			"exitCode", code, "signal", signal)
		c.install.cancelServe(executorID)
		discardTicket()
		return "", installErr(protocol.ErrUnavailable, "relaunch failed via %s: %s",
			launcherMachineName(launcher.Executor), conn.stderrTail())
	case <-timer.C:
		discardTicket()
		c.install.cancelServe(executorID)
		// The executor being launched is named FIRST. Naming the launcher here
		// told an operator to go and debug the machine that was working.
		return "", installErr(protocol.ErrUnavailable,
			"executor %s on %s (via %s) did not connect within %s; ssh said: %s",
			c.launcherName(ctx, executorID), row.Target, launcherMachineName(launcher.Executor),
			c.install.connectTimeout, conn.stderrTail())
	case <-ctx.Done():
		// The caller gave up, and it was the leader whose context this flight
		// rides, so the flight is over: nobody is waiting on this session.
		//
		// If the executor nevertheless CONNECTED, report the success rather than
		// tearing down a row that is already up; otherwise discard the ticket (a
		// credential for a session nobody awaits, which an ssh that finally starts
		// could still redeem) and end the session.
		if _, live := c.liveExecutor(executorID); live {
			return "relay", nil
		}
		discardTicket()
		c.install.cancelServe(executorID)
		return "", ctx.Err()
	}
}

// runDirectLaunch starts a DETACHED executor on the target that dials the daemon
// itself, and waits for the connection.
//
// The ticket it carries mints a credential on redemption, so the process keeps
// authenticating on its own after this call returns, after the launcher goes to
// sleep, and after the daemon restarts. Nothing about the launch is retried
// here: the daemon's lazy relaunch is what brings a dead process back.
func (c *Controller) runDirectLaunch(
	ctx context.Context, executorID string, row executorinstall.Row,
	launcher execpool.LiveExecutor, bin depotBinary,
) (string, error) {
	// MintCredential is what makes this ticket an ENROLMENT as well as a
	// one-shot: a detached process has no other way to hold a credential, and
	// rotating it invalidates the previous launch's.
	ticket, terr := c.execPool.Tickets().Mint(execpool.TicketGrant{
		RowID: executorID, MintCredential: true,
	})
	if terr != nil {
		return "", installErr(protocol.ErrInternal, "mint an install ticket: %s", terr.Error())
	}
	discardTicket := func() { c.execPool.Tickets().Revoke(ticket) }

	// Registered BEFORE the launch runs: the remote process can dial the daemon
	// before the launch's ssh has even returned.
	connected, unwatch := c.install.watchConnect(executorID)
	defer unwatch()

	// The ssh that starts the process is bound to the DAEMON's lifetime, exactly
	// like the tunnel's session: a caller that times out did not start this
	// process, and cancelling the launch mid-flight would leave a target whose
	// executor may or may not exist.
	parent := c.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	launchCtx, cancelLaunch := context.WithCancel(parent)
	defer cancelLaunch()

	launcherClient, lerr := c.launcherFor(launcher)
	if lerr != nil {
		discardTicket()
		return "", lerr
	}
	start := &executorpb.RemoteLaunchStart{
		Target:     &executorpb.RemoteTarget{Destination: row.Target, Instance: row.Instance},
		BinaryPath: depotRemotePath(bin),
		Root:       row.Root,
		InstallId:  row.ID,
	}
	started := time.Now()
	resp, lerr := launcherClient.Launch(launchCtx, start, ticket)
	if lerr != nil {
		// The ticket goes even on a failure the launcher never saw: a launch
		// that timed out on the client side can still have reached the target.
		discardTicket()
		slog.Warn("executor install: launch", "target", row.Target,
			"mode", executorinstall.ModeDirect, "error", sshText(lerr))
		return "", installLauncherFailure("launch", row.Target, lerr)
	}
	state := resp.GetState()
	slog.Info("executor install: launch", "target", row.Target,
		"mode", executorinstall.ModeDirect, "state", state, "pid", resp.GetPid())
	if state == "running" {
		// The instance was already held, so nothing redeemed this ticket: the
		// live process reconnects on its own and a minted credential nobody read
		// would only be one more secret to revoke later.
		discardTicket()
	}

	timer := time.NewTimer(c.install.connectTimeout)
	defer timer.Stop()
	select {
	case <-connected:
		slog.Info("executor install: connected", "executor", c.launcherName(ctx, executorID),
			"target", row.Target, "mode", executorinstall.ModeDirect,
			"took", time.Since(started).Round(100*time.Millisecond))
		return state, nil
	case <-ctx.Done():
		if _, live := c.liveExecutor(executorID); live {
			return state, nil
		}
		discardTicket()
		return state, ctx.Err()
	case <-timer.C:
		discardTicket()
		tail := c.remoteLogTail(ctx, launcherClient, row)
		msg := fmt.Sprintf("executor %s on %s (via %s) did not connect within %s; remote log:\n%s",
			c.launcherName(ctx, executorID), row.Target, launcherMachineName(launcher.Executor),
			c.install.connectTimeout, tail)
		slog.Warn("executor install: launch", "target", row.Target,
			"mode", executorinstall.ModeDirect, "error", oneLine(msg))
		return state, installErr(protocol.ErrUnavailable, "%s", msg)
	}
}

// remoteLogTail reads the target's executor.log for a failure message. A
// detached process's only diagnostic is that file, and a tail that cannot be
// read is still worth saying out loud rather than an error that reads complete.
func (c *Controller) remoteLogTail(
	ctx context.Context, launcher installLauncher, row executorinstall.Row,
) string {
	tail, err := launcher.LogTail(ctx,
		&executorpb.RemoteTarget{Destination: row.Target, Instance: row.Instance}, 40)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %s)", sshText(err))
	}
	if strings.TrimSpace(tail) == "" {
		return "(executor.log is empty)"
	}
	return tail
}

// UninstallExecutor removes an executor this lineage installed: the ssh session
// ends, the target's install process and marker go, the recipe is tombstoned and
// the executor row is deleted.
func (c *Controller) UninstallExecutor(ctx context.Context, caller installCaller, ref string) error {
	if err := c.requireInstaller(); err != nil {
		return err
	}
	ref = strings.TrimSpace(ref)

	// The SAME per-(owner, machine) lock InstallExecutor takes, so installing and
	// uninstalling one name serialise instead of interleaving: without it an
	// uninstall could tombstone and delete the row a reinstall was already
	// launching, and the ssh session would come up for a row that no longer
	// exists.
	//
	// The key names the row, so the ref has to be resolved first, and the row is
	// then resolved AGAIN under the lock: what this call acts on must be what the
	// lock protects, not what it saw on the way in.
	key, err := c.resolveInstallExecutor(ctx, caller, ref)
	if err != nil {
		return err
	}
	release := c.install.lockInstall(key.OwnerUserID, installLockName(key))
	defer release()

	e, err := c.resolveInstallExecutor(ctx, caller, ref)
	if err != nil {
		return err
	}
	row, ok, rerr := c.install.store.ByExecutor(ctx, e.ID)
	if rerr != nil {
		return executorinstallErr(rerr)
	}
	if !ok {
		return installErr(protocol.ErrFailedPrecondition,
			"executor %s was not installed over ssh", installNameOf(e))
	}
	if caller.ChildID != "" && !c.installedByLineage(caller.ChildID, row.InstalledByChild) {
		// The SAME sentence the policy layer uses for a foreign row, so an agent
		// cannot tell a sibling's executor from a stranger's.
		return installErr(protocol.ErrPermissionDenied, "executor %s belongs to another user", shortID(e.ID))
	}

	// End the session FIRST: the remote must be gone before its marker is
	// removed, or a still-running executor rewrites it.
	c.install.cancelServe(e.ID)

	// The target is cleaned through the launcher when it is live: the marker
	// goes, and a DIRECT install's detached process is stopped as well. A failure
	// here does not fail the uninstall — the tombstone and the row deletion below
	// are the guarantee, and the leftover marker is named by the next install's
	// clobber guard — but for a direct row it is the difference between the
	// process stopping now and stopping once its next connection is refused.
	if row.LauncherExecutorID != "" {
		c.cleanInstall(ctx, row)
	}

	if terr := c.install.store.Tombstone(ctx, row.ID); terr != nil {
		return executorinstallErr(terr)
	}
	if derr := c.execStore.Delete(ctx, e.ID); derr != nil && !errors.Is(derr, executors.ErrNotFound) {
		return translateExecutorErr(derr)
	}
	// Cut the connection NOW rather than on the pool's next row refresh (about
	// 30s): the row it serves is gone, and until the pool notices the daemon
	// still hands work to a machine the caller has just uninstalled. AFTER the
	// delete on purpose — a connection cut first can reconnect into its own
	// still-existing row. Nil-safe: a Controller built without a listener has no
	// pool, and nothing connected to it to cut.
	if c.execPool != nil {
		c.execPool.Evict(e.ID)
	}
	// The row is gone, so nothing can be listed for this id again; drop its
	// in-memory launch outcome rather than holding an entry nobody can read.
	c.install.forgetOutcome(e.ID)
	return nil
}

// directStillRunning is what an uninstall could not do, said out loud: a direct
// install's process is DETACHED, so releasing this daemon's session ends
// nothing. The deleted executor row is what ends it — the remote's next
// connection is refused with a terminal 401, so it exits instead of retrying.
const directStillRunning = "the remote executor is still running; it stops itself the next time it reconnects, " +
	"because its executor row is deleted and the daemon answers 401"

// cleanInstall removes the target's install marker iff it holds this install's
// id, and — for a DIRECT install — asks the launcher to stop the detached
// process first. The shared binary cache is left alone: it is content-addressed
// and every install on that target uses it.
//
// Nothing here can fail the uninstall. For a tunnel row that is just tidiness
// (its remote process is the launcher's own ssh child, ended with the session
// above); for a direct row it is a fact worth naming, because the row deletion
// that follows is what ends that process, so a launcher this call could not
// reach — or a target that could not confirm the process gone, and therefore
// removed nothing — leaves work running there until the remote reconnects.
func (c *Controller) cleanInstall(ctx context.Context, row executorinstall.Row) {
	stop := row.EffectiveMode() == executorinstall.ModeDirect
	launcherExec, live := c.liveExecutor(row.LauncherExecutorID)
	if !live {
		cleanInstallMissed(stop, false,
			"uninstall: launcher is not connected; leaving the target's install marker",
			"launcher", shortID(row.LauncherExecutorID), "target", row.Target)
		return
	}
	// The same gate the install and the relaunch apply: no path asks a machine
	// that is not a launcher to run ssh on the daemon's behalf.
	if err := installLauncherGate(launcherExec); err != nil {
		cleanInstallMissed(stop, false,
			"uninstall: launcher may not run ssh; leaving the target's install marker",
			"launcher", shortID(row.LauncherExecutorID), "target", row.Target, "error", err)
		return
	}
	launcher, err := c.install.launcher(row.LauncherExecutorID)
	if err != nil {
		cleanInstallMissed(stop, true,
			"uninstall: could not reach the launcher; leaving the target's install marker",
			"launcher", shortID(row.LauncherExecutorID), "target", row.Target, "error", err)
		return
	}
	removed, stopped, cerr := launcher.Clean(ctx, &executorpb.RemoteTarget{
		Destination: row.Target, Instance: row.Instance,
	}, row.ID, stop)
	if cerr != nil {
		cleanInstallMissed(stop, true,
			"uninstall: could not remove the target's install marker; the next install will name it",
			"target", row.Target, "install", row.ID, "error", cerr)
		return
	}
	if stop && !stopped {
		// A stop the target could not vouch for removes nothing there, the
		// marker included (remoteStopScript), so this is the partially cleaned
		// uninstall: the row goes and the process may not.
		cleanInstallMissed(stop, false,
			"uninstall: the target could not confirm the detached executor stopped; it left every file in place",
			"target", row.Target, "install", row.ID, "removed", removed)
		return
	}
	slog.Info("uninstall: cleaned the target", "target", row.Target,
		"install", row.ID, "mode", row.EffectiveMode(), "removed", removed, "stopped", stopped)
}

// RULING — a partially cleaned uninstall is reported through the daemon's log,
// not through the response. UninstallExecutorResponse is an EMPTY message, and
// the launcher's RemoteCleanResponse carries only the two booleans
// `removed`/`stopped`, so there is nowhere to put a warning; failing the RPC to
// signal it instead would leave the AUTHORITATIVE row and recipe undeleted —
// worse than the condition, because the deleted row is what ends a detached
// process at all. The operator-visible channels are therefore exactly two, and
// both are named: the Warn cleanInstallMissed emits, carrying the target, the
// install id and the consequence, and the next install's clobber guard, which
// names the leftover marker or credential (with the install id, since a failed
// stop keeps the marker on purpose). A structured warning field would be a wire
// change — proto, both SDKs and the generated clients — deliberately outside
// this change.
//
// What being wrong costs: an operator who does not read the daemon log can
// believe the remote process is gone for up to one reconnect, which is when the
// deleted row refuses it a terminal 401 and it exits by itself.

// cleanInstallMissed reports ONE reason an uninstall could not clean the target.
// Each reason returns, so a direct row's consequence sentence is said once per
// uninstall rather than once per line.
//
// stop is whether the target runs a DETACHED process (a direct install), whose
// only ending is the deleted executor row: that always logs at Warn and always
// carries the consequence, whatever the reason. warn is the level a tunnel row's
// wording has always used for this reason; a tunnel row's remote process is the
// launcher's ssh child and dies with the session, so there is nothing to name.
func cleanInstallMissed(stop, warn bool, msg string, args ...any) {
	switch {
	case stop:
		slog.Warn(msg+"; "+directStillRunning, args...)
	case warn:
		slog.Warn(msg, args...)
	default:
		slog.Debug(msg, args...)
	}
}

// installedByLineage reports whether a caller may uninstall a row whose
// installer is installedByChild: it may when IT installed the row, or when one
// of its ANCESTORS did.
//
// Authority flows DOWN the tree, which is why the walk is IsDescendant(installer,
// caller) and not the other way round. A subtree may tidy up what its ancestors
// provisioned FOR it — a coordinator installs a machine, a worker cleans it up —
// while a sibling's or a descendant's install is not the caller's to remove. It
// is also symmetric with install-time launcher reach: an agent installs through
// a machine it may reach, and uninstalls what the same lineage put there.
//
// A row a human installed has no installer child, so no child may uninstall it;
// the owner's own credential is the way to remove that one.
func (c *Controller) installedByLineage(callerChildID, installedByChild string) bool {
	if installedByChild == "" || callerChildID == "" {
		return false
	}
	if installedByChild == callerChildID {
		return true
	}
	return c.st != nil && c.st.IsDescendant(installedByChild, callerChildID)
}

// installLockName is the (owner, machine) lock's name half for an existing row:
// its machine label, which is exactly what InstallExecutor locks on. The id is
// only a fallback — no installed row lacks a machine label, since the install
// writes it — so that the lock can never silently become a no-op.
func installLockName(e executors.Executor) string {
	if m := e.Labels["machine"]; m != "" {
		return m
	}
	return e.ID
}

// resolveInstallExecutor resolves a name or id among the caller's OWN rows. A
// foreign or unknown ref answers one identical not-found, so a caller cannot
// probe another owner's fleet.
func (c *Controller) resolveInstallExecutor(
	ctx context.Context, caller installCaller, ref string,
) (executors.Executor, error) {
	if ref == "" {
		return executors.Executor{}, installErr(protocol.ErrInvalidArgs, "executor is required")
	}
	rows, err := c.execStore.List(ctx)
	if err != nil {
		return executors.Executor{}, translateExecutorErr(err)
	}
	var exact, byName, byID []executors.Executor
	for _, e := range rows {
		if e.OwnerUserID != caller.OwnerUserID {
			continue
		}
		if e.ID == ref {
			exact = append(exact, e)
			continue
		}
		if e.Labels["machine"] == ref {
			byName = append(byName, e)
		}
		if len(ref) >= executorRefMinLen && strings.HasSuffix(e.ID, ref) {
			byID = append(byID, e)
		}
	}
	for _, set := range [][]executors.Executor{exact, byName, byID} {
		if len(set) == 1 {
			return set[0], nil
		}
		if len(set) > 1 {
			return executors.Executor{}, installErr(protocol.ErrInvalidArgs,
				"executor %q is ambiguous — it matches %d executors of yours", ref, len(set))
		}
	}
	return executors.Executor{}, installErr(protocol.ErrNotFound,
		"executor %q: no such executor of yours", ref)
}

// installDisplay is the installed-row decoration the ExecutorRow mapping adds:
// the launcher's machine name, the ssh destination, the stored transport and
// the outcome of the last launch this daemon performed.
type installDisplay struct {
	Via        string
	Target     string
	Mode       string
	LastLaunch string
}

// installDisplays returns that decoration for every LIVE install row, keyed by
// executor id. Nil when the installer is not wired.
//
// ListLiveAll is deliberate here: the executor LISTING is the operator surface,
// and it scopes rows by owner itself after this decoration is applied. The
// caller-bound paths never come through here (they resolve one executor id
// inside the caller's own rows first).
func (c *Controller) installDisplays(ctx context.Context) map[string]installDisplay {
	return c.installDisplaysFor(ctx, c.liveInstallRows(ctx))
}

// installDisplaysFor is installDisplays over rows the caller already read.
//
// A caller that needs BOTH the recipes and their decoration — the agent
// listing, which scopes a child by who installed what — reads ListLiveAll once
// and passes it here, so the two halves can never disagree about which rows are
// live.
func (c *Controller) installDisplaysFor(
	ctx context.Context, rows []executorinstall.Row,
) map[string]installDisplay {
	if len(rows) == 0 {
		return nil
	}
	// One read of the executor table answers both halves: the launcher's machine
	// name, and — when the launcher row is GONE (an operator deleted it, or a
	// transient session executor that has since ended) — the machine name the
	// install wrote onto the executor's own row. A listing must not lose the
	// provenance of a row that is still installed.
	byID := map[string]executors.Executor{}
	if c.execStore != nil {
		if all, lerr := c.execStore.List(ctx); lerr == nil {
			for _, e := range all {
				byID[e.ID] = e
			}
		}
	}
	out := make(map[string]installDisplay, len(rows))
	for _, r := range rows {
		via := ""
		if launcher, ok := byID[r.LauncherExecutorID]; ok {
			via = installNameOf(launcher)
		} else if installed, ok := byID[r.ExecutorID]; ok {
			via = installed.Labels[installedViaLabel]
		}
		view := installDisplay{Via: via, Target: r.Target, Mode: r.EffectiveMode()}
		if c.install != nil {
			view.LastLaunch = c.install.outcomeOf(r.ExecutorID)
		}
		out[r.ExecutorID] = view
	}
	return out
}

// installDisplayFor is installDisplays for one executor.
func (c *Controller) installDisplayFor(ctx context.Context, executorID string) installDisplay {
	return c.installDisplays(ctx)[executorID]
}

// launcherName names an executor row for an error message: its machine label,
// else a short id. Both halves of an install message use it — the executor being
// launched and the launcher doing the launching are different rows.
func (c *Controller) launcherName(ctx context.Context, executorID string) string {
	if executorID == "" {
		return ""
	}
	if e, err := c.execStore.Get(ctx, executorID); err == nil {
		return installNameOf(e)
	}
	return shortID(executorID)
}

// installedProtoRow renders the row an install answers with, plus its install
// decoration and the pool's live view.
func (c *Controller) installedProtoRow(ctx context.Context, executorID string) (*rafikiv1.ExecutorRow, error) {
	e, err := c.execStore.Get(ctx, executorID)
	if err != nil {
		return nil, translateExecutorErr(err)
	}
	var connectedAt time.Time
	if le, ok := c.liveExecutor(executorID); ok {
		e.Connected = true
		connectedAt = le.ConnectedAt
	}
	return protoExecutorRow(e, connectedAt, c.installDisplayFor(ctx, executorID)), nil
}

// executorinstallErr maps the recipe store's domain sentinel and wraps anything
// else without letting a database error's text reach a caller.
func executorinstallErr(err error) error {
	if errors.Is(err, executorinstall.ErrExists) {
		return installErr(protocol.ErrAlreadyExists,
			"an install recipe already exists for that executor; uninstall it first")
	}
	slog.Error("executor install: store error", "error", err)
	return installErr(protocol.ErrUnavailable, "the executor install table is unavailable")
}

// installLabelsAllowed refuses the label keys the daemon owns. `owner` and
// `machine` are stamped by executorTrustLabelsFor; a rafiki/ or fundi/ key is
// the daemon's record of where an executor came from, which a caller could
// otherwise forge into a selector match.
func installLabelsAllowed(given map[string]string) error {
	for k := range given {
		switch {
		case k == "owner":
			return installErr(protocol.ErrInvalidArgs,
				"owner is derived from the connection and cannot be set with --label")
		case strings.HasPrefix(k, "rafiki/"), strings.HasPrefix(k, "fundi/"):
			return installErr(protocol.ErrInvalidArgs,
				"label %q is reserved: labels under rafiki/ or fundi/ are written by the daemon", k)
		}
	}
	return nil
}

// installNameFromTarget derives a machine name from an ssh destination: the host
// part, with any user@ and :port removed.
func installNameFromTarget(dest string) string {
	host := dest
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if strings.HasPrefix(host, "[") {
		// A bracketed IPv6 literal. ValidateMachineName refuses ':' anyway, so
		// this only makes the refusal name the host rather than a fragment.
		if j := strings.LastIndex(host, "]"); j >= 0 {
			host = host[1:j]
		}
	} else if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

// depotRemotePath is where the depot's binary lives on a target: the
// content-addressed cache, named by this daemon's tag and the first 12 hex of
// the binary's sha256. The launcher's probe validates exactly this shape.
func depotRemotePath(b depotBinary) string {
	sum := b.SHA256
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return installBinaryDir + "/rafiki-" + b.Tag + "-" + sum
}

// splitInstallPlatform reads back the "goos/goarch" a recipe recorded.
func splitInstallPlatform(platform string) (goos, goarch string, err error) {
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok || goos == "" || goarch == "" {
		return "", "", fmt.Errorf("the install recipe has no usable platform (%q); uninstall and install it again", platform)
	}
	return goos, goarch, nil
}

// remoteCredentialPath and remoteMarkerPath spell the target's files the way the
// launcher's probe script computes them (pkg/executor.RemoteDataDirExpr), so a
// refusal names a path the caller can paste on the target.
func remoteCredentialPath(instance string) string {
	return executor.RemoteDataDirExpr(instance) + "/executor.cred"
}

func remoteMarkerPath(instance string) string {
	return executor.RemoteDataDirExpr(instance) + "/installed-executor"
}

// installNameOf names an executor the way an operator and a model see it: its
// machine label, else a short id.
func installNameOf(e executors.Executor) string {
	if m := e.Labels["machine"]; m != "" {
		return m
	}
	return shortID(e.ID)
}

// launcherMachineName is the launcher's machine label, which is what a
// "via <name>" message must name.
func launcherMachineName(e executors.Executor) string { return installNameOf(e) }

// rootOrTilde is the executor row's root list entry: the operator's --root, or
// the executor's default root.
func rootOrTilde(root string) string {
	if root == "" {
		return "~"
	}
	return root
}

// containsExecutorID reports whether id is in set.
func containsExecutorID(set []executors.Executor, id string) bool {
	for _, e := range set {
		if e.ID == id {
			return true
		}
	}
	return false
}

// protoExecutorRow renders one executor as the wire's row, with the install
// decoration and the pool's live view applied.
//
// This duplicates connectapi's package-private toProtoExecutor on purpose: that
// mapping takes a connectapi.ExecutorRow, and exporting it to reach it from here
// would make two packages share one wire shape through a third type.
func protoExecutorRow(e executors.Executor, connectedAt time.Time, view installDisplay) *rafikiv1.ExecutorRow {
	out := &rafikiv1.ExecutorRow{
		Id:              e.ID,
		Machine:         e.Labels["machine"],
		Labels:          e.Labels,
		Isolation:       e.Isolation,
		WorkspaceMode:   e.WorkspaceMode,
		Roots:           e.Roots,
		Admits:          e.Admits,
		Enabled:         e.Enabled,
		Connected:       e.Connected,
		InstalledVia:    view.Via,
		InstalledTarget: view.Target,
		InstallMode:     view.Mode,
		LastLaunch:      view.LastLaunch,
	}
	if !connectedAt.IsZero() {
		out.ConnectedAt = timestamppb.New(connectedAt)
	}
	if !e.LastSeenAt.IsZero() {
		out.LastSeen = timestamppb.New(e.LastSeenAt)
	}
	return out
}

// installCallerFromContext resolves who is asking from the connection's
// identity — never from a request field, which carries no owner at all.
//
// A user credential contributes its own owner. A child credential contributes
// its child id plus its owner, and its display name comes from the child's own
// attested `owner` label: every child-provenance identity carries an EMPTY
// Username, and falling back to the daemon's OS user would stamp the wrong
// owner on the row (the sandboxOwnerName trap).
func installCallerFromContext(ctx context.Context, c *Controller) installCaller {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		// The unix socket's local trust: an unattributed caller owns nothing.
		return installCaller{}
	}
	caller := installCaller{OwnerUserID: id.UserID, OwnerName: id.Username}
	if id.Via == server.ProvenanceChildToken {
		caller.ChildID = id.ChildID
		if caller.OwnerName == "" && c != nil && c.st != nil && id.ChildID != "" {
			if snap, ok := c.st.Get(id.ChildID); ok {
				caller.OwnerName = snap.Labels["owner"]
			}
		}
	}
	return caller
}
