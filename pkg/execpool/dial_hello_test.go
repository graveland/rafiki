package execpool

import "testing"

func TestBuildHelloCarriesATicket(t *testing.T) {
	req, err := buildHello(ConnectOptions{Ticket: "tkt-abc"})
	if err != nil {
		t.Fatalf("a ticket is a complete credential on its own; buildHello "+
			"must not demand --credential or --enroll-token beside it: %v", err)
	}
	if req.Ticket != "tkt-abc" {
		t.Fatalf("Ticket = %q, want %q", req.Ticket, "tkt-abc")
	}
	if req.Credential != "" || req.Token != "" {
		t.Fatalf("a ticket-authenticated hello must carry nothing else, got "+
			"credential=%q token=%q", req.Credential, req.Token)
	}
}

// A ticket is mutually exclusive with the durable paths (see ConnectOptions.Ticket).
// It wins so that an interactive client with a stale executor.cred on disk still
// gets a transient executor rather than silently reusing another identity.
func TestBuildHelloPrefersTheTicketOverACredential(t *testing.T) {
	req, err := buildHello(ConnectOptions{Ticket: "tkt", Credential: "cred"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Ticket != "tkt" || req.Credential != "" {
		t.Fatalf("ticket must win: got ticket=%q credential=%q", req.Ticket, req.Credential)
	}
}

func TestBuildHelloStillRefusesAnEmptyOptions(t *testing.T) {
	if _, err := buildHello(ConnectOptions{}); err == nil {
		t.Fatal("no ticket, no credential, no token: buildHello must refuse " +
			"rather than send an unauthenticated hello")
	}
}
