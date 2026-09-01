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
	pid, err := s.host.Restart(req.Msg.GetArgv(), time.Duration(req.Msg.GetGraceMs())*time.Millisecond)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&darajapb.RestartResponse{Pid: int32(pid)}), nil
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

// Relay pumps stdin in one goroutine and events out on this one.
//
// The receive side runs concurrently because a bidi stream's Receive blocks;
// doing both on one goroutine would mean the child's output only moved when the
// caller happened to send something.
func (s *Server) Relay(
	ctx context.Context,
	stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse],
) error {
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-s.host.Events():
			if !ok {
				return nil
			}
			resp, err := relayResponse(ev)
			if err != nil {
				return err
			}
			if err := stream.Send(resp); err != nil {
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
