// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// contentSecurityPolicy is set on every response, refusal or not.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data: blob:; " +
	"style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// gatedHeader marks a response the web gate itself refused. A response the
// daemon produced never carries it, which is how the UI tells a gone session
// apart from the daemon wanting a login.
const gatedHeader = "Rafiki-Web-Gate"

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// gate applies the Host, Origin and OPTIONS checks to every request, sets the
// security headers on every response, and then dispatches to next.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSecurityHeaders(w)
		if !s.hostAllowed(r.Host) {
			refuse(w, http.StatusForbidden, "permission_denied",
				fmt.Sprintf("rafiki web refuses Host %q; open it at 127.0.0.1:%d", r.Host, s.cfg.Port))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.originAllowed(r.Header.Get("Origin")) {
				refuse(w, http.StatusForbidden, "permission_denied",
					"rafiki web refuses a write from a foreign or missing Origin")
				return
			}
		}
		if r.Method == http.MethodOptions {
			refuse(w, http.StatusMethodNotAllowed, "unimplemented",
				"rafiki web answers no CORS preflight")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSecret wraps a handler that only a live session secret may reach.
func (s *Server) requireSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.valid(r.Header.Get("X-Rafiki-Web")) {
			refuse(w, http.StatusUnauthorized, "unauthenticated",
				"rafiki web: missing or invalid X-Rafiki-Web session secret")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether host is exactly 127.0.0.1:<Port> or
// localhost:<Port>.
func (s *Server) hostAllowed(host string) bool {
	return host == fmt.Sprintf("127.0.0.1:%d", s.cfg.Port) || host == fmt.Sprintf("localhost:%d", s.cfg.Port)
}

// originAllowed reports whether origin is exactly http://127.0.0.1:<Port> or
// http://localhost:<Port>. An empty origin does not match.
func (s *Server) originAllowed(origin string) bool {
	return origin == fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port) || origin == fmt.Sprintf("http://localhost:%d", s.cfg.Port)
}

// writeSecurityHeaders sets the three headers present on every response.
func writeSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// refuse answers a gate refusal: the gate marker header plus a Connect-style
// JSON error body.
func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set(gatedHeader, "refused")
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// writeJSON writes v as JSON with the given status. The body shape is always a
// Connect-style {code,message} for errors, or the endpoint's own object.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"code":"internal","message":"rafiki web: could not encode a response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		// The client is gone; there is nothing to recover.
		return
	}
}
