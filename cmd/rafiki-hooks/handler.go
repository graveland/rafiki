// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// notifier is the one thing the handler does to the daemon. It is an interface
// so the handler's whole status ladder can be exercised without a daemon.
type notifier interface {
	Notify(ctx context.Context, req *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error)
}

// controlNotifier adapts the generated Connect client to notifier: the client
// speaks in connect.Request/Response envelopes, the handler in the messages.
type controlNotifier struct {
	client rafikiv1connect.ControlClient
}

func (n controlNotifier) Notify(ctx context.Context, req *rafikiv1.NotifyRequest) (*rafikiv1.NotifyResponse, error) {
	resp, err := n.client.Notify(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// gateway is the single http.Handler over every configured route.
//
// The order of checks is the security property, not an implementation detail:
// the path decides which secret applies before anything reads the body, the
// body is bounded before it is read, and the signature is verified before the
// JSON is parsed. A body that is merely malformed JSON cannot change the answer
// to a sender that cannot sign.
type gateway struct {
	routes   map[string]*Route
	notifier notifier
	dedup    *dedup
	timeout  time.Duration
	now      func() time.Time
}

func newGateway(cfg *Config, n notifier) *gateway {
	routes := make(map[string]*Route, len(cfg.Routes))
	for i := range cfg.Routes {
		routes[cfg.Routes[i].Path] = &cfg.Routes[i]
	}
	return &gateway{
		routes:   routes,
		notifier: n,
		dedup:    newDedup(),
		timeout:  cfg.timeout(),
		now:      time.Now,
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := g.routes[r.URL.Path]
	if !ok {
		// An unconfigured path gets nothing at all: no body, and no hint that
		// some other path on this listener exists.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, route.MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			slog.Warn("rafiki-hooks: webhook body over the route's max_body",
				"path", route.Path, "remote", r.RemoteAddr, "max_body", route.MaxBody)
			writeText(w, http.StatusRequestEntityTooLarge, "too large")
			return
		}
		slog.Warn("rafiki-hooks: unreadable webhook body",
			"path", route.Path, "remote", r.RemoteAddr, "error", err)
		writeText(w, http.StatusBadRequest, "unreadable body")
		return
	}

	source := route.verifier()
	if source == nil {
		// Unreachable through Load, which validates Source against this table.
		slog.Error("rafiki-hooks: route has no source", "path", route.Path)
		writeText(w, http.StatusInternalServerError, "misconfigured")
		return
	}
	if err := source.Verify(r, body, route.secret); err != nil {
		slog.Warn("rafiki-hooks: webhook rejected",
			"path", route.Path, "remote", r.RemoteAddr, "error", err)
		writeText(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	eventType, deliveryID := source.Event(r)
	now := g.now()
	if g.dedup.contains(deliveryID, now) {
		slog.Debug("rafiki-hooks: duplicate delivery", "path", route.Path, "delivery", deliveryID)
		writeText(w, http.StatusOK, "duplicate")
		return
	}

	payload, err := parsePayload(body)
	if err != nil {
		slog.Warn("rafiki-hooks: webhook body is not JSON",
			"path", route.Path, "delivery", deliveryID, "error", err)
		writeText(w, http.StatusBadRequest, "invalid json")
		return
	}

	rule, ok := firstMatch(route.Rules, eventType, payload)
	if !ok {
		slog.Debug("rafiki-hooks: no rule matched", "path", route.Path, "event", eventType, "delivery", deliveryID)
		writeText(w, http.StatusOK, "ignored")
		return
	}

	key, fields := rule.build(payload)
	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()
	if _, err := g.notifier.Notify(ctx, &rafikiv1.NotifyRequest{
		Target: route.Target,
		Source: route.Notify,
		Key:    key,
		Fields: fields,
	}); err != nil {
		g.reportFailure(w, route, deliveryID, err)
		return
	}

	// Only now is the delivery recorded: a 503 the sender retries must reach
	// the agent, so an id may only be claimed once the daemon has accepted it.
	g.dedup.record(deliveryID, now)
	writeText(w, http.StatusOK, "ok")
}

// reportFailure turns a Notify failure into the status the sender should see.
//
// The split is "can this delivery ever succeed?", not "what did the daemon
// call it":
//   - 422: the sender's own input is wrong or the target does not exist, so a
//     retry delivers the same refusal;
//   - 503: the daemon or the path to it is temporarily unavailable, so a
//     retrying sender should try again;
//   - 500: everything else, including an expired, revoked or rotated gateway
//     token. That one is fixable by the OPERATOR, and a redelivery after the
//     fix succeeds — answering 422 would tell a retrying sender to give up for
//     good.
func (g *gateway) reportFailure(w http.ResponseWriter, route *Route, deliveryID string, err error) {
	code := classify(err)
	switch code {
	case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
		slog.Error("rafiki-hooks: notification refused",
			"path", route.Path, "target", route.Target, "delivery", deliveryID, "code", code.String(), "error", err)
		writeText(w, http.StatusUnprocessableEntity, "refused")
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		slog.Error("rafiki-hooks: notification not delivered",
			"path", route.Path, "target", route.Target, "delivery", deliveryID, "code", code.String(), "error", err)
		writeText(w, http.StatusServiceUnavailable, "unavailable")
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		// The gateway's own credential was refused: the daemon is up and the
		// sender's request is fine, so this is an operator problem, not the
		// sender's. Say so in the log — it is the only clue that the token was
		// revoked or expired, and the sender can only retry and wait.
		slog.Error("rafiki-hooks: the gateway's daemon credential was refused; mint a new notify-scoped token "+
			"and restart the gateway",
			"path", route.Path, "target", route.Target, "delivery", deliveryID, "code", code.String(), "error", err)
		writeText(w, http.StatusInternalServerError, "error")
	default:
		slog.Error("rafiki-hooks: notification failed",
			"path", route.Path, "target", route.Target, "delivery", deliveryID, "code", code.String(), "error", err)
		writeText(w, http.StatusInternalServerError, "error")
	}
}

// classify names the failure. A Connect error carries the daemon's own verdict;
// anything else never became one — a refused connection, a TLS failure, an
// unusable response — which is a transport failure and retried, not a 500.
func classify(err error) connect.Code {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr.Code()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return connect.CodeDeadlineExceeded
	}
	return connect.CodeUnavailable
}

// parsePayload decodes the body keeping numbers as their literal text, so a
// notification field shows the sender's number rather than a float64 round
// trip.
func parsePayload(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return payload, nil
}

// writeText answers with a small plain-text body. No status is written before
// the caller has decided it, which is what keeps 2xx off the wire until Notify
// has returned.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}
