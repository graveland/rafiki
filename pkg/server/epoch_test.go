// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// roundTripFunc lets a test observe the response headers a Connect client saw
// without reimplementing the transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRequireEpochRefusesMissingAndWrongValues pins the refusal: a missing or
// wrong Rafiki-Protocol is a Connect failed_precondition with ErrorInfo reason
// protocol_mismatch, and the response STILL carries the daemon's own epoch so a
// client can always read it.
func TestRequireEpochRefusesMissingAndWrongValues(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(RequireEpoch(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	var last http.Header
	base := srv.Client().Transport
	client := rafikiv1connect.NewControlClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := base.RoundTrip(r)
			if resp != nil {
				last = resp.Header
			}
			return resp, err
		}),
	}, srv.URL)

	for _, tc := range []struct {
		name  string
		epoch string
		peer  string
	}{
		{"missing header is epoch 1", "", "none"},
		{"wrong value", "3", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := connect.NewRequest(&rafikiv1.ListModelsRequest{})
			if tc.epoch != "" {
				req.Header().Set(protocol.EpochHeader, tc.epoch)
			}
			_, err := client.ListModels(context.Background(), req)
			c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
			c.Eq(protocol.ErrProtocolMismatch, rpcreason.Reason(err), "reason")
			c.StrContains(err.Error(), "this daemon speaks rafiki protocol 2", "message")
			c.StrContains(err.Error(), "peer sent "+tc.peer, "message names the peer")
			c.Eq(strconv.Itoa(protocol.Epoch), last.Get(protocol.EpochHeader),
				"a refusal must still carry the daemon's epoch")
		})
	}
}

// TestRequireEpochPassesMatching pins the happy path: a matching header reaches
// the handler, and the response carries the epoch.
func TestRequireEpochPassesMatching(t *testing.T) {
	c := assert.NewAborting(t)
	ran := false
	h := RequireEpoch(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/rafiki.v1.Control/ListModels", nil)
	req.Header.Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	c.True(ran, "handler never ran for a matching epoch")
	c.Eq(http.StatusOK, rec.Code, "status")
	c.Eq(strconv.Itoa(protocol.Epoch), rec.Header().Get(protocol.EpochHeader), "response header")
}

// TestRequireEpochIgnoresNonConnectPaths pins that the gate is scoped to
// /rafiki.v1.*: the MCP face, /healthz and the upgrade routes are not Connect
// and must pass through untouched, with no epoch header forced onto them.
func TestRequireEpochIgnoresNonConnectPaths(t *testing.T) {
	c := assert.NewAborting(t)
	ran := false
	h := RequireEpoch(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/mcp/", "/healthz", "/executor/connect", "/daraja/connect"} {
		ran = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		c.True(ran, "handler never ran for %s", path)
		c.Eq(http.StatusOK, rec.Code, "status for %s", path)
		c.Eq("", rec.Header().Get(protocol.EpochHeader), "no epoch header on %s", path)
	}
}

// connectProcedurePaths returns every RPC path the generated control.proto
// descriptor declares — Control and Login — so the route walk below cannot miss
// a verb when one is added.
func connectProcedurePaths(t *testing.T) []string {
	t.Helper()
	fd := rafikiv1.File_rafiki_v1_control_proto
	svcs := fd.Services()
	var paths []string
	for i := 0; i < svcs.Len(); i++ {
		svc := svcs.Get(i)
		methods := svc.Methods()
		for j := 0; j < methods.Len(); j++ {
			paths = append(paths, "/"+string(svc.FullName())+"/"+string(methods.Get(j).Name()))
		}
	}
	return paths
}

// TestEveryRafikiV1RouteIsGatedOnBothMounts walks every Control and Login
// procedure on both Connect mounts — the proxy face (Handler.Mount) and the UDS
// shape serveConnectUDS builds — and fails if any answers without the epoch
// header refused. On the proxy face the Control mount sits behind a deny-all
// wrap, so the 400 (not a 401) proves the gate is OUTSIDE authentication.
func TestEveryRafikiV1RouteIsGatedOnBothMounts(t *testing.T) {
	c := assert.NewAborting(t)
	paths := connectProcedurePaths(t)
	c.True(len(paths) > 2, "expected Control and Login procedures, got %d", len(paths))

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}

	// Mount 1: the proxy face, built by Handler.Mount exactly as proxy.go does.
	proxy := http.NewServeMux()
	h := &Handler{ControlPath: "/rafiki.v1.Control/", Control: ok, LoginPath: "/rafiki.v1.Login/", Login: ok}
	h.Mount(proxy, deny)

	// Mount 2: the UDS shape serveConnectUDS builds — Control and Login each
	// wrapped in RequireEpoch.
	uds := http.NewServeMux()
	uds.Handle("/rafiki.v1.Control/", RequireEpoch(ok))
	uds.Handle("/rafiki.v1.Login/", RequireEpoch(ok))

	for _, mux := range []struct {
		name string
		mux  *http.ServeMux
	}{{"proxy", proxy}, {"uds", uds}} {
		for _, path := range paths {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			handler, pattern := mux.mux.Handler(req)
			c.NotEq("", pattern, "%s: no route for %s", mux.name, path)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			c.Eq(http.StatusBadRequest, rec.Code, "%s: %s answered without the epoch header refused", mux.name, path)
			c.Eq(strconv.Itoa(protocol.Epoch), rec.Header().Get(protocol.EpochHeader),
				"%s: %s refusal must carry the epoch", mux.name, path)
		}
	}
}

// TestEpochGatesEveryRafikiV1Route is the -run 'Epoch'-visible entry for the
// route walk above, whose own name does not contain "Epoch" and would otherwise
// be skipped by the task's `-run 'Epoch'` verify pattern.
func TestEpochGatesEveryRafikiV1Route(t *testing.T) {
	t.Run("EveryRafikiV1RouteIsGatedOnBothMounts", TestEveryRafikiV1RouteIsGatedOnBothMounts)
}
