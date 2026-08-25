// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// captureSlog redirects the default slog logger into a buffer.
func captureSlog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// startStubServer starts an HTTP/2 (H2C) server on a temporary Unix socket
// serving the given handler, and returns a Connect client wired to it.  The
// server is shut down when the test completes.
func startStubServer(t *testing.T, handler http.Handler) executorpbconnect.ExecutorServiceClient {
	t.Helper()

	sockPath := fmt.Sprintf("/tmp/rafiki-interceptor-%s.sock", strings.ReplaceAll(t.Name(), "/", "_"))
	os.Remove(sockPath)
	t.Cleanup(func() { os.Remove(sockPath) })

	mux := http.NewServeMux()
	mux.Handle("/", handler)

	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protos}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	return executorpbconnect.NewExecutorServiceClient(
		&http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(_ context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		}}},
		"http://executor",
	)
}

func TestRPCLogInterceptorUnary(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	interceptor := NewRPCInterceptor()
	handler := &stubHandler{unaryResult: &executorpb.DescribeResponse{}}
	_, h := executorpbconnect.NewExecutorServiceHandler(handler, connect.WithInterceptors(interceptor))
	client := startStubServer(t, h)

	_, err := client.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatal(err)
	}

	m := parseLogLine(t, buf.String())
	if m["rpc"] != "/rafiki.executor.v1.ExecutorService/Describe" {
		t.Errorf("want rpc=Describe, got %v", m["rpc"])
	}
	if m["result"] != "ok" {
		t.Errorf("want result=ok, got %v", m["result"])
	}
	if _, ok := m["duration"]; !ok {
		t.Error("missing duration")
	}
}

func TestRPCLogInterceptorUnaryError(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	interceptor := NewRPCInterceptor()
	handler := &stubHandler{unaryErr: errors.New("boom")}
	_, h := executorpbconnect.NewExecutorServiceHandler(handler, connect.WithInterceptors(interceptor))
	client := startStubServer(t, h)

	_, err := client.Health(context.Background(), connect.NewRequest(&executorpb.HealthRequest{}))
	if err == nil {
		t.Fatal("expected error")
	}

	m := parseLogLine(t, buf.String())
	if m["result"] != "error" {
		t.Errorf("want result=error, got %v", m["result"])
	}
	if m["code"] != "unknown" {
		t.Errorf("want code=unknown, got %v", m["code"])
	}
}

func TestRPCLogInterceptorStreamingExecute(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	interceptor := NewRPCInterceptor()
	handler := &stubHandler{streamFunc: func(ctx context.Context, stream *connect.ServerStream[executorpb.ExecuteResponse]) error {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Result{
				Result: &executorpb.Result{Content: []*executorpb.ContentBlock{
					{Block: &executorpb.ContentBlock_Text{Text: "hello"}},
				}},
			},
		})
	}}
	_, h := executorpbconnect.NewExecutorServiceHandler(handler, connect.WithInterceptors(interceptor))
	client := startStubServer(t, h)

	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      "read",
		InputJson: []byte(`{}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for stream.Receive() {
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}

	m := parseLogLine(t, buf.String())
	if m["rpc"] != "/rafiki.executor.v1.ExecutorService/Execute" {
		t.Errorf("want rpc=Execute, got %v", m["rpc"])
	}
	if m["tool"] != "read" {
		t.Errorf("want tool=read, got %v", m["tool"])
	}
	if m["result"] != "ok" {
		t.Errorf("want result=ok, got %v", m["result"])
	}
}

func TestRPCLogInterceptorStreamingError(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	interceptor := NewRPCInterceptor()
	handler := &stubHandler{
		streamErr: connect.NewError(connect.CodeNotFound, errors.New("no such workspace")),
	}
	_, h := executorpbconnect.NewExecutorServiceHandler(handler, connect.WithInterceptors(interceptor))
	client := startStubServer(t, h)

	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      "read",
		InputJson: []byte(`{}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for stream.Receive() {
	}
	if stream.Err() == nil {
		t.Fatal("expected error from stream")
	}

	m := parseLogLine(t, buf.String())
	if m["result"] != "error" {
		t.Errorf("want result=error, got %v", m["result"])
	}
	if m["code"] != "not_found" {
		t.Errorf("want code=not_found, got %v", m["code"])
	}
}

func TestRPCLogInterceptorStreamingBidiDoesNotCrash(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	interceptor := NewRPCInterceptor()
	handler := &stubHandler{streamErr: connect.NewError(connect.CodePermissionDenied, errors.New("proxy not allowed"))}
	_, h := executorpbconnect.NewExecutorServiceHandler(handler, connect.WithInterceptors(interceptor))
	client := startStubServer(t, h)

	proxyStream := client.Proxy(context.Background())
	// Proxy is bidi; sending start first triggers the handler.
	_ = proxyStream.Send(&executorpb.ProxyRequest{
		Msg: &executorpb.ProxyRequest_Start{
			Start: &executorpb.ProxyStart{Path: "/v1/chat"},
		},
	})
	if err := proxyStream.CloseRequest(); err != nil {
		t.Logf("close request: %v", err)
	}
	// Read error from stream.
	_, rcvErr := proxyStream.Receive()
	if rcvErr == nil {
		t.Fatal("expected error from bidi stream")
	}

	if strings.Contains(buf.String(), `"tool"`) {
		t.Errorf("unexpected tool in log for bidi stream: %s", buf.String())
	}
}

// parseLogLine unmarshals the first JSON line from raw.
func parseLogLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("invalid JSON log line: %v\n%s", err, raw)
	}
	return m
}

// ─── stub handler ─────────────────────────────────────────────────────────────

// stubHandler implements executorpbconnect.ExecutorServiceHandler with
// configurable results so tests can control what the handler returns without
// wiring up a real executor server.
type stubHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	unaryResult *executorpb.DescribeResponse
	unaryErr    error
	streamFunc  func(context.Context, *connect.ServerStream[executorpb.ExecuteResponse]) error
	streamErr   error
}

func (s *stubHandler) Describe(_ context.Context, _ *connect.Request[executorpb.DescribeRequest]) (*connect.Response[executorpb.DescribeResponse], error) {
	if s.unaryErr != nil {
		return nil, s.unaryErr
	}
	if s.unaryResult != nil {
		return connect.NewResponse(s.unaryResult), nil
	}
	return connect.NewResponse(&executorpb.DescribeResponse{}), nil
}

func (s *stubHandler) Health(_ context.Context, _ *connect.Request[executorpb.HealthRequest]) (*connect.Response[executorpb.HealthResponse], error) {
	if s.unaryErr != nil {
		return nil, s.unaryErr
	}
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

func (s *stubHandler) Execute(_ context.Context, _ *connect.Request[executorpb.ExecuteRequest], stream *connect.ServerStream[executorpb.ExecuteResponse]) error {
	if s.streamErr != nil {
		return s.streamErr
	}
	if s.streamFunc != nil {
		return s.streamFunc(context.Background(), stream)
	}
	return nil
}

func (s *stubHandler) Proxy(ctx context.Context, stream *connect.BidiStream[executorpb.ProxyRequest, executorpb.ProxyResponse]) error {
	if s.streamErr != nil {
		return s.streamErr
	}
	return nil
}

var _ executorpbconnect.ExecutorServiceHandler = (*stubHandler)(nil)
