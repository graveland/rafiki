// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

func TestParseProxyFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := executor.ParseProxyFlags([]string{
		"vmlx=http://localhost:8005",
		"ollama=http://localhost:11434",
	})
	c.Require().NoError(err, "ParseProxyFlags")
	c.False(got["vmlx"] != "http://localhost:8005" || got["ollama"] != "http://localhost:11434", "got %v", got)
}

func TestParseProxyFlagsRejects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no equals", "vmlx", "want name=url"},
		{"empty name", "=http://x", "empty proxy name"},
		{"empty url", "vmlx=", "empty base url"},
		{"not a url", "vmlx=:::", "invalid base url"},
		{"non-http scheme", "vmlx=file:///etc/passwd", "must be http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, err := executor.ParseProxyFlags([]string{tc.in})
			c.Require().Error(err, "accepted %q", tc.in)
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}

func TestParseProxyFlagsRejectsDuplicate(t *testing.T) {
	_, err := executor.ParseProxyFlags([]string{"a=http://1", "a=http://2"})
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "duplicate"), "err = %v, want a duplicate-name error", err)
}

func TestProxyUnixFlagRejectsEmptyPath(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, in := range []string{"x=unix://", "x=unix://relative/sock", "x=unix:relative"} {
		_, err := executor.ParseProxyFlags([]string{in})
		c.Require().Error(err, "accepted %q", in)
		c.StrContains(err.Error(), "unix", "error for %q", in)
	}
}

// proxyOnce drives one Proxy round trip through a real Connect server and
// returns the response head, the streamed body, and the stream's final error
// (nil on a clean EOF).
func proxyOnce(t *testing.T, srv *executor.Server, name, method, p string, headers map[string]string, body []byte) (*executorpb.ProxyHead, []byte, error) {
	t.Helper()
	c := assert.NewAborting(t)
	client := newTestClient(t, srv)
	stream := client.Proxy(context.Background())
	c.NoError(stream.Send(&executorpb.ProxyRequest{
		Msg: &executorpb.ProxyRequest_Start{
			Start: &executorpb.ProxyStart{ProxyName: name, Method: method, Path: p, Headers: headers},
		},
	}), "send start")
	if len(body) > 0 {
		_ = stream.Send(&executorpb.ProxyRequest{
			Msg: &executorpb.ProxyRequest_Body{Body: body},
		})
	}
	_ = stream.CloseRequest()

	var head *executorpb.ProxyHead
	var out bytes.Buffer
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return head, out.Bytes(), nil
			}
			return head, out.Bytes(), err
		}
		switch m := msg.Msg.(type) {
		case *executorpb.ProxyResponse_Head:
			head = m.Head
		case *executorpb.ProxyResponse_Body:
			out.Write(m.Body)
		}
	}
}

func TestProxyUnixRelaysToASocket(t *testing.T) {
	c := assert.NewAborting(t)

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	c.NoError(err, "listen unix")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ := io.ReadAll(r.Body)
		// Echo what arrived so the client can assert each piece.
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("X-Test") + " " + string(gotBody)))
	})}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ln) }()

	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"llm": "unix://" + sockPath},
	})
	head, body, err := proxyOnce(t, esrv, "llm", "POST", "/v1/chat?x=1", map[string]string{"X-Test": "yes"}, []byte(`{"hi":true}`))
	c.NoError(err, "proxy round trip")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("POST /v1/chat?x=1 yes {\"hi\":true}", string(body), "relayed method/path/header/body")
}

// entrypoint records the last request an upstream received.
type recordedRequest struct {
	method string
	path   string
	query  string
	body   []byte
	header string
}

// echoUpstream starts an httptest-style HTTP server that records what it
// received and answers 200 with a fixed body.
func echoUpstream(t *testing.T) (string, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NewAborting(t).NoError(err, "listen")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.body = b
		rec.header = r.Header.Get("X-Test")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("upstream-ok"))
	})}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), rec
}

func TestDockerGuardRefusesOversizeBody(t *testing.T) {
	c := assert.NewAborting(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": "http://127.0.0.1:1"},
	})
	body := bytes.Repeat([]byte("a"), (1<<20)+10)
	_, _, err := proxyOnce(t, esrv, "docker", "POST", "/containers/create", nil, body)
	c.Require().Error(err, "oversize body must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestDockerGuardOnlyAppliesToProxyNamedDocker(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"llm": url},
	})
	body := []byte(`{"HostConfig":{"Binds":["/etc:/host-etc"]}}`)
	head, respBody, err := proxyOnce(t, esrv, "llm", "POST", "/containers/create", nil, body)
	c.NoError(err, "proxy through a non-docker name")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("upstream-ok", string(respBody), "upstream body")
	c.Eq("POST", rec.method, "upstream method")
	c.Eq(string(body), string(rec.body), "upstream body intact")
}

func TestDockerGuardOnlyAppliesToContainersCreate(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	head, respBody, err := proxyOnce(t, esrv, "docker", "GET", "/containers/json", nil, nil)
	c.NoError(err, "GET docker request")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("upstream-ok", string(respBody), "upstream body")
	c.Eq("GET", rec.method, "upstream method")
	c.Eq("/containers/json", rec.path, "upstream path")
}

func TestDockerGuardAppliesToVersionedPath(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	body := []byte(`{"HostConfig":{"Binds":["/etc:/host-etc"]}}`)
	_, _, err := proxyOnce(t, esrv, "docker", "POST", "/v1.43/containers/create", nil, body)
	c.Require().Error(err, "versioned create must be guarded")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
	c.Eq("", rec.method, "upstream must never be reached")
}

func TestDockerGuardReplaysTheBufferedBodyIntact(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	// A volume mount passes the guard with no roots declared; the extra fields
	// make sure the whole body is replayed, not just what the guard parsed.
	body := []byte(`{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"volume","Source":"vol","Target":"/data"}]},"Cmd":["true"]}`)
	head, respBody, err := proxyOnce(t, esrv, "docker", "POST", "/containers/create", nil, body)
	c.NoError(err, "create through the guard")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("upstream-ok", string(respBody), "upstream body")
	c.Eq(string(body), string(rec.body), "upstream received byte-identical body")
}

func TestDockerGuardAppliesToVersionGrammar(t *testing.T) {
	// Every path form docker's version middleware can route to a container
	// create must be guarded — including three-part, single-part and trailing-
	// dot versions, and a mixed-case route.
	paths := []string{
		"/v1.43/containers/create",
		"/v1.43.0/containers/create",
		"/v1/containers/create",
		"/v1.43./containers/create",
		"/Containers/Create",
	}
	bad := []byte(`{"HostConfig":{"Binds":["/:/host"]}}`)
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			c := assert.NewAborting(t)
			url, rec := echoUpstream(t)
			esrv := executor.NewServer(executor.Options{
				Root:    t.TempDir(),
				Proxies: map[string]string{"docker": url},
			})
			_, _, err := proxyOnce(t, esrv, "docker", "POST", p, nil, bad)
			c.Require().Error(err, "create at %q must be guarded", p)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
			c.Eq("", rec.method, "upstream must never be reached for %q", p)
		})
	}
}

func TestDockerGuardRefusesEncodedCreatePath(t *testing.T) {
	// The engine routes on the decoded path and a Go client drops the
	// fragment, so the guard must treat these as creates rather than stream
	// their body unchecked.
	paths := []string{
		"/containers/create#x",
		"/containers/creat%65",
		"/%63ontainers/create",
	}
	bad := []byte(`{"HostConfig":{"Binds":["/:/host"]}}`)
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			c := assert.NewAborting(t)
			url, rec := echoUpstream(t)
			esrv := executor.NewServer(executor.Options{
				Root:    t.TempDir(),
				Proxies: map[string]string{"docker": url},
			})
			_, _, err := proxyOnce(t, esrv, "docker", "POST", p, nil, bad)
			c.Require().Error(err, "create at %q must be guarded", p)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
			c.Eq("", rec.method, "upstream must never be reached for %q", p)
		})
	}
}

func TestDockerGuardPassesPullThrough(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	// A registry-qualified image ref is percent-escaped in the query. The
	// `%`/`#` fail-closed test must apply to the path only, or the daemon cannot
	// pull such an image at all.
	head, respBody, err := proxyOnce(t, esrv, "docker", "POST",
		"/images/create?fromImage=localhost%3A5000%2Fa%2Fb&tag=latest", nil, nil)
	c.NoError(err, "a pull must stream through")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("upstream-ok", string(respBody), "upstream body")
	c.Eq("POST", rec.method, "upstream method")
	c.Eq("/images/create", rec.path, "upstream path")
	c.Eq("fromImage=localhost%3A5000%2Fa%2Fb&tag=latest", rec.query, "upstream query")
}

func TestDockerGuardRefusesDuplicateKeyBypass(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	// A duplicate HostConfig carries a Binds the guard's map decode would not
	// see; the guard must refuse it, not forward it to the engine.
	body := []byte(`{"HostConfig":{"Binds":["/:/host"]},"HostConfig":{"NetworkMode":"none"}}`)
	_, _, err := proxyOnce(t, esrv, "docker", "POST", "/containers/create", nil, body)
	c.Require().Error(err, "duplicate-key bypass must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq("", rec.method, "upstream must never be reached")
}

func TestDockerGuardPassesNormalPostThrough(t *testing.T) {
	c := assert.NewAborting(t)
	url, rec := echoUpstream(t)
	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"docker": url},
	})
	head, respBody, err := proxyOnce(t, esrv, "docker", "POST", "/containers/json", nil, []byte(`{"all":true}`))
	c.NoError(err, "a non-create POST must stream through")
	c.Require().NotNil(head, "head")
	c.Eq(int32(200), head.Status, "status")
	c.Eq("upstream-ok", string(respBody), "upstream body")
	c.Eq("POST", rec.method, "upstream method")
	c.Eq("/containers/json", rec.path, "upstream path")
	c.Eq(`{"all":true}`, string(rec.body), "body reached upstream intact")
}

// A request the caller sent without a body must reach the upstream without
// one: no chunked transfer encoding and a zero length. Docker refuses a
// container start that carries a chunked body.
func TestProxyBodylessRequestIsNotChunked(t *testing.T) {
	c := assert.NewAborting(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err, "listen")
	type seen struct {
		encoding []string
		length   int64
	}
	got := make(chan seen, 1)
	up := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got <- seen{encoding: r.TransferEncoding, length: r.ContentLength}
		w.WriteHeader(204)
	})}
	t.Cleanup(func() { _ = up.Close() })
	go func() { _ = up.Serve(ln) }()

	esrv := executor.NewServer(executor.Options{
		Root:    t.TempDir(),
		Proxies: map[string]string{"up": "http://" + ln.Addr().String()},
	})
	head, _, err := proxyOnce(t, esrv, "up", "POST", "/containers/abc/start", nil, nil)
	c.NoError(err, "proxy round trip")
	c.Require().NotNil(head, "head")
	c.Eq(int32(204), head.Status, "status")

	s := <-got
	c.EqDeep([]string(nil), s.encoding, "a bodyless request must not be chunked")
	c.Eq(int64(0), s.length, "a bodyless request has zero length")
}
