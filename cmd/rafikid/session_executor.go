package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"

	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/execpool"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// ExecutorSession is the framed ctrl_executor_session face. It anchors its
// transient executor's lifetime on this connection's session context (see
// connSessionContext), keyed by the connection itself: repeated calls from
// the same connection therefore share one context, cancelled exactly once
// when the connection closes (OnConnectionClose -> endConnSession).
//
// conn is nil in dispatch tests; a ticket with no connection to key off is
// still one-shot and still dies with the daemon, so no lifetime tracking is
// registered for it at all.
func (c *Controller) ExecutorSession(
	conn control.Connection,
	id users.Identity,
	req protocol.ExecutorSessionRequest,
) (protocol.ExecutorSessionResponseData, error) {
	ctx := context.Background()
	var key any
	if conn != nil {
		key = conn
		ctx = c.connSessionContext(conn)
	}
	return c.executorSession(ctx, key, id, req)
}

// connectExecutorSessions adapts Controller.executorSession to
// connectapi.ExecutorSessions, the seam behind Connect's server-streaming
// ExecutorSession RPC.
type connectExecutorSessions struct{ c *Controller }

// Open mints (or finds) the caller's session executor for one Connect
// stream. ctx is the STREAM's context and doubles as the opaque session key:
// unlike the framed connection, a Connect stream never repeats a call, so it
// needs no separate lookup to reuse — and using a fresh key per call is what
// keeps two concurrent streams from the same identity from evicting each
// other's executors. Stream end cancels ctx, which is the eviction trigger
// (see executorSession's ctx.Done() watcher).
func (a connectExecutorSessions) Open(
	ctx context.Context,
	req *rafikiv1.ExecutorSessionRequest,
) (*rafikiv1.ExecutorSessionReady, error) {
	id := connectIdentity(ctx)
	resp, err := a.c.executorSession(ctx, ctx, id, protocol.ExecutorSessionRequest{
		Name:  req.GetName(),
		Roots: req.GetRoots(),
	})
	if err != nil {
		return nil, err
	}
	return &rafikiv1.ExecutorSessionReady{
		ExecutorId: resp.ExecutorID,
		RunLocal:   resp.RunLocal,
		Ticket:     resp.Ticket,
		Selector:   resp.Selector,
	}, nil
}

// connectIdentity maps the Connect face's authenticated identity onto the
// daemon's. A nil identity is the unix socket's local trust, which
// sessionOwner already treats as "fall back to the daemon's own OS user" —
// the same UDS rule the framed path relies on.
func connectIdentity(ctx context.Context) users.Identity {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return users.Identity{}
	}
	return users.Identity{UserID: id.UserID, Username: id.Username, IsAdmin: id.IsAdmin}
}

// executorSession is the shared implementation behind both the framed
// ExecutorSession face and Connect's ExecutorSessions.Open: it tells an
// interactive client how to reach an executor that shares its filesystem.
//
// Two answers, one selector. When a durable executor already covers this
// machine and owner, the client starts nothing and uses it — it outlives the
// terminal, which is what a running agent needs. Otherwise the client serves a
// TRANSIENT executor: no database row, no credential file, authenticated by a
// one-shot ticket and evicted when ctx ends.
//
// The selector is IDENTICAL in both cases, and deliberately so: it names the
// machine rather than a specific executor, so a child can be moved between the
// two without its stored selector — the thing its whole subtree inherits — ever
// being rewritten.
//
// Every field that gates access is written HERE, from the identity, never
// from the request. A client that could name its own owner or admits would be
// granting itself access.
//
// key identifies this session for eviction purposes. A nil key (the framed
// path's conn==nil case) registers no lifetime tracking at all: the ticket is
// still one-shot and still dies with the daemon.
func (c *Controller) executorSession(
	ctx context.Context,
	key any,
	id users.Identity,
	req protocol.ExecutorSessionRequest,
) (protocol.ExecutorSessionResponseData, error) {
	owner, err := sessionOwner(id)
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}
	if req.Name == "" {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "this machine has no executor name, so the daemon cannot tell " +
				"which durable executor shares its filesystem: run " +
				"`rafiki executor name <name>` on it, or export " + paths.ExecutorName,
		}
	}
	if err := paths.ValidateMachineName(req.Name); err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code: protocol.ErrInvalidArgs, Message: err.Error(),
		}
	}
	if c.execPool == nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "no executor pool is configured",
		}
	}

	selector := "owner=" + owner + ",machine=" + req.Name

	// A live durable executor on this machine wins. Matched on LABELS, which
	// the operator wrote at mint time -- never on SelfReported, which carries
	// only os/arch/version and which the previous version of this check
	// consulted for a "name" nothing has ever written.
	for _, le := range c.execPool.Live() {
		e := le.Executor
		if !e.Enabled || e.Labels["kind"] == "session" {
			continue
		}
		if e.Labels["owner"] == owner && e.Labels["machine"] == req.Name {
			return protocol.ExecutorSessionResponseData{
				ExecutorID: e.ID,
				Selector:   selector,
			}, nil
		}
	}

	execID := "sess-" + ulid.Make().String()
	ticket, err := c.execPool.Tickets().Mint(execpool.TicketGrant{
		ExecutorID:  execID,
		Owner:       owner,
		MachineName: req.Name,
		Roots:       req.Roots,
	})
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "mint session ticket: " + err.Error(),
		}
	}

	if key != nil {
		c.sessionExecMu.Lock()
		prev, had := c.sessionExecs[key]
		if c.sessionExecs == nil {
			c.sessionExecs = make(map[any]sessionExecutor)
		}
		c.sessionExecs[key] = sessionExecutor{executorID: execID, ticket: ticket}
		c.sessionExecMu.Unlock()

		// One per key. Overwriting silently orphaned the incumbent: its
		// ticket was never revoked and its executor never evicted, so it
		// stayed in Pool.live for the daemon's lifetime. Released AFTER the
		// map is updated and OUTSIDE the lock — Evict closes a connection.
		if had {
			slog.Warn("a second session executor was requested for one session key; "+
				"releasing the first", "previous", prev.executorID, "current", execID)
			c.execPool.Tickets().Revoke(prev.ticket)
			c.execPool.Evict(prev.executorID)
		}

		// ctx ending is the eviction trigger on both paths: a cancelled
		// connection context (framed) or a stream that has ended (Connect).
		// releaseSessionExecutor is idempotent and acts on whatever is
		// CURRENTLY stored under key, so a watcher spawned for an earlier
		// call on the same key still evicts correctly if a later call
		// replaced the entry before ctx ended. On the framed plane the close
		// path (endConnSession) releases synchronously as well, so this
		// watcher is the backstop there, not the only trigger.
		c.sessionExecWg.Add(1)
		go func() {
			defer c.sessionExecWg.Done()
			<-ctx.Done()
			c.releaseSessionExecutor(key)
		}()
	}

	return protocol.ExecutorSessionResponseData{
		RunLocal:   true,
		ExecutorID: execID,
		Ticket:     ticket,
		Selector:   selector,
	}, nil
}

// sessionExecutor is one session's transient executor.
type sessionExecutor struct {
	executorID string
	ticket     string
}

// connSession pairs a context with the cancel that ends it, so the framed
// ExecutorSession verb can hand out the SAME context on every call from one
// connection and cancel it exactly once, from OnConnectionClose.
type connSession struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// connSessionContext returns the context anchoring conn's session-executor
// lifetime, minting one on first use so repeated ExecutorSession calls from
// the same connection share it. This is what lets the framed verb evict
// through the same ctx.Done() watcher as Connect's stream context, instead of
// a bespoke connection-keyed release path.
func (c *Controller) connSessionContext(conn control.Connection) context.Context {
	c.connSessionsMu.Lock()
	defer c.connSessionsMu.Unlock()
	if cs, ok := c.connSessions[conn]; ok {
		return cs.ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	if c.connSessions == nil {
		c.connSessions = make(map[control.Connection]connSession)
	}
	c.connSessions[conn] = connSession{ctx: ctx, cancel: cancel}
	return ctx
}

// endConnSession ends conn's session: it cancels and forgets the connection's
// session context, then releases the connection's session executor
// SYNCHRONOUSLY, so the executor is no longer live in the pool by the time
// OnConnectionClose returns — the pre-re-anchor behavior. releaseSessionExecutor
// is idempotent, so whichever of this call or the ctx.Done() watcher gets there
// first releases and the other no-ops; the watcher remains the only trigger on
// the Connect plane, where there is no OnConnectionClose.
//
// Called once from OnConnectionClose; a no-op for a connection that never
// called ExecutorSession.
func (c *Controller) endConnSession(conn control.Connection) {
	c.connSessionsMu.Lock()
	cs, ok := c.connSessions[conn]
	if ok {
		delete(c.connSessions, conn)
	}
	c.connSessionsMu.Unlock()
	if !ok {
		return
	}
	cs.cancel()
	c.releaseSessionExecutor(conn)
}

// releaseSessionExecutor revokes a session's ticket and evicts its executor.
//
// Both halves are needed and neither is sufficient: revoking stops an executor
// that has not connected yet, evicting stops one that already has.
func (c *Controller) releaseSessionExecutor(key any) {
	c.sessionExecMu.Lock()
	se, ok := c.sessionExecs[key]
	if ok {
		delete(c.sessionExecs, key)
	}
	c.sessionExecMu.Unlock()
	if !ok || c.execPool == nil {
		return
	}
	c.execPool.Tickets().Revoke(se.ticket)
	c.execPool.Evict(se.executorID)
	slog.Info("released a transient session executor", "executorId", se.executorID)
}

// sessionOwner resolves who a session executor belongs to.
//
// An authenticated connection carries a username. A local UDS connection does
// not — its identity is deliberately zero, "locally trusted, not a user" — so
// the owner is the DAEMON's own OS user. That is not a fudge: the control
// socket is created under a 0177 umask and owned by that user, so anyone who
// can open it already is them. The daemon is reading a fact from its own
// environment rather than believing a claim, which is why the request has no
// username field and must never grow one.
func sessionOwner(id users.Identity) (string, error) {
	if id.Username != "" {
		return id.Username, nil
	}
	u, err := osUser()
	if err != nil {
		return "", fmt.Errorf("cannot determine an owner for this session executor: "+
			"the connection is not authenticated and the daemon's own user is unknown: %w", err)
	}
	return u, nil
}

// osUser returns the daemon's own OS username. Shared by sessionOwner and the
// spawn path so the UDS owner fallback — anyone who can open the control socket
// already is this user — is derived in one place rather than duplicated.
func osUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.Username == "" {
		return "", fmt.Errorf("current OS user has no username")
	}
	return u.Username, nil
}

// attestOwner is Controller.Spawn's owner attestation, extracted so it can
// run before agentRunner (which needs it for a top-level child's own
// admission check — see admissionLabels) rather than only afterward when
// initLabels is built. From the connection or an ancestor, never from the
// request: it is matched by executor admission selectors (admits:
// owner=<user>), so a client that could name it could claim to be any owner —
// the request cannot carry it, reservedLabelKeys rejects it.
func attestOwner(st *childstore.Store, req protocol.SpawnRequest, owner users.Identity) string {
	ownerName := owner.Username
	if req.ParentChildID != "" {
		if snap, ok := st.Get(req.ParentChildID); ok && snap.Labels["owner"] != "" {
			ownerName = snap.Labels["owner"]
		}
		return ownerName
	}
	if ownerName == "" {
		// Local UDS: the connection is "locally trusted, not a user". Reuse the
		// same fact sessionOwner reads — anyone who can open the socket already
		// is the daemon's OS user.
		if u, err := osUser(); err == nil {
			ownerName = u
		}
	}
	return ownerName
}
