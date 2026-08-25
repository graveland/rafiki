// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"connectrpc.com/connect"
)

// udsHTTPClient dials the given unix socket and speaks h2c over it.
func udsHTTPClient(sock string) *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func TestServeConnectUDSAnswersRPCs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	// NewServer takes a HistoryLoader; nil is fine because this test never
	// calls GetHistory. GetChild fails closed with no lister, which is the
	// error we assert on — proving the ROUTE exists, which is the point.
	srv := connectapi.NewServer(nil)

	ln, err := serveConnectUDS(ctx, srv, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	if fi, statErr := os.Stat(sock); statErr != nil {
		t.Fatalf("socket not created: %v", statErr)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	_, err = client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"}))
	if err == nil {
		t.Fatal("want an error from GetChild with no lister wired")
	}
	// The critical assertion: a REAL Connect error, not the CodeUnimplemented
	// that a missing route produces. Unimplemented here means the handler was
	// never mounted.
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("route not mounted: got CodeUnimplemented (%v)", err)
	}
}

func TestServeConnectUDSRefusesALiveSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := serveConnectUDS(ctx, connectapi.NewServer(nil), sock)
	if err != nil {
		t.Fatalf("first serveConnectUDS: %v", err)
	}
	defer ln.Close()

	if _, err := serveConnectUDS(ctx, connectapi.NewServer(nil), sock); err == nil {
		t.Fatal("want the second bind on a live socket to be refused")
	}
}
