package execpool

import "testing"

func TestTicketRedeemsOnceThenIsSpent(t *testing.T) {
	r := NewTicketRegistry()
	tk, err := r.Mint(TicketGrant{ExecutorID: "e1", Owner: "brent"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Redeem(tk); !ok {
		t.Fatal("a freshly minted ticket must redeem")
	}
	if _, ok := r.Redeem(tk); ok {
		t.Fatal("a ticket is ONE-SHOT: a replayed ticket must not authenticate " +
			"a second executor onto the same identity")
	}
}

func TestTicketRevokeBeforeRedeem(t *testing.T) {
	r := NewTicketRegistry()
	tk, err := r.Mint(TicketGrant{ExecutorID: "e1", Owner: "brent"})
	if err != nil {
		t.Fatal(err)
	}
	r.Revoke(tk)
	if _, ok := r.Redeem(tk); ok {
		t.Fatal("a revoked ticket must not redeem: revocation is how a closed " +
			"control connection stops its executor reconnecting")
	}
}

func TestTicketUnknownIsRefused(t *testing.T) {
	r := NewTicketRegistry()
	if _, ok := r.Redeem("not-a-ticket"); ok {
		t.Fatal("an unknown ticket must be refused")
	}
}

func TestTicketsAreDistinct(t *testing.T) {
	r := NewTicketRegistry()
	a, _ := r.Mint(TicketGrant{ExecutorID: "e1"})
	b, _ := r.Mint(TicketGrant{ExecutorID: "e2"})
	if a == b {
		t.Fatal("two mints must produce different tickets")
	}
	if len(a) < 32 {
		t.Fatalf("a ticket is a bearer credential and must be unguessable; got %d chars", len(a))
	}
}

func TestGrantBuildsADaemonWrittenExecutorRow(t *testing.T) {
	g := TicketGrant{
		ExecutorID:  "e1",
		Owner:       "brent",
		MachineID:   "abc123",
		DisplayName: "brent's terminal",
		Roots:       []string{"/Users/brent/src"},
	}
	e := g.Executor()

	if e.Admits != "owner=brent" {
		t.Fatalf("a transient executor must admit ONLY its owner, got %q", e.Admits)
	}
	if e.Labels["owner"] != "brent" || e.Labels["machine"] != "abc123" {
		t.Fatalf("owner and machine must be daemon-written labels, got %v", e.Labels)
	}
	if e.Labels["kind"] != "session" {
		t.Fatalf("kind=session is what sortCandidates ranks below a durable executor, got %q", e.Labels["kind"])
	}
	if e.Isolation != "none" {
		t.Fatalf("an operator's own terminal is not sandboxed, got %q", e.Isolation)
	}
	if e.WorkspaceMode != "pinned" {
		t.Fatalf("want pinned, got %q", e.WorkspaceMode)
	}
	if !e.Enabled {
		t.Fatal("a redeemed grant is enabled")
	}
	if len(e.SelfReported) != 0 {
		t.Fatal("NOTHING about a transient executor is self-reported; every " +
			"field here is written by the daemon from the authenticated connection")
	}
}
