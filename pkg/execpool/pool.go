package execpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// Pool is the live executor registry. It holds connections; the DATABASE holds
// identity. Nothing authoritative is cached here beyond the last Describe and
// Health, both of which are self-reported and therefore non-authoritative by
// construction.
const parkTimeout = 5 * time.Minute

const (
	// defaultHealthInterval is how often a live executor is polled.
	defaultHealthInterval = 30 * time.Second
	// defaultHealthTimeout bounds ONE Health call. Unbounded, a black-holed
	// connection wedges the health loop forever: the executor is never
	// parked, its children never learn it is gone, and the goroutine leaks
	// for the lifetime of the daemon.
	defaultHealthTimeout = 10 * time.Second
	// defaultJoinTimeout bounds the Describe that admits an executor. The
	// hello frame has its own read deadline, but that deadline is cleared
	// before Describe runs -- an executor that completes the handshake and
	// then stops answering held an accept goroutine open indefinitely.
	defaultJoinTimeout = 10 * time.Second
)

type Pool struct {
	mu     sync.RWMutex
	store  executors.Store
	live   map[string]*liveConn
	parked map[string]*parkedEntry
	onLost func(executorID string) // fired when a park expires; see SetOnLost

	// Timeouts, fields rather than constants so the health path is testable
	// in milliseconds instead of in half-minutes. Nothing outside tests
	// changes them.
	healthInterval time.Duration
	healthTimeout  time.Duration
	joinTimeout    time.Duration
}

type liveConn struct {
	executor executors.Executor
	describe *executorpb.DescribeResponse
	client   *executorClient
	draining bool
	// remoteAddr is the peer address, kept for the log line when a second
	// connection is refused: two different addresses for one credential is the
	// shape of a credential on two machines.
	remoteAddr string
	done       chan struct{}
	// closeOnce guards done. Teardown arrives from two independent
	// directions — a health failure on this connection, and a reconnect that
	// displaces it — and closing a channel twice panics the daemon rather
	// than erroring.
	closeOnce sync.Once
}

// shutdown signals everything waiting on this connection to unwind: its
// handleConn (blocked on <-done) and its healthLoop. Idempotent by design.
func (lc *liveConn) shutdown() {
	lc.closeOnce.Do(func() { close(lc.done) })
}

// ErrAlreadyConnected refuses a second connection for one executor identity.
var ErrAlreadyConnected = errors.New("execpool: this executor already has a live connection")

// admit decides whether a newly authenticated connection may join.
//
// One credential names one row, and the pool holds one connection per row, so a
// second connection is one of two things: a reconnect whose predecessor is not
// yet known to be dead, or the same credential on two machines. Arrival order
// cannot tell those apart — but LIVENESS can, so the incumbent is probed and
// only one that fails to answer is replaced.
//
// Refusing rather than displacing is the point. Displacing meant a stolen
// credential could kick the legitimate executor off its own daemon, and the two
// would then flap against each other indefinitely, each displacement looking
// exactly like an ordinary reconnect in the log.
//
// Displacing WAS defensible when a dead connection took ~15 minutes of TCP
// retransmission to notice, because defending an incumbent that had silently
// gone away would have stranded the real executor for that long. The h2
// keepalive added for A2 changed that: a black-holed peer now fails this probe
// within healthTimeout, so defending the incumbent costs a waking laptop one
// bounded probe rather than a quarter of an hour.
func (p *Pool) admit(ctx context.Context, id, remote string) error {
	p.mu.RLock()
	incumbent := p.live[id]
	p.mu.RUnlock()

	if incumbent == nil {
		return nil
	}

	// Probed with the lock RELEASED. Blocking on a network call while holding
	// p.mu wedges Live(), ClientFor() and every subsequent accept — one unwell
	// executor taking the whole executor plane down.
	if p.connIsAlive(ctx, incumbent) {
		slog.Warn("execpool: refused a second connection for an executor that is already connected. "+
			"If this repeats, the credential is in use on more than one machine",
			"executorId", id, "incumbent", incumbent.remoteAddr, "refused", remote)
		return ErrAlreadyConnected
	}

	slog.Info("execpool: the previous connection no longer answers; admitting the new one",
		"executorId", id, "previous", incumbent.remoteAddr, "new", remote)
	return nil
}

// connIsAlive reports whether a connection still answers, bounded by
// healthTimeout so a black hole cannot stall an accept.
func (p *Pool) connIsAlive(ctx context.Context, lc *liveConn) bool {
	ctx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()
	_, err := lc.client.inner.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
	return err == nil
}

// installLive publishes lc as THE connection for id, tearing down whatever it
// displaces.
//
// An executor restart reconnects long before the old connection's health loop
// notices its socket is dead — up to a full 30s tick later. Both connections
// are therefore live at once, and the map can only hold one. Displacing the
// old one is the right call rather than refusing the new: the whole point of
// park/reattach is that a laptop waking up recovers immediately, and making it
// wait out the previous connection's health timeout would undo that.
//
// But a displaced connection MUST be signalled. Its handleConn is parked on
// <-done and its healthLoop on the same channel; with nothing to close it they
// both run forever, holding a TLS connection and writing TouchSeen every 30s
// on behalf of an executor that has already left.
func (p *Pool) installLive(id string, lc *liveConn) {
	p.mu.Lock()
	displaced := p.live[id]
	p.live[id] = lc
	p.mu.Unlock()

	if displaced != nil && displaced != lc {
		slog.Info("execpool: executor reconnected; tearing down the previous connection", "executorId", id)
		displaced.shutdown()
	}
}

// removeLive deletes id's entry only if lc is still the connection mapped
// there, and reports whether it did.
//
// Keying the delete on the ID alone let a stale connection evict its own
// replacement: the old health loop fails, deletes the entry a reconnect had
// just installed, and parks an executor that is working perfectly — after
// which the park expiring fires onLost against children that are running fine.
func (p *Pool) removeLive(id string, lc *liveConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.live[id]; ok && cur == lc {
		delete(p.live, id)
		return true
	}
	return false
}

// New creates a Pool backed by the executor store.
func New(store executors.Store) *Pool {
	return &Pool{
		store:          store,
		live:           make(map[string]*liveConn),
		parked:         make(map[string]*parkedEntry),
		healthInterval: defaultHealthInterval,
		healthTimeout:  defaultHealthTimeout,
		joinTimeout:    defaultJoinTimeout,
	}
}

// UpgradeHandler is the executor endpoint as an http.Handler, for mounting on a
// mux alongside anything else.
//
// The executor is reached by PATH and upgraded out of HTTP/1.1, which is what
// lets it share a port and a certificate with the control plane. Exposed as a
// handler rather than a listener so the daemon can mount it on the listener it
// already has.
func (p *Pool) UpgradeHandler() http.Handler {
	return upgradeconn.Handler(upgradeconn.Executor, func(c *upgradeconn.Conn) {
		p.handleConn(c)
	})
}

// Serve runs the executor endpoint on a listener of its own.
//
// It mounts UpgradeHandler on a private mux, so a dedicated executor port and a
// shared one run byte-for-byte the same code. That matters more than the
// convenience: this repo has twice shipped a correct guard that something else
// routed around, and two accept paths is exactly that shape.
func (p *Pool) Serve(ln net.Listener) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.parkSweep(ctx)

	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), p.UpgradeHandler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// StartSweeper runs the park sweeper for a pool whose connections arrive from
// someone else's listener. Serve does this itself; a mounted pool needs it
// started once.
func (p *Pool) StartSweeper(ctx context.Context) { go p.parkSweep(ctx) }

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

	switch {
	case hello.Token != "":
		// First enrollment.
		e, credential, err = p.store.Enroll(ctx, hello.Token, hello.SelfReported)
	case hello.Credential != "":
		e, err = p.store.Authenticate(ctx, hello.Credential)
	default:
		writeHelloError(conn, "no token or credential in hello")
		return
	}
	if err != nil {
		writeAuthFailure(conn, err)
		return
	}

	// Decided BEFORE the hello response, so a refused peer is told why rather
	// than watching an authenticated connection close under it.
	remote := conn.RemoteAddr().String()
	if err := p.admit(ctx, e.ID, remote); err != nil {
		writeHelloResponse(conn, protocol.ExecutorHelloResponse{
			Type: "executor_hello",
			Error: "another connection for this executor is already live and answering; " +
				"if this machine is not sharing its credential with another, retry shortly",
			Retryable: true,
		})
		return
	}

	writeHelloResponse(conn, protocol.ExecutorHelloResponse{
		Type:       "executor_hello",
		ExecutorID: e.ID,
		Credential: credential, // empty except on first enrollment
	})

	httpClient, err := ClientForConn(conn)
	if err != nil {
		slog.Warn("execpool: client for conn failed", "error", err, "executorId", e.ID)
		return
	}

	cl := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")

	// Bounded: the hello frame's read deadline was cleared above, and it has
	// to be — it is a CONNECTION deadline, and this connection is about to
	// live for hours. That leaves Describe as the one unbounded call on the
	// admission path, where a peer that completes the handshake and then goes
	// silent holds this goroutine and its connection open forever.
	joinCtx, cancelJoin := context.WithTimeout(ctx, p.joinTimeout)
	desc, err := cl.Describe(joinCtx, connect.NewRequest(&executorpb.DescribeRequest{}))
	cancelJoin()
	if err != nil {
		slog.Warn("execpool: Describe failed on join", "error", err, "executorId", e.ID)
		return
	}

	_ = p.store.TouchSeen(ctx, e.ID)

	// If this executor was parked, reattach it.
	p.reattach(e.ID)

	lc := &liveConn{
		executor:   e,
		describe:   desc.Msg,
		client:     &executorClient{inner: cl},
		remoteAddr: remote,
		done:       make(chan struct{}),
	}

	p.installLive(e.ID, lc)

	slog.Info("execpool: executor joined", "id", e.ID, "displayName", e.DisplayName, "tools", desc.Msg.Tools)

	// Poll Health every 30s.
	go p.healthLoop(ctx, e.ID, lc)

	// Block until the connection is done (ServeInverted-style, but since
	// we're the HTTP client, we detect departure when health fails).
	<-lc.done

	p.removeLive(e.ID, lc)
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
		if lc.draining {
			return nil, fmt.Errorf("executor %s: %w", executorID, ErrDraining)
		}
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

// ProjectContext returns the workspace's project instruction files, includes
// already expanded. It belongs on the workspace client rather than the shared
// executorClient because it needs a workspace id, and only a workspace-scoped
// client carries one.
func (c *workspaceClient) ProjectContext(ctx context.Context) (string, error) {
	resp, err := c.inner.ProjectContext(ctx, connect.NewRequest(&executorpb.ProjectContextRequest{
		WorkspaceId: c.workspaceID,
	}))
	if err != nil {
		return "", fmt.Errorf("executor project context: %w", err)
	}
	return resp.Msg.ContextFiles, nil
}

func (p *Pool) healthLoop(ctx context.Context, id string, lc *liveConn) {
	ticker := time.NewTicker(p.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.refreshRow(ctx, id, lc); err != nil {
				slog.Warn("execpool: executor revoked; removing it from the pool",
					"executorId", id, "error", err)
				p.onHealthFailure(id, lc)
				return
			}
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

// ErrExecutorRevoked reports that an executor's row says it may no longer serve.
var ErrExecutorRevoked = errors.New("execpool: executor is disabled")

// refreshRow re-reads the executor's row and applies it to the live connection.
//
// `conversations.executors` is authoritative, and the design says so — but the
// row was previously read exactly ONCE, at Authenticate during the handshake,
// and then cached in liveConn.executor for the connection's lifetime. Selection
// reads that cache through Live(). So an executor that stayed connected kept its
// enrollment-time labels for as long as it stayed connected, and
// `rafiki executor disable` did not stop it serving: revocation took effect at
// the next reconnect, which for a long-lived laptop connection may be days away
// or never. "Authoritative on every connection" is only true if connections are
// short, and these are deliberately long.
//
// A failure to READ the row is not a revocation — the A3 lesson, in a second
// place. A database blip must not take the fleet out of service, so an
// unreadable row keeps the last known one and tries again on the next tick.
// Only an answer ("this row says disabled") removes the executor.
func (p *Pool) refreshRow(ctx context.Context, id string, lc *liveConn) error {
	ctx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()

	e, err := p.store.Get(ctx, id)
	if err != nil {
		slog.Warn("execpool: could not re-read the executor row; keeping the last known one",
			"executorId", id, "error", err)
		return nil
	}
	if !e.Enabled {
		return ErrExecutorRevoked
	}

	// Under the write lock: Live() reads this field under RLock.
	p.mu.Lock()
	lc.executor = e
	p.mu.Unlock()
	return nil
}

// onHealthFailure drops a failed executor from the live set and parks it,
// giving it parkTimeout to reconnect before its children are declared lost.
//
// Two things are load-bearing here.
//
// First, the park is conditional on the delete having HAPPENED. removeLive
// reports false when this connection is no longer the mapped one, which means
// a reconnect has already replaced it — the executor is healthy and parking it
// would take a working machine out of service and eventually declare its
// children lost. In that case this call tears down only its own connection.
// (Park's own re-check of p.live would also catch this, but relying on it
// leaves the intent invisible at exactly the site that gets it wrong.)
//
// Second, the delete and the Park are deliberately NOT one critical section.
// Park takes p.mu itself and sync.RWMutex is not reentrant, so holding the lock
// across the call deadlocks this goroutine WHILE IT HOLDS the pool lock — every
// later Live(), ClientFor() and accept blocks behind it, and one unwell
// executor takes the whole executor plane down. The gap this opens is narrow
// and benign: a reconnect landing inside it installs a new entry, and Park sees
// it and no-ops.
func (p *Pool) onHealthFailure(id string, lc *liveConn) {
	if p.removeLive(id, lc) {
		p.Park(id, parkTimeout)
	}
	lc.shutdown()
}

func (p *Pool) healthCheck(ctx context.Context, id string, lc *liveConn) error {
	// A health check that can hang is not a health check. The caller's ctx is
	// the daemon's lifetime, so without this bound the one failure mode the
	// poll exists to detect -- a peer that accepts and never answers -- is
	// precisely the one it cannot report.
	callCtx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()

	resp, err := lc.client.inner.Health(callCtx, connect.NewRequest(&executorpb.HealthRequest{}))
	if err != nil {
		return err
	}
	if resp.Msg.GetDraining() {
		// Graceful leave, learned at dispatch. Children are told by the next
		// ClientFor call, not by a polling interval.
		p.mu.Lock()
		lc.draining = true
		p.mu.Unlock()
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

// writeAuthFailure answers a failed Enroll or Authenticate, telling the peer
// whether the answer is about its CREDENTIAL or about our ability to check it.
//
// The retryable branch deliberately does not forward err.Error(). A store
// failure is an internal error — a pgx message carrying the DSN, a hostname,
// a query — and the peer on the other end has, by definition, not yet proved
// who it is. The real error goes to the log, where it belongs.
func writeAuthFailure(conn net.Conn, err error) {
	if executors.IsTerminalAuthError(err) {
		writeHelloResponse(conn, protocol.ExecutorHelloResponse{
			Type:  "executor_hello",
			Error: err.Error(),
		})
		return
	}
	slog.Error("execpool: could not verify an executor credential", "error", err)
	writeHelloResponse(conn, protocol.ExecutorHelloResponse{
		Type:      "executor_hello",
		Error:     "rafikid could not verify the credential right now; retry",
		Retryable: true,
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

// Provision provisions a workspace on executorID and returns the response.
func (p *Pool) Provision(ctx context.Context, executorID string, req *executorpb.ProvisionRequest) (*executorpb.ProvisionResponse, error) {
	p.mu.RLock()
	lc, ok := p.live[executorID]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("execpool: executor %s not live", executorID[:12])
	}
	resp, err := lc.client.inner.Provision(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Release releases a workspace on executorID.
func (p *Pool) Release(ctx context.Context, executorID, workspaceID string) error {
	p.mu.RLock()
	lc, ok := p.live[executorID]
	p.mu.RUnlock()
	if !ok {
		return nil // executor is gone; workspace is effectively released
	}
	_, err := lc.client.inner.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{
		WorkspaceId: workspaceID,
	}))
	return err
}
