// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// RPCLogInterceptor logs every RPC call this executor handles: its duration,
// the method name, whether it succeeded or failed, and any error message.  For
// the Execute RPC it also logs which tool was invoked.
//
// It implements [connect.Interceptor] so it handles unary RPCs (Describe,
// Health, Cancel, JobOutput, Provision, Release, ProjectContext, ProjectSkills,
// SkillBody), server-streaming RPCs (Execute, Attach), and bidirectional
// streaming (Proxy) — without needing per-method wiring.
type RPCLogInterceptor struct{}

// NewRPCInterceptor returns an interceptor suitable for passing to
// [connect.WithInterceptors].
func NewRPCInterceptor() *RPCLogInterceptor {
	return &RPCLogInterceptor{}
}

// WrapUnary implements [connect.Interceptor].
func (r *RPCLogInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		procedure := req.Spec().Procedure
		resp, err := next(ctx, req)
		r.log(procedure, "", time.Since(start), err)
		return resp, err
	})
}

// WrapStreamingClient implements [connect.Interceptor]; a no-op because this
// interceptor is only installed on the handler (server) side.
func (r *RPCLogInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements [connect.Interceptor].
//
// For the Execute RPC we peek at the request message to extract the tool name
// before the stream is consumed by the actual handler.  We do this by wrapping
// the StreamingHandlerConn's Receive to capture the first message.
func (r *RPCLogInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		procedure := conn.Spec().Procedure

		// Wrap Receive to capture the first message for logging.
		var tool string
		wrapped := &interceptingStream{
			StreamingHandlerConn: conn,
			onFirstReceive: func(msg any) {
				if execReq, ok := msg.(*executorpb.ExecuteRequest); ok {
					tool = execReq.Tool
				}
			},
		}

		err := next(ctx, wrapped)
		r.log(procedure, tool, time.Since(start), err)
		return err
	}
}

// log emits one structured log line per RPC completion.
func (r *RPCLogInterceptor) log(procedure, tool string, d time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("rpc", procedure),
		slog.Duration("duration", d),
	}
	if tool != "" {
		attrs = append(attrs, slog.String("tool", tool))
	}
	if err != nil {
		code := connect.CodeOf(err)
		attrs = append(attrs,
			slog.String("result", "error"),
			slog.String("code", code.String()),
			slog.Any("error", err),
		)
	} else {
		attrs = append(attrs, slog.String("result", "ok"))
	}

	// Slog level: info for all — the executor's RPC volume is low enough that
	// every call is interesting.  Errors already carry their code, so callers
	// can filter on `result=error` if they only want failures.
	slog.LogAttrs(context.Background(), slog.LevelInfo, "executor rpc", attrs...)
}

// interceptingStream wraps a [connect.StreamingHandlerConn] to call a hook on
// the first [Receive] that returns a non-nil message.  This lets us capture
// the request body in the streaming interceptor before the handler consumes it.
type interceptingStream struct {
	connect.StreamingHandlerConn
	firstDone      bool
	onFirstReceive func(any)
}

func (s *interceptingStream) Receive(msg any) error {
	err := s.StreamingHandlerConn.Receive(msg)
	if err != nil {
		return err
	}
	if !s.firstDone {
		s.firstDone = true
		s.onFirstReceive(msg)
	}
	return nil
}

var _ connect.StreamingHandlerConn = (*interceptingStream)(nil)
