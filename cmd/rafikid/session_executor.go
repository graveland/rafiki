package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// ExecutorSession gives a client an executor row for the machine it runs on.
//
// Every field that gates access is written HERE, from the connection, never
// from the request: a client that could name its own owner or admits would be
// granting itself access, which is the same violation SelfReported prevents on
// the executor side of the same registry.
//
// It defers when a persistent executor already covers this name and owner. That
// is not an optimisation — a client-run executor dies when the operator closes
// their terminal, and an agent whose workspace went with it stops working. A
// machine that already has a durable executor should keep using it.
func (c *Controller) ExecutorSession(
	id users.Identity,
	req protocol.ExecutorSessionRequest,
) (protocol.ExecutorSessionResponseData, error) {
	if err := c.requireExecutorStore(); err != nil {
		return protocol.ExecutorSessionResponseData{}, err
	}

	owner, err := sessionOwner(id)
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	name := req.Name
	if name == "" {
		name = "unknown"
	}

	// Defer to a durable executor if one already covers this name and owner.
	// A List error is NOT fatal: minting a second row is recoverable, while
	// refusing the session over a transient read is not.
	if covered, existingID, err := c.nameAlreadyCovered(context.Background(), owner, name); err != nil {
		logSessionWarning(err)
	} else if covered {
		return protocol.ExecutorSessionResponseData{ExecutorID: existingID, Selector: "owner=" + owner}, nil
	}

	// The client already has a row and the secret that names it. Minting
	// another would leave the first orphaned forever: the store has SetEnabled
	// but no Delete.
	if req.HasCredential {
		return protocol.ExecutorSessionResponseData{RunLocal: true, Selector: "owner=" + owner + ",kind=client"}, nil
	}

	// Minting while an enabled kind=client row already exists for this owner
	// and name means the client LOST its credential — that row can never be
	// used again, because only its hash was stored. Disable it so the operator
	// sees one live row per machine rather than a growing list of identical
	// dead ones.
	c.disableStaleClientExecutors(context.Background(), owner, name)

	e, credential, err := c.execStore.Create(context.Background(), executors.NewToken{
		Labels: map[string]string{
			"owner":       owner,
			"name":        name,
			"kind":        "client",
			"interactive": "true",
		},
		Roots: req.Roots,
		// An operator's own machine is not a container and is not
		// interchangeable with anyone else's, so losing it must park or fail
		// the child rather than silently rescheduling its work elsewhere.
		Isolation:     "none",
		WorkspaceMode: "pinned",
		// Nobody else's children land on this operator's laptop.
		Admits:   "owner=" + owner,
		MintedBy: owner,
	})
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, &control.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "create session executor: " + err.Error(),
		}
	}
	return protocol.ExecutorSessionResponseData{
		RunLocal:   true,
		ExecutorID: e.ID,
		Credential: credential,
		Selector:   "owner=" + owner + ",kind=client",
	}, nil
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
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "", fmt.Errorf("cannot determine an owner for this session executor: " +
			"the connection is not authenticated and the daemon's own user is unknown")
	}
	return u.Username, nil
}

// nameAlreadyCovered reports whether an enabled, non-kind=client executor
// already covers this owner and name. The name is matched against SelfReported,
// not trust labels — it is the executor's own claim about itself.
func (c *Controller) nameAlreadyCovered(ctx context.Context, owner, name string) (bool, string, error) {
	execs, err := c.execStore.List(ctx)
	if err != nil {
		return false, "", err
	}
	for _, e := range execs {
		if !e.Enabled {
			continue
		}
		if e.Labels["kind"] == "client" {
			continue
		}
		if e.Labels["owner"] == owner && e.SelfReported["name"] == name {
			return true, e.ID, nil
		}
	}
	return false, "", nil
}

// disableStaleClientExecutors disables enabled kind=client rows for the
// given owner and name. Best-effort: failures are logged, never returned.
func (c *Controller) disableStaleClientExecutors(ctx context.Context, owner, name string) {
	execs, err := c.execStore.List(ctx)
	if err != nil {
		logSessionWarning(err)
		return
	}
	for _, e := range execs {
		if !e.Enabled || e.Labels["kind"] != "client" {
			continue
		}
		if e.Labels["owner"] != owner || e.Labels["name"] != name {
			continue
		}
		if err := c.execStore.SetEnabled(ctx, e.ID, false); err != nil {
			logSessionWarning(err)
		}
	}
}

func logSessionWarning(err error) {
	slog.Warn("session_executor", "error", err)
}
