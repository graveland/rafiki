package upgradeconn

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// rawUpgradeRequest sends an HTTP/1.1 Upgrade request by hand — the only way
// to control the Rafiki-Protocol header and observe the exact status line, since
// http.Client does not speak the upgrade exchange. An empty epoch omits the
// header entirely (an epoch-1 peer).
func rawUpgradeRequest(t *testing.T, addr, epoch string) *http.Response {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	assert.NewAborting(t).NoError(err, "dial")
	t.Cleanup(func() { _ = raw.Close() })

	var b strings.Builder
	b.WriteString("GET " + PathFor(Executor) + " HTTP/1.1\r\n")
	b.WriteString("Host: " + addr + "\r\n")
	b.WriteString("Upgrade: " + string(Executor) + "\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	if epoch != "" {
		b.WriteString(protocol.EpochHeader + ": " + epoch + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(raw, b.String()); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	assert.NewAborting(t).NoError(err, "read response")
	return resp
}

// TestUpgradeEpochMismatchIsAPlain400AndNeverHijacks pins the server half of
// the upgrade epoch gate: a missing or wrong Rafiki-Protocol is an ordinary
// HTTP 400 with a body naming both epochs, authorize never runs (no side
// effect is spent), and the connection is never hijacked.
func TestUpgradeEpochMismatchIsAPlain400AndNeverHijacks(t *testing.T) {
	c := assert.NewAborting(t)
	var authorized atomic.Bool
	addr := serveAuth(t, Executor,
		func(*http.Request) (struct{}, http.Header, error) {
			authorized.Store(true)
			return struct{}{}, nil, nil
		},
		func(conn *Conn, _ struct{}) { conn.Close() })

	for _, tc := range []struct {
		name  string
		epoch string
		peer  string
	}{
		{"missing header is epoch 1", "", "none"},
		{"wrong value", "3", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := rawUpgradeRequest(t, addr, tc.epoch)
			defer resp.Body.Close()
			c.Eq(http.StatusBadRequest, resp.StatusCode, "status")
			body, _ := io.ReadAll(resp.Body)
			want := "rafiki protocol mismatch: this side speaks " +
				strconv.Itoa(protocol.Epoch) + ", peer sent " + tc.peer
			c.StrContains(string(body), want, "body")
		})
	}
	c.False(authorized.Load(), "authorize ran on an epoch mismatch; a one-shot credential would be spent")
}

// TestDialRejectsA101WithTheWrongEpoch pins the client half: a peer that answers
// 101 without the epoch header (an old executor) is refused as a terminal 400
// *Refused, the same classification a plain 400 refusal gets — never retried.
func TestDialRejectsA101WithTheWrongEpoch(t *testing.T) {
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err, "listen")
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, brw, err := hj.Hijack()
			if err != nil {
				return
			}
			// A 101 with NO Rafiki-Protocol header — the old-peer shape.
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: " + string(Executor) + "\r\n" +
				"Connection: Upgrade\r\n\r\n")
			_ = brw.Flush()
			_ = conn.Close()
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	raw, err := net.Dial("tcp", ln.Addr().String())
	c.NoError(err, "dial")
	defer raw.Close()
	_, _, err = Dial(raw, Executor, ln.Addr().String(), nil)
	c.Error(err, "Dial against a 101 without the epoch")
	var ref *Refused
	c.Require().True(errors.As(err, &ref), "want *Refused, got %v", err)
	c.Eq(http.StatusBadRequest, ref.Status, "status: 400 is terminal for the reconnect loops")
	c.StrContains(ref.Reason, "rafiki protocol mismatch: this side speaks 2", "reason")
}

// TestDialSendsTheEpoch pins that the epoch is set inside Dial, so no caller
// can forget it, and that the 101 carries it back.
func TestDialSendsTheEpoch(t *testing.T) {
	c := assert.NewAborting(t)
	got := make(chan string, 1)
	addr := serveAuth(t, Executor,
		func(r *http.Request) (struct{}, http.Header, error) {
			got <- r.Header.Get(protocol.EpochHeader)
			return struct{}{}, nil, nil
		},
		func(conn *Conn, _ struct{}) { conn.Close() })

	conn, resp, err := dialToHdr(t, addr, Executor)
	c.NoError(err, "Dial")
	defer conn.Close()
	c.Eq(strconv.Itoa(protocol.Epoch), <-got, "request header")
	c.Eq(strconv.Itoa(protocol.Epoch), resp.Get(protocol.EpochHeader), "101 header")
}

func dialToHdr(t *testing.T, addr string, proto Protocol) (*Conn, http.Header, error) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	assert.NewAborting(t).NoError(err, "dial")
	t.Cleanup(func() { _ = raw.Close() })
	return Dial(raw, proto, addr, nil)
}
