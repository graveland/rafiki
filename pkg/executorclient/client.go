// Package executorclient provides a Connect client that dials the executor's
// unix socket and an in-memory fake for parent-side tests.
package executorclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// Compile-time interface check.
var _ tools.ExecutorClient = (*Client)(nil)

// Client is a tools.ExecutorClient backed by a Connect transport to an
// executor process over a local unix socket.
type Client struct {
	inner executorpbconnect.ExecutorServiceClient
}

// Dial connects to the executor listening at socketPath and returns a Client
// ready to dispatch tool calls.
func Dial(socketPath string) (*Client, error) {
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	httpClient := &http.Client{Transport: transport}
	inner := executorpbconnect.NewExecutorServiceClient(httpClient, "http://executor")
	return &Client{inner: inner}, nil
}

// Execute runs the named tool on the executor and returns the flattened
// result string. This is the tools.ExecutorClient interface.
func (c *Client) Execute(ctx context.Context, tool string, input json.RawMessage) (string, error) {
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      tool,
		InputJson: input,
		TimeoutMs: 600_000, // 10 minutes; matches bash.go's maxBashTimeout
	}))
	if err != nil {
		return "", fmt.Errorf("executor execute: %w", err)
	}
	defer stream.Close()

	var resultText string
	var failure *executorpb.Failure
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			failure = ev.Failed
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor stream: %w", err)
	}
	if failure != nil {
		return "", fmt.Errorf("executor: %s (code %v)", failure.Message, failure.Code)
	}
	return resultText, nil
}
