// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
)

// loginCallbackLifetime is how long a pending login may sit before it is
// discarded and its pinned listener closed.
const loginCallbackLifetime = 10 * time.Minute

// loginCallTimeout bounds a single LoginBackend call.
const loginCallTimeout = 30 * time.Second

// LoginBackend is how the browser login reaches the daemon and the profile.
type LoginBackend interface {
	// Begin calls BeginLogin with redirectPort and returns its answer.
	Begin(ctx context.Context, redirectPort uint32) (loginID, authorizeURL string, pinnedPort uint32, err error)
	// Complete calls CompleteLogin, writes the token to the profile, and returns
	// the upstream to switch the proxy to.
	Complete(ctx context.Context, loginID, callbackQuery string) (Upstream, error)
}

// EnableLogin registers POST /_rafiki/login/begin (gated with the secret) and
// GET /oidc/callback (Host check only) on s.
func (s *Server) EnableLogin(b LoginBackend) {
	l := &loginState{srv: s, backend: b}
	s.Handle("POST /_rafiki/login/begin", http.HandlerFunc(l.serveBegin), true)
	s.Handle("GET /oidc/callback", http.HandlerFunc(l.serveCallback), false)
}

// loginState is the login bookkeeping for one Server. The handlers EnableLogin
// registers capture it, so it lives here rather than as a Server field.
type loginState struct {
	srv     *Server
	backend LoginBackend

	beginM sync.Mutex // serialises begins, which share the pinned callback port

	mu sync.Mutex
	// pending is the current login, nil if none. completing is a pending whose
	// callback has taken it and is running Complete — it is cleared from pending
	// so a second callback gets 404, but kept here so a begin arriving during the
	// (up to 30s) Complete can still release its pinned listener before binding
	// the same port again.
	pending    *pendingLogin
	completing *pendingLogin
}

// pendingLogin is at most one in-flight login.
type pendingLogin struct {
	loginID  string
	listener net.Listener // nil unless the daemon pinned a callback port
	server   *http.Server // the pinned listener's server, nil with it
	timer    *time.Timer  // clears the pending login after loginCallbackLifetime
}

// serveBegin starts a login. A new begin replaces any pending one; its pinned
// listener is closed before Begin runs and the same port is bound again.
func (l *loginState) serveBegin(w http.ResponseWriter, r *http.Request) {
	l.beginM.Lock()
	defer l.beginM.Unlock()

	l.replacePending()

	ctx, cancel := context.WithTimeout(r.Context(), loginCallTimeout)
	defer cancel()
	loginID, authorizeURL, pinnedPort, err := l.backend.Begin(ctx, uint32(l.srv.cfg.Port))
	if err != nil {
		writeLoginError(w, err)
		return
	}

	p := &pendingLogin{loginID: loginID}
	if pinnedPort != 0 && int(pinnedPort) != l.srv.cfg.Port {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pinnedPort))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{
				Code: "failed_precondition",
				Message: fmt.Sprintf(
					"cannot listen on callback port %d (pinned by the daemon's oidc.toml): %v",
					pinnedPort, err),
			})
			return
		}
		p.listener = ln
		p.server = &http.Server{
			Handler:           l.pinnedCallback(uint32(pinnedPort)),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	l.mu.Lock()
	l.pending = p
	p.timer = time.AfterFunc(loginCallbackLifetime, func() { l.expire(p) })
	l.mu.Unlock()

	if p.server != nil {
		go func() { _ = p.server.Serve(p.listener) }()
	}

	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authorizeURL})
}

// serveCallback completes the single pending login. It runs on the main mux
// (behind the Host gate) and, when the daemon pinned a callback port, on a
// listener that serves only this route.
func (l *loginState) serveCallback(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	p := l.pending
	l.pending = nil
	if p != nil {
		l.completing = p
	}
	l.mu.Unlock()
	if p == nil {
		writeJSON(w, http.StatusNotFound, errorBody{
			Code:    "failed_precondition",
			Message: "rafiki web: no login is pending",
		})
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}

	ctx, cancel := context.WithTimeout(r.Context(), loginCallTimeout)
	defer cancel()
	up, err := l.backend.Complete(ctx, p.loginID, r.URL.RawQuery)
	if err != nil {
		writeLoginFailedPage(w, err)
	} else {
		l.srv.SetUpstream(up)
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/#/login-done", l.srv.cfg.Port), http.StatusFound)
	}

	// The pinned listener existed only for this callback; close it once the
	// response is written.
	if p.server != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := p.server.Shutdown(ctx); err != nil {
				_ = p.server.Close()
			}
			l.clearCompleting(p)
		}()
	} else {
		l.clearCompleting(p)
	}
}

// pinnedCallback serves the OIDC callback on the port the daemon pinned,
// accepting only a Host that names that port.
func (l *loginState) pinnedCallback(pinned uint32) http.Handler {
	loopback := fmt.Sprintf("127.0.0.1:%d", pinned)
	localhost := fmt.Sprintf("localhost:%d", pinned)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oidc/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Host != loopback && r.Host != localhost {
			writeJSON(w, http.StatusForbidden, errorBody{
				Code:    "permission_denied",
				Message: fmt.Sprintf("rafiki web refuses Host %q on the callback port", r.Host),
			})
			return
		}
		l.serveCallback(w, r)
	})
}

// replacePending drops any pending login and any login whose callback is
// mid-Complete, closing their pinned listeners so the caller can bind the same
// port again. Closing a listener twice is safe, so a begin racing a callback's
// own shutdown is not a hazard.
func (l *loginState) replacePending() {
	l.mu.Lock()
	old := l.pending
	l.pending = nil
	completing := l.completing
	l.completing = nil
	l.mu.Unlock()
	discardLogin(old)
	discardLogin(completing)
}

// clearCompleting forgets p once its callback has finished shutting its pinned
// listener down, unless a newer begin has already replaced it.
func (l *loginState) clearCompleting(p *pendingLogin) {
	l.mu.Lock()
	if l.completing == p {
		l.completing = nil
	}
	l.mu.Unlock()
}

// expire clears p if it is still the pending login (the 10 minute lifetime).
func (l *loginState) expire(p *pendingLogin) {
	l.mu.Lock()
	if l.pending != p {
		l.mu.Unlock()
		return
	}
	l.pending = nil
	l.mu.Unlock()
	discardLogin(p)
}

// discardLogin stops a pending login's timer and releases its socket. The
// listener is closed directly as well as through the server: a server whose
// Serve goroutine has not started yet tracks no listeners, so Close on it alone
// would leave the port bound.
func discardLogin(p *pendingLogin) {
	if p == nil {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.server != nil {
		_ = p.server.Close()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
}

// writeLoginError answers a Begin failure with the Connect-style JSON error and
// the status its Connect code maps to.
func writeLoginError(w http.ResponseWriter, err error) {
	code := "internal"
	status := http.StatusInternalServerError
	message := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		code = ce.Code().String()
		status = httpStatusForConnectCode(ce.Code())
		if ce.Message() != "" {
			message = ce.Message()
		}
	}
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// writeLoginFailedPage answers a Complete failure with an HTML page the browser
// shows instead of the app.
func writeLoginFailedPage(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w,
		"<p>rafiki: login failed: %s. Close this window and try again.</p>",
		html.EscapeString(err.Error()))
}
