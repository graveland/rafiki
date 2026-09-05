// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/adminpb/adminpbconnect"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// defaultReapGrace is the window between SIGTERM and SIGKILL, matching
// daraja's own stopLocked rather than inventing a second policy.
const defaultReapGrace = 3 * time.Second

// AdminOptions configures the machine-admin surface.
type AdminOptions struct {
	// SelfBinary is the path to this `rafiki` binary, which is re-executed as
	// `rafiki daraja serve`. daraja is a subcommand rather than a third
	// artifact, so the executor already has it.
	SelfBinary string
	// ChildBinary is the hosted child's binary, e.g. claude.
	ChildBinary string
	// LaunchKinds is the operator's declaration, from --launch.
	LaunchKinds []string
	// SocketDir is where daraja's unix sockets are created.
	SocketDir string
}

// launched is one daraja this executor started and is responsible for.
//
// A zero value (nil cmd) is a LAUNCH CLAIM in flight: Launch inserts it under
// the dup-check lock before cmd.Start and swaps in the real entry afterwards.
// Every reader must treat nil cmd as "nothing to signal yet" — the pgid field
// is still zero, and kill(-0, ...) targets the caller's own group.
type launched struct {
	cmd     *exec.Cmd
	pgid    int
	supDone chan struct{} // closed when supervise exits
}

// AdminServer launches, supervises and reaps darajas.
//
// It tracks what it launched because a process group id is RECYCLED once its
// group empties: signalling a number a peer handed us could reach an unrelated
// group. Reap therefore takes a child_id and resolves the pgid here.
type AdminServer struct {
	opts AdminOptions

	mu sync.Mutex
	m  map[string]*launched
}

func NewAdminServer(o AdminOptions) *AdminServer {
	return &AdminServer{opts: o, m: map[string]*launched{}}
}

func (a *AdminServer) Routes() (string, http.Handler) {
	return adminpbconnect.NewAdminServiceHandler(a)
}

// Close reaps everything still running and waits for the supervise goroutine
// to settle, so this process never leaves a stray daraja behind.
//
// Without waiting, supervise may still be executing its delete+syscall.Kill
// when Close returns, and TestLaunchGivesDarajaItsOwnGroup can strand a daraja
// whose group the executor cannot reach. The supervised goroutine holds no locks,
// so it proceeds without blocking on Close's own lock; we just wait for it to
// finish before returning.
func (a *AdminServer) Close() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.m))
	for id := range a.m {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	// reap's nil-cmd guard covers entries that are still launch claims: Close
	// must never signal one, because its pgid is zero and kill(-0, ...) would
	// signal this process's OWN group.
	for _, id := range ids {
		a.reap(id, defaultReapGrace)
	}
	// Wait until every supervise goroutine has finished. supervise runs outside
	// the lock (delete+syscall/Kill), so it makes progress regardless of who
	// calls Close; we join it here rather than dropping untracked goroutines.
	a.waitSupervise()
}

func (a *AdminServer) Launch(
	ctx context.Context, req *connect.Request[adminpb.LaunchRequest],
) (*connect.Response[adminpb.LaunchResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_id is required"))
	}
	kind, err := a.kindFor(req.Msg.GetSpec())
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	if _, dup := a.m[childID]; dup {
		a.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("child %s is already hosted here", childID))
	}
	// Claim the slot BEFORE starting the process. The dup check and the insert
	// used to be two separate lock acquisitions, so two concurrent Launches of
	// one child could both pass, both start a daraja, and the second's insert
	// would overwrite the first — leaving the first daraja running untracked
	// (Close would never signal it) while its supervise unlinked the second's
	// live socket. The claim makes the second Launch see AlreadyExists.
	claim := &launched{}
	a.m[childID] = claim
	a.mu.Unlock()

	c := req.Msg.GetSpec().GetClaude()
	argv := []string{
		"daraja", "serve",
	}
	// Pass the dial address: either a host:port (--connect) or a Unix path
	// (--connect-socket). The reverse-dialled connection replaces the old
	// direct-connect socket handle.
	if addr := req.Msg.GetDialAddr(); addr != "" {
		// If it looks like a path (starts with /), use --connect-socket;
		// otherwise treat it as host:port.
		if len(addr) > 0 && addr[0] == '/' {
			argv = append(argv, "--connect-socket", addr)
		} else {
			argv = append(argv, "--connect", addr)
		}
	}
	argv = append(argv, "--child-id", childID)
	argv = append(argv, "--binary", a.opts.ChildBinary)
	argv = append(argv, "--cwd", req.Msg.GetCwd())
	argv = append(argv, "--kind", kind)
	if c.GetModel() != "" {
		argv = append(argv, "--model", c.GetModel())
	}
	if c.GetResumeSession() != "" {
		argv = append(argv, "--resume", c.GetResumeSession())
	}
	if c.GetPermissionMode() != "" {
		argv = append(argv, "--permission-mode", c.GetPermissionMode())
	}

	// The ticket is one-shot auth for the daraja's reverse dial. It must not
	// travel in argv because every process on the machine can read it via ps —
	// set it in the child's environment instead. Replaced by a credential on
	// first successful hello.
	envVar := "RAFIKI_DARAJA_TICKET=" + req.Msg.GetTicket()

	cmd := exec.Command(a.opts.SelfBinary, argv...)
	cmd.Env = append(os.Environ(), envVar)
	// daraja LEADS a new group and its claude joins it, so this pgid is the one
	// handle that reaches the whole child — and keeps reaching claude after a
	// SIGKILLed daraja orphans it to launchd. Without Setpgid, daraja would sit
	// in the EXECUTOR's group and a reap would signal the executor itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Unset Stdout/Stderr route to /dev/null, which silently swallowed daraja's
	// own diagnostic output — including the connection-failure reason it logs
	// on exit — leaving an operator with no way to tell why a launch failed.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		a.mu.Lock()
		if a.m[childID] == claim {
			delete(a.m, childID)
		}
		a.mu.Unlock()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("daraja stderr pipe: %w", err))
	}

	if err := cmd.Start(); err != nil {
		// Release the claim: the slot must not outlive a failed start, or every
		// later Launch of this child is refused with AlreadyExists forever.
		a.mu.Lock()
		if a.m[childID] == claim {
			delete(a.m, childID)
		}
		a.mu.Unlock()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start daraja: %w", err))
	}
	pid := cmd.Process.Pid
	go logDarajaStderr(childID, stderr)

	// Swap the claim for the real entry. The entry is fully built BEFORE it
	// enters the map and is never mutated after, so reap's read of l.pgid
	// outside the lock is race-free; overwriting is safe because the claim
	// itself is what blocked any other claimant to this childID.
	done := make(chan struct{})
	l := &launched{cmd: cmd, pgid: pid, supDone: done}
	a.mu.Lock()
	a.m[childID] = l
	a.mu.Unlock()

	go a.supervise(childID, l)

	slog.Info("admin: launched daraja", "childID", childID, "pid", pid, "pgid", pid)
	return connect.NewResponse(&adminpb.LaunchResponse{
		Pid:  int32(pid),
		Pgid: int32(pid),
	}), nil
}

// Reap ends one launched daraja and its child, on demand.
func (a *AdminServer) Reap(
	ctx context.Context, req *connect.Request[adminpb.ReapRequest],
) (*connect.Response[adminpb.ReapResponse], error) {
	grace := time.Duration(req.Msg.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = defaultReapGrace
	}
	return connect.NewResponse(&adminpb.ReapResponse{
		Reaped: a.reap(req.Msg.GetChildId(), grace),
	}), nil
}

// reap signals the child's process group: SIGTERM, wait out grace, then
// SIGKILL. It mirrors daraja's own stopLocked rather than inventing a second
// escalation policy.
//
// It resolves the pgid from this executor's own launch table and never from the
// request, because a pgid is recycled once its group empties — signalling a
// number a peer supplied could reach an unrelated group. An unknown child_id is
// not an error: reaping something already gone is the normal case.
func (a *AdminServer) reap(childID string, grace time.Duration) bool {
	a.mu.Lock()
	l := a.m[childID]
	a.mu.Unlock()
	if l == nil {
		return false
	}
	// A nil cmd is a launch claim still in flight (see Launch): its pgid is
	// the zero value, and kill(-0, ...) would signal this process's OWN group —
	// never signal it. Close and Reap both arrive here; refusing is the honest
	// answer (nothing has been signalled) and the in-flight launch either
	// fills its entry moments later or fails and releases the claim.
	if l.cmd == nil {
		slog.Warn("admin: reap arrived while the launch was still in flight; nothing signalled",
			"childID", childID)
		return false
	}

	if err := syscall.Kill(-l.pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("admin: SIGTERM to child group failed", "childID", childID, "pgid", l.pgid, "error", err)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-l.pgid, 0); errors.Is(err, syscall.ESRCH) {
			slog.Info("admin: child group ended on SIGTERM", "childID", childID, "pgid", l.pgid)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	slog.Warn("admin: child group outlived its grace; escalating", "childID", childID, "pgid", l.pgid)
	if err := syscall.Kill(-l.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Error("admin: SIGKILL to child group failed", "childID", childID, "pgid", l.pgid, "error", err)
	}
	return true
}

// supervise waits on daraja, which is this process's DIRECT child, so it never
// zombies and its exit is logged.
//
// It cannot wait on claude: darwin has no PR_SET_CHILD_SUBREAPER, so a claude
// orphaned by a SIGKILLed daraja reparents to launchd rather than here. The
// process group is what covers that case, not this goroutine.
func (a *AdminServer) supervise(childID string, l *launched) {
	err := l.cmd.Wait()
	code := l.cmd.ProcessState.ExitCode()
	slog.Info("admin: daraja exited", "childID", childID, "pid", l.pgid, "exitCode", code, "error", err)

	// Dropping the entry is what stops a recycled pgid from being signalled
	// later: once the group is likely empty, this executor no longer claims it.
	a.mu.Lock()
	if a.m[childID] == l {
		delete(a.m, childID)
	}
	a.mu.Unlock()
	close(l.supDone)
}

// waitSupervise blocks until every supervise goroutine has exited.
// Called by Close after all reaps have been issued; joins goroutines that hold
// no locks, so they complete promptly. Skips nil entries (launch claims).
func (a *AdminServer) waitSupervise() {
	a.mu.Lock()
	for _, l := range a.m {
		if l != nil && l.supDone != nil {
			<-l.supDone
		}
	}
	a.mu.Unlock()
}

// kindFor validates the requested kind against the operator's declaration.
func (a *AdminServer) kindFor(spec *darajapb.ChildSpec) (string, error) {
	if spec.GetKind() != darajapb.Kind_KIND_CLAUDE {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported child kind %v", spec.GetKind()))
	}
	const kind = "claude"
	if !slices.Contains(a.opts.LaunchKinds, kind) {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this executor does not host %q; start it with --launch %s", kind, kind))
	}
	return kind, nil
}

// logDarajaStderr relays a launched daraja's stderr into the executor's own
// log, line by line, so its connection-failure diagnostics reach an operator
// instead of /dev/null. Ends when the pipe closes (the process exited).
func logDarajaStderr(childID string, stderr io.Reader) {
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		slog.Warn("daraja stderr", "childID", childID, "line", sc.Text())
	}
}
