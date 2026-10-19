package execpool

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// returnsWithin runs fn and reports whether it returned inside d. A regression
// leaves fn blocked, so the test fails instead of hanging the suite.
func returnsWithin(d time.Duration, fn func() error) (error, bool) {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(d):
		return nil, false
	}
}

// A peer that accepts the TCP connection and then says nothing leaves the TLS
// handshake waiting forever. The dial must give up when ctx does.
func TestDialDaemonGivesUpOnAStalledHandshakeWhenCtxIsDone(t *testing.T) {
	ck := assert.NewCollecting(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	ck.Require().NoError(err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err, returned := returnsWithin(3*time.Second, func() error {
		conn, _, err := dialDaemon(ctx, ConnectOptions{Addr: ln.Addr().String(), ServerName: "localhost"})
		if conn != nil {
			conn.Close()
		}
		return err
	})

	ck.Require().True(returned, "dialDaemon outlived its context: the TLS handshake is not bounded by ctx")
	ck.Error(err, "a stalled handshake must be an error")
}

// A peer that never answers the HTTP upgrade is bounded only by a fixed
// deadline. Cancelling ctx must end the attempt without waiting that out.
func TestConnectOnceReturnsWhenCtxIsCancelledDuringTheUpgrade(t *testing.T) {
	ck := assert.NewCollecting(t)
	dir, err := os.MkdirTemp("/tmp", "ep-")
	ck.Require().NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	ck.Require().NoError(err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, returned := returnsWithin(3*time.Second, func() error {
		return connectOnce(ctx, ConnectOptions{SocketPath: sock, Credential: "cred"})
	})

	ck.True(returned, "connectOnce waited out the fixed upgrade deadline after ctx was done")
}
