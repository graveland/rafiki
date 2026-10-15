package executor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
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

	// subscriberCapacity bounds how many unread events one WatchJobs
	// subscriber may have buffered before the registry ends it. A subscriber
	// that cannot keep up is CLOSED, never silently skipped, and its
	// resubscribe replays everything unacked.
	subscriberCapacity = 256

	// stderrHeadSize is how many stderr bytes a provisional job keeps in its
	// head buffer, for the rtk-refusal check. The rest is discarded: stderr is
	// already merged into the job's output file, so the head is a copy, not
	// the record.
	stderrHeadSize = 4096
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
	closed bool  // the store was removed (file deleted)

	// lastWrite is when the last byte was written, for the handoff's "last
	// output N ago" line. Zero means nothing has been written yet.
	lastWrite time.Time
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
	if o.closed || o.f == nil {
		// The job was evicted, its workspace released, or it exited and its
		// file was closed. Discard rather than error: the process may outlive
		// the record, and a write error here would surface as a spurious job
		// failure.
		return len(p), nil
	}
	n, err := o.f.Write(p)
	o.total += int64(n)
	o.lastWrite = time.Now()
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

// reader returns a file handle to read from, reopening by path when the
// descriptor was closed at exit. closeAfter reports whether the caller owns the
// handle and must close it. Caller holds o.mu.
func (o *jobOutput) reader() (f *os.File, closeAfter bool, err error) {
	if o.f != nil {
		return o.f, false, nil
	}
	f, err = os.Open(o.path)
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

// since returns bytes from `from` onward, capped at maxJobResponse and taken
// from the END. dropped reports that bytes between `from` and the returned
// slice were not included — they are still in the file when they were dropped
// by the cap, and gone when they were dropped by compaction.
//
// A finished job's file is CLOSED (see closeFile), so this reopens it by path
// when needed. The file stays on disk; only the descriptor is gone.
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
		f, closeAfter, err := o.reader()
		if err != nil {
			return nil, o.total, true
		}
		if closeAfter {
			defer f.Close()
		}
		if _, err := f.ReadAt(out, start-o.base); err != nil {
			return nil, o.total, true
		}
	}
	return out, o.total, dropped
}

// all returns every RETAINED byte, up to maxJobSpill and NOT capped at
// maxJobResponse: the bash tool clips and spills the handoff's snapshot itself,
// so the registry hands it the record rather than a pre-trimmed slice.
func (o *jobOutput) all() ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n := o.total - o.base
	if n <= 0 {
		return nil, nil
	}
	if o.closed {
		return nil, os.ErrClosed
	}
	f, closeAfter, err := o.reader()
	if err != nil {
		return nil, err
	}
	if closeAfter {
		defer f.Close()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return nil, err
	}
	return buf, nil
}

// lastWriteAt is when the job last wrote output, zero if it never has.
func (o *jobOutput) lastWriteAt() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastWrite
}

// size is the bytes this job currently holds on disk, for the workspace budget.
func (o *jobOutput) size() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total - o.base
}

// closeFile closes the output descriptor and leaves the file on disk. It runs
// when the job exits, so a long-lived executor with thousands of small finished
// jobs does not hold a descriptor for each — only the bytes, which is what
// retention is for. A later read reopens by path. `closed` still means
// "removed", not "closed file".
func (o *jobOutput) closeFile() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f != nil {
		_ = o.f.Close()
		o.f = nil
	}
}

// remove closes the file and deletes it. Idempotent. Handles a file whose
// descriptor was already closed at exit.
func (o *jobOutput) remove() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	if o.f != nil {
		_ = o.f.Close()
		o.f = nil
	}
	_ = os.Remove(o.path)
}

// headBuffer keeps the FIRST limit bytes written and discards the rest. Used
// for a provisional job's stderr copy, where only the head can hold the rtk
// refusal marker that decides whether the command is re-run.
type headBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newHeadBuffer(limit int) *headBuffer {
	return &headBuffer{limit: limit}
}

func (h *headBuffer) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.limit - len(h.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		h.buf = append(h.buf, p[:room]...)
	}
	return len(p), nil
}

func (h *headBuffer) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.buf)
}

// job holds a single background process and its output.
type job struct {
	cmd    *exec.Cmd
	handle string
	out    *jobOutput
	// workspaceID is the workspace this job belongs to, "" for a call that named
	// no workspace. Releasing a workspace must end ITS jobs and no others.
	workspaceID string
	// owner is the daemon's id for the agent that started this job, opaque
	// here. It rides every event so the daemon can route the event to its
	// child with no handle bookkeeping of its own.
	owner string
	// command is the command line, for the event and the daemon's fragment.
	command string
	// done is closed once the process has exited and its state recorded, so a
	// reader that sees it closed can read the full result and waitErr.
	done chan struct{}
	// stderrHead keeps the first stderrHeadSize bytes of stderr for a
	// PROVISIONAL job (nil otherwise), for the rtk-refusal check on the
	// auto-background path.
	stderrHead *headBuffer

	mu         sync.Mutex
	exited     bool
	exitCode   int
	waitErr    error
	finishedAt time.Time

	// acked/provisional/exitPublished are guarded by the REGISTRY's mutex, not
	// j.mu: they are read and written by the registry (kill, ack, promote,
	// discard, publish) under r.mu.
	//
	// acked marks a job whose exit must produce no event: one the agent
	// killed, or one whose exit the daemon has already acknowledged.
	acked bool
	// provisional marks a job that is running but not yet visible as a job —
	// invisible to output/kill/outputPath/snapshot and publishing no events
	// until promote.
	provisional bool
	// exitPublished guards the exited event so promote and the Wait goroutine
	// never publish it twice.
	exitPublished bool
}

// jobEvent is one background-job lifecycle notification, fanned out to every
// WatchJobs subscriber.
type jobEvent struct {
	running     bool // false: exited
	handle      string
	workspaceID string
	owner       string
	command     string
	exitCode    int
	finishedAt  time.Time
}

// jobRegistry tracks running background jobs and retains finished ones.
type jobRegistry struct {
	mu       sync.Mutex
	jobs     map[string]*job
	subs     map[chan jobEvent]struct{}
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
		subs:     make(map[chan jobEvent]struct{}),
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
// dir is where the job starts — the workspace's workdir, which is NOT
// necessarily the executor's root. An empty dir means the registry's own
// root, which keeps a caller that has no workspace to name honest.
//
// owner is the daemon's id for the agent this job runs for, carried on every
// event.
//
// It returns once the process is RUNNING, so cmd.Process is non-nil for every
// caller that sees a nil error — kill must never race a start.
func (r *jobRegistry) start(command, handle, workspaceID, dir, owner string) (string, error) {
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
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = out
	cmd.Stderr = out
	// Jobs inherit the executor's environment (a nil env means the process
	// environment, the registry's existing contract), plus the
	// non-interactive defaults so a command that would prompt fails instead.
	// PWD is pinned to the job's directory: a non-nil Env suppresses exec's
	// automatic PWD=Dir, and the inherited PWD would name the executor's own
	// directory, changing what `pwd` reports (see runSubprocess in
	// pkg/fundi/tools/bash.go for the same trap). The pin is guarded on a
	// non-empty dir, matching runSubprocess — an empty dir leaves exec's
	// default rather than setting PWD=.
	cmd.Env = tools.NonInteractiveEnv(nil)
	if cmd.Dir != "" {
		cmd.Env = tools.MergeEnv(cmd.Env, []string{"PWD=" + cmd.Dir})
	}
	// Own session AND process group, so kill can signal the whole tree: a
	// background `bash -c "npm run dev"` spawns children that outlive a bare
	// Process.Kill on the bash itself. Setsid rather than Setpgid: a new
	// session has no controlling terminal, so a program that opens /dev/tty
	// (sudo, ssh, gpg) gets ENXIO and exits rather than stopping on SIGTTIN;
	// a session leader still leads its own process group, so the group kill
	// is unchanged.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = jobWaitDelay

	if err := cmd.Start(); err != nil {
		out.remove()
		return "", err
	}

	j := &job{
		cmd:         cmd,
		handle:      handle,
		out:         out,
		workspaceID: workspaceID,
		owner:       owner,
		command:     command,
		done:        make(chan struct{}),
	}
	r.mu.Lock()
	r.jobs[handle] = j
	// A started job is immediately visible: publish its running event now,
	// from the place that knows the process is up.
	r.publish(r.runningEvent(j))
	r.enforceBudget(workspaceID)
	r.mu.Unlock()

	go r.wait(j, cmd, out, workspaceID)
	return handle, nil
}

// startProvisional runs argv[0] argv[1:] under env as a PROVISIONAL job: the
// process runs and its output is recorded, but it is not listed as a job and
// publishes no event until promote. It is the first half of auto-backgrounding
// a bash call — a command that finishes under the threshold is discarded before
// it is ever a job.
//
// Unlike start it runs the argv directly (no `bash -c`) and uses the caller's
// pinned env as-is. stderr is teed to a head buffer so the rtk-refusal check
// can see it without the full output being buffered twice.
func (r *jobRegistry) startProvisional(argv, env []string, dir, workspaceID, owner, command string) (*job, error) {
	handle := randomID()
	out, err := newJobOutput(filepath.Join(r.spillDir, "job-"+handle+".log"))
	if err != nil {
		return nil, fmt.Errorf("background job output file: %w", err)
	}
	head := newHeadBuffer(stderrHeadSize)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.cwd
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	cmd.Stdout = out
	// Stdout and Stderr are DIFFERENT writers, so exec runs two goroutines over
	// them; both `out` and `head` are mutex-guarded, so this is safe.
	cmd.Stderr = io.MultiWriter(out, head)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = jobWaitDelay

	if err := cmd.Start(); err != nil {
		out.remove()
		return nil, err
	}

	j := &job{
		cmd:         cmd,
		handle:      handle,
		out:         out,
		workspaceID: workspaceID,
		owner:       owner,
		command:     command,
		done:        make(chan struct{}),
		stderrHead:  head,
		provisional: true,
	}
	r.mu.Lock()
	r.jobs[handle] = j
	r.enforceBudget(workspaceID)
	r.mu.Unlock()

	go r.wait(j, cmd, out, workspaceID)
	return j, nil
}

// wait is the goroutine behind every job: it reaps the process and records its
// outcome in an order a reader can rely on.
//
// The order is load-bearing. exited, exitCode, waitErr and finishedAt are
// recorded FIRST; then the output file is closed; then done is closed. A reader
// that observes done closed can therefore read the complete result and the Wait
// error. The exited event is published LAST, and only for a job that is visible
// and unacked — a killed job and a provisional job that was never promoted both
// publish nothing, and promote (which may publish it instead) is guarded
// against publishing it twice.
func (r *jobRegistry) wait(j *job, cmd *exec.Cmd, out *jobOutput, workspaceID string) {
	err := cmd.Wait()

	// Written BEFORE the job is marked exited, so a reader that sees
	// "exited" has already been able to see the note.
	if errors.Is(err, exec.ErrWaitDelay) {
		_, _ = out.Write([]byte("\n[job exited but left background processes holding its output pipes; " +
			"later output is not captured]\n"))
	}

	j.mu.Lock()
	j.exited = true
	j.waitErr = err
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

	out.closeFile()
	close(j.done)

	r.mu.Lock()
	r.publishExitLocked(j)
	r.enforceBudget(workspaceID)
	r.mu.Unlock()
}

// promote makes a provisional job visible: it becomes listed and its running
// event is published. If it had already exited, its exited event is published
// too (the Wait goroutine could not publish it while the job was provisional).
// Returns the job's handle.
func (r *jobRegistry) promote(j *job) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !j.provisional {
		return j.handle
	}
	j.provisional = false
	r.publish(r.runningEvent(j))
	j.mu.Lock()
	exited := j.exited
	j.mu.Unlock()
	if exited {
		r.publishExitLocked(j)
	}
	return j.handle
}

// discard removes a job entirely: it disappears from the registry and its
// output file is deleted. A job whose process is still running has its group
// killed first, and it is acked so the exit it causes publishes nothing.
func (r *jobRegistry) discard(j *job) {
	j.mu.Lock()
	exited := j.exited
	j.mu.Unlock()
	if !exited && j.cmd.Process != nil {
		_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
	}
	r.mu.Lock()
	j.acked = true
	delete(r.jobs, j.handle)
	r.mu.Unlock()
	j.out.remove()
}

// runningEvent builds the visible-job event for a running job. Caller holds
// r.mu. The job's identity fields are immutable once it is inserted.
func (r *jobRegistry) runningEvent(j *job) jobEvent {
	return jobEvent{
		running:     true,
		handle:      j.handle,
		workspaceID: j.workspaceID,
		owner:       j.owner,
		command:     j.command,
	}
}

// publishExitLocked publishes a job's exited event at most once, and only for a
// visible, unacked job. A provisional job's exit is deferred to promote; a
// killed or acked job's exit is suppressed. Caller holds r.mu.
func (r *jobRegistry) publishExitLocked(j *job) {
	if j.provisional || j.exitPublished || j.acked {
		return
	}
	j.exitPublished = true
	j.mu.Lock()
	ev := jobEvent{
		running:     false,
		handle:      j.handle,
		workspaceID: j.workspaceID,
		owner:       j.owner,
		command:     j.command,
		exitCode:    j.exitCode,
		finishedAt:  j.finishedAt,
	}
	j.mu.Unlock()
	r.publish(ev)
}

// subscribe registers a WatchJobs subscriber and returns its channel plus a
// cancel. The channel is CLOSED by the registry if it ever fills: a subscriber
// that cannot keep up is ended, never silently skipped, and its resubscribe
// replays everything unacked.
func (r *jobRegistry) subscribe() (<-chan jobEvent, func()) {
	ch := make(chan jobEvent, subscriberCapacity)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			if _, ok := r.subs[ch]; ok {
				delete(r.subs, ch)
				close(ch)
			}
			r.mu.Unlock()
		})
	}
	return ch, cancel
}

// publish fans an event out to every subscriber. A non-blocking send; a full
// channel means the subscriber fell behind, so it is closed and removed rather
// than skipped. Caller holds r.mu.
func (r *jobRegistry) publish(ev jobEvent) {
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// snapshot returns every visible running job and every visible finished,
// unacked job, for the replay a WatchJobs subscriber gets on subscribe.
func (r *jobRegistry) snapshot() (running, exited []jobEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.provisional {
			continue
		}
		j.mu.Lock()
		exitedJob := j.exited
		ev := jobEvent{
			running:     !exitedJob,
			handle:      j.handle,
			workspaceID: j.workspaceID,
			owner:       j.owner,
			command:     j.command,
			exitCode:    j.exitCode,
			finishedAt:  j.finishedAt,
		}
		j.mu.Unlock()
		if exitedJob {
			if j.acked {
				continue
			}
			exited = append(exited, ev)
			continue
		}
		running = append(running, ev)
	}
	return running, exited
}

// enforceBudget evicts finished jobs, oldest-finished first, until the
// workspace is within its byte budget. Callers must hold r.mu.
//
// Running jobs are never evicted: their output is a live stream, and dropping
// it loses what is arriving rather than an archive. An UNACKED finished job is
// never evicted either: its exit has not been delivered, and evicting it would
// lose the report as well as the output. A workspace whose running or unacked
// jobs alone exceed the budget therefore stays over it, bounded only by
// maxJobSpill per job — which is the right failure, since the alternative is
// silently cutting a stream the agent is still watching or dropping an
// undelivered exit.
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
		if exited && j.acked {
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
// It reads the LIVE file, not a snapshot taken at exit: a read of a running job
// must see output as it arrives, which is the entire reason handles exist. A
// provisional job is invisible here: its handle answers like an unknown one.
func (r *jobRegistry) output(handle string, since int64) (data []byte, total int64, exited bool, exitCode int, found bool) {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	if ok && j.provisional {
		ok = false
	}
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

// outputPath is where this job's full output is recorded, "" if unknown. A
// provisional job reports no path, matching its invisibility.
func (r *jobRegistry) outputPath(handle string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j, ok := r.jobs[handle]; ok && !j.provisional {
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
// running. start() sets Setsid so the job leads its own session and process
// group, making the group id equal the pid.
//
// The job is marked acked BEFORE it is signalled, so the exit this kill causes
// publishes no event — a job the agent stopped is not reported as finished. A
// provisional job is invisible: killing its handle is a no-op, like killing an
// unknown one.
func (r *jobRegistry) kill(handle string) error {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	if ok && j.provisional {
		ok = false
	}
	if ok {
		j.acked = true
	}
	r.mu.Unlock()
	if !ok {
		return nil // already gone or never visible
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

// ack marks finished jobs as delivered: their exits stop being replayed and
// they become eligible for byte-budget eviction. Unknown handles are ignored and
// a running job is not acked (there is no exit yet to deliver). The budget is
// re-enforced for every workspace that was affected.
func (r *jobRegistry) ack(handles []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	affected := make(map[string]struct{})
	for _, h := range handles {
		j, ok := r.jobs[h]
		if !ok {
			continue
		}
		j.mu.Lock()
		exited := j.exited
		j.mu.Unlock()
		if !exited {
			continue
		}
		j.acked = true
		affected[j.workspaceID] = struct{}{}
	}
	for ws := range affected {
		r.enforceBudget(ws)
	}
}

// running returns the handles of all non-exited, visible jobs.
func (r *jobRegistry) running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var handles []string
	for h, j := range r.jobs {
		if j.provisional {
			continue
		}
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
