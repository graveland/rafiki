// SPDX-License-Identifier: Apache-2.0

package daraja

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
)

// Server exposes one Host over Connect.
type Server struct {
	host *Host

	mu       sync.Mutex
	pending  *darajapb.RelayResponse
	attached bool

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

func NewServer(h *Host) *Server {
	return &Server{host: h, shutdownCh: make(chan struct{})}
}

// ShutdownRequested closes when a peer has called Shutdown. daraja dies with
// its child, so the process must exit rather than serve an empty host.
func (s *Server) ShutdownRequested() <-chan struct{} { return s.shutdownCh }

func (s *Server) Routes() (string, http.Handler) {
	return darajapbconnect.NewDarajaServiceHandler(s)
}

func (s *Server) Health(
	ctx context.Context, req *connect.Request[darajapb.HealthRequest],
) (*connect.Response[darajapb.HealthResponse], error) {
	return connect.NewResponse(&darajapb.HealthResponse{
		Pid:     int32(s.host.PID()),
		Running: s.host.Running(),
	}), nil
}

func (s *Server) Restart(
	ctx context.Context, req *connect.Request[darajapb.RestartRequest],
) (*connect.Response[darajapb.RestartResponse], error) {
	pid, err := s.host.Restart(
		specFromProto(req.Msg.GetSpec()),
		time.Duration(req.Msg.GetGraceMs())*time.Millisecond,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&darajapb.RestartResponse{Pid: int32(pid)}), nil
}

// specFromProto maps the wire spec onto the host's. A nil message yields the
// zero ChildSpec, which Restart reads as "reuse what you hold".
func specFromProto(p *darajapb.ChildSpec) ChildSpec {
	if p == nil || p.GetKind() != darajapb.Kind_KIND_CLAUDE {
		return ChildSpec{}
	}
	c := p.GetClaude()
	return ChildSpec{
		Kind:           KindClaude,
		Model:          c.GetModel(),
		ResumeSession:  c.GetResumeSession(),
		PermissionMode: c.GetPermissionMode(),
	}
}

func (s *Server) Shutdown(
	ctx context.Context, req *connect.Request[darajapb.ShutdownRequest],
) (*connect.Response[darajapb.ShutdownResponse], error) {
	code, sig, err := s.host.Shutdown(time.Duration(req.Msg.GetGraceMs()) * time.Millisecond)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
	return connect.NewResponse(&darajapb.ShutdownResponse{ExitCode: int32(code), Signal: sig}), nil
}

// attach claims the single Relay slot. Two streams consuming one event channel
// would split the child's output between them at random.
func (s *Server) attach() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached {
		return false
	}
	s.attached = true
	return true
}

func (s *Server) detach() {
	s.mu.Lock()
	s.attached = false
	s.mu.Unlock()
}

// stash keeps an event that was taken off the host's channel and not
// delivered, so the next stream sends it first.
//
// This is what keeps the backpressure guarantee honest across a reconnect. The
// host blocks rather than dropping output (see Host.emit), but that protects
// only events still IN the channel; one already dequeued is the server's
// responsibility and was previously discarded on a failed send.
func (s *Server) stash(resp *darajapb.RelayResponse) {
	s.mu.Lock()
	s.pending = resp
	s.mu.Unlock()
}

func (s *Server) takePending() *darajapb.RelayResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	return p
}

// Relay pumps stdin in one goroutine and events out on this one.
//
// The receive side runs concurrently because a bidi stream's Receive blocks;
// doing both on one goroutine would mean the child's output only moved when the
// caller happened to send something.
func (s *Server) Relay(
	ctx context.Context,
	stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse],
) error {
	if !s.attach() {
		return connect.NewError(connect.CodeAlreadyExists,
			errors.New("daraja: a relay stream is already attached"))
	}
	defer s.detach()

	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				return
			}
			if b := req.GetStdin(); len(b) > 0 {
				if werr := s.host.WriteStdin(b); werr != nil {
					return
				}
			}
		}
	}()

	if p := s.takePending(); p != nil {
		if err := stream.Send(p); err != nil {
			s.stash(p)
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.host.Done():
			// The event channel is never closed — it has several senders and
			// closing it under one would panic. Done is the end signal.
			return nil
		case ev := <-s.host.Events():
			resp, err := relayResponse(ev)
			if err != nil {
				return err
			}
			if err := stream.Send(resp); err != nil {
				// Taken off the channel and not delivered: hold it for the
				// next stream rather than dropping it on the floor.
				s.stash(resp)
				return err
			}
		}
	}
}

func relayResponse(ev Event) (*darajapb.RelayResponse, error) {
	switch {
	case len(ev.Stdout) > 0:
		return &darajapb.RelayResponse{
			Event: &darajapb.RelayResponse_Stdout{Stdout: ev.Stdout},
		}, nil
	case ev.Restarted != nil:
		return &darajapb.RelayResponse{
			Event: &darajapb.RelayResponse_Restarted{
				Restarted: &darajapb.ProcessRestarted{Pid: int32(*ev.Restarted)},
			},
		}, nil
	case ev.Exited != nil:
		return &darajapb.RelayResponse{
			Event: &darajapb.RelayResponse_Exited{
				Exited: &darajapb.ProcessExited{
					ExitCode: int32(ev.Exited.ExitCode),
					Signal:   ev.Exited.Signal,
				},
			},
		}, nil
	}
	return nil, errors.New("daraja: empty event")
}
