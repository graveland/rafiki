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

// StartJob launches command as a background job on the executor and returns
// its handle. It returns as soon as the executor confirms the process is
// running.
func (c *Client) StartJob(ctx context.Context, command string) (string, error) {
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	stream, err := c.inner.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:       "bash",
		InputJson:  input,
		Background: true,
	}))
	if err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	defer stream.Close()

	var handle string
	var failure *executorpb.Failure
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Handle:
			handle = ev.Handle
		case *executorpb.ExecuteResponse_Failed:
			failure = ev.Failed
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("executor start job: %w", err)
	}
	if failure != nil {
		return "", &FailureError{Failure: failure}
	}
	if handle == "" {
		return "", fmt.Errorf("executor start job: no handle returned")
	}
	return handle, nil
}

// JobOutput polls a background job. It never blocks.
func (c *Client) JobOutput(ctx context.Context, handle string, since int64) (tools.JobSnapshot, error) {
	resp, err := c.inner.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{
		Handle: handle, Since: since,
	}))
	if err != nil {
		return tools.JobSnapshot{}, fmt.Errorf("executor job output: %w", err)
	}
	return tools.JobSnapshot{
		Data:     string(resp.Msg.Data),
		Total:    resp.Msg.Total,
		Exited:   resp.Msg.Exited,
		ExitCode: int(resp.Msg.ExitCode),
		Found:    resp.Msg.Found,
	}, nil
}

// KillJob terminates a background job and everything it spawned.
func (c *Client) KillJob(ctx context.Context, handle string) error {
	if _, err := c.inner.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{
		CallId: handle,
	})); err != nil {
		return fmt.Errorf("executor kill job: %w", err)
	}
	return nil
}

// Ping verifies the executor is reachable by calling Describe.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.inner.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		return fmt.Errorf("executor ping: %w", err)
	}
	return nil
}
