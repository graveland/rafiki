package execpool

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// A ticket-authenticated executor must reach Pool.Live() with no row, no
// credential and no enrollment. This is the end-to-end path `rafiki create`
// takes on a machine with no durable executor.
func TestTicketExecutorConnectsAndGoesLive(t *testing.T) {
	store := stubStore{}
	addr, pin, p := servePool(t, store)

	// Mint BEFORE dialling — the ticket must already be in the registry when
	// the hello arrives.
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
	resp := helloExchangeOn(t, p, protocolHello(ticket))
	if resp.Error == "" {
		t.Fatal("a spent ticket must be refused")
	}
	if resp.Retryable {
		t.Fatal("a spent ticket is terminal; Retryable=true makes the executor " +
			"spin for the life of the process")
	}
}
