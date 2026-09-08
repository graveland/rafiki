package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountRegistersMCPUnderWrap(t *testing.T) {
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
	if !ran {
		t.Fatal("marker handler never ran")
	}
	if got := rec.Header().Get("X-Mount-Wrap"); got != "applied" {
		t.Fatalf("wrap did not run before the handler: sentinel header %q", got)
	}
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
			if rec.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d", rec.Code)
			}
		})
	}
}
