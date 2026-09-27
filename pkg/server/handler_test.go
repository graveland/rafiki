package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestMountRegistersMCPUnderWrap(t *testing.T) {
	c := assert.NewAborting(t)
	ran := false
	h := Handler{
		MCPPath: "/mcp/",
		MCP: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			ran = true
			w.WriteHeader(http.StatusOK)
		}),
	}
	mux := http.NewServeMux()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Mount-Wrap", "applied")
			next.ServeHTTP(w, r)
		})
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/", nil))
	c.True(ran, "marker handler never ran")
	c.Eq("applied", rec.Header().Get("X-Mount-Wrap"), "wrap did not run before the handler: sentinel header")
}

func TestMountSkipsMCPWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		h    Handler
	}{
		{
			name: "path with nil handler",
			h:    Handler{MCPPath: "/mcp/"},
		},
		{
			name: "handler with empty path",
			h: Handler{
				MCP: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			tc.h.Mount(mux, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/", nil))
			assert.NewAborting(t).Eq(http.StatusNotFound, rec.Code, "want 404, got")
		})
	}
}
