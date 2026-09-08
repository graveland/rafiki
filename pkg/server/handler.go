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
		mux.Handle(h.ControlPath, wrap(h.Control))
	}
	if h.MCP != nil && h.MCPPath != "" {
		mux.Handle(h.MCPPath, wrap(h.MCP))
	}
}
