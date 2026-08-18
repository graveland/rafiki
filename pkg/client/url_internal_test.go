package client

import (
	"encoding/json"
	"net"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// parseControlURL is unexported, so its cases live here (package client)
// rather than in client_test.go, which is package client_test.

func TestParseControlURLRequiresHTTPS(t *testing.T) {
	if _, err := parseControlURL("https://host:443"); err != nil {
		t.Fatalf("https rejected: %v", err)
	}
	if _, err := parseControlURL("tls://host:443"); err == nil {
		t.Fatal("the retired tls:// scheme was accepted")
	}
	if _, err := parseControlURL("http://host:8035"); err == nil {
		t.Fatal("http:// was accepted for the control plane; there is no plaintext listener")
	}
	if _, err := parseControlURL("https://"); err == nil {
		t.Fatal("missing host was accepted")
	}
}

// sendAuthFrame is the exact code DialURL calls once the TLS dial and
// upgrade have succeeded — testing it directly here sidesteps the fact that
// DialURL itself only ever trusts system root CAs, so a self-signed test
// server can never get far enough through TLS verification to observe what
// DialURL wrote on the wire.

// TestSendAuthFrame_EmptyTokenWritesNothing is the no-token dial path: DialURL
// must skip the ctrl_auth frame entirely so a bootstrap daemon's first frame
// is the caller's real request, not an auth handshake it would reject.
func TestSendAuthFrame_EmptyTokenWritesNothing(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()

	// No goroutine reading server: if sendAuthFrame wrote anything, this
	// call would block forever on the unbuffered pipe and the test would
	// hang rather than pass — a header-less "nothing was sent" assertion by
	// construction, not just leftover-bytes inspection at the end.
	if err := sendAuthFrame(cli, ""); err != nil {
		t.Fatalf("sendAuthFrame with empty token returned an error: %v", err)
	}
}

// TestSendAuthFrame_TokenPresentWritesCtrlAuth is the existing (non-bootstrap)
// path: a present token must still produce exactly the ctrl_auth frame the
// daemon's authHandshake expects.
func TestSendAuthFrame_TokenPresentWritesCtrlAuth(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- sendAuthFrame(cli, "secret-token") }()

	fr := protocol.NewFrameReader(server, protocol.MaxFrameBytes)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("sendAuthFrame: %v", err)
	}

	var req protocol.AuthRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("unmarshal auth frame: %v", err)
	}
	if req.Type != protocol.TypeCtrlAuth {
		t.Errorf("type = %q, want %q", req.Type, protocol.TypeCtrlAuth)
	}
	if req.Token != "secret-token" {
		t.Errorf("token = %q, want %q", req.Token, "secret-token")
	}
}
