// SPDX-License-Identifier: Apache-2.0

package server

import "net/http"

// Handler bundles the proxy faces for mounting: hosts mount both faces under
// their own middleware stack (sc's auth+tailnet middlewares in embedded mode,
// UserTokenAuth.Middleware standalone).
type Handler struct {
	Messages *MessagesProxy
	Chat     *ChatCompletionsProxy // optional; nil when the OpenAI face is disabled

	// ControlPath and Control mount the Connect control plane, protected by
	// the same wrap as every other face — there is no separate auth path.
	ControlPath string
	Control     http.Handler

	// MCPPath and MCP mount the MCP agent-control surface, protected by the
	// same wrap as every other face. It is mounted HERE and not in main.go:
	// a mux.Handle beside /control would shadow this one's authentication,
	// because ServeMux prefers the longer pattern.
	MCPPath string
	MCP     http.Handler

	// LoginPath and Login mount the OIDC login engine, registered WITHOUT
	// wrap by Mount — see the comment there.
	LoginPath string
	Login     http.Handler
}

// Mount registers the faces on mux, each wrapped by wrap (identity when nil).
func (h *Handler) Mount(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	if wrap == nil {
		wrap = func(next http.Handler) http.Handler { return next }
	}
	if h.Messages != nil {
		mux.Handle("/v1/messages", wrap(h.Messages))
		mux.Handle("/v1/messages/count_tokens", wrap(http.HandlerFunc(h.Messages.ServeCountTokens)))
	}
	if h.Chat != nil {
		mux.Handle("/v1/chat/completions", wrap(h.Chat))
	}
	if h.Control != nil && h.ControlPath != "" {
		// RequireEpoch is OUTERMOST, before the caller's wrap: a request whose
		// epoch is missing or wrong must be refused as a protocol mismatch,
		// never as an auth failure the caller's middleware would report first.
		mux.Handle(h.ControlPath, RequireEpoch(wrap(h.Control)))
	}
	// Login is how a caller without a valid credential gets one, so it must
	// not sit behind wrap — every other face is wrapped by the caller's auth
	// middleware, which 401s exactly the caller Login exists to serve. A
	// credential-less caller reaches Login unwrapped; everything the Login
	// service itself must not answer is guarded inside the engine (its routes
	// come from connectapi.Server.LoginRoutes, which mounts no identity
	// resolution of its own).
	if h.Login != nil && h.LoginPath != "" {
		// Login is unwrapped by the caller's auth (see the comment above), so
		// the epoch gate is applied HERE, by Mount itself, on top of the
		// unwrapped handler — a stale Login peer must still be refused.
		mux.Handle(h.LoginPath, RequireEpoch(h.Login))
	}
	if h.MCP != nil && h.MCPPath != "" {
		mux.Handle(h.MCPPath, wrap(h.MCP))
	}
}
