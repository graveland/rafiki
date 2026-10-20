// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// fakeNotifier records what the handler asked for, and answers whatever the
// test wants: a nil error, a Connect error, or a plain transport error.
type fakeNotifier struct {
	calls int
	reqs  []*rafikiv1.NotifyRequest
	err   error
	// onCall runs inside Notify, before it returns, so a test can observe the
	// response recorder at the moment of the call.
	onCall func(req *rafikiv1.NotifyRequest)
}

func (f *fakeNotifier) Notify(_ context.Context, req *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error) {
	f.calls++
	f.reqs = append(f.reqs, req)
	if f.onCall != nil {
		f.onCall(req)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &rafikiv1.NotifyResponse{ChildId: "child-1"}, nil
}

// testConfig is a minimal github route, through the same applyDefaults the real
// config goes through.
func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		Daemon: Daemon{URL: "http://127.0.0.1:1", TokenFile: "unused"},
		Routes: []Route{{
			Path:       "/hooks/github",
			Source:     "github",
			SecretFile: "unused",
			Target:     "oncall",
			Rules: []Rule{{
				Event: "workflow_run",
				Key:   "github/{repository.full_name}/{workflow_run.id}",
				Fields: map[string]string{
					"repo":   "repository.full_name",
					"branch": "workflow_run.head_branch",
					"absent": "no.such.path",
				},
			}},
		}},
	}
	cfg.applyDefaults()
	cfg.Routes[0].secret = []byte(testSecret)
	return cfg
}

const workflowBody = `{"action":"failure","repository":{"full_name":"graveland/rafiki"},"workflow_run":{"id":99,"head_branch":"main"}}`

func signedRequest(path, body string, opts ...func(*http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set(githubSignatureHeader, "sha256="+signHex(testSecret, body))
	r.Header.Set("X-GitHub-Event", "workflow_run")
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func delivery(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-GitHub-Delivery", id) }
}

func serve(g *gateway, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, r)
	return rr
}

func TestHandlerDeliversAndPassesTheRequest(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	g := newGateway(testConfig(t), fake)

	resp := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-1")))
	c.Eq(http.StatusOK, resp.Code)
	c.Eq("ok", resp.Body.String())
	c.Eq(1, fake.calls)

	req := fake.reqs[0]
	c.Eq("oncall", req.Target)
	c.Eq("github", req.Source)
	c.Eq("github/graveland/rafiki/99", req.Key)
	c.Eq("graveland/rafiki", req.Fields["repo"])
	c.Eq("main", req.Fields["branch"])
	c.Eq("", req.Fields["absent"])
}

func TestHandlerStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, http.StatusOK},
		{"not found", connect.NewError(connect.CodeNotFound, errors.New(`no live top-level agent named "oncall"`)), http.StatusUnprocessableEntity},
		{"invalid argument", connect.NewError(connect.CodeInvalidArgument, errors.New("bad field")), http.StatusUnprocessableEntity},
		{"failed precondition", connect.NewError(connect.CodeFailedPrecondition, errors.New("protocol mismatch")), http.StatusUnprocessableEntity},
		{"permission denied", connect.NewError(connect.CodePermissionDenied, errors.New("denied")), http.StatusUnprocessableEntity},
		{"unauthenticated", connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token")), http.StatusUnprocessableEntity},
		{"unavailable", connect.NewError(connect.CodeUnavailable, errors.New("down")), http.StatusServiceUnavailable},
		{"deadline exceeded", connect.NewError(connect.CodeDeadlineExceeded, errors.New("slow")), http.StatusServiceUnavailable},
		// A plain error never became a Connect error: the request did not reach
		// the daemon, so the sender should retry.
		{"transport error", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), http.StatusServiceUnavailable},
		{"interrupted", errors.New("read tcp: connection reset by peer"), http.StatusServiceUnavailable},
		{"internal", connect.NewError(connect.CodeInternal, errors.New("boom")), http.StatusInternalServerError},
		{"unknown", connect.NewError(connect.CodeUnknown, errors.New("?")), http.StatusInternalServerError},
		{"resource exhausted", connect.NewError(connect.CodeResourceExhausted, errors.New("slow down")), http.StatusInternalServerError},
	}
	for i, tc := range cases {
		name := tc.name
		err := tc.err
		want := tc.want
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			fake := &fakeNotifier{err: err}
			g := newGateway(testConfig(t), fake)
			resp := serve(g, signedRequest("/hooks/github", workflowBody, delivery(fmt.Sprintf("status-%d", i))))
			c.Eq(want, resp.Code)
			c.Eq(1, fake.calls)
			if want != http.StatusOK && want != http.StatusNoContent {
				c.NotEmpty(resp.Body.String())
			}
		})
	}
}

func TestHandlerUnknownPath(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	g := newGateway(testConfig(t), fake)

	resp := serve(g, signedRequest("/hooks/elsewhere", workflowBody, delivery("d-2")))
	c.Eq(http.StatusNotFound, resp.Code)
	c.Eq("", resp.Body.String(), "an unconfigured path gets no body")
	c.Eq(0, fake.calls)
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	g := newGateway(testConfig(t), fake)

	r := httptest.NewRequest(http.MethodGet, "/hooks/github", nil)
	resp := serve(g, r)
	c.Eq(http.StatusMethodNotAllowed, resp.Code)
	c.Eq("POST", resp.Header().Get("Allow"))
	c.Eq(0, fake.calls)
}

func TestHandlerVerifyRunsBeforeParse(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	g := newGateway(testConfig(t), fake)

	r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader("{not json"))
	r.Header.Set(githubSignatureHeader, "sha256="+signHex("another-secret", "{not json"))
	r.Header.Set("X-GitHub-Event", "workflow_run")
	resp := serve(g, r)
	c.Eq(http.StatusUnauthorized, resp.Code, "an unverifiable sender is refused before its body is looked at")
	c.Eq("unauthorized", resp.Body.String())
	c.Eq(0, fake.calls)

	// And the same invalid JSON, correctly signed, is a 400.
	resp = serve(g, signedRequest("/hooks/github", "{not json", delivery("d-3")))
	c.Eq(http.StatusBadRequest, resp.Code)
	c.Eq(0, fake.calls)

	// A missing signature takes the same 401 path.
	resp = serve(g, httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(workflowBody)))
	c.Eq(http.StatusUnauthorized, resp.Code)
	c.Eq(0, fake.calls)
}

func TestHandlerBodyTooLarge(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	cfg := testConfig(t)
	cfg.Routes[0].MaxBody = 32
	g := newGateway(cfg, fake)

	resp := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-4")))
	c.Eq(http.StatusRequestEntityTooLarge, resp.Code)
	c.Eq(0, fake.calls, "the notifier is never called for a body over the cap")
}

func TestHandlerDuplicateAfterSuccess(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	g := newGateway(testConfig(t), fake)

	first := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-5")))
	c.Eq(http.StatusOK, first.Code)
	c.Eq("ok", first.Body.String())

	second := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-5")))
	c.Eq(http.StatusOK, second.Code)
	c.Eq("duplicate", second.Body.String())
	c.Eq(1, fake.calls, "a duplicate delivery is not sent twice")
}

func TestHandlerRetryAfterUnavailableCallsNotifyAgain(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{err: connect.NewError(connect.CodeUnavailable, errors.New("daemon restarting"))}
	g := newGateway(testConfig(t), fake)

	first := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-6")))
	c.Eq(http.StatusServiceUnavailable, first.Code)
	c.Eq(1, fake.calls)

	// The sender retries the same delivery once the daemon is back.
	fake.err = nil
	second := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-6")))
	c.Eq(http.StatusOK, second.Code)
	c.Eq("ok", second.Body.String())
	c.Eq(2, fake.calls, "an id is only claimed once Notify succeeded")

	third := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-6")))
	c.Eq("duplicate", third.Body.String())
	c.Eq(2, fake.calls)
}

// TestHandlerNoSuccessBeforeNotifyReturns pins the global constraint: the
// gateway never answers 2xx before Notify has returned.
func TestHandlerNoSuccessBeforeNotifyReturns(t *testing.T) {
	c := assert.NewAborting(t)
	rr := newTrackingRecorder()
	var wroteInsideCall bool
	fake := &fakeNotifier{onCall: func(*rafikiv1.NotifyRequest) {
		wroteInsideCall = rr.wrote
	}}
	g := newGateway(testConfig(t), fake)

	g.ServeHTTP(rr, signedRequest("/hooks/github", workflowBody, delivery("d-7")))
	c.False(wroteInsideCall, "nothing was written to the response while Notify was still running")
	c.Eq(http.StatusOK, rr.Code)
	c.Eq("ok", rr.Body.String())
}

func TestHandlerIgnoredWhenNoRuleMatches(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	cfg := testConfig(t)
	cfg.Routes[0].Rules = []Rule{{Event: "push"}}
	g := newGateway(cfg, fake)

	resp := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-8")))
	c.Eq(http.StatusOK, resp.Code)
	c.Eq("ignored", resp.Body.String())
	c.Eq(0, fake.calls)

	// An ignored delivery is not recorded: a later event type on the same
	// delivery id is still delivered.
	cfg.Routes[0].Rules = []Rule{{Event: "workflow_run"}}
	resp = serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-8")))
	c.Eq("ok", resp.Body.String())
	c.Eq(1, fake.calls)
}

func TestHandlerGenericRoute(t *testing.T) {
	c := assert.NewAborting(t)
	fake := &fakeNotifier{}
	cfg := &Config{
		Daemon: Daemon{URL: "http://127.0.0.1:1", TokenFile: "unused"},
		Routes: []Route{{
			Path:       "/hooks/omada",
			Source:     "generic",
			Header:     "X-Omada-Secret",
			SecretFile: "unused",
			Target:     "network-ops",
			Notify:     "omada",
			Rules: []Rule{{
				Match:  map[string]string{"event": "client_offline"},
				Fields: map[string]string{"mac": "client.mac"},
			}},
		}},
	}
	cfg.applyDefaults()
	cfg.Routes[0].secret = []byte(testSecret)
	g := newGateway(cfg, fake)

	const body = `{"event":"client_offline","client":{"mac":"aa:bb:cc"}}`
	r := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/hooks/omada", strings.NewReader(body))
		req.Header.Set("X-Omada-Secret", testSecret)
		return req
	}
	resp := serve(g, r())
	c.Eq(http.StatusOK, resp.Code)
	c.Eq("ok", resp.Body.String())
	c.Eq("omada", fake.reqs[0].Source, "notify_source overrides the route's source name")
	c.Eq("network-ops", fake.reqs[0].Target)
	c.Eq("", fake.reqs[0].Key)
	c.Eq("aa:bb:cc", fake.reqs[0].Fields["mac"])

	// A generic sender carries no delivery id, so nothing is deduped.
	resp = serve(g, r())
	c.Eq("ok", resp.Body.String())
	c.Eq(2, fake.calls)

	// Wrong secret: 401, nothing called.
	bad := httptest.NewRequest(http.MethodPost, "/hooks/omada", strings.NewReader(body))
	bad.Header.Set("X-Omada-Secret", "wrong")
	resp = serve(g, bad)
	c.Eq(http.StatusUnauthorized, resp.Code)
	c.Eq(2, fake.calls)
}

// TestHandlerNotifyTimeout pins that a hung daemon is bounded by
// timeout_seconds rather than the sender's patience.
func TestHandlerNotifyTimeout(t *testing.T) {
	c := assert.NewAborting(t)
	cfg := testConfig(t)
	cfg.Daemon.TimeoutSeconds = 7
	g := newGateway(cfg, &fakeNotifier{})

	var got context.Context
	g.notifier = notifierFunc(func(ctx context.Context, _ *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error) {
		got = ctx
		return &rafikiv1.NotifyResponse{}, nil
	})
	resp := serve(g, signedRequest("/hooks/github", workflowBody, delivery("d-9")))
	c.Eq(http.StatusOK, resp.Code)
	deadline, ok := got.Deadline()
	c.True(ok, "Notify is called with a deadline")
	remaining := time.Until(deadline)
	c.True(remaining > 6*time.Second && remaining <= 7*time.Second, "the deadline is timeout_seconds from now, got %s", remaining)
}

type notifierFunc func(ctx context.Context, req *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error)

func (f notifierFunc) Notify(ctx context.Context, req *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error) {
	return f(ctx, req)
}

// trackingRecorder records whether the handler has written anything yet.
type trackingRecorder struct {
	*httptest.ResponseRecorder
	wrote bool
}

func newTrackingRecorder() *trackingRecorder {
	return &trackingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (t *trackingRecorder) WriteHeader(code int) {
	t.wrote = true
	t.ResponseRecorder.WriteHeader(code)
}

func (t *trackingRecorder) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseRecorder.Write(b)
}
