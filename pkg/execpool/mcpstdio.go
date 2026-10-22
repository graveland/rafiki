// SPDX-License-Identifier: Apache-2.0

package execpool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// mcpStdioStartTimeout bounds how long a dial waits for the executor to report
// that the process is running. A wedged executor must fail the dial rather than
// hang the child's first tool call.
const mcpStdioStartTimeout = 10 * time.Second

// MCPStdioTransport is an mcp.Transport that hosts one declared MCP server
// process on an executor and relays its stdio.
//
// It is the SDK's stdio seam and nothing more: the executor relays bytes, the
// daemon frames newline-delimited JSON-RPC, and neither side parses the other's
// protocol. One process exists for the life of one connection; closing the
// connection ends the stream, which is what makes the executor kill the
// process group.
type MCPStdioTransport struct {
	pool       *Pool
	executorID string
	name       string

	mu     sync.Mutex
	exited bool
	status string
	tail   string
}

// NewMCPStdioTransport builds the transport for one declared command on one
// executor. The executor ID is resolved ONCE, at spawn: a child's MCP server
// cannot hop machines mid-life.
func NewMCPStdioTransport(p *Pool, executorID, name string) *MCPStdioTransport {
	return &MCPStdioTransport{pool: p, executorID: executorID, name: name}
}

// Exited reports whether the executor OBSERVED the server process end, and how.
// It is a flag rather than an error identity on purpose: the SDK wraps and
// unwraps errors on the way up, so an error value would be a rule that
// silently stops holding after an SDK bump.
func (t *MCPStdioTransport) Exited() (status, tail string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status, t.tail, t.exited
}

func (t *MCPStdioTransport) noteExited(status, tail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exited, t.status, t.tail = true, status, tail
}

// Connect opens the MCPStdio stream, starts the declared process, and returns a
// Connection over its stdio. A spawn failure surfaces here, synchronously, as
// the dial error — the caller learns why before it begins its handshake.
func (t *MCPStdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if t.pool == nil {
		return nil, fmt.Errorf("execpool: mcp server %q: no executor pool", t.name)
	}
	client, err := t.pool.connectClientFor(t.executorID)
	if err != nil {
		return nil, fmt.Errorf("execpool: mcp server %q: %w", t.name, err)
	}
	// NOT the caller's context: the session outlives the CONNECT (and a redial
	// happens on a tool call's context, which is cancelled the moment that call
	// returns). The stream ends when the connection is closed, or when the
	// process exits — never because a handshake's ctx expired.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stream := client.MCPStdio(sctx)

	if err := stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Start{Start: &executorpb.MCPStdioStart{Name: t.name}},
	}); err != nil {
		cancel()
		return nil, t.dialError(err)
	}

	first, err := receiveWithTimeout(stream, mcpStdioStartTimeout)
	if err != nil {
		cancel()
		return nil, t.dialError(err)
	}
	if first.GetStarted() == nil {
		cancel()
		return nil, fmt.Errorf("execpool: mcp server %q: first response message was not started", t.name)
	}
	slog.Debug("execpool: mcp server started", "server", t.name, "executor", t.executorID,
		"pid", first.GetStarted().GetPid(), "program", first.GetStarted().GetProgram())

	pr, pw := io.Pipe()
	conn := &mcpStdioConn{
		stream: stream,
		pr:     pr,
		done:   make(chan struct{}),
		cancel: cancel,
	}
	go t.pump(conn, pw)

	// IOTransport is the SDK's newline-delimited-JSON seam; the same conn is
	// both halves so Close reaches the stream through either.
	inner, err := (&mcp.IOTransport{Reader: conn, Writer: conn}).Connect(ctx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("execpool: mcp server %q: %w", t.name, err)
	}
	return inner, nil
}

// dialError names a dial failure. An Unimplemented answer is a VERSION GAP, not
// a server failure: only a binary that implements MCPStdio can advertise a name
// for it, so this should be unreachable in practice — and when it is not, the
// operator needs to know the machine is running an older executor rather than
// chase a broken MCP server.
func (t *MCPStdioTransport) dialError(err error) error {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("execpool: mcp server %q: executor %s does not implement MCPStdio — it is older than this daemon, so update the executor: %w",
			t.name, t.executorID, err)
	}
	return fmt.Errorf("execpool: mcp server %q: %w", t.name, err)
}

// pump relay frames until the stream ends: stdout into the pipe the SDK reads,
// stderr into the health record, exit into the pipe's error.
func (t *MCPStdioTransport) pump(conn *mcpStdioConn, pw *io.PipeWriter) {
	var tail []byte
	for {
		msg, err := conn.stream.Receive()
		if err != nil {
			// No exit frame: the stream died without the executor OBSERVING the
			// process end — a disconnect, a rolling pod, a closed session. That
			// is an ambiguous death, which the caller may retry (unlike an
			// observed exit, which noteExited records).
			pw.CloseWithError(fmt.Errorf("mcp server %q: stream ended: %w", t.name, err))
			return
		}
		switch {
		case msg.GetStdout() != nil:
			// Written as it arrives: the executor does not know where lines
			// are and neither do we; IOTransport reassembles frames.
			if _, werr := pw.Write(msg.GetStdout().GetData()); werr != nil {
				return
			}
		case msg.GetStderr() != nil:
			chunk := msg.GetStderr().GetData()
			slog.Debug("execpool: mcp server stderr", "server", t.name, "executor", t.executorID,
				"data", string(chunk))
			tail = append(tail, chunk...)
			if len(tail) > mcpStdioStderrMax {
				tail = tail[len(tail)-mcpStdioStderrMax:]
			}
		case msg.GetExit() != nil:
			exit := msg.GetExit()
			status := mcpExitStatus(int(exit.GetCode()), exit.GetSignal())
			text := exit.GetStderrTail()
			if text == "" {
				text = string(tail)
			}
			t.noteExited(status, text)
			pw.CloseWithError(fmt.Errorf("mcp server %q exited (%s): %s", t.name, status, text))
			return
		}
	}
}

// mcpStdioStderrMax matches the executor's ring buffer: enough to carry the
// reason a server died, bounded because stderr is attacker-controlled text.
const mcpStdioStderrMax = 4 * 1024

// mcpExitStatus renders a process's fate the same way the executor does.
func mcpExitStatus(code int, signal string) string {
	if signal != "" {
		return "signal " + signal
	}
	return fmt.Sprintf("exit code %d", code)
}

// receiveWithTimeout is one Receive bounded by a deadline, so a wedged executor
// fails a dial instead of hanging it. A timeout is reported as a version gap:
// an executor that cannot answer at all is either wedged or older than this
// RPC.
func receiveWithTimeout(
	stream *connect.BidiStreamForClient[executorpb.MCPStdioRequest, executorpb.MCPStdioResponse],
	timeout time.Duration,
) (*executorpb.MCPStdioResponse, error) {
	type result struct {
		msg *executorpb.MCPStdioResponse
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := stream.Receive()
		ch <- result{msg: msg, err: err}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-time.After(timeout):
		// Unblock the pending Receive so the goroutine does not leak.
		_ = stream.CloseResponse()
		return nil, fmt.Errorf("no start from the executor within %v (is it wedged, or older than MCPStdio?)", timeout)
	}
}

// mcpStdioConn is the Connection's stdio: reads come from the pipe the pump
// fills, writes become stdin frames, and Close ends the stream.
type mcpStdioConn struct {
	stream *connect.BidiStreamForClient[executorpb.MCPStdioRequest, executorpb.MCPStdioResponse]
	pr     *io.PipeReader

	cancelOnce sync.Once
	cancel     context.CancelFunc
	done       chan struct{} // closed when the caller closes the connection
	writeMu    sync.Mutex
}

func (c *mcpStdioConn) Read(p []byte) (int, error) { return c.pr.Read(p) }

// Write sends one stdin chunk. Half-closing the request stream is what tells
// the executor the process's stdin is done, so the EOF case is a CloseRequest.
func (c *mcpStdioConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return 0, io.ErrClosedPipe
	default:
	}
	if err := c.stream.Send(&executorpb.MCPStdioRequest{
		Msg: &executorpb.MCPStdioRequest_Stdin{Stdin: append([]byte(nil), p...)},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close ends the session: the request stream is half-closed, the pipe reader is
// released, and the stream is cancelled — which is what makes the executor kill
// the process group. Idempotent.
func (c *mcpStdioConn) Close() error {
	c.cancelOnce.Do(func() {
		close(c.done)
		_ = c.stream.CloseRequest()
		c.cancel()
		c.pr.Close()
	})
	return nil
}
