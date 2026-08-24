package main

import (
	"fmt"
	"log/slog"
	"os/user"

	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// ExecutorSession tells an interactive client how to reach an executor that
// shares its filesystem.
//
// Two answers, one selector. When a durable executor already covers this
// machine and owner, the client starts nothing and uses it — it outlives the
// terminal, which is what a running agent needs. Otherwise the client serves a
// TRANSIENT executor: no database row, no credential file, authenticated by a
// one-shot ticket and evicted when this control connection closes.
//
// The selector is IDENTICAL in both cases, and deliberately so: it names the
// machine rather than a specific executor, so a child can be moved between the
// two without its stored selector — the thing its whole subtree inherits — ever
// being rewritten.
//
// Every field that gates access is written HERE, from the connection, never
// from the request. A client that could name its own owner or admits would be
// granting itself access.
func (c *Controller) ExecutorSession(
	conn control.Connection,
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
	if req.MachineID == "" {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "machineId is required: without it the daemon cannot tell " +
				"which durable executor shares this client's filesystem",
		}
	}
	if c.execPool == nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "no executor pool is configured",
		}
	}

	selector := "owner=" + owner + ",machine=" + req.MachineID

	// A live durable executor on this machine wins. Matched on LABELS, which
	// the operator wrote at mint time -- never on SelfReported, which carries
	// only os/arch/version and which the previous version of this check
	// consulted for a "name" nothing has ever written.
	for _, le := range c.execPool.Live() {
		e := le.Executor
		if !e.Enabled || e.Labels["kind"] == "session" {
			continue
		}
		if e.Labels["owner"] == owner && e.Labels["machine"] == req.MachineID {
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
		MachineID:   req.MachineID,
		DisplayName: owner + "@" + req.MachineID[:min(8, len(req.MachineID))],
		Roots:       req.Roots,
	})
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "mint session ticket: " + err.Error(),
		}
	}

	// Tie the executor's life to this connection. conn is nil in dispatch
	// tests; a ticket with no connection to revoke it is still one-shot and
	// still dies with the daemon.
	if conn != nil {
		c.sessionExecMu.Lock()
		if c.sessionExecs == nil {
			c.sessionExecs = make(map[control.Connection]sessionExecutor)
		}
		c.sessionExecs[conn] = sessionExecutor{executorID: execID, ticket: ticket}
		c.sessionExecMu.Unlock()
	}

	return protocol.ExecutorSessionResponseData{
		RunLocal:   true,
		ExecutorID: execID,
		Ticket:     ticket,
		Selector:   selector,
	}, nil
}

// sessionExecutor is one control connection's transient executor.
type sessionExecutor struct {
	executorID string
	ticket     string
}

// releaseSessionExecutor revokes a connection's ticket and evicts its executor.
//
// Both halves are needed and neither is sufficient: revoking stops an executor
// that has not connected yet, evicting stops one that already has.
func (c *Controller) releaseSessionExecutor(conn control.Connection) {
	c.sessionExecMu.Lock()
	se, ok := c.sessionExecs[conn]
	if ok {
		delete(c.sessionExecs, conn)
	}
	c.sessionExecMu.Unlock()
	if !ok || c.execPool == nil {
		return
	}
	c.execPool.Tickets().Revoke(se.ticket)
	c.execPool.Evict(se.executorID)
	slog.Info("released this connection's transient executor", "executorId", se.executorID)
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