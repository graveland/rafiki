package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/paths"
)

func TestRequireLoopback(t *testing.T) {
	c := assert.NewAborting(t)
	for _, ok := range []string{"127.0.0.1:6060", "[::1]:6060", "127.0.0.1:0"} {
		c.NoError(requireLoopback(ok), ok)
	}
	for _, bad := range []string{":6060", "0.0.0.0:6060", "localhost:6060", "10.0.0.1:6060", "nope"} {
		c.Error(requireLoopback(bad), bad)
	}
}

func TestStartPprofServesAndRefusesNonLoopback(t *testing.T) {
	c := assert.NewAborting(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pick a free loopback port, release it, and hand it to startPprof.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	addr := probe.Addr().String()
	c.NoError(probe.Close())

	t.Setenv(paths.PprofListen, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.NoError(startPprof(ctx, logger))

	resp, err := http.Get("http://" + addr + "/debug/pprof/cmdline")
	c.NoError(err)
	defer resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode)

	t.Setenv(paths.PprofListen, "0.0.0.0:6060")
	c.Error(startPprof(ctx, logger))

	t.Setenv(paths.PprofListen, "off")
	c.NoError(startPprof(ctx, logger))
}
