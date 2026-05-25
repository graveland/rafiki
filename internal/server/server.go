// Package server implements a Unix-domain-socket listener with JSONL framing.
//
// Each accepted connection runs in its own goroutine. Frames are read with
// protocol.NewFrameReader (16 MB cap) and dispatched through a FrameHandler.
// Non-empty handler responses are written back as frames.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"graveland.dev/pi-controller/internal/protocol"
)

// FrameHandler is called once per inbound JSONL frame. It may return a
// response frame (written back to the client) or nil/empty to send nothing.
type FrameHandler func(frame []byte) []byte

// Server is a running Unix-domain-socket listener. Call Close to stop it.
type Server struct {
	ln      net.Listener
	handler FrameHandler
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	ctx     context.Context
}

// Listen creates a Unix-domain-socket listener at path. If a stale socket
// file exists at path and no process is actively listening on it, the file is
// removed before binding. If a live process is listening, Listen returns an
// error rather than overwriting the socket.
//
// The socket is chmod'd to 0600 after binding.
func Listen(path string, handler FrameHandler) (*Server, error) {
	// Stale-socket probe: if the path exists, try to dial. If dial fails the
	// old listener is gone — safe to unlink. If dial succeeds, a live process
	// owns the socket and we must not clobber it.
	if _, err := os.Stat(path); err == nil {
		if dialErr := tryDial(path); dialErr != nil {
			_ = os.Remove(path)
		} else {
			return nil, errors.New("socket in use by a live process")
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ln: ln, handler: handler, ctx: ctx, cancel: cancel}
	go s.acceptLoop()
	return s, nil
}

// tryDial probes path with a disposable connection. Error means no listener.
func tryDial(path string) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Distinguish "listener closed by Close()" from real errors.
			if s.ctx.Err() != nil {
				return
			}
			slog.Warn("server: accept", "error", err)
			continue
		}
		// Guard against a Close() that ran between Accept returning and wg.Add:
		// if the context is already cancelled, drop this connection and exit.
		if s.ctx.Err() != nil {
			conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Close this connection when the server shuts down so ReadFrame unblocks.
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go func() {
		select {
		case <-s.ctx.Done():
			conn.Close()
		case <-shutdownDone:
		}
	}()

	r := protocol.NewFrameReader(conn, 16<<20)
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if err != io.EOF && s.ctx.Err() == nil {
				slog.Warn("server: read frame", "remote", conn.RemoteAddr(), "error", err)
			}
			return
		}
		resp := s.handler(frame)
		if len(resp) > 0 {
			if err := protocol.WriteFrame(conn, resp); err != nil {
				if s.ctx.Err() == nil {
					slog.Warn("server: write frame", "remote", conn.RemoteAddr(), "error", err)
				}
				return
			}
		}
	}
}

// Close shuts down the server: it cancels the internal context (which closes
// any in-progress per-connection reads), closes the listener (which unblocks
// the accept loop), and waits for all connection goroutines to finish.
func (s *Server) Close() error {
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}
