// Package executor implements the executor server: a Connect RPC surface over
// which the daemon dispatches filesystem and shell tool calls.
package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

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
	msg := req.Msg

	// Verify expect_mtime before dispatching: if the file changed on disk
	// since the parent last read it, refuse the write/edit. This is the
	// TOCTOU guard — parent-side checking alone is insufficient.
	for path, expectedNS := range msg.ExpectMtime {
		info, err := os.Stat(path)
		if err != nil {
			return stream.Send(&executorpb.ExecuteResponse{
				Event: &executorpb.ExecuteResponse_Failed{
					Failed: &executorpb.Failure{
						Code:    executorpb.Failure_CODE_TOOL_FAILED,
						Message: "file changed under us: " + err.Error(),
					},
				},
			})
		}
		expected := time.Unix(0, expectedNS)
		if !info.ModTime().Equal(expected) {
			return stream.Send(&executorpb.ExecuteResponse{
				Event: &executorpb.ExecuteResponse_Failed{
					Failed: &executorpb.Failure{
						Code:    executorpb.Failure_CODE_DENIED,
						Message: fmt.Sprintf("%s was modified on disk since it was last read (expected mtime %s, found %s)",
							path, expected, info.ModTime()),
					},
				},
			})
		}
	}

	name := msg.Tool
	resultStr, err := s.reg.Execute(ctx, name, json.RawMessage(msg.InputJson))
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

	result := &executorpb.Result{
		Content: []*executorpb.ContentBlock{
			{Block: &executorpb.ContentBlock_Text{Text: resultStr}},
		},
		ObservedMtime: s.collectObservedMtimes(msg.Tool, msg.InputJson),
	}
	return stream.Send(&executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Result{Result: result},
	})
}

// collectObservedMtimes stats files touched by a tool call and returns their
// current mtimes. The parent uses these to populate its FileTracker so it can
// pass expect_mtime on the next write.
func (s *Server) collectObservedMtimes(tool string, input json.RawMessage) map[string]int64 {
	paths := filePathsFromInput(tool, input)
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]int64, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		out[p] = info.ModTime().UnixNano()
	}
	return out
}

// filePathsFromInput extracts file paths from a tool's JSON input for mtime
// reporting. Best-effort: it only parses fields that carry a single file path
// (read, write, edit); directory tools (glob, grep, ls) are omitted because
// their observed set is unbounded.
func filePathsFromInput(tool string, input json.RawMessage) []string {
	switch tool {
	case "read", "write", "edit":
		var in struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &in) == nil && in.FilePath != "" {
			return []string{in.FilePath}
		}
	}
	return nil
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Warn("executor: crypto/rand failed, using fallback id", "err", err)
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}
