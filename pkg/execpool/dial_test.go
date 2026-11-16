package execpool

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

// A caller-supplied connection is one-shot: Connect must use it exactly once
// and return that attempt's result, never redial. A ticket is spent by the
// first exchange and this process cannot re-establish a tunnel it does not own,
// so a redial loop would spin on a dead connection forever.
func TestConnectOverConnMakesOneAttempt(t *testing.T) {
	c := assert.NewAborting(t)

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	requests := make(chan string, 4)
	go func() {
		br := bufio.NewReader(server)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			requests <- req.URL.Path
			_, _ = io.WriteString(server, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\n\r\nnope")
		}
	}()

	_, handler := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: "sess-conn"})
	err := Connect(context.Background(), ConnectOptions{
		Conn:    client,
		Ticket:  "one-shot-ticket",
		Handler: handler,
	})
	c.ErrorIs(err, ErrEnrollmentRejected, "a 401 over an injected connection is terminal, not retried")

	select {
	case got := <-requests:
		c.Eq(upgradeconn.PathFor(upgradeconn.Executor), got, "the injected connection must carry the upgrade request")
	case <-time.After(2 * time.Second):
		c.Fail("the injected connection was never used")
	}

	select {
	case got := <-requests:
		c.Fail("Connect made a second attempt on a one-shot connection: %s", got)
	case <-time.After(150 * time.Millisecond):
	}

	// dialDaemon hands the injected connection back unchanged, with the
	// conventional Host filler (the daemon's mux routes on path alone).
	got, host, dialErr := dialDaemon(context.Background(), ConnectOptions{Conn: client})
	c.Require().NoError(dialErr, "dialDaemon")
	c.True(got == client, "dialDaemon returned a different connection")
	c.Eq("localhost", host, "Host filler")
}
