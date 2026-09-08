package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const mcpNotifyWait = 2 * time.Second

// settleSession is one real SDK server session wired to an in-memory client
// whose LoggingMessageHandler records what arrives. The registry's sessions
// are the concrete *mcp.ServerSession — the SDK offers no way to construct one
// by hand — so every delivery assertion here goes through the real ss.Log
// path, including the LogLevel gate.
type settleSession struct {
	ss  *mcp.ServerSession
	cs  *mcp.ClientSession
	got chan string
}

// newSettleSession connects a server session to a recording client over an
// in-memory transport pair. level is the client's logging/setLevel request;
// an empty level means the client never sends one, which is the state in
// which the SDK silently drops every Log call.
func newSettleSession(t *testing.T, level string) *settleSession {
	t.Helper()

	rec := &settleSession{got: make(chan string, 8)}
	st, ct := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki-test", Version: "test"}, nil)
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcp-test-client", Version: "test"},
		&mcp.ClientOptions{
			LoggingMessageHandler: func(_ context.Context, r *mcp.LoggingMessageRequest) {
				rec.got <- fmt.Sprint(r.Params.Data)
			},
		})
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if level != "" {
		if err := cs.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{Level: mcp.LoggingLevel(level)}); err != nil {
			t.Fatal(err)
		}
	}
	rec.ss, rec.cs = ss, cs
	t.Cleanup(func() { ss.Close() })
	t.Cleanup(func() { cs.Close() })
	return rec
}

// waitFor receives one notification or fails after a bounded wait — never a
// sleep, so -race scheduling cannot turn a slow delivery into a flake.
func waitFor(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(mcpNotifyWait):
		t.Fatal("timed out waiting for a notification")
		return ""
	}
}

// assertSilence reports whatever arrives within a short bounded window; a
// non-empty result is a failure the caller words.
func assertSilence(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(150 * time.Millisecond):
		return ""
	}
}

func TestMCPNotifyReachesEverySessionForAUser(t *testing.T) {
	reg := newMCPSessions()
	aliceA := newSettleSession(t, "info")
	aliceB := newSettleSession(t, "info")
	bob := newSettleSession(t, "info")
	reg.Add("u-alice", aliceA.ss)
	reg.Add("u-alice", aliceB.ss)
	reg.Add("u-bob", bob.ss)

	msg := "agent c_1 (worker) settled (idle). Read what it did with task_list(assignee=\"c_1\")."
	reg.Notify(context.Background(), "u-alice", msg)

	if got := waitFor(t, aliceA.got); got != msg {
		t.Errorf("first session got %q, want the message verbatim", got)
	}
	if got := waitFor(t, aliceB.got); got != msg {
		t.Errorf("second session got %q, want the message verbatim", got)
	}
	if got := assertSilence(t, bob.got); got != "" {
		t.Errorf("another user's session received %q", got)
	}
}

// TestMCPNotifyDropsWhenTheClientNeverSetALevel pins the SDK behavior the
// best-effort caveat rests on: mcp/server.go's Log returns nil without
// writing while the client has never issued logging/setLevel, so an unlevel
// client receives nothing and the send reports success.
func TestMCPNotifyDropsWhenTheClientNeverSetALevel(t *testing.T) {
	reg := newMCPSessions()
	ss := newSettleSession(t, "") // no logging/setLevel, ever
	reg.Add("u-quiet", ss.ss)

	reg.Notify(context.Background(), "u-quiet", "settle")

	if got := assertSilence(t, ss.got); got != "" {
		t.Errorf("a client that never set a level must receive nothing; got %q", got)
	}
}

// blockableConn is an mcp.Connection whose Write blocks once armed, until its
// release channel is closed. It speaks newline-delimited JSON, the framing the
// SDK's ioConn applies over a byte stream.
type blockableConn struct {
	c        net.Conn
	release  chan struct{}
	released sync.Once
	arm      atomic.Bool
	blocked  chan struct{} // closed the first time Write blocks
	blockOne sync.Once
	wmu      sync.Mutex
}

func (c *blockableConn) Read(context.Context) (jsonrpc.Message, error) {
	dec := json.NewDecoder(c.c)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(raw)
}

func (c *blockableConn) Write(_ context.Context, msg jsonrpc.Message) error {
	if c.arm.Load() {
		c.blockOne.Do(func() { close(c.blocked) })
		<-c.release
	}
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.c.Write(append(data, '\n'))
	return err
}

func (c *blockableConn) Close() error { return c.c.Close() }

// SessionID is part of mcp.Connection in v1.6.1; the stub has no session id.
func (c *blockableConn) SessionID() string { return "" }

// Release unblocks every armed Write. Safe to call more than once.
func (c *blockableConn) Release() { c.released.Do(func() { close(c.release) }) }

// connectFunc adapts a plain function to mcp.Transport.
type connectFunc func(context.Context) (mcp.Connection, error)

func (f connectFunc) Connect(ctx context.Context) (mcp.Connection, error) { return f(ctx) }

// newBlockedSession returns a real server session whose every write blocks
// once armed, a release function, and a channel closed when a write is
// provably in flight. There is no way to construct or fake an
// *mcp.ServerSession, so the stub is a raw JSON-RPC peer over a net.Pipe:
// newline-delimited frames speaking initialize, notifications/initialized and
// logging/setLevel — the three messages that put the session into the state
// where Log actually writes.
func newBlockedSession(t *testing.T, srv *mcp.Server) (*mcp.ServerSession, func(), <-chan struct{}) {
	t.Helper()

	peer, srvEnd := net.Pipe()
	var conn *blockableConn
	ss, err := srv.Connect(context.Background(), connectFunc(func(context.Context) (mcp.Connection, error) {
		conn = &blockableConn{c: srvEnd, release: make(chan struct{}), blocked: make(chan struct{})}
		return conn, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	t.Cleanup(func() { ss.Close() })
	t.Cleanup(conn.Release) // runs first (LIFO), so nothing stays blocked

	// Drain the server's writes and close `ready` once the two responses
	// (initialize, setLevel) have been seen: each response is written only
	// after its handler returned, so the second one proves state.LogLevel is
	// set. Draining continues so no later write can block the pipe.
	ready := make(chan struct{})
	go func() {
		dec := json.NewDecoder(peer)
		responses := 0
		var once sync.Once
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return
			}
			if msg, err := jsonrpc.DecodeMessage(raw); err == nil {
				if _, ok := msg.(*jsonrpc.Response); ok {
					responses++
					if responses == 2 {
						once.Do(func() { close(ready) })
					}
				}
			}
		}
	}()

	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stub","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info"}}`,
	}
	for _, f := range frames {
		if _, err := peer.Write([]byte(f + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ready:
	case <-time.After(mcpNotifyWait):
		t.Fatal("stub session never finished its handshake")
	}
	conn.arm.Store(true)
	return ss, conn.Release, conn.blocked
}

// TestMCPNotifyDoesNotHoldTheLockAcrossSend drives the invariant the brief
// names: the session set is snapshotted under the lock and the lock is
// released before any send, so Add and Remove keep working while one client's
// send is stuck on the network. Run under -race; every wait is bounded.
func TestMCPNotifyDoesNotHoldTheLockAcrossSend(t *testing.T) {
	reg := newMCPSessions()
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki-test", Version: "test"}, nil)
	blockedSS, release, inFlight := newBlockedSession(t, srv)
	reg.Add("u-slow", blockedSS)
	live := newSettleSession(t, "info")
	reg.Add("u-slow", live.ss)

	go reg.Notify(context.Background(), "u-slow", "agent c_2 (worker) exited")

	// The send must actually be stuck before the critical section is judged.
	select {
	case <-inFlight:
	case <-time.After(mcpNotifyWait):
		t.Fatal("the send never blocked; the fixture is broken")
	}

	done := make(chan struct{})
	go func() {
		reg.Remove("u-slow", live.ss)
		reg.Add("u-slow", live.ss)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(mcpNotifyWait):
		t.Fatal("Add/Remove blocked behind an in-flight send: the mutex is held across Log")
	}

	// Unstick the send; the fan-out must continue past the dead session and
	// still deliver to the live one.
	release()
	if got := waitFor(t, live.got); !strings.Contains(got, "c_2") {
		t.Errorf("the live session must still receive after a stuck one; got %q", got)
	}
}

func TestMCPNotifyRemoveEmptiesTheUserEntry(t *testing.T) {
	reg := newMCPSessions()
	first := newSettleSession(t, "info")
	second := newSettleSession(t, "info")
	reg.Add("u-1", first.ss)
	reg.Add("u-1", second.ss)

	reg.Remove("u-1", first.ss)
	if got := len(reg.byUID["u-1"]); got != 1 {
		t.Fatalf("want 1 session left for u-1, got %d", got)
	}
	reg.Remove("u-1", second.ss)
	if _, ok := reg.byUID["u-1"]; ok {
		t.Fatal("the user's entry must be deleted once its last session is removed")
	}
	// Removing an unknown user or session is a no-op, not a panic.
	reg.Remove("u-never", first.ss)
	reg.Remove("u-1", first.ss)
}

func TestMCPNotifySurvivesAFailingSession(t *testing.T) {
	reg := newMCPSessions()
	dead := newSettleSession(t, "info")
	live := newSettleSession(t, "info")
	reg.Add("u-2", dead.ss)
	reg.Add("u-2", live.ss)

	// The skip branch must be observable: swap in a capturing default logger
	// at debug level, where Notify records a failed send. Package tests run
	// sequentially, so the global swap is safe; restored via cleanup.
	prevLog := slog.Default()
	logs := &capturingHandler{}
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	// Tearing the client down breaks the transport under its server session;
	// the next Log on it errors instead of delivering.
	if err := dead.cs.Close(); err != nil {
		t.Fatal(err)
	}
	reg.Notify(context.Background(), "u-2", "agent c_3 (worker) exited")

	if got := waitFor(t, live.got); !strings.Contains(got, "c_3") {
		t.Errorf("one failing session must not stop the others; got %q", got)
	}
	if got := logs.String(); !strings.Contains(got, "mcp settlement notification failed") {
		t.Errorf("the failed send must be logged at debug and skipped; log:\n%s", got)
	}
}

// capturingHandler collects slog records so a test can assert a debug-level
// skip actually happened. Every method is safe for concurrent use because the
// send goroutines log concurrently.
type capturingHandler struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.WriteString(r.Message)
	h.buf.WriteByte('\n')
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// TestMCPNotifyReturnsWhileASendIsStuck proves the fan-out's wait is real:
// the SDK's streamable Write path takes no context, so a send to a
// stalled-but-open client blocks indefinitely and cannot be interrupted — the
// caller's timeout must therefore ABANDON that send rather than wait behind
// it, and the user's other sessions must receive while the stalled one is
// stuck (concurrent sends, not a sequential loop).
func TestMCPNotifyReturnsWhileASendIsStuck(t *testing.T) {
	reg := newMCPSessions()
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki-test", Version: "test"}, nil)
	blockedSS, release, inFlight := newBlockedSession(t, srv)
	reg.Add("u-slow", blockedSS)
	live := newSettleSession(t, "info")
	reg.Add("u-slow", live.ss)

	// The same shape notifyMCPSettled gives the fan-out: a bounded wait.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	returned := make(chan struct{})
	go func() {
		reg.Notify(ctx, "u-slow", "agent c_4 (worker) exited")
		close(returned)
	}()

	// The send is provably stuck on the wire...
	select {
	case <-inFlight:
	case <-time.After(mcpNotifyWait):
		t.Fatal("the send never blocked; the fixture is broken")
	}
	// ...and the other session must still receive while it is.
	if got := waitFor(t, live.got); !strings.Contains(got, "c_4") {
		t.Errorf("the live session must receive while another send is stuck; got %q", got)
	}
	// The deadline returns the call even though the stuck send never ends.
	select {
	case <-returned:
	case <-time.After(mcpNotifyWait):
		t.Fatal("Notify waited behind a stalled send instead of abandoning it")
	}

	release()
}

// TestMCPNotifySkipsADescendantOfAnMCPChild pins the owner-propagation truth:
// OwnerUserID is stamped only from the identity argument at fresh spawn, and
// the child-bound spawner passes an empty identity, so a descendant of an
// MCP-spawned child carries an EMPTY id — only the display-only
// Labels["owner"] username propagates, via attestOwner — and gets no MCP
// fan-out. Its settlement reaches the MCP caller's own child (its parent)
// through the parent-gated inbox push instead.
func TestMCPNotifySkipsADescendantOfAnMCPChild(t *testing.T) {
	prev := mcpSettlements
	reg := newMCPSessions()
	mcpSettlements = reg
	t.Cleanup(func() { mcpSettlements = prev })

	ss := newSettleSession(t, "info")
	reg.Add("u-op", ss.ss)

	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{
		ChildID: "c_mcp_top", Name: "mcp worker", OwnerUserID: "u-op",
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	// The descendant exactly as controllerSpawner leaves it: parent labels
	// set, OwnerUserID empty.
	c.st.Insert(&childstore.Session{
		ChildID: "c_mcp_desc", Name: "descendant",
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_mcp_top",
			childstore.LabelRoot:   "c_mcp_top",
		},
	})

	c.notifySubagentSettled("c_mcp_desc", "exited")

	if got := assertSilence(t, ss.got); got != "" {
		t.Errorf("a descendant must not fan out to the caller's session; got %q", got)
	}
}

// TestMCPFaceWiresSessionsIntoTheSettlementFanOut is the end-to-end
// registration proof: a session initialized against the FACE's own
// getServer — owner on the context the way UserTokenAuth leaves it — must
// land in the settlement registry via the bridge's ServerOptions escape
// hatch, receive a real settlement, and leave the registry when its
// connection closes. The six TestMCPNotify tests cover the fan-out
// mechanics; this covers the wiring.
func TestMCPFaceWiresSessionsIntoTheSettlementFanOut(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	prev := mcpSettlements
	reg := newMCPSessions()
	mcpSettlements = reg
	t.Cleanup(func() { mcpSettlements = prev })

	srv := face.getServer(mcpRequestFor("u-op"))
	if srv == nil {
		t.Fatal("getServer returned nil for an authenticated user")
	}

	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	got := make(chan string, 8)
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcp-wiring-test", Version: "0"},
		&mcp.ClientOptions{
			LoggingMessageHandler: func(_ context.Context, r *mcp.LoggingMessageRequest) {
				got <- fmt.Sprint(r.Params.Data)
			},
		})
	cs, err := client.Connect(context.Background(), ct, nil) // fires notifications/initialized → Add
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// Registration happens on the server's handling goroutine; await it with
	// a bounded poll, never a bare check.
	deadline := time.Now().Add(mcpNotifyWait)
	for {
		reg.mu.Lock()
		registered := len(reg.byUID["u-op"])
		reg.mu.Unlock()
		if registered == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the face never registered the initialized session with the settlement registry")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cs.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{Level: "info"}); err != nil {
		t.Fatal(err)
	}
	ctrl := face.controller()
	ctrl.st.Insert(&childstore.Session{
		ChildID:     "c_mcp_wired",
		Name:        "mcp worker",
		OwnerUserID: "u-op",
		Status:      protocol.StatusStreaming,
		StartedAt:   time.Now(),
	})
	ctrl.notifySubagentSettled("c_mcp_wired", "exited")

	if gotMsg := waitFor(t, got); !strings.Contains(gotMsg, "c_mcp_wired") {
		t.Errorf("the settled fragment must reach the initialized client: %q", gotMsg)
	}

	// The Wait goroutine removes the session once its connection closes; the
	// same bounded poll, because the removal also rides another goroutine.
	_ = cs.Close()
	deadline = time.Now().Add(mcpNotifyWait)
	for {
		reg.mu.Lock()
		registered := len(reg.byUID["u-op"])
		reg.mu.Unlock()
		if registered == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed session was never removed from the settlement registry")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMCPNotifyFiresForATopLevelChild is the source change itself: the
// settlement path must reach the MCP fan-out for a child with no parent,
// which the pre-existing parent gate skipped every time. Every MCP-spawned
// child is top-level, so without this the whole notification never fired.
func TestMCPNotifyFiresForATopLevelChild(t *testing.T) {
	prev := mcpSettlements
	reg := newMCPSessions()
	mcpSettlements = reg
	t.Cleanup(func() { mcpSettlements = prev })

	ss := newSettleSession(t, "info")
	reg.Add("u-op", ss.ss)

	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{
		ChildID:     "c_mcp_top",
		Name:        "mcp worker",
		OwnerUserID: "u-op",
		Status:      protocol.StatusStreaming,
		StartedAt:   time.Now(),
	})

	// No evbuf at all: the fan-out must not depend on the event buffer being
	// wired, only the parent push does.
	c.notifySubagentSettled("c_mcp_top", "exited")

	got := waitFor(t, ss.got)
	if !strings.Contains(got, "c_mcp_top") || !strings.Contains(got, "exited") {
		t.Errorf("notification must reuse the settle fragment verbatim: %q", got)
	}
	if !strings.Contains(got, "task_list") {
		t.Errorf("fragment must point at the ledger like the inbox one: %q", got)
	}
}
