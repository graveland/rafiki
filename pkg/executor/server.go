// Package executor implements the executor server: a Connect RPC surface over
// which the daemon dispatches filesystem and shell tool calls.
package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	jobs   *jobRegistry
	sem    chan struct{} // bounds concurrent Execute calls
}

// NewServer returns a Server ready to be mounted on an HTTP mux.
func NewServer(opts Options) *Server {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 6
	}
	tr := tools.NewFileTracker()
	// MaterializeOnly, not MaterializeAll: the full blueprint would give the
	// executor the parent's credentialed tools and a nil task store.
	reg := tools.DefaultBlueprint.MaterializeOnly(tools.ToolOpts{
		Cwd:         opts.Root,
		FileTracker: tr,
	}, tools.ExecutorLocalTools())
	return &Server{
		id:   randomID(),
		opts: opts,
		labels: map[string]string{
			"rafiki/executor-version": opts.Version,
		},
		reg:  reg,
		jobs: newJobRegistry(),
		sem:  make(chan struct{}, opts.Concurrency),
	}
}

func (s *Server) Describe(
	_ context.Context,
	_ *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId:         s.id,
		Platform:           runtime.GOOS + "/" + runtime.GOARCH,
		Roots:              []string{s.opts.Root},
		Concurrency:        int32(s.opts.Concurrency),
		Isolation:          "none",
		WorkspaceMode:      "pinned",
		Tools:              tools.RoutedToExecutor(),
		Version:            s.opts.Version,
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
		DiskFreeBytes:  diskFree,
		RunningHandles: s.jobs.running(),
	}), nil
}

func (s *Server) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	msg := req.Msg

	// Background bash — start a job, return a handle immediately.
	if msg.Background && msg.Tool == "bash" {
		return s.startBackground(ctx, msg, stream)
	}

	// Bound concurrent tool calls. Describe advertises Options.Concurrency
	// and the README documents it; without this it is decoration.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// Honour the client's deadline. Without it CODE_TIMEOUT — one of four
	// documented failure codes, with its own retry semantics — can never be
	// produced, and a read or grep on a pathological tree runs forever.
	if msg.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(msg.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	// Verify expect_mtime before dispatching.
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
						Code: executorpb.Failure_CODE_DENIED,
						Message: fmt.Sprintf("%s was modified on disk since it was last read (expected mtime %s, found %s)",
							path, expected, info.ModTime()),
					},
				},
			})
		}
	}

	name := msg.Tool
	resultStr, err := s.reg.Execute(ctx, name, json.RawMessage(msg.InputJson))

	// Check the deadline before the tool's own error, and even when the
	// tool reports success: bash specifically treats a killed-by-context
	// command as a completed call and folds a "[bash: command timed out
	// ...]" note into its (nil-error) result text rather than returning an
	// error, so err == nil is not proof the deadline held. Once our own
	// timeout has elapsed the call is CODE_TIMEOUT regardless of what the
	// tool itself reports.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Failed{
				Failed: &executorpb.Failure{
					Code:    executorpb.Failure_CODE_TIMEOUT,
					Message: fmt.Sprintf("tool %q exceeded timeout_ms=%d", name, msg.TimeoutMs),
				},
			},
		})
	}
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

// startBackground launches a bash command as a background job and streams the
// handle back immediately.
func (s *Server) startBackground(
	ctx context.Context,
	msg *executorpb.ExecuteRequest,
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(msg.InputJson, &in); err != nil || in.Command == "" {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Failed{
				Failed: &executorpb.Failure{
					Code:    executorpb.Failure_CODE_TOOL_FAILED,
					Message: "background bash requires a command",
				},
			},
		})
	}

	handle := msg.CallId
	if handle == "" {
		handle = randomID()
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", in.Command)
	cmd.Dir = s.opts.Root
	if err := s.jobs.start(cmd, handle); err != nil {
		return stream.Send(&executorpb.ExecuteResponse{
			Event: &executorpb.ExecuteResponse_Failed{
				Failed: &executorpb.Failure{
					Code:    executorpb.Failure_CODE_TOOL_FAILED,
					Message: "background bash failed to start: " + err.Error(),
				},
			},
		})
	}

	return stream.Send(&executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Handle{Handle: handle},
	})
}

func (s *Server) Attach(
	ctx context.Context,
	req *connect.Request[executorpb.AttachRequest],
	stream *connect.ServerStream[executorpb.AttachResponse],
) error {
	handle := req.Msg.Handle

	// cursor is a byte offset into the job's lifetime output, not a length:
	// the ring drops old bytes, so comparing lengths would replay or skip.
	var cursor int64

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		data, total, exited, code, found := s.jobs.output(handle, cursor)
		if !found {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("no such handle: %s", handle))
		}
		if len(data) > 0 {
			if err := stream.Send(&executorpb.AttachResponse{
				Event: &executorpb.AttachResponse_Output{
					Output: &executorpb.OutputChunk{Data: data},
				},
			}); err != nil {
				return err
			}
			cursor = total
		}
		if exited {
			return stream.Send(&executorpb.AttachResponse{
				Event: &executorpb.AttachResponse_ExitCode{ExitCode: int32(code)},
			})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) Cancel(
	_ context.Context,
	req *connect.Request[executorpb.CancelRequest],
) (*connect.Response[executorpb.CancelResponse], error) {
	if err := s.jobs.kill(req.Msg.CallId); err != nil {
		slog.Warn("executor: cancel failed", "handle", req.Msg.CallId, "error", err)
	}
	return connect.NewResponse(&executorpb.CancelResponse{}), nil
}

// JobOutput is the one-shot poll behind bash_output. It never blocks: it
// answers from the ring as it stands and returns immediately, whether or not
// the job has exited.
func (s *Server) JobOutput(
	_ context.Context,
	req *connect.Request[executorpb.JobOutputRequest],
) (*connect.Response[executorpb.JobOutputResponse], error) {
	data, total, exited, code, found := s.jobs.output(req.Msg.Handle, req.Msg.Since)
	return connect.NewResponse(&executorpb.JobOutputResponse{
		Data:     data,
		Total:    total,
		Exited:   exited,
		ExitCode: int32(code),
		Found:    found,
	}), nil
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
