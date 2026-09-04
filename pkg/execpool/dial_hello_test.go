package execpool

import (
	"encoding/json"
	"io"
	"net"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

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

// The daemon writes its hello response and, as the HTTP/2 CLIENT, immediately
// sends the connection preface. Those bytes routinely share a segment with the
// response, so a buffered reader that consumes past the response's newline
// swallows the preface — and ServeInverted then starts mid-frame and rejects a
// perfectly good connection.
//
// This drives the exact shape: one Write carrying the response AND the preface.
// A test that sleeps between them passes regardless and proves nothing.
func TestHelloReaderDoesNotSwallowPipelinedBytes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	go func() {
		var req protocol.ExecutorHelloRequest
		_ = json.NewDecoder(server).Decode(&req)
		resp, _ := json.Marshal(protocol.ExecutorHelloResponse{
			Type: "executor_hello", ExecutorID: "e1",
		})
		// One Write: the response, its newline, and the preface behind it.
		_, _ = server.Write(append(append(resp, '\n'), []byte(preface)...))
	}()

	rd, _, _, err := writeHello(client, ConnectOptions{Credential: "c"})
	if err != nil {
		t.Fatalf("writeHello: %v", err)
	}

	got := make([]byte, len(preface))
	if _, err := io.ReadFull(rd, got); err != nil {
		t.Fatalf("reading what followed the hello response: %v", err)
	}
	if string(got) != preface {
		t.Errorf("after the hello response the stream held %q, want the h2 preface %q",
			string(got), preface)
	}
}
