package execpool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Pool is the live executor registry. It holds connections; the DATABASE holds
// identity. Nothing authoritative is cached here beyond the last Describe and
// Health, both of which are self-reported and therefore non-authoritative by
// construction.
const parkTimeout = 5 * time.Minute

type Pool struct {
	mu     sync.RWMutex
	store  executors.Store
	live   map[string]*liveConn
	parked map[string]*parkedEntry
	onLost func(executorID string) // fired when a park expires; see SetOnLost
}

type liveConn struct {
	executor executors.Executor
	describe *executorpb.DescribeResponse
	client   *executorClient
	done     chan struct{}
}

// New creates a Pool backed by the executor store.
func New(store executors.Store) *Pool {
	return &Pool{
		store:  store,
		live:   make(map[string]*liveConn),
		parked: make(map[string]*parkedEntry),
	}
}

// Serve accepts executor connections on ln, authenticating each against the
// store and promoting them to pool entries. It blocks until ln is closed.
func (p *Pool) Serve(ln net.Listener) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.parkSweep(ctx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handleConn(conn)
	}
}

func (p *Pool) handleConn(conn net.Conn) {
	defer conn.Close()

	// Set a read deadline for the hello frame — a silent client must not
	// wedge the accept loop (see pkg/control/server.go:346-400).
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	hello, err := readHelloFrame(conn)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		slog.Warn("execpool: hello read failed", "error", err)
		return
	}

	var e executors.Executor
	var credential string
	ctx := context.Background()

	if hello.Token != "" {
		// First enrollment.
		e, credential, err = p.store.Enroll(ctx, hello.Token, hello.SelfReported)
		if err != nil {
			writeHelloError(conn, err.Error())
			return
		}
		writeHelloResponse(conn, protocol.ExecutorHelloResponse{
			Type:       "executor_hello",
			ExecutorID: e.ID,
			Credential: credential,
		})
	} else if hello.Credential != "" {
		e, err = p.store.Authenticate(ctx, hello.Credential)
		if err != nil {
			writeHelloError(conn, err.Error())
			return
		}
		writeHelloResponse(conn, protocol.ExecutorHelloResponse{
			Type:       "executor_hello",
			ExecutorID: e.ID,
		})
	} else {
		writeHelloError(conn, "no token or credential in hello")
		return
	}

	httpClient, err := ClientForConn(conn)
	if err != nil {
		slog.Warn("execpool: client for conn failed", "error", err, "executorId", e.ID)
		return
	}

	cl := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")
	desc, err := cl.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		slog.Warn("execpool: Describe failed on join", "error", err, "executorId", e.ID)
		return
	}

	_ = p.store.TouchSeen(ctx, e.ID)

	// If this executor was parked, reattach it.
	p.reattach(e.ID)

	lc := &liveConn{
		executor: e,
		describe: desc.Msg,
		client:   &executorClient{inner: cl},
		done:     make(chan struct{}),
	}

	p.mu.Lock()
	p.live[e.ID] = lc
	p.mu.Unlock()

	slog.Info("execpool: executor joined", "id", e.ID, "displayName", e.DisplayName, "tools", desc.Msg.Tools)

	// Poll Health every 30s.
	go p.healthLoop(ctx, e.ID, lc)

	// Block until the connection is done (ServeInverted-style, but since
	// we're the HTTP client, we detect departure when health fails).
	<-lc.done

	p.mu.Lock()
	delete(p.live, e.ID)
	p.mu.Unlock()
	slog.Info("execpool: executor left", "id", e.ID)
}

// Live returns all currently connected executors with their last Describe info.
func (p *Pool) Live() []LiveExecutor {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []LiveExecutor
	for _, lc := range p.live {
		out = append(out, LiveExecutor{
			Executor: lc.executor,
			Describe: lc.describe,
		})
	}
	return out
}

// LiveExecutor carries a connected executor's current state.
type LiveExecutor struct {
	Executor executors.Executor
	Describe *executorpb.DescribeResponse
}

// ClientFor returns a tools.ExecutorClient for executorID, or an error if
// the executor is not currently connected.
func (p *Pool) ClientFor(executorID string) (tools.ExecutorClient, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if lc, ok := p.live[executorID]; ok {
		return lc.client, nil
	}
	// Parked and lost are different answers, and the caller acts differently on
	// each: a parked executor may still come back inside its timeout, so the
	// child should wait for it; a lost one never will, so the child has to be
	// rehomed or failed. Collapsing both into one "not connected" string makes
	// that undecidable at the call site, which is what the typed sentinels in
	// departure.go exist to prevent.
	if _, parked := p.parked[executorID]; parked {
		return nil, fmt.Errorf("executor %s: %w", executorID, ErrParked)
	}
	return nil, fmt.Errorf("executor %s: %w", executorID, ErrExecutorLost)
}

// ClientForWorkspace returns a tools.ExecutorClient for executorID whose
// Execute calls are scoped to workspaceID. The connection is shared; the
// workspace is not. Returning the pool's shared client for a workspaced child
// would run its tools in whatever workspace the last caller happened to use —
// a cross-child data leak that no test would catch, because both children
// usually see plausible files.
func (p *Pool) ClientForWorkspace(executorID, workspaceID string) (tools.ExecutorClient, error) {
	base, err := p.ClientFor(executorID)
	if err != nil {
		return nil, err
	}
	ec, ok := base.(*executorClient)
	if !ok {
		return nil, fmt.Errorf("execpool: client is not an *executorClient")
	}
	return &workspaceClient{executorClient: ec, workspaceID: workspaceID}, nil
}

// workspaceClient wraps an executorClient, adding a workspace id to every
// Execute and StartJob call.
type workspaceClient struct {
	*executorClient
	workspaceID string
}

func (c *workspaceClient) Execute(ctx context.Context, tool string, input json.RawMessage) (string, error) {
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        tool,
		InputJson:   input,
		TimeoutMs:   600_000,
		WorkspaceId: c.workspaceID,
	}))
	if err != nil {
		return "", fmt.Errorf("executor execute: %w", err)
	}
	defer stream.Close()

	var resultText string
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			return "", fmt.Errorf("executor: %s", ev.Failed.Message)
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor stream: %w", err)
	}
	return resultText, nil
}

func (c *workspaceClient) StartJob(ctx context.Context, command string) (string, error) {
	input, _ := json.Marshal(map[string]string{"command": command})
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "bash",
		InputJson:   input,
		Background:  true,
		WorkspaceId: c.workspaceID,
	}))
	if err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	defer stream.Close()
	var handle string
	for stream.Receive() {
		if h := stream.Msg().GetHandle(); h != "" {
			handle = h
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	if handle == "" {
		return "", fmt.Errorf("executor start job: no handle returned")
	}
	return handle, nil
}

func (p *Pool) healthLoop(ctx context.Context, id string, lc *liveConn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.healthCheck(ctx, id, lc); err != nil {
				slog.Warn("execpool: health check failed; parking executor", "executorId", id, "error", err)
				p.onHealthFailure(id, lc)
				return
			}
		case <-lc.done:
			return
		}
	}
}

// onHealthFailure drops a failed executor from the live set and parks it,
// giving it parkTimeout to reconnect before its children are declared lost.
//
// The live-set delete and the Park are deliberately NOT one critical section.
// Park takes p.mu itself and sync.RWMutex is not reentrant, so holding the lock
// across the call deadlocks this goroutine WHILE IT HOLDS the pool lock — every
// later Live(), ClientFor() and accept blocks behind it, and one unwell
// executor takes the whole executor plane down. Splitting the lock also loses
// nothing: Park re-checks p.live and no-ops if the executor reconnected in the
// gap, which is the correct outcome rather than a race.
func (p *Pool) onHealthFailure(id string, lc *liveConn) {
	p.mu.Lock()
	delete(p.live, id)
	p.mu.Unlock()

	p.Park(id, parkTimeout)
	close(lc.done)
}

func (p *Pool) healthCheck(ctx context.Context, id string, lc *liveConn) error {
	_, err := lc.client.inner.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
	if err != nil {
		return err
	}
	_ = p.store.TouchSeen(ctx, id)
	return nil
}

// ─── hello frame read, byte-at-a-time ──────────────────────────────────────

// readHelloFrame reads the newline-delimited hello frame from conn ONE BYTE
// AT A TIME. Unlike the control listener which reuses its bufio.Reader
// (because the client pipelines its first request behind auth), here
// everything after the hello frame is HTTP/2 framing that http2.Transport
// must read itself. A buffered reader that consumes past the newline leaves
// the transport starting mid-frame, and the connection dies with an
// unhelpful protocol error.
func readHelloFrame(conn net.Conn) (protocol.ExecutorHelloRequest, error) {
	var buf [4096]byte
	n := 0
	for {
		if n >= len(buf) {
			return protocol.ExecutorHelloRequest{}, fmt.Errorf("hello frame exceeds %d bytes", len(buf))
		}
		if _, err := io.ReadFull(conn, buf[n:n+1]); err != nil {
			return protocol.ExecutorHelloRequest{}, fmt.Errorf("read hello: %w", err)
		}
		if buf[n] == '\n' {
			break
		}
		n++
	}
	var req protocol.ExecutorHelloRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return protocol.ExecutorHelloRequest{}, fmt.Errorf("parse hello: %w", err)
	}
	return req, nil
}

func writeHelloResponse(conn net.Conn, resp protocol.ExecutorHelloResponse) {
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	conn.Write(b) //nolint:errcheck
}

func writeHelloError(conn net.Conn, msg string) {
	writeHelloResponse(conn, protocol.ExecutorHelloResponse{
		Type:  "executor_hello",
		Error: msg,
	})
}

// ─── executorClient adapts a Connect client to tools.ExecutorClient ────────

var _ tools.ExecutorClient = (*executorClient)(nil)

type executorClient struct {
	inner executorpbconnect.ExecutorServiceClient
}

func (c *executorClient) Execute(ctx context.Context, tool string, input json.RawMessage) (string, error) {
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      tool,
		InputJson: input,
		TimeoutMs: 600_000,
	}))
	if err != nil {
		return "", fmt.Errorf("executor execute: %w", err)
	}
	defer stream.Close()

	var resultText string
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			return "", fmt.Errorf("executor: %s", ev.Failed.Message)
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor stream: %w", err)
	}
	return resultText, nil
}

func (c *executorClient) StartJob(ctx context.Context, command string) (string, error) {
	input, _ := json.Marshal(map[string]string{"command": command})
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:       "bash",
		InputJson:  input,
		Background: true,
	}))
	if err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	defer stream.Close()
	var handle string
	for stream.Receive() {
		if h := stream.Msg().GetHandle(); h != "" {
			handle = h
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	if handle == "" {
		return "", fmt.Errorf("executor start job: no handle returned")
	}
	return handle, nil
}

func (c *executorClient) JobOutput(ctx context.Context, handle string, since int64) (tools.JobSnapshot, error) {
	resp, err := c.inner.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{
		Handle: handle, Since: since,
	}))
	if err != nil {
		return tools.JobSnapshot{}, fmt.Errorf("executor job output: %w", err)
	}
	return tools.JobSnapshot{
		Data:     string(resp.Msg.Data),
		Total:    resp.Msg.Total,
		Exited:   resp.Msg.Exited,
		ExitCode: int(resp.Msg.ExitCode),
		Found:    resp.Msg.Found,
	}, nil
}

func (c *executorClient) KillJob(ctx context.Context, handle string) error {
	if _, err := c.inner.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{
		CallId: handle,
	})); err != nil {
		return fmt.Errorf("executor kill job: %w", err)
	}
	return nil
}

func (c *executorClient) Ping(ctx context.Context) error {
	_, err := c.inner.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		return fmt.Errorf("executor ping: %w", err)
	}
	return nil
}
