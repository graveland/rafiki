// Package sandboxrelay exposes the daemon to a sandbox over a unix socket and
// splices every accepted connection to a fresh, authenticated dial of the
// daemon.
//
// The relay never reads or writes the bytes it carries: the sandbox speaks the
// daemon's own protocol end to end, so the daemon-side credential — not the
// relay — is the gate. That is also why the socket is chmodded world-connectable
// (the container user differs from the host user).
//
// It is stdlib plus log/slog only: cmd/rafiki imports it, and the client must
// never link postgres.
package sandboxrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// SocketName is the file inside the relay dir the sandbox dials.
const SocketName = "daemon.sock"

// DialFunc opens a fresh, raw connection to the daemon (TLS and pin already
// applied, no HTTP written yet). The relay never reads or writes the bytes.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Serve listens on <dir>/daemon.sock and splices every accepted connection to
// a fresh dial(). It returns nil when ctx is cancelled, and a non-nil error
// only if it cannot listen or accept fails for a reason other than shutdown.
func Serve(ctx context.Context, dir string, dial DialFunc) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sandboxrelay: create relay dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, SocketName)

	// A leftover socket file from a previous run is ours to reclaim; anything
	// else at the path is not, and must be left untouched.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("sandboxrelay: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("sandboxrelay: remove stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandboxrelay: stat %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("sandboxrelay: listen on %s: %w", path, err)
	}
	defer ln.Close()
	// The container user is not the host user, so the socket must be
	// world-connectable; the daemon-side credential is the real gate.
	if err := os.Chmod(path, 0o666); err != nil {
		return fmt.Errorf("sandboxrelay: chmod %s: %w", path, err)
	}

	var (
		mu   sync.Mutex
		live = make(map[*pair]struct{})
		wg   sync.WaitGroup
	)

	// Shutdown: close the listener to unblock Accept, then end every live
	// splice. The pairs are snapshotted under the lock and closed outside it,
	// so the mutex is never held across I/O.
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
			mu.Lock()
			closers := make([]io.Closer, 0, len(live))
			for p := range live {
				closers = append(closers, p)
			}
			mu.Unlock()
			for _, c := range closers {
				_ = c.Close()
			}
		case <-watcherDone:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown: let the in-flight splices finish, then report clean.
				wg.Wait()
				return nil
			}
			return fmt.Errorf("sandboxrelay: accept on %s: %w", path, err)
		}

		p := &pair{client: conn}
		mu.Lock()
		if ctx.Err() != nil {
			// Cancelled between Accept and registration: the watcher has
			// already taken its snapshot, so this pair would never be closed.
			// Close it here instead.
			mu.Unlock()
			_ = p.Close()
			wg.Wait()
			return nil
		}
		live[p] = struct{}{}
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(live, p)
				mu.Unlock()
			}()
			serve(ctx, p, dial)
		}()
	}
}

// serve splices one accepted sandbox connection to one fresh daemon dial. It
// owns the pair for its lifetime; the caller keeps it in the live set so a
// shutdown can close it out from under a blocked splice.
func serve(ctx context.Context, p *pair, dial DialFunc) {
	up, err := dial(ctx)
	if err != nil {
		// Close the client so the sandbox sees a closed connection rather than
		// a hung one, and keep serving: one daemon outage must not wedge the
		// listener for the next sandbox.
		_ = p.Close()
		slog.Warn("relay dial failed", "error", err)
		return
	}
	if !p.setUpstream(up) {
		// Shutdown closed the pair while we were dialling; up is already closed.
		return
	}

	client := p.client // immutable after construction
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, up)
		done <- struct{}{}
	}()

	// The relay carries long-lived upgraded HTTP/2 connections, so when EITHER
	// direction ends both are finished: close both rather than attempt
	// half-close choreography. The other copy returns as soon as it is closed.
	<-done
	_ = p.Close()
	<-done
}

// pair is one sandbox connection spliced to one daemon connection. It is
// closed from the splice goroutine and from shutdown concurrently, so its
// fields are mutex-guarded and Close is idempotent.
type pair struct {
	mu     sync.Mutex
	client net.Conn
	up     net.Conn
	closed bool
}

// setUpstream records the daemon-side connection. It reports false if the pair
// was already closed (shutdown won the race), in which case up is closed here.
func (p *pair) setUpstream(up net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = up.Close()
		return false
	}
	p.up = up
	return true
}

// Close ends both halves of the pair. It is idempotent and safe to call from
// any goroutine.
func (p *pair) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	client, up := p.client, p.up
	p.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
	if up != nil {
		_ = up.Close()
	}
	return nil
}
