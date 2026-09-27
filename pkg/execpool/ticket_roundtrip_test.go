package execpool

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// A ticket-authenticated executor must reach Pool.Live() with no row, no
// credential and no enrollment. This is the end-to-end path `rafiki create`
// takes on a machine with no durable executor.
func TestTicketExecutorConnectsAndGoesLive(t *testing.T) {
	store := stubStore{}
	addr, pin, p := servePool(t, store)

	// Mint BEFORE dialling — the ticket must already be in the registry when
	// the upgrade request arrives.
	ticket, err := p.Tickets().Mint(TicketGrant{
		ExecutorID:  "sess-01J0",
		Owner:       "brent",
		MachineName: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}

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
			if got := live[0].Executor.Labels["machine"]; got != "laptop" {
				t.Fatalf(`Labels["machine"] = %q, want "laptop" — the grant's `+
					`daemon-written labels must survive onto the live row`, got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transient executor never went live; Live() = %+v", live)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A spent ticket is terminal, not retryable: the control connection that minted
// it is what the ticket stands for, and retrying cannot make it valid again.
func TestSpentTicketIsRefusedTerminally(t *testing.T) {
	p := New(stubStore{})
	ticket, err := p.Tickets().Mint(TicketGrant{ExecutorID: "sess-1", Owner: "brent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Tickets().Redeem(ticket); !ok {
		t.Fatal("first redeem must succeed")
	}
	_, _, err = upgradeExchange(t, p, http.Header{
		"Authorization": {string(upgradeconn.SchemeTicket) + " " + ticket},
	})
	var ref *upgradeconn.Refused
	if !errors.As(err, &ref) || ref.Status != http.StatusUnauthorized {
		t.Fatalf("a second redemption must be refused 401, got %v", err)
	}
	if !strings.Contains(ref.Reason, "session ticket is unknown, already used, or revoked") {
		t.Errorf("the refusal must say the ticket is spent: %q", ref.Reason)
	}
	// Terminal: classifying the refusal must yield the one error that stops
	// the reconnect loop, not something the executor spins on.
	if !errors.Is(classifyRefusal(err), ErrEnrollmentRejected) {
		t.Error("a spent ticket is terminal; a 401 must classify as ErrEnrollmentRejected")
	}
}
