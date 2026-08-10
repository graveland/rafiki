// Package executor implements the executor server: a Connect RPC surface over
// which the daemon dispatches filesystem and shell tool calls.
package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"runtime"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// Options configures an executor server.
type Options struct {
	// Root is the absolute working directory this executor serves.
	Root string
	// Concurrency is the maximum number of concurrent tool calls.
	Concurrency int
	// Version is the executor build version, reported in Describe.
	Version string
}

// Server implements executorpbconnect.ExecutorServiceHandler.
type Server struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	id     string
	opts   Options
	labels map[string]string
	reg    *tools.Registry
}

// NewServer returns a Server ready to be mounted on an HTTP mux.
func NewServer(opts Options) *Server {
	tr := tools.NewFileTracker()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd:         opts.Root,
		FileTracker: tr,
	})
	return &Server{
		id:   randomID(),
		opts: opts,
		labels: map[string]string{
			"rafiki/executor-version": opts.Version,
		},
		reg: reg,
	}
}

func (s *Server) Describe(
	_ context.Context,
	_ *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId:   s.id,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Roots:        []string{s.opts.Root},
		Concurrency:  int32(s.opts.Concurrency),
		Isolation:    "none",
		WorkspaceMode: "pinned",
		Tools: []string{
			"read", "write", "edit", "glob", "grep", "bash",
			"bash_start", "bash_output", "bash_kill",
		},
		Version:           s.opts.Version,
		SelfReportedLabels: s.labels,
	}), nil
}

func (s *Server) Health(
	_ context.Context,
	_ *connect.Request[executorpb.HealthRequest],
) (*connect.Response[executorpb.HealthResponse], error) {
	var diskFree int64
	var stat unix.Statfs_t
	if err := unix.Statfs(s.opts.Root, &stat); err == nil {
		diskFree = int64(stat.Bavail) * int64(stat.Bsize)
	}
	return connect.NewResponse(&executorpb.HealthResponse{
		DiskFreeBytes: diskFree,
	}), nil
}

func (s *Server) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	name := req.Msg.Tool
	result, err := s.reg.Execute(ctx, name, json.RawMessage(req.Msg.InputJson))
	if err != nil {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Failed{
				Failed: &executorpb.Failure{
					Code:    executorpb.Failure_CODE_TOOL_FAILED,
					Message: err.Error(),
				},
			},
		})
	}
	return stream.Send(&executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Result{
			Result: &executorpb.Result{
				Content: []*executorpb.ContentBlock{
					{Block: &executorpb.ContentBlock_Text{Text: result}},
				},
			},
		},
	})
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Warn("executor: crypto/rand failed, using fallback id", "err", err)
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}
