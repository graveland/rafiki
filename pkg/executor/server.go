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
	"runtime"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/lspadapter"
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
	// RTK is whether tools may rewrite commands through rtk.
	//
	// Explicit, because the zero RTKMode is NOT RTKOff — rtkRewrite only
	// short-circuits on the literal "off" — so leaving this unset made every
	// executor rewrite through rtk with nobody having chosen it. It is the
	// executor operator's setting rather than the child's: the executor is a
	// different machine, and the child cannot know what is installed there.
	RTK tools.RTKMode
	// SpillDir is where oversized tool results and background job output are
	// written. Empty means os.TempDir().
	SpillDir string
	// JobOutputBudget is how many bytes of retained background-job output one
	// workspace may hold. Zero means defaultJobBudget.
	JobOutputBudget int64
	// LSPConfig is the path to an lsp.json describing language servers this
	// executor may start. Empty means auto-detect what is installed on PATH.
	//
	// The executor is the right place to decide this and the child is not: the
	// child cannot know what toolchain is installed on a machine it has never
	// seen, which is the same reasoning that made RTK an executor option.
	LSPConfig string

	// NoLSP disables language servers entirely on this executor.
	NoLSP bool
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
	wsReg  *workspaceRegistry
	lsp    *lsp.Manager
}

// NewServer returns a Server ready to be mounted on an HTTP mux.
func NewServer(opts Options) *Server {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 6
	}
	if opts.SpillDir == "" {
		opts.SpillDir = os.TempDir()
	}
	if opts.RTK == "" {
		opts.RTK = tools.RTKAuto
	}
	// MaterializeOnly, not MaterializeAll: the full blueprint would give the
	// executor the parent's credentialed tools and a nil task store.
	tracker := tools.NewFileTracker()
	opt := toolOptsFor(opts, tracker)

	// Language servers run HERE because the files are here. A manager started
	// in the daemon would index the daemon's filesystem and answer about files
	// the agent is not editing — and lsp_rename would write to them.
	lspMgr := newLSPManager(opts)
	if lspMgr != nil {
		opt.LSP = lspadapter.New(lspMgr, tracker)
		opt.FileChanged = lspMgr
	}

	reg := tools.DefaultBlueprint.MaterializeOnly(opt, tools.ExecutorLocalTools())
	return &Server{
		id:   randomID(),
		opts: opts,
		labels: map[string]string{
			"rafiki/executor-version": opts.Version,
		},
		reg:   reg,
		jobs:  newJobRegistry(opts.SpillDir, opts.Root, opts.JobOutputBudget),
		sem:   make(chan struct{}, opts.Concurrency),
		wsReg: newWorkspaceRegistry(),
		lsp:   lspMgr,
	}
}

// newLSPManager builds the executor's language-server manager, or returns nil
// when there is nothing to manage.
//
// nil is a real answer, not a failure: an executor on a machine with no
// toolchain installed should serve no LSP tools at all rather than eight that
// can only answer "executable file not found in $PATH", which costs the model a
// turn to learn nothing.
func newLSPManager(opts Options) *lsp.Manager {
	if opts.NoLSP {
		return nil
	}

	var cfg lsp.Config
	if opts.LSPConfig != "" {
		var err error
		cfg, err = lsp.LoadConfig(opts.LSPConfig)
		if err != nil {
			slog.Warn("executor: lsp config unreadable; continuing without language servers",
				"path", opts.LSPConfig, "error", err)
			return nil
		}
	} else {
		cfg = lsp.AutoDetect()
	}
	if len(cfg.Servers) == 0 {
		return nil
	}

	mgr := lsp.NewManager(cfg, opts.Root)
	if !mgr.HasInstalledServer() {
		slog.Warn("executor: no configured language server found on PATH; lsp tools disabled",
			"config", opts.LSPConfig)
		return nil
	}
	return mgr
}

// Close stops everything the server owns.
//
// Language servers are subprocesses that index a whole tree; leaving one
// running after the executor exits leaks a process holding significant memory,
// and on a laptop executor that memory is the operator's.
//
// It exists only now because the executor previously owned nothing with a
// lifetime — the tool registry is values, and jobs are bounded by the workspace
// they belong to. A subprocess is the first thing that outlives the request
// that made it.
func (s *Server) Close() error {
	if s.lsp != nil {
		s.lsp.Shutdown(context.Background())
	}
	return nil
}

// toolOptsFor maps the executor's options onto the tool options its registry is
// built from. Extracted so the mapping is testable: every field here was once
// simply absent, and an unset ToolOpts field does not fail — it takes a zero
// value that may not mean what the zero value looks like. RTK is the example:
// RTKMode("") is not RTKOff, so leaving it unset made every executor rewrite
// commands through rtk with nobody having chosen it.
func toolOptsFor(opts Options, tr *tools.FileTracker) tools.ToolOpts {
	return tools.ToolOpts{
		Cwd:          opts.Root,
		FileTracker:  tr,
		RTK:          opts.RTK,
		OutputPolicy: tools.OutputPolicy{SpillDir: opts.SpillDir},
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

func (s *Server) Provision(
	_ context.Context,
	req *connect.Request[executorpb.ProvisionRequest],
) (*connect.Response[executorpb.ProvisionResponse], error) {
	// The request's mounts, network, workdir and workspace_mode are all
	// ignored, and the response's isolation is left EMPTY on purpose. This
	// process serves the root it was started with; what that root can reach is
	// decided by its filesystem view — the container's mounts, chosen in
	// `docker run`, or the host user's permissions — and described, for humans
	// and for selectors, on the executor's row. An isolation string invented
	// here would be the executor asserting a fact that gates it.
	ws := &workspace{
		id:      randomID(),
		workdir: s.opts.Root,
		roots:   []string{s.opts.Root},
	}
	s.wsReg.put(ws)
	return connect.NewResponse(&executorpb.ProvisionResponse{
		WorkspaceId: ws.id,
		Roots:       ws.roots,
		Workdir:     ws.workdir,
	}), nil
}

func (s *Server) Release(
	_ context.Context,
	req *connect.Request[executorpb.ReleaseRequest],
) (*connect.Response[executorpb.ReleaseResponse], error) {
	id := req.Msg.WorkspaceId
	if _, ok := s.wsReg.get(id); ok {
		// End THIS workspace's background jobs before tearing it down: kill the
		// running ones, drop the finished ones, remove their output files. A
		// background job in a released workspace is not a job, and reporting it
		// as running is worse than reporting it gone.
		//
		// This is also what ends retention. There is no timer anywhere: a
		// wall-clock window cannot know when an async agent will come back for
		// its output, and the workspace already outlives exactly the agent that
		// could ask.
		s.jobs.releaseWorkspace(id)
		s.wsReg.remove(id)
	}
	// Idempotent: release a non-existent workspace without error.
	return connect.NewResponse(&executorpb.ReleaseResponse{}), nil
}

func (s *Server) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	msg := req.Msg

	// Look up the workspace. An empty workspace_id means the executor's own
	// root.
	var ws *workspace
	if msg.WorkspaceId != "" {
		var ok bool
		ws, ok = s.wsReg.get(msg.WorkspaceId)
		if !ok {
			return stream.Send(&executorpb.ExecuteResponse{
				Event: &executorpb.ExecuteResponse_Failed{
					Failed: &executorpb.Failure{
						Code:    executorpb.Failure_CODE_EXECUTOR_LOST,
						Message: fmt.Sprintf("unknown workspace %q", msg.WorkspaceId),
					},
				},
			})
		}
	}

	// Background bash — start a job, return a handle immediately.
	if msg.Background && msg.Tool == "bash" {
		return s.startBackground(ctx, msg, ws, stream)
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

	resultStr, err := s.reg.Execute(ctx, msg.Tool, json.RawMessage(msg.InputJson))

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
					Message: fmt.Sprintf("tool %q exceeded timeout_ms=%d", msg.Tool, msg.TimeoutMs),
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
	ws *workspace,
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

	wsID := ""
	if ws != nil {
		wsID = ws.id
	}

	// A background job is never rewritten through rtk, whatever Options.RTK
	// says, and that is deliberate rather than an omission. The rewrite is only
	// safe because bash.go can watch stderr after the command exits and re-run
	// the original when rtk itself refused — a long-running job offers no exit
	// to inspect and no output it could take back, so a rewrite here would be
	// one with no way to undo it.
	//
	// The registry builds the command. It used to be built here, which is how
	// the background path came to run `sh -c` for a workspaced job and
	// `bash -c` for a bare one.
	handle, err := s.jobs.start(in.Command, msg.CallId, wsID)
	if err != nil {
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
	ctx context.Context,
	req *connect.Request[executorpb.CancelRequest],
) (*connect.Response[executorpb.CancelResponse], error) {
	handle := req.Msg.CallId
	if err := s.jobs.kill(handle); err != nil {
		slog.Warn("executor: cancel failed", "handle", handle, "error", err)
	}
	return connect.NewResponse(&executorpb.CancelResponse{}), nil
}

// JobOutput is the one-shot poll behind bash_output. It never blocks: it
// answers from the ring as it stands and returns immediately, whether or not
// the job has exited.
func (s *Server) JobOutput(
	ctx context.Context,
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
