// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
type launched struct {
	cmd    *exec.Cmd
	pgid   int
	socket string
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

// Close reaps everything still running. The executor's own shutdown must not
// strand a daraja: nothing else on this machine knows the pgid.
func (a *AdminServer) Close() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.m))
	for id := range a.m {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.reap(id, defaultReapGrace)
	}
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
	_, dup := a.m[childID]
	a.mu.Unlock()
	if dup {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("child %s is already hosted here", childID))
	}

	socket := filepath.Join(a.opts.SocketDir, "daraja-"+childID+".sock")
	_ = os.Remove(socket)

	c := req.Msg.GetSpec().GetClaude()
	argv := []string{
		"daraja", "serve",
		"--socket", socket,
		"--binary", a.opts.ChildBinary,
		"--cwd", req.Msg.GetCwd(),
		"--kind", kind,
	}
	if c.GetModel() != "" {
		argv = append(argv, "--model", c.GetModel())
	}
	if c.GetResumeSession() != "" {
		argv = append(argv, "--resume", c.GetResumeSession())
	}
	if c.GetPermissionMode() != "" {
		argv = append(argv, "--permission-mode", c.GetPermissionMode())
	}

	cmd := exec.Command(a.opts.SelfBinary, argv...)
	// daraja LEADS a new group and its claude joins it, so this pgid is the one
	// handle that reaches the whole child — and keeps reaching claude after a
	// SIGKILLed daraja orphans it to launchd. Without Setpgid, daraja would sit
	// in the EXECUTOR's group and a reap would signal the executor itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start daraja: %w", err))
	}
	pid := cmd.Process.Pid

	l := &launched{cmd: cmd, pgid: pid, socket: socket}
	a.mu.Lock()
	a.m[childID] = l
	a.mu.Unlock()

	go a.supervise(childID, l)

	slog.Info("admin: launched daraja", "childID", childID, "pid", pid, "pgid", pid, "socket", socket)
	return connect.NewResponse(&adminpb.LaunchResponse{
		Pid:    int32(pid),
		Pgid:   int32(pid),
		Socket: socket,
	}), nil
}

// Reap arrives in Task 11. The generated AdminServiceHandler interface requires
// the method; until then it refuses, so a caller gets an explicit "not yet"
// rather than a silent no-op.
func (a *AdminServer) Reap(
	_ context.Context, _ *connect.Request[adminpb.ReapRequest],
) (*connect.Response[adminpb.ReapResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Reap is not implemented yet"))
}

// reap ends one launched daraja and its child.
//
// Task 10 ships the SIGKILL-only form, so Close() compiles and is correct from
// day one: an executor's own shutdown must not strand a daraja, and SIGKILL to
// the group reaches daraja and the claude that joined it immediately — the
// direct-child cmd.Wait in supervise reaps the corpse. Task 11 replaces this
// wholesale with the SIGTERM→grace→SIGKILL version the Reap RPC wants; the
// signature is already final so that replacement is drop-in.
func (a *AdminServer) reap(childID string, grace time.Duration) bool {
	_ = grace // consumed by Task 11's SIGTERM window; SIGKILL-only has none
	a.mu.Lock()
	l, ok := a.m[childID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	// Negative pid: the whole process group, daraja plus the claude that joined
	// it. ESRCH means the group is already gone, which is success for us.
	if err := syscall.Kill(-l.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("admin: reap could not signal the group",
			"childID", childID, "pgid", l.pgid, "error", err)
		return false
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
	_ = os.Remove(l.socket)
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
