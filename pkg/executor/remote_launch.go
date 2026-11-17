// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// The direct-dial half of the launcher's ssh surface. RemoteServe carries a
// remote executor's stdio for a whole session; these two calls instead start a
// DETACHED executor that dials the daemon itself, and read back the log it
// writes, because a detached process's only diagnostic is its file.
//
// The ticket is the one secret here, and it travels in exactly one way: the
// bytes of a client-streaming frame, written to ssh's stdin. It is never an
// argv element, never part of a script, never an environment variable, and
// never a field of any RPC message — and because a target's shell can echo
// what it read, every byte of text this call returns is scrubbed of it.

const (
	// remoteLaunchStdoutMax bounds what a launch's stdout may be parsed from.
	// `executor serve --detach` prints exactly one line, so a larger answer
	// means the target ran something else and must not be buffered whole.
	remoteLaunchStdoutMax = 4 << 10

	// remoteLogTailMax bounds the log tail handed to a daemon. It keeps the END
	// of the log, which is where the failure is.
	remoteLogTailMax = 16 << 10

	// remoteLogTailDefaultLines is what a request of 0 asks for.
	remoteLogTailDefaultLines = 40

	// remoteLogTailMaxLines bounds a request's own line count.
	remoteLogTailMaxLines = 200
)

// remoteLaunchScript is the one command a direct launch runs on the target: a
// detached, self-dialling executor that reads its ticket from stdin. Every
// field is a validated typed value rendered through shellQuote (or, for
// binary_path, remoteBinaryExpr), so nothing caller-supplied can become shell
// syntax. `exec` replaces the shell, so the exit status is the executor's.
func remoteLaunchScript(binaryPath, instance, root, installID, addr string) string {
	s := `exec ` + remoteBinaryExpr(binaryPath) + ` executor serve --connect ` + shellQuote(addr) +
		` -P ` + shellQuote(instance)
	if root != "" {
		s += ` --root ` + shellQuote(root)
	}
	return s + ` --install-id ` + shellQuote(installID) + ` --ticket-stdin --detach`
}

// scrubTicket removes the ticket from any text this file returns or logs. ssh's
// stderr is returned verbatim because that is where the operator's answer lives
// ("Host key verification failed"), but a target that echoes its stdin would
// otherwise put the ticket in the daemon's error and its logs.
func scrubTicket(text, ticket string) string {
	if ticket == "" {
		return text
	}
	return strings.ReplaceAll(text, ticket, "[redacted]")
}

// runSSHScriptTicket runs one ssh with the ticket as the only thing on its
// stdin. The write happens before Wait, so ssh has a line to hand the remote
// shell; a failed write is ignored because ssh's own stderr explains the exit.
func (s *Server) runSSHScriptTicket(ctx context.Context, destination, script, ticket string) (stdout, stderr []byte, err error) {
	cmd, err := s.sshCommand(ctx, destination, script)
	if err != nil {
		return nil, nil, err
	}
	var out, errBuf cappedBuffer
	out.max = remoteLaunchStdoutMax
	errBuf.max = stderrTailMax
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	if _, werr := stdin.Write([]byte(ticket + "\n")); werr != nil {
		// Never the ticket itself, only the fact that the write failed.
		slog.Debug("executor: remote launch ticket write failed", "destination", destination, "error", werr)
	}
	_ = stdin.Close()
	err = cmd.Wait()
	return out.Bytes(), errBuf.Bytes(), err
}

// parseRemoteLaunchOutput accepts exactly the one line `executor serve
// --detach` prints: `started pid=<n>` for a launch that re-exec'd itself, or
// `running pid=<n>` for one that found the instance already held. Anything else
// is a target running something other than the binary it was told to run.
func parseRemoteLaunchOutput(out string) (*executorpb.RemoteLaunchResponse, error) {
	line := strings.TrimSuffix(out, "\n")
	if strings.Contains(line, "\n") {
		return nil, fmt.Errorf("expected one line")
	}
	state, rest := "", ""
	switch {
	case strings.HasPrefix(line, "started pid="):
		state, rest = "started", strings.TrimPrefix(line, "started pid=")
	case strings.HasPrefix(line, "running pid="):
		state, rest = "running", strings.TrimPrefix(line, "running pid=")
	default:
		return nil, fmt.Errorf("line is not started pid=<n> or running pid=<n>")
	}
	pid, err := strconv.Atoi(rest)
	if err != nil || pid <= 0 || pid > math.MaxInt32 {
		return nil, fmt.Errorf("pid %q is not a positive number", rest)
	}
	return &executorpb.RemoteLaunchResponse{State: state, Pid: int32(pid)}, nil
}

// RemoteLaunch starts a detached, self-dialling executor on the target over
// ssh. The frames are: one `start`, then exactly one `stdin` carrying the
// install ticket. Refusing a second stdin frame matters: two tickets would mean
// two remote executors and one spent credential.
//
// The ticket cannot be launched without an address to dial — a launcher that
// reaches the daemon over a unix socket or the sandbox relay has none, and
// there is nothing to hand the remote executor as its --connect.
func (s *Server) RemoteLaunch(
	ctx context.Context,
	stream *connect.ClientStream[executorpb.RemoteLaunchRequest],
) (*connect.Response[executorpb.RemoteLaunchResponse], error) {
	if err := s.refuseSSHLaunch(); err != nil {
		return nil, err
	}
	start, err := receiveRemoteLaunchFrame(stream, "start")
	if err != nil {
		return nil, err
	}
	if start.GetStart() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("remote launch: first frame must be a start"))
	}
	msg := start.GetStart()
	target := msg.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteBinaryPath(msg.GetBinaryPath(), false); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteRoot(msg.GetRoot()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteInstallID(msg.GetInstallId()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	addr := s.opts.ConnectAddr
	if addr == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("this launcher dials the daemon by no address, so it cannot start a self-dialling executor"))
	}

	ticketFrame, err := receiveRemoteLaunchFrame(stream, "ticket")
	if err != nil {
		return nil, err
	}
	ticket := strings.TrimRight(string(ticketFrame.GetStdin()), "\r\n")
	if ticket == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("remote launch: the ticket frame is empty"))
	}
	// TrimRight takes only the frame's own line ending off. A newline that is
	// still there would be a SECOND line on the remote shell's stdin, reaching
	// the executor's stdin, and scrubbing the echoed ticket would then have to
	// match it in pieces.
	if strings.ContainsAny(ticket, "\r\n") {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("remote launch: the ticket must be one line"))
	}
	// The client half-closes after the ticket, so a further frame is a bug loud
	// enough to name — and it is refused BEFORE any ssh runs, because a second
	// launch would spend a second ticket.
	if stream.Receive() {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("remote launch: stdin sent more than once"))
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	stdout, stderr, err := s.runSSHScriptTicket(ctx, target.GetDestination(),
		remoteLaunchScript(msg.GetBinaryPath(), target.GetInstance(), msg.GetRoot(), msg.GetInstallId(), addr),
		ticket)
	if err != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		fail := sshFailure(target.GetDestination(), stderr, err)
		return nil, connect.NewError(connect.CodeUnavailable, errors.New(scrubTicket(fail.Error(), ticket)))
	}

	resp, perr := parseRemoteLaunchOutput(string(stdout))
	if perr != nil {
		// The scrub runs on the RAW output, before %q quotes it: quoting first
		// would render a quote or a backslash inside the ticket escaped, so the
		// scrub would no longer match and the ticket would ride out in pieces.
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("remote launch: %v; raw output: %q", perr, scrubTicket(string(stdout), ticket)))
	}
	slog.Debug("executor: remote launch", "destination", target.GetDestination(),
		"installId", msg.GetInstallId(), "state", resp.GetState(), "pid", resp.GetPid())
	return connect.NewResponse(resp), nil
}

// receiveRemoteLaunchFrame reads one frame, answering InvalidArgument with a
// message naming what was missing. A stream that ends (or fails) before the
// frame arrives is the caller's protocol error, not a remote failure.
func receiveRemoteLaunchFrame(
	stream *connect.ClientStream[executorpb.RemoteLaunchRequest], what string,
) (*executorpb.RemoteLaunchRequest, error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("remote launch: no %s frame: %w", what, err))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("remote launch: no %s frame", what))
	}
	return stream.Msg(), nil
}

// remoteLogTailScript prints the last lines of the target's executor.log, or
// says the file is not there. A missing log is an answer, not an error: the
// launch that produced it may be exactly the one that failed before writing
// anything. The sentinel is printed with no trailing newline, so it is the
// whole response rather than the last line of one.
func remoteLogTailScript(instance string, lines int) string {
	return remoteDataDirScript(instance) +
		`; if [ -f "$d/executor.log" ]; then tail -n ` + strconv.Itoa(lines) +
		` "$d/executor.log" 2>&1 || true; else printf '%s' '(no executor.log)'; fi`
}

// remoteLogTailLines clamps a request's line count: 0 means the default, and
// nothing asks for fewer than one line or more than the cap.
func remoteLogTailLines(lines int32) int {
	switch {
	case lines <= 0:
		if lines == 0 {
			return remoteLogTailDefaultLines
		}
		return 1
	case int(lines) > remoteLogTailMaxLines:
		return remoteLogTailMaxLines
	default:
		return int(lines)
	}
}

// clampTail keeps the last remoteLogTailMax bytes of a log tail. A cut can land
// mid-rune, and a protobuf string field must be valid UTF-8, so the partial
// leading rune is dropped.
func clampTail(tail string) string {
	if len(tail) <= remoteLogTailMax {
		return tail
	}
	tail = tail[len(tail)-remoteLogTailMax:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

// RemoteLogTail returns the last lines of the target's executor.log, so a failed
// direct launch is diagnosed by the remote executor's own words instead of the
// silence a detached process leaves.
func (s *Server) RemoteLogTail(
	ctx context.Context,
	req *connect.Request[executorpb.RemoteLogTailRequest],
) (*connect.Response[executorpb.RemoteLogTailResponse], error) {
	if err := s.refuseSSHLaunch(); err != nil {
		return nil, err
	}
	target := req.Msg.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	stdout, stderr, err := s.runSSHScript(ctx, target.GetDestination(),
		remoteLogTailScript(target.GetInstance(), remoteLogTailLines(req.Msg.GetLines())))
	if err != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		return nil, connect.NewError(connect.CodeUnavailable, sshFailure(target.GetDestination(), stderr, err))
	}
	return connect.NewResponse(&executorpb.RemoteLogTailResponse{Tail: clampTail(string(stdout))}), nil
}
