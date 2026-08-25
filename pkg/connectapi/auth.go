// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// NewAuthInterceptor checks a bearer token on every Connect call.
//
// An EMPTY configured token disables the check entirely. That is the unix
// socket mount, where the trust mechanism is filesystem permissions (a 0600
// socket inside a 0700 directory) — the same model the framed-JSON control
// socket has always used. The TCP/TLS mount configures a real token.
//
// The comparison is constant-time. The error text never echoes the presented
// credential, and never distinguishes "absent" from "wrong": a caller that has
// not authenticated is told only that it has not authenticated.
func NewAuthInterceptor(token string) connect.Interceptor {
	return &authInterceptor{token: token}
}

type authInterceptor struct{ token string }

func (a *authInterceptor) authorize(header string) error {
	if a.token == "" {
		return nil
	}
	presented := strings.TrimPrefix(header, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) != 1 {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("invalid or missing control-plane credential"))
	}
	return nil
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.authorize(req.Header().Get("Authorization")); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authorize(conn.RequestHeader().Get("Authorization")); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
