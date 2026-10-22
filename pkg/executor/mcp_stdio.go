// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// ParseMCPCommandFlags parses repeated --mcp-command name=argv... declarations
// into the argv allowlist. This is the whole of what an executor will host as
// an MCP server process: the operator running this process decides what their
// own machine is willing to run, and nothing the daemon sends can widen it —
// a request carries a NAME, never an argv.
//
// The value grammar is whitespace-separated words with a backslash escaping a
// space, a backslash or a quote. No shell is involved, so a glob or $(…) in an
// argument is literal text.
func ParseMCPCommandFlags(args []string) (map[string][]string, error) {
	out := make(map[string][]string, len(args))
	for _, a := range args {
		name, raw, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("executor: --mcp-command %q: want name=argv", a)
		}
		if name == "" {
			return nil, fmt.Errorf("executor: --mcp-command %q: empty command name", a)
		}
		if raw == "" {
			return nil, fmt.Errorf("executor: --mcp-command %q: empty argv", a)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("executor: --mcp-command %q: duplicate command name %q", a, name)
		}
		argv, err := splitMCPCommandArgv(raw)
		if err != nil {
			return nil, fmt.Errorf("executor: --mcp-command %q: %w", a, err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("executor: --mcp-command %q: empty argv", a)
		}
		out[name] = argv
	}
	return out, nil
}

// splitMCPCommandArgv splits one declared argv on unescaped whitespace. It is
// deliberately not shlex: there is no quoting, no expansion and no comment
// syntax, because this string is a plist or unit-file value an operator writes
// by hand and a shell-flavoured parser would invite a shell-flavoured bug.
func splitMCPCommandArgv(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		in    bool
	)
	flush := func() {
		if in {
			words = append(words, cur.String())
			cur.Reset()
			in = false
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
			in = true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	flush()
	return words, nil
}

// MCPCommandNames returns the declared MCP command names, for
// DescribeResponse. Sorted so Describe is stable across calls.
func (s *Server) MCPCommandNames() []string {
	out := make([]string, 0, len(s.opts.MCPCommands))
	for name := range s.opts.MCPCommands {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Crash-loop guard and pump limits. The numbers are the design doc's: a
// shared laptop can have ten children retrying at once, so the budget is
// per NAME across every stream on this machine, not per stream.
const (
	// mcpQuickFailureWindow is how long a process must run to count as having
	// started successfully; a death sooner than this is a failure.
	mcpQuickFailureWindow = 5 * time.Second
	// mcpCrashLoopWindow is the window mcpCrashLoopThreshold failures are
	// counted over.
	mcpCrashLoopWindow = 60 * time.Second
	// mcpCrashLoopThreshold failures inside the window trip the cool-off.
	mcpCrashLoopThreshold = 3
	// mcpCrashLoopCoolOff is how long spawns are refused once tripped.
	mcpCrashLoopCoolOff = 60 * time.Second
	// mcpMaxStreamsPerName bounds concurrent streams for one declared name.
	mcpMaxStreamsPerName = 8
	// mcpStdoutChunk matches Proxy's relay chunk.
	mcpStdoutChunk = 32 << 10
	// mcpStderrRingMax is the same cap and intent as stderrTailMax in
	// admin.go: enough to carry the reason a server died, bounded because
	// stderr is attacker-controlled text.
	mcpStderrRingMax = 4 * 1024
	// mcpDrainGrace bounds how long the handler waits for the stdout and
	// stderr pumps to finish after the process exits, so a forked child
	// holding the pipes open past its parent's death cannot wedge the close.
	mcpDrainGrace = 500 * time.Millisecond
)

// mcpBudget is one declared name's spawn budget: the crash-loop record and
// the concurrent-stream count, shared by every stream hosting that name on
// this machine.
type mcpBudget struct {
	mu     sync.Mutex
	failAt []time.Time // spawn/quick-death failures, oldest first
	refuse time.Time   // spawns refused until this instant
	// tripped is how many failures opened the cool-off. failAt is cleared when
	// that happens (so recovery starts from a clean record), which is why the
	// count the refusal message reports has to be kept here — printing
	// len(failAt) told the operator "after 0 failures".
	tripped int
	lastErr string // the last failure's stderr tail, for the refusal
	active  int
}

// mcpBudget returns the budget record for one declared name, creating it on
// first use. Safe on a Server whose budget map was never initialized.
func (s *Server) mcpBudget(name string) *mcpBudget {
	s.mcpBudgetsMu.Lock()
	defer s.mcpBudgetsMu.Unlock()
	if s.mcpBudgets == nil {
		s.mcpBudgets = make(map[string]*mcpBudget)
	}
	b, ok := s.mcpBudgets[name]
	if !ok {
		b = &mcpBudget{}
		s.mcpBudgets[name] = b
	}
	return b
}

// acquire reserves one concurrent stream for name, or refuses with the reason.
// The caller must call the returned release exactly once.
func (b *mcpBudget) acquire(name string) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Before(b.refuse) {
		return nil, fmt.Errorf("mcp server %q is refusing new spawns for another %v after %d failures: %s",
			name, b.refuse.Sub(now).Round(time.Second), b.tripped, b.lastErr)
	}
	if b.active >= mcpMaxStreamsPerName {
		return nil, fmt.Errorf("mcp server %q already has %d live streams on this executor (limit %d)",
			name, b.active, mcpMaxStreamsPerName)
	}
	b.active++
	return func() {
		b.mu.Lock()
		b.active--
		b.mu.Unlock()
	}, nil
}

// noteFailure records a spawn failure or a quick death, tripping the cool-off
// once the threshold is reached inside the window.
func (b *mcpBudget) noteFailure(detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	kept := b.failAt[:0]
	for _, t := range b.failAt {
		if now.Sub(t) <= mcpCrashLoopWindow {
			kept = append(kept, t)
		}
	}
	b.failAt = append(kept, now)
	b.lastErr = detail
	if len(b.failAt) >= mcpCrashLoopThreshold {
		b.refuse = now.Add(mcpCrashLoopCoolOff)
		b.tripped = len(b.failAt)
		b.failAt = nil
		slog.Warn("executor: mcp server crash-looping; refusing new spawns",
			"failures", mcpCrashLoopThreshold, "coolOff", mcpCrashLoopCoolOff, "detail", detail)
	}
}

// noteExit feeds one process end into the crash-loop record.
//
// A kill THIS EXECUTOR performed is neither a failure nor a success: three
// ordinary child shutdowns must not quarantine the server for the whole
// machine, and our own kill says nothing about the server's health, so the
// record is left exactly as it was.
func (b *mcpBudget) noteExit(killedByUs bool, elapsed time.Duration, detail string) {
	switch {
	case killedByUs:
		return
	case elapsed < mcpQuickFailureWindow:
		b.noteFailure(fmt.Sprintf("exited after %v %s", elapsed.Round(time.Millisecond), detail))
	default:
		b.noteSuccess()
	}
}

// noteSuccess clears the failure record: a process that ran longer than
// mcpQuickFailureWindow and then died is not a crash loop.
func (b *mcpBudget) noteSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failAt = nil
	b.lastErr = ""
}

// mcpStderrRing keeps the last mcpStderrRingMax bytes of a server's stderr,
// so the exit frame can carry diagnostics for a caller that dropped frames.
type mcpStderrRing struct {
	mu  sync.Mutex
	buf []byte
}

func (r *mcpStderrRing) append(b []byte) {
	if len(b) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, b...)
	if len(r.buf) > mcpStderrRingMax {
		r.buf = r.buf[len(r.buf)-mcpStderrRingMax:]
	}
}

// tail returns the retained bytes as a string, invalid UTF-8 dropped at the
// front so a cut mid-rune does not render as a replacement character.
func (r *mcpStderrRing) tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buf
	for len(b) > 0 && b[0]&0xC0 == 0x80 {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "")
}

// mcpSink serializes sends on one stream. Connect's server-side BidiStream is
// not documented as safe for concurrent Send, and the stdout pump, the stderr
// pump and the exit frame all send.
//
// stderr uses TryLock: a stalled send must never let a chatty server deadlock a
// tool call, so a stderr frame is DROPPED rather than queued behind a blocked
// stdout write. The ring buffer is the record that survives.
type mcpSink struct {
	mu     sync.Mutex
	stream *connect.BidiStream[executorpb.MCPStdioRequest, executorpb.MCPStdioResponse]
}

func (s *mcpSink) send(msg *executorpb.MCPStdioResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(msg)
}

// trySend sends without waiting for the lock, reporting whether it did.
func (s *mcpSink) trySend(msg *executorpb.MCPStdioResponse) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.stream.Send(msg) == nil
}

// MCPStdio hosts one declared MCP server process for one client session and
// relays its stdio.
//
// The executor relays BYTES and never parses MCP: stdout is opaque and the
// caller frames it. The request carries only a name — the argv, env and cwd
// belong to this machine's operator (D1/D2) — and the name is checked before
// anything is spawned or even stat'd.
//
// One process exists for the life of its stream. Stream end (the caller going
// away, the child being killed, a pod rolling) kills the process group, so no
// server process outlives the session that asked for it.
func (s *Server) MCPStdio(
	ctx context.Context,
	stream *connect.BidiStream[executorpb.MCPStdioRequest, executorpb.MCPStdioResponse],
) error {
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mcp stdio: no start message: %w", err))
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("mcp stdio: first message must be a start"))
	}
	name := start.GetName()
	argv, ok := s.opts.MCPCommands[name]
	if !ok {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("mcp stdio: undeclared mcp command %q (this executor declares: %s)",
				name, strings.Join(s.MCPCommandNames(), ", ")))
	}

	budget := s.mcpBudget(name)
	release, err := budget.acquire(name)
	if err != nil {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("mcp stdio: %w", err))
	}
	defer release()

	// argv is copied: the declared slice is the operator's, and this process
	// must not be able to change what a later spawn runs.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = s.opts.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Dir = s.opts.Root
	// A server that forks (npx, a shell wrapper) dies whole only if it leads
	// its own process group: without Setpgid the group kill below would either
	// miss the grandchild or reach this executor's own group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// The pipes are built by hand rather than with cmd.StdinPipe/StdoutPipe/
	// StderrPipe: os/exec closes those on Wait, which discards whatever the
	// process wrote just before it died — and a dying server's last stderr
	// line is exactly the diagnostic this feature exists to carry (the same
	// reasoning as AdminServer.Launch's hand-built stderr pipe).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		budget.noteFailure(err.Error())
		return connect.NewError(connect.CodeInternal, fmt.Errorf("mcp stdio: %q: stdin pipe: %w", name, err))
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		budget.noteFailure(err.Error())
		return connect.NewError(connect.CodeInternal, fmt.Errorf("mcp stdio: %q: stdout pipe: %w", name, err))
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		budget.noteFailure(err.Error())
		return connect.NewError(connect.CodeInternal, fmt.Errorf("mcp stdio: %q: stderr pipe: %w", name, err))
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	closeAll := func() {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW} {
			f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		budget.noteFailure(err.Error())
		return connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("mcp stdio: spawning %q: %w", argv[0], err))
	}
	// The child holds its own copies now. The parent's copies of the three
	// child-side ends must go, or the pumps would never see EOF.
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	startedAt := time.Now()
	pid := cmd.Process.Pid
	slog.Debug("executor: mcp server started", "name", name, "program", argv[0], "pid", pid)

	// The process is this process's direct child, so Wait reaps it; it must be
	// reaped exactly once, hence the single goroutine.
	//
	// procMu/reaped enforce the one rule every signal below depends on: a pgid
	// belongs to this executor only while the group's leader is unreaped. Once
	// Wait has returned the kernel may reuse the pid, so a late kill could reach
	// a group that is somebody else's — the flag (set under the lock, read under
	// the same lock by every killer) is what makes "no signal after the wait" an
	// invariant rather than a timing argument.
	var (
		procMu sync.Mutex
		reaped bool
	)
	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		procMu.Lock()
		reaped = true
		procMu.Unlock()
		waitCh <- err
	}()

	// Stream end kills the process group, SIGTERM then SIGKILL after the same
	// grace AdminServer.reap uses. This covers the caller disconnecting, the
	// child being killed and a pod rolling.
	//
	// THE WATCHER IS STOPPED THE INSTANT THE PROCESS IS REAPED (stopWatcher,
	// below). A pgid belongs to this executor only while the group's leader is
	// unreaped: once cmd.Wait returns, the kernel may hand that pid to a new
	// process, which becomes the leader of a group that is somebody else's. So
	// no signal is ever sent after the wait — every kill below happens either
	// before it or immediately after it, and never from a goroutine that could
	// fire minutes later.
	watcherDone := make(chan struct{})
	var stopOnce sync.Once
	stopWatcher := func() { stopOnce.Do(func() { close(watcherDone) }) }
	defer stopWatcher()
	// killedByUs records that this executor ended the process rather than the
	// process dying on its own. It is what keeps a normal stream end (the daemon
	// disconnecting, the child being killed) from counting as a CRASH: three
	// ordinary child shutdowns must not trip the crash-loop budget.
	var killedByUs atomic.Bool
	kill := func(grace time.Duration) {
		procMu.Lock()
		defer procMu.Unlock()
		if reaped {
			return
		}
		killedByUs.Store(true)
		killMCPGroup(pid, grace)
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-watcherDone:
		}
		if ctx.Err() != nil {
			kill(child.TermGrace)
		}
	}()

	sink := &mcpSink{stream: stream}
	// started is sent before any stdout frame: the caller learns the spawn
	// succeeded before it begins its handshake.
	if err := sink.send(&executorpb.MCPStdioResponse{
		Msg: &executorpb.MCPStdioResponse_Started{Started: &executorpb.MCPStdioStarted{
			Pid:     int32(pid),
			Program: argv[0],
		}},
	}); err != nil {
		// The pumps have not started, so the parent's remaining pipe ends are
		// nobody else's to close: leaving them would leak three fds per stream
		// that failed at its first frame.
		kill(child.TermGrace)
		<-waitCh
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return err
	}

	ring := &mcpStderrRing{}
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		buf := make([]byte, mcpStdoutChunk)
		for {
			n, rerr := stdoutR.Read(buf)
			if n > 0 {
				if err := sink.send(&executorpb.MCPStdioResponse{
					Msg: &executorpb.MCPStdioResponse_Stdout{Stdout: &executorpb.MCPStdioStdout{
						Data: append([]byte(nil), buf[:n]...),
					}},
				}); err != nil {
					// The stream is gone; the kill watcher below ends the
					// process. Stop reading rather than discard into a
					// dead stream.
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		defer pumps.Done()
		buf := make([]byte, 16<<10)
		for {
			n, rerr := stderrR.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				ring.append(chunk)
				sink.trySend(&executorpb.MCPStdioResponse{
					Msg: &executorpb.MCPStdioResponse_Stderr{Stderr: &executorpb.MCPStdioStderr{Data: chunk}},
				})
			}
			if rerr != nil {
				return
			}
		}
	}()

	// The stdin pump runs on its own goroutine: a PROCESS exit must end the
	// stream even while the caller still holds the request stream open, which
	// is the ordinary MCP case (the client keeps stdin open for its whole
	// session, and the server crashes).
	stdinDone := make(chan error, 1)
	go func() {
		defer stdinW.Close() // a half-close closes the process's stdin
		for {
			msg, rerr := stream.Receive()
			if rerr != nil {
				// io.EOF is the ordinary half-close: the caller is done
				// sending and the process may finish on its own. Anything else
				// is the caller going away, which the ctx watcher turns into a
				// group kill.
				if errors.Is(rerr, io.EOF) {
					stdinDone <- nil
				} else {
					stdinDone <- nil
					slog.Debug("executor: mcp stdio stream ended", "name", name, "error", rerr)
				}
				return
			}
			if msg.GetStart() != nil {
				// The daemon is the only caller, so a second start is a bug
				// loud enough to name rather than a condition to tolerate.
				stdinDone <- connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("mcp stdio: %q: start sent more than once", name))
				return
			}
			if data := msg.GetStdin(); len(data) > 0 {
				if _, werr := stdinW.Write(data); werr != nil {
					// A write to a dead process's stdin is the process being
					// gone, which the exit frame reports properly.
					slog.Debug("executor: mcp stdio stdin write failed", "name", name, "error", werr)
					stdinDone <- nil
					return
				}
			}
		}
	}()

	// Wait for the process, the caller going away, or a protocol violation —
	// whichever happens first.
	var (
		waitErr error
		pumpErr error
	)
	select {
	case waitErr = <-waitCh:
	case pumpErr = <-stdinDone:
		if pumpErr == nil {
			// The caller half-closed; the process may still be running, and
			// killing it now would truncate a server that answers on EOF.
			select {
			case waitErr = <-waitCh:
			case <-ctx.Done():
				kill(child.TermGrace)
				waitErr = <-waitCh
			}
		} else {
			kill(child.TermGrace)
			waitErr = <-waitCh
		}
	case <-ctx.Done():
		kill(child.TermGrace)
		waitErr = <-waitCh
	}

	// The process is reaped. Stop the stream-end watcher NOW, so a cancellation
	// arriving during the drain below cannot signal a pid the kernel is free to
	// reuse.
	stopWatcher()

	// The leader is gone, but anything it FORKED may still be alive and holding
	// the pipes — an MCP server that spawns a helper would otherwise outlive the
	// session that asked for it, which is exactly what D6 forbids. Sweep the
	// group now (immediately, before the pid can be recycled): SIGTERM, a short
	// grace, SIGKILL. An empty group answers ESRCH and costs one syscall.
	//
	// The sweep runs BEFORE the drain, so a surviving child releasing the pipes
	// lets the pumps finish normally instead of being cut off by the grace.
	sweepMCPGroup(pid)

	// Give the stdout and stderr pumps a bounded drain so their last frames
	// are not lost to the exit frame racing them.
	drained := make(chan struct{})
	go func() { pumps.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(mcpDrainGrace):
	}
	stdoutR.Close()
	stderrR.Close()
	stdinW.Close()

	code := -1
	signal := ""
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			signal = ws.Signal().String()
		}
	}
	tail := ring.tail()
	budget.noteExit(killedByUs.Load(), time.Since(startedAt),
		fmt.Sprintf("(%s): %s", mcpExitStatus(code, signal), tail))
	slog.Info("executor: mcp server exited", "name", name, "pid", pid, "exitCode", code, "signal", signal, "error", waitErr)

	if pumpErr != nil {
		return pumpErr
	}
	if err := sink.send(&executorpb.MCPStdioResponse{
		Msg: &executorpb.MCPStdioResponse_Exit{Exit: &executorpb.MCPStdioExit{
			Code:       int32(code),
			Signal:     signal,
			StderrTail: tail,
		}},
	}); err != nil {
		return err
	}
	return nil
}

// mcpExitStatus renders a process's fate for a message a model may read.
func mcpExitStatus(code int, signal string) string {
	if signal != "" {
		return "signal " + signal
	}
	return fmt.Sprintf("exit code %d", code)
}

// sweepMCPGroup removes anything left in a finished process group.
//
// It is called the instant the leader's wait returns, and that instant is the
// whole point: a process group's id is not reused while the group still has
// members, so signalling is safe exactly while something is left to signal —
// which is the case this exists for. The residual risk is the empty group,
// where the pid could in principle have been recycled into a NEW group in the
// microseconds since the reap; an empty group answers ESRCH, and the window is
// two adjacent instructions, which is the same trade Reap makes. It is kept
// SHORT for the same reason: a long grace here would widen what little window
// there is.
func sweepMCPGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return // ESRCH: the group is already empty, which is the normal case
	}
	deadline := time.Now().Add(mcpDrainGrace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("executor: SIGKILL to leftover mcp server group failed", "pgid", pgid, "error", err)
	}
}

// killMCPGroup ends a process group: SIGTERM, wait out grace, SIGKILL. It is
// the same escalation AdminServer.reap uses rather than a second invented
// policy.
func killMCPGroup(pgid int, grace time.Duration) {
	if pgid <= 0 {
		// Never signal group 0: that is this process's own group.
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Debug("executor: SIGTERM to mcp server group failed", "pgid", pgid, "error", err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("executor: SIGKILL to mcp server group failed", "pgid", pgid, "error", err)
	}
}
