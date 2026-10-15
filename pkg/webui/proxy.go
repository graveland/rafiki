// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"connectrpc.com/connect"
)

// serveControl forwards a Control-plane request to the resolved daemon.
func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUpstream()
	if err != nil {
		if errors.Is(err, ErrNoCredential) {
			writeJSON(w, http.StatusUnauthorized, errorBody{
				Code:    "unauthenticated",
				Message: "rafiki web: the profile has no token; sign in",
			})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Code:    "unavailable",
			Message: "rafiki web: cannot resolve the daemon: " + err.Error(),
		})
		return
	}
	base, err := url.Parse(u.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Code:    "internal",
			Message: "rafiki web: bad upstream base URL: " + err.Error(),
		})
		return
	}
	s.controlProxy(base, u.Client).ServeHTTP(w, r)
}

// controlProxy builds the passthrough proxy for the Control plane. It streams
// (FlushInterval -1), forwards without an allowlist and without parsing
// messages, and strips the browser's own credentials — the upstream transport
// adds the bearer and the epoch.
func (s *Server) controlProxy(base *url.URL, client *http.Client) *httputil.ReverseProxy {
	transport := http.RoundTripper(http.DefaultTransport)
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			h := pr.Out.Header
			h.Del("Authorization")
			h.Del("Cookie")
			h.Del("X-Rafiki-Web")
			h.Del("Origin")
		},
		Transport:      transport,
		FlushInterval:  -1,
		ModifyResponse: stripUpstreamResponseHeaders,
		ErrorHandler:   writeProxyError,
	}
}

// stripUpstreamResponseHeaders removes headers the browser must never see from
// an upstream response: CORS (there is no Access-Control-* header on any
// response, ever, dev mode included), the gate marker (a response the daemon
// produced never carries it, even against a lying upstream), and Set-Cookie
// (there are no cookies anywhere).
func stripUpstreamResponseHeaders(resp *http.Response) error {
	h := resp.Header
	for name := range h {
		if strings.HasPrefix(name, "Access-Control-") {
			h.Del(name)
		}
	}
	h.Del(gatedHeader)
	h.Del("Set-Cookie")
	return nil
}

// writeProxyError answers a transport error. A Connect error carries its own
// code; anything else is unavailable. This answer is about the daemon, not the
// gate, so it never sets the gate marker header.
func writeProxyError(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusServiceUnavailable
	code := "unavailable"
	message := "rafiki web: the daemon could not be reached"
	var ce *connect.Error
	if errors.As(err, &ce) {
		code = ce.Code().String()
		status = httpStatusForConnectCode(ce.Code())
		message = ce.Message()
		if message == "" {
			message = "rafiki web: the daemon returned " + code
		}
	}
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// httpStatusForConnectCode maps a Connect code to the HTTP status its JSON body
// rides on.
func httpStatusForConnectCode(c connect.Code) int {
	switch c {
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeFailedPrecondition:
		return http.StatusBadRequest
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// serveLoginNotProxied refuses the Login service: the web UI never proxies it,
// so the browser can never drive a login through the proxy.
func serveLoginNotProxied(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, errorBody{
		Code:    "unimplemented",
		Message: "rafiki web never proxies the Login service",
	})
}

// serveStatic answers every other request: embedded assets (with SPA fallback)
// or, in dev, the Vite dev server.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{
			Code:    "unimplemented",
			Message: "rafiki web serves static assets over GET",
		})
		return
	}
	if s.cfg.DevAssets != nil {
		s.serveDevAssets(w, r)
		return
	}
	s.serveEmbedded(w, r)
}

// serveDevAssets reverse-proxies static requests to the Vite dev server.
func (s *Server) serveDevAssets(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.DevAssets
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Header.Del("X-Rafiki-Web")
		},
		FlushInterval:  -1,
		ModifyResponse: stripUpstreamResponseHeaders,
	}
	proxy.ServeHTTP(w, r)
}

// serveEmbedded serves a file from Assets, falling back to index.html for a
// path with no file and no extension (SPA routing).
func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request) {
	fsys := s.cfg.Assets
	if fsys == nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Code:    "internal",
			Message: "rafiki web: no assets configured",
		})
		return
	}
	p := path.Clean(r.URL.Path)
	name := strings.TrimPrefix(p, "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
		http.FileServerFS(fsys).ServeHTTP(w, r)
		return
	}
	if path.Ext(p) == "" {
		s.serveIndex(w, r, fsys)
		return
	}
	http.NotFound(w, r)
}

// serveIndex serves index.html for an SPA route. It reads the file directly
// rather than routing through http.FileServerFS, which would 301 "/index.html"
// to "./" as a directory index.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Code:    "internal",
			Message: "rafiki web: cannot read index.html",
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		// The client is gone; there is nothing to recover.
		return
	}
}
