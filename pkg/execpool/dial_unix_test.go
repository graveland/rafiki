package execpool

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// The executor must be able to reach a daemon on the same machine without TLS,
// a certificate, or a hostname — and must still speak the identical protocol
// from the upgrade onward.
func TestConnectOverUnixSocketReachesTheUpgradeEndpoint(t *testing.T) {
	// macOS TempDir paths often exceed the ~104-char unix-socket-path limit.
	// Use /tmp directly with a unique short name.
	dir, err := os.MkdirTemp("/tmp", "ep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	reached := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.Handle(upgradeconn.PathFor(upgradeconn.Executor),
		upgradeconn.Handler(upgradeconn.Executor, func(c *upgradeconn.Conn) {
			select {
			case reached <- struct{}{}:
			default:
			}
			c.Close()
		}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// connectOnce, not Connect: Connect retries forever and this test wants the
	// single attempt's outcome. The error is expected — the handler closes the
	// connection immediately — so assert on having REACHED the endpoint.
	_ = connectOnce(ctx, ConnectOptions{
		SocketPath:  sock,
		EnrollToken: "t_test",
		Handler:     http.NewServeMux(),
	})

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("the unix dial never reached the executor upgrade endpoint")
	}
}
