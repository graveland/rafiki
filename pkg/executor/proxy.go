// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// ParseProxyFlags parses repeated --proxy name=base_url pairs into the
// allowlist. This is the whole of what an executor will forward to: the
// operator running this process decides what their own localhost is willing to
// serve, and nothing the daemon sends can widen it.
func ParseProxyFlags(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))
	for _, a := range args {
		name, raw, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("executor: --proxy %q: want name=url", a)
		}
		if name == "" {
			return nil, fmt.Errorf("executor: --proxy %q: empty proxy name", a)
		}
		if raw == "" {
			return nil, fmt.Errorf("executor: --proxy %q: empty base url", a)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("executor: --proxy %q: duplicate proxy name %q", a, name)
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("executor: --proxy %q: invalid base url: %w", a, err)
		}
		if u.Scheme == "unix" {
			// A unix base names the socket to dial, not an HTTP origin: the
			// request URL is built against the fixed http://docker origin and
			// the socket path is used only by the dialer. Accept it only in the
			// authority-less absolute form (unix:///var/run/docker.sock) so a
			// relative path or a host component cannot be mistaken for a socket.
			if u.Host != "" || u.Opaque != "" || u.Path == "" || !filepath.IsAbs(u.Path) {
				return nil, fmt.Errorf("executor: --proxy %q: unix base must be an absolute socket path (unix:///path/to.sock)", a)
			}
			out[name] = strings.TrimSuffix(raw, "/")
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("executor: --proxy %q: scheme must be http or https", a)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("executor: --proxy %q: invalid base url: no host", a)
		}
		out[name] = strings.TrimSuffix(raw, "/")
	}
	return out, nil
}

// ProxyNames returns the declared proxy names, for DescribeResponse.
func (s *Server) ProxyNames() []string {
	out := make([]string, 0, len(s.opts.Proxies))
	for name := range s.opts.Proxies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Proxy relays one HTTP request to a pre-declared LLM endpoint and streams the
// response back. It is an HTTP forwarder, not a generic TCP proxy: the target
// is CONSTRUCTED from the operator's base URL plus a validated path, so a
// compromised daemon reaches only what the operator pre-approved.
func (s *Server) Proxy(
	ctx context.Context,
	stream *connect.BidiStream[executorpb.ProxyRequest, executorpb.ProxyResponse],
) error {
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("proxy: no start message: %w", err))
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("proxy: first message must be a start"))
	}
	base, ok := s.opts.Proxies[start.GetProxyName()]
	if !ok {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("proxy: undeclared proxy name %q", start.GetProxyName()))
	}
	// A unix base names a socket, not an origin: the target is still built as
	// an http:// URL — joinProxyPath keeps validating the path — and the dialer
	// is what carries the socket. http/https bases join as before.
	dialUnix := ""
	httpBase := base
	if strings.HasPrefix(base, "unix://") {
		dialUnix = strings.TrimPrefix(base, "unix://")
		httpBase = "http://docker"
	}
	target, err := joinProxyPath(httpBase, start.GetPath())
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	pr, pw := io.Pipe()
	defer pr.Close() // unblock the filler goroutine on every return path
	go func() {
		defer pw.Close()
		for {
			msg, err := stream.Receive()
			if err != nil {
				return // io.EOF on half-close: the body is complete
			}
			if b := msg.GetBody(); len(b) > 0 {
				if _, err := pw.Write(b); err != nil {
					return
				}
			}
		}
	}()

	req, err := http.NewRequestWithContext(ctx, start.GetMethod(), target, pr)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("proxy: build request: %w", err))
	}
	for k, v := range start.GetHeaders() {
		req.Header.Set(k, v)
	}

	// The docker create-guard needs the whole body before it can decide; every
	// other request streams the pipe straight through.
	if guardContainersCreate(start) {
		body, err := io.ReadAll(io.LimitReader(pr, maxContainerCreateBody+1))
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("proxy: read container create body: %w", err))
		}
		if len(body) > maxContainerCreateBody {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("proxy: container create body exceeds %d bytes", maxContainerCreateBody))
		}
		if err := checkCreateBody(body, s.opts.SandboxMountRoots, s.opts.SandboxRelayDir); err != nil {
			return err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	client := http.DefaultClient
	if dialUnix != "" {
		client = s.unixProxyClient(start.GetProxyName(), dialUnix)
	}

	resp, err := client.Do(req)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("proxy: %s: %w", start.GetProxyName(), err))
	}
	defer resp.Body.Close()

	head := &executorpb.ProxyHead{Status: int32(resp.StatusCode), Headers: map[string]string{}}
	for k := range resp.Header {
		head.Headers[k] = resp.Header.Get(k)
	}
	if err := stream.Send(&executorpb.ProxyResponse{
		Msg: &executorpb.ProxyResponse_Head{Head: head},
	}); err != nil {
		return err
	}

	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if err := stream.Send(&executorpb.ProxyResponse{
				Msg: &executorpb.ProxyResponse_Body{Body: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf("proxy: read response: %w", rerr))
		}
	}
}

// joinProxyPath builds the target URL. The path must be rooted and must not
// climb out of the base after cleaning — the base is the operator's grant, and
// a path that escapes it is a request for a target nobody approved.
func joinProxyPath(base, p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("proxy: path %q must be rooted", p)
	}
	raw, query, _ := strings.Cut(p, "?")
	clean := path.Clean(raw)
	if clean != raw && clean+"/" != raw {
		return "", fmt.Errorf("proxy: path %q escapes the declared base", p)
	}
	if strings.HasPrefix(clean, "/..") {
		return "", fmt.Errorf("proxy: path %q escapes the declared base", p)
	}
	out := base + clean
	if query != "" {
		out += "?" + query
	}
	return out, nil
}

// unixProxyClient returns the cached HTTP client for a proxy whose base is a
// unix socket, dialing socket. One client per proxy name, built lazily under
// the mutex and returned before any request is made: the lock is never held
// across a dial or a request, so a slow socket cannot serialize the executor.
func (s *Server) unixProxyClient(name, socket string) *http.Client {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if s.proxyClients == nil {
		s.proxyClients = make(map[string]*http.Client)
	}
	if c, ok := s.proxyClients[name]; ok {
		return c
	}
	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	s.proxyClients[name] = c
	return c
}
