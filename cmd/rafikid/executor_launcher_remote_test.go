// SPDX-License-Identifier: Apache-2.0

package main

// The launcher adapter's own tests, over a REAL connect handler.
//
// Everything else in this package fakes installLauncher, which answers a Go call
// and so can never show what the WIRE does: a fake hands back the *connect.Error
// it was given, while a client stream reports the same refusal through Send, as
// an error wrapping io.EOF, because the status itself only arrives at
// CloseAndReceive. That difference is load-bearing — it decides between falling
// back to the relay and refusing an install as too old — so the adapter is driven
// here against a real service handler and a real HTTP/2 stream.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

// deadStreamFrame is a frame larger than an HTTP/2 flow-control window, so a send
// of it CANNOT complete while the server refuses to read. That is what makes a
// client stream's Send observe the refusal: with a small frame the write is
// buffered by the transport, the status is read at CloseAndReceive, and the
// adapter looks correct whether or not it reads the status off the stream.
const deadStreamFrame = 8 << 20

// launcherStub is a real ExecutorService handler. refuse, when set, is what
// RemoteLaunch/RemoteServe/RemoteInstall answer — the shape of a launcher binary
// built before the direct-dial RPCs existed — and it is answered WITHOUT reading
// the request stream, exactly as a service that has no such method does.
type launcherStub struct {
	executorpbconnect.UnimplementedExecutorServiceHandler

	refuse error

	mu       sync.Mutex
	launches int
	requests []*executorpb.RemoteLaunchRequest
}

func (s *launcherStub) RemoteLaunch(
	_ context.Context, stream *connect.ClientStream[executorpb.RemoteLaunchRequest],
) (*connect.Response[executorpb.RemoteLaunchResponse], error) {
	s.mu.Lock()
	s.launches++
	refuse := s.refuse
	s.mu.Unlock()
	if refuse != nil {
		return nil, refuse
	}
	for stream.Receive() {
		s.mu.Lock()
		s.requests = append(s.requests, stream.Msg())
		s.mu.Unlock()
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&executorpb.RemoteLaunchResponse{State: "started", Pid: 4242}), nil
}

func (s *launcherStub) RemoteServe(
	_ context.Context, _ *connect.BidiStream[executorpb.RemoteServeRequest, executorpb.RemoteServeResponse],
) error {
	return s.refusal()
}

func (s *launcherStub) RemoteInstall(
	_ context.Context, _ *connect.ClientStream[executorpb.RemoteInstallRequest],
) (*connect.Response[executorpb.RemoteInstallResponse], error) {
	return nil, s.refusal()
}

// refusal is what this stub answers every RPC with while it is standing in for an
// old launcher.
func (s *launcherStub) refusal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuse
}

// launchCount returns how many launch streams the handler was asked to serve.
func (s *launcherStub) launchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launches
}

// launchFrames returns the launch stream's frames, in order.
func (s *launcherStub) launchFrames() []*executorpb.RemoteLaunchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*executorpb.RemoteLaunchRequest(nil), s.requests...)
}

// newLauncherRemote serves stub over a real connect handler and returns the
// adapter's own client for it.
func newLauncherRemote(t *testing.T, stub *launcherStub) launcherRemote {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(stub))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return launcherRemote{cl: executorpbconnect.NewExecutorServiceClient(srv.Client(), srv.URL)}
}

// refuseOldLauncher is what a launcher built before the direct-dial RPCs answers.
func refuseOldLauncher() error {
	return connect.NewError(connect.CodeUnimplemented,
		errors.New("unknown service rafiki.executor.v1.ExecutorService"))
}

// An old launcher's CodeUnimplemented must survive the client stream: connect
// reports it from Send as an error wrapping io.EOF, and only CloseAndReceive
// carries the status. Returning the Send error as-is turns the refusal into
// CodeUnknown, which is what isUnimplemented reads — and both the relay fallback
// and the "launcher is too old" refusal hang off that.
func TestLauncherRemoteLaunchReadsTheRefusalOffTheDeadStream(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &launcherStub{refuse: refuseOldLauncher()}
	remote := newLauncherRemote(t, stub)

	// A ticket this size cannot be written while the refusal has stopped the
	// server reading, so the Send fails the way it does over a real link.
	_, err := remote.Launch(context.Background(),
		&executorpb.RemoteLaunchStart{Target: &executorpb.RemoteTarget{Destination: "brent@ublk-s2"}},
		strings.Repeat("t", deadStreamFrame))

	c.Require().Error(err, "an unimplemented launcher must fail the launch")
	c.True(isUnimplemented(err),
		"the status must survive the stream, not read as CodeUnknown: %v (code %v)", err, connect.CodeOf(err))
	c.Eq(connect.CodeUnimplemented, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "unknown service", "the launcher's own words ride the error")
	c.Eq(1, stub.launchCount(), "one launch stream")
}

// The relay's start frame has the same problem — an old launcher's refusal to
// Serve arrives from Send — and the tunnel install's whole diagnostic is that
// error's text.
func TestLauncherRemoteServeReadsTheRefusalOffTheDeadStream(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &launcherStub{refuse: refuseOldLauncher()}
	remote := newLauncherRemote(t, stub)

	_, err := remote.Serve(context.Background(), &executorpb.RemoteServeStart{
		Target: &executorpb.RemoteTarget{Destination: "brent@ublk-s2"},
		Root:   strings.Repeat("r", deadStreamFrame),
	})

	c.Require().Error(err, "an unimplemented launcher must fail the relay")
	c.True(isUnimplemented(err),
		"the status must survive the stream: %v (code %v)", err, connect.CodeOf(err))
}

// And the binary push, whose chunk sends are what a slow link blocks on.
func TestLauncherRemotePushReadsTheRefusalOffTheDeadStream(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &launcherStub{refuse: refuseOldLauncher()}
	remote := newLauncherRemote(t, stub)

	err := remote.Push(context.Background(), &executorpb.RemoteTarget{Destination: "brent@ublk-s2"},
		"/home/brent/.cache/rafiki/bin/rafiki", "sha", bytes.NewReader(bytes.Repeat([]byte("b"), deadStreamFrame)))

	c.Require().Error(err, "an unimplemented launcher must fail the push")
	c.True(isUnimplemented(err),
		"the status must survive the stream: %v (code %v)", err, connect.CodeOf(err))
}

// The ticket is the stream's stdin frame and NOTHING else: frame one is the start
// frame with the ticket in no field, frame two is the ticket and a newline, and
// the handler sees no third frame.
func TestLauncherRemoteLaunchSendsTheTicketAsTheOnlyStdinFrame(t *testing.T) {
	c := assert.NewAborting(t)
	const ticket = "0193e2a1-7c4d-7f3b-9c2e-1a2b3c4d5e6f"
	stub := &launcherStub{}
	remote := newLauncherRemote(t, stub)

	start := &executorpb.RemoteLaunchStart{
		Target:     &executorpb.RemoteTarget{Destination: "brent@ublk-s2", Instance: "work"},
		BinaryPath: "/home/brent/.cache/rafiki/bin/rafiki",
		Root:       "/srv/rafiki",
		InstallId:  "recipe-1",
	}
	resp, err := remote.Launch(context.Background(), start, ticket)
	c.Require().NoError(err, "launch")
	c.Eq("started", resp.GetState(), "the handler's answer rides back")
	c.Eq(int32(4242), resp.GetPid(), "pid")

	c.Eq(1, stub.launchCount(), "exactly one launch stream reached the handler")
	frames := stub.launchFrames()
	c.Require().Len(frames, 2, "two frames and no more")

	// Frame one: the daemon-written start, which carries no secret.
	c.Require().NotNil(frames[0].GetStart(), "frame 1 is the start frame")
	c.Nil(frames[0].GetStdin(), "frame 1 carries no stdin")
	c.True(proto.Equal(start, frames[0].GetStart()), "the start frame is the caller's own: got %v", frames[0].GetStart())
	c.NotStrContains(prototext.Format(frames[0]), ticket,
		"no field of the start frame may carry the ticket")

	// Frame two: the ticket, terminated, as the stream's only stdin.
	c.Require().NotNil(frames[1].GetStdin(), "frame 2 is the stdin frame")
	c.Nil(frames[1].GetStart(), "frame 2 is not a second start frame")
	c.True(bytes.Equal([]byte(ticket+"\n"), frames[1].GetStdin()),
		"the stdin frame is exactly the ticket and a newline, got %q", string(frames[1].GetStdin()))
}
