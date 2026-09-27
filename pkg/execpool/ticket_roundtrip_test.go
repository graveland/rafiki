package execpool

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

// A ticket-authenticated executor must reach Pool.Live() with no row, no
// credential and no enrollment. This is the end-to-end path `rafiki create`
// takes on a machine with no durable executor.
func TestTicketExecutorConnectsAndGoesLive(t *testing.T) {
	c := assert.NewAborting(t)
	store := stubStore{}
	addr, pin, p := servePool(t, store)

	// Mint BEFORE dialling — the ticket must already be in the registry when
	// the upgrade request arrives.
	ticket, err := p.Tickets().Mint(TicketGrant{
		ExecutorID:  "sess-01J0",
		Owner:       "brent",
		MachineName: "laptop",
	})
	c.NoError(err)

	_, handler := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: "sess-01J0"})
	o := connectOpts(t, addr, pin)
	o.Ticket = ticket
	o.CredentialFile = "" // a transient executor persists nothing
	o.Handler = handler

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Connect(ctx, o) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		live := p.Live()
		if len(live) == 1 && live[0].Executor.ID == "sess-01J0" {
			got := live[0].Executor.Labels["machine"]
			c.Eq("laptop", got, `Labels["machine"] = %q, want "laptop" — the grant's `+
				`daemon-written labels must survive onto the live row`, got)
			return
		}
		c.False(time.Now().After(deadline), "transient executor never went live; Live() = %+v", live)
		time.Sleep(20 * time.Millisecond)
	}
}

// A spent ticket is terminal, not retryable: the control connection that minted
// it is what the ticket stands for, and retrying cannot make it valid again.
func TestSpentTicketIsRefusedTerminally(t *testing.T) {
	c := assert.NewCollecting(t)
	p := New(stubStore{})
	ticket, err := p.Tickets().Mint(TicketGrant{ExecutorID: "sess-1", Owner: "brent"})
	c.Require().NoError(err)
	_, ok := p.Tickets().Redeem(ticket)
	c.Require().True(ok, "first redeem must succeed")
	_, _, err = upgradeExchange(t, p, http.Header{
		"Authorization": {string(upgradeconn.SchemeTicket) + " " + ticket},
	})
	var ref *upgradeconn.Refused
	c.Require().False(!errors.As(err, &ref) || ref.Status != http.StatusUnauthorized, "a second redemption must be refused 401, got %v", err)
	c.StrContains(ref.Reason, "session ticket is unknown, already used, or revoked", "the refusal must say the ticket is spent")
	// Terminal: classifying the refusal must yield the one error that stops
	// the reconnect loop, not something the executor spins on.
	c.ErrorIs(classifyRefusal(err), ErrEnrollmentRejected, "a spent ticket is terminal; a 401 must classify as ErrEnrollmentRejected")
}
