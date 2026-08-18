package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	// jobWaitDelay bounds how long Wait blocks for the output pipes to reach
	// EOF after the process itself has exited.
	//
	// It is load-bearing, not a tuning knob. exec.Cmd.Wait waits for the
	// process AND for its pipes, and a backgrounded grandchild inherits those
	// pipes and holds them for as long as it runs — so `npm run dev &` exits
	// instantly and Wait NEVER returns. The job then reports "still running"
	// forever, Health counts it forever, and its goroutine leaks. Mirrors
	// bashWaitDelay in pkg/fundi/tools/bash.go, which solves the same problem
	// on the foreground path.
	jobWaitDelay = 5 * time.Second

	// maxJobResponse bounds what a single read returns, taken from the END of
	// what the job has written: a poll wants the newest output. Anything older
	// stays on disk, and the reader is told where.
	maxJobResponse = 100_000

	// maxJobSpill is how much of one job's output is retained on disk, and
	// spillCompactAt is the size at which the file is rewritten down to it.
	// Compacting with slack rather than on every write keeps the rewrite cost
	// amortised to about one extra write per byte.
	maxJobSpill    = 8 << 20
	spillCompactAt = 12 << 20

	// defaultJobBudget is how many bytes of retained job output one workspace
	// may hold. Finished jobs are evicted oldest-first to stay under it.
	//
	// A byte budget rather than a job count: 32 `git status` jobs and 32
	// saturated build logs differ by three orders of magnitude, and a count
	// cannot tell them apart. And a budget rather than a timer: a wall-clock
	// retention window is meaningless for an async agent, whose turn can end
	// and resume hours later — output must live as long as the agent that
	// might ask for it, which is what the workspace's lifetime already means.
	defaultJobBudget = 256 << 20
)

// jobOutput is one job's output: a file with drop-oldest semantics.
//
// On disk rather than in memory, and one store rather than a memory ring plus
// a spill copy, because reads are capped at maxJobResponse either way. What the
// file buys is that dropped bytes are not DESTROYED — the reader is handed a
// path, and the model can reach it with read/grep on the same executor. That is
// the rule every other tool result already follows (OutputPolicy.Clip); the
// background path was the one place output was unbounded and also the one place
// it was thrown away.
type jobOutput struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	base   int64 // absolute offset of the first byte still in the file
	total  int64 // bytes ever written
	closed bool
}

func newJobOutput(path string) (*jobOutput, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &jobOutput{f: f, path: path}, nil
}

// Write appends to the file, compacting when it outgrows its retention.
//
// Stdout and Stderr both point here. exec.Cmd shares a single pipe only when
// the two fields are the IDENTICAL value — pointing them at two writers over
// one destination is a data race, not merely interleaving (see the syncWriter
// note in pkg/fundi/tools/bash.go).
func (o *jobOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		// The job was evicted or its workspace released. Discard rather than
		// error: the process may outlive the record, and a write error here
		// would surface as a spurious job failure.
		return len(p), nil
	}
	n, err := o.f.Write(p)
	o.total += int64(n)
	if o.total-o.base > spillCompactAt {
		o.compact()
	}
	return n, err
}

// compact rewrites the file to hold only its most recent maxJobSpill bytes.
// The TAIL is what survives: a build's failure and a server's most recent
// request are both at the end, and keeping the head would preserve startup
// noise instead.
func (o *jobOutput) compact() {
	keep := int64(maxJobSpill)
	size := o.total - o.base
	if size <= keep {
		return
	}
	buf := make([]byte, keep)
	if _, err := o.f.ReadAt(buf, size-keep); err != nil {
		return // leave the file as it is; over-retention beats losing it
	}
	if err := o.f.Truncate(0); err != nil {
		return
	}
	if _, err := o.f.Seek(0, 0); err != nil {
		return
	}
	if _, err := o.f.Write(buf); err != nil {
		return
	}
	o.base = o.total - keep
}

// since returns bytes from `from` onward, capped at maxJobResponse and taken
// from the END. dropped reports that bytes between `from` and the returned
// slice were not included — they are still in the file when they were dropped
// by the cap, and gone when they were dropped by compaction.
func (o *jobOutput) since(from int64) (data []byte, total int64, dropped bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	start := from
	if start < o.base {
		start, dropped = o.base, true
	}
	if start > o.total {
		start = o.total
	}
	if o.total-start > maxJobResponse {
		start, dropped = o.total-maxJobResponse, true
	}
	out := make([]byte, o.total-start)
	if len(out) > 0 && !o.closed {
		if _, err := o.f.ReadAt(out, start-o.base); err != nil {
			return nil, o.total, true
		}
	}
	return out, o.total, dropped
}

// size is the bytes this job currently holds on disk, for the workspace budget.
func (o *jobOutput) size() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total - o.base
}

// remove closes the file and deletes it. Idempotent.
func (o *jobOutput) remove() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	_ = o.f.Close()
	_ = os.Remove(o.path)
}

// job holds a single background process and its output.
type job struct {
	cmd    *exec.Cmd
	handle string
	out    *jobOutput
	// workspaceID is the workspace this job belongs to, "" for a call that named
	// no workspace. Releasing a workspace must end ITS jobs and no others.
	workspaceID string

	mu         sync.Mutex
	exited     bool
	exitCode   int
	finishedAt time.Time
}

// jobRegistry tracks running background jobs and retains finished ones.
type jobRegistry struct {
	mu       sync.Mutex
	jobs     map[string]*job
	spillDir string
	cwd      string
	budget   int64
}

func newJobRegistry(spillDir, cwd string, budget int64) *jobRegistry {
	if budget <= 0 {
		budget = defaultJobBudget
	}
	return &jobRegistry{
		jobs:     make(map[string]*job),
		spillDir: spillDir,
		cwd:      cwd,
		budget:   budget,
	}
}

// start launches command as a background job and returns its handle.
//
// The registry builds the command rather than accepting one, so the background
// path cannot drift from the foreground one again: it once ran `sh -c` for a
// workspaced job and `bash -c` for a bare one, so the same script met dash or
// bash depending on whether the caller had provisioned.
//
// It returns once the process is RUNNING, so cmd.Process is non-nil for every
// caller that sees a nil error — kill must never race a start.
func (r *jobRegistry) start(command, handle, workspaceID string) (string, error) {
	if handle == "" {
		handle = randomID()
	}

	out, err := newJobOutput(filepath.Join(r.spillDir, "job-"+handle+".log"))
	if err != nil {
		// Loud rather than degraded. A background job with nowhere to record
		// its output is one whose result cannot be read back, and silently
		// falling back to memory would reintroduce the destroy-on-overflow
		// behaviour this replaced.
		return "", fmt.Errorf("background job output file: %w", err)
	}

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = r.cwd
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group, so kill can signal the whole tree: a background
	// `bash -c "npm run dev"` spawns children that outlive a bare
	// Process.Kill on the bash itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = jobWaitDelay

	if err := cmd.Start(); err != nil {
		out.remove()
		return "", err
	}

	j := &job{cmd: cmd, handle: handle, out: out, workspaceID: workspaceID}
	r.mu.Lock()
	r.jobs[handle] = j
	r.enforceBudget(workspaceID)
	r.mu.Unlock()

	go func() {
		err := cmd.Wait()

		// Written BEFORE the job is marked exited, so a reader that sees
		// "exited" has already been able to see the note.
		if errors.Is(err, exec.ErrWaitDelay) {
			_, _ = out.Write([]byte("\n[job exited but left background processes holding its output pipes; " +
				"later output is not captured]\n"))
		}

		j.mu.Lock()
		j.exited = true
		// ProcessState, not the error: when WaitDelay expires the error is
		// exec.ErrWaitDelay rather than an *exec.ExitError, so unwrapping it
		// records -1 for a job that exited 0.
		if cmd.ProcessState != nil {
			j.exitCode = cmd.ProcessState.ExitCode()
		} else {
			j.exitCode = -1
		}
		j.finishedAt = time.Now()
		j.mu.Unlock()

		r.mu.Lock()
		r.enforceBudget(workspaceID)
		r.mu.Unlock()
	}()
	return handle, nil
}

// enforceBudget evicts finished jobs, oldest-finished first, until the
// workspace is within its byte budget. Callers must hold r.mu.
//
// Running jobs are never evicted: their output is a live stream, and dropping
// it loses what is arriving rather than an archive. A workspace whose RUNNING
// jobs alone exceed the budget therefore stays over it, bounded only by
// maxJobSpill per job — which is the right failure, since the alternative is
// silently cutting a stream the agent is still watching.
func (r *jobRegistry) enforceBudget(workspaceID string) {
	var total int64
	var finished []*job
	for _, j := range r.jobs {
		if j.workspaceID != workspaceID {
			continue
		}
		total += j.out.size()
		j.mu.Lock()
		exited := j.exited
		j.mu.Unlock()
		if exited {
			finished = append(finished, j)
		}
	}
	if total <= r.budget {
		return
	}

	sort.Slice(finished, func(a, b int) bool {
		finished[a].mu.Lock()
		ta := finished[a].finishedAt
		finished[a].mu.Unlock()
		finished[b].mu.Lock()
		tb := finished[b].finishedAt
		finished[b].mu.Unlock()
		return ta.Before(tb)
	})

	for _, j := range finished {
		if total <= r.budget {
			return
		}
		total -= j.out.size()
		j.out.remove()
		delete(r.jobs, j.handle)
	}
}

// output returns the job's bytes from offset `since` onward.
//
// It reads the LIVE file, not a snapshot taken at exit: an Attach to a running
// job must see output as it arrives, which is the entire reason handles exist.
func (r *jobRegistry) output(handle string, since int64) (data []byte, total int64, exited bool, exitCode int, found bool) {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return nil, 0, false, 0, false
	}
	data, total, dropped := j.out.since(since)
	if dropped {
		// The marker counts against maxJobResponse: the cap is a promise about
		// the whole response, and a note explaining that output was clipped
		// must not be the thing that pushes it over.
		marker := []byte(fmt.Sprintf(
			"\n... [earlier output not shown here; the full record is at %s — read or grep it on this executor] ...\n",
			j.out.path))
		if room := maxJobResponse - len(marker); room < len(data) {
			if room < 0 {
				room = 0
			}
			data = data[len(data)-room:]
		}
		data = append(marker, data...)
	}
	j.mu.Lock()
	exited, exitCode = j.exited, j.exitCode
	j.mu.Unlock()
	return data, total, exited, exitCode, true
}

// outputPath is where this job's full output is recorded, "" if unknown.
func (r *jobRegistry) outputPath(handle string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j, ok := r.jobs[handle]; ok {
		return j.out.path
	}
	return ""
}

// workspaceBytes is the retained output one workspace currently holds.
func (r *jobRegistry) workspaceBytes(workspaceID string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, j := range r.jobs {
		if j.workspaceID == workspaceID {
			total += j.out.size()
		}
	}
	return total
}

// kill terminates job handle and everything it spawned.
//
// It signals the process GROUP, not the process: a background
// `bash -c "npm run dev"` spawns children that a bare Process.Kill leaves
// running. start() sets Setpgid so the group id equals the pid.
func (r *jobRegistry) kill(handle string) error {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return nil // already gone
	}
	if j.cmd.Process == nil {
		return nil // start() failed; nothing to signal
	}
	pgid := j.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		// The group may already be gone; fall back to the direct process so
		// a racing exit is not reported as a failure.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return j.cmd.Process.Kill()
	}
	return nil
}

// running returns the handles of all non-exited jobs.
func (r *jobRegistry) running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var handles []string
	for h, j := range r.jobs {
		j.mu.Lock()
		exited := j.exited
		j.mu.Unlock()
		if !exited {
			handles = append(handles, h)
		}
	}
	return handles
}

// releaseWorkspace ends one workspace's jobs: kills the running ones, drops the
// finished ones, and removes every output file. This is the ONLY thing that
// ends retention besides the byte budget — there is no timer, because a
// wall-clock window cannot know when an async agent will come back for its
// output, and the workspace's lifetime already does.
//
// Scoped to one workspace: it replaces a killAll that took no argument, so
// releasing ONE child's workspace killed every other child's background jobs on
// the executor. That was finding D4.
//
// Signals the process GROUP, matching kill, so a background `npm run dev` does
// not leave its tree running at teardown — which is when it matters most.
func (r *jobRegistry) releaseWorkspace(workspaceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for handle, j := range r.jobs {
		if j.workspaceID != workspaceID {
			continue
		}
		if j.cmd.Process != nil {
			_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
		}
		j.out.remove()
		delete(r.jobs, handle)
	}
}
