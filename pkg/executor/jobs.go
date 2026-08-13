package executor

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// jobRetention is how long a finished job stays readable before its ring is
// released. bash_output on a job that just finished must still work; holding
// every job forever is a leak.
const jobRetention = 10 * time.Minute

// maxJobOutput is the byte budget for a single job's live output ring. Once
// exceeded, the OLDEST bytes are dropped: a live tail wants the newest
// output, and eliding the middle (which is what bash.go does for a completed
// result) would invalidate the byte offsets Attach uses for deltas.
const maxJobOutput = 100_000

// ring is a mutex-guarded bounded buffer that remembers how many bytes have
// ever passed through it. Stdout and Stderr both point at the same ring —
// exec.Cmd only shares one pipe when the two writers are the identical
// value, and pointing them at two writers over one buffer is a data race
// (see pkg/fundi/tools/bash.go).
type ring struct {
	mu    sync.Mutex
	buf   []byte
	total int64 // bytes ever written, including those dropped from buf
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += int64(len(p))
	r.buf = append(r.buf, p...)
	if len(r.buf) > maxJobOutput {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-maxJobOutput:]...)
	}
	return len(p), nil
}

// since returns the bytes from offset `from` onward, plus the running total.
// When `from` is older than the ring's oldest retained byte, the returned
// slice starts at that oldest byte and `dropped` is true — the caller has
// missed data and must say so rather than silently splicing.
func (r *ring) since(from int64) (data []byte, total int64, dropped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest := r.total - int64(len(r.buf))
	if from < oldest {
		from = oldest
		dropped = true
	}
	if from > r.total {
		from = r.total
	}
	out := make([]byte, r.total-from)
	copy(out, r.buf[from-oldest:])
	return out, r.total, dropped
}

// job holds a single background process and its output ring.
type job struct {
	cmd    *exec.Cmd
	handle string
	out    *ring

	mu       sync.Mutex
	exited   bool
	exitCode int
	done     chan struct{}
}

// jobRegistry tracks running background jobs.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*job
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: make(map[string]*job)}
}

// start launches cmd as a background job and captures its combined output.
// It returns once the process is RUNNING, so cmd.Process is non-nil for
// every caller that sees a nil error — kill must never race a start.
func (r *jobRegistry) start(cmd *exec.Cmd, handle string) error {
	rg := &ring{}
	cmd.Stdout = rg
	cmd.Stderr = rg
	// Own process group, so kill can signal the whole tree: a background
	// `bash -c "npm run dev"` spawns children that outlive a bare
	// Process.Kill on the bash itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return err
	}

	j := &job{cmd: cmd, handle: handle, out: rg, done: make(chan struct{})}
	r.mu.Lock()
	r.jobs[handle] = j
	r.mu.Unlock()

	go func() {
		err := cmd.Wait()
		j.mu.Lock()
		j.exited = true
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				j.exitCode = ee.ExitCode()
			} else {
				j.exitCode = -1
			}
		}
		j.mu.Unlock()
		close(j.done)
		// Release the ring after the retention window. bash_output on a job
		// that just finished must still work, but nothing may hold every
		// job's output for the executor's lifetime.
		time.AfterFunc(jobRetention, func() { r.purge(handle) })
	}()
	return nil
}

// output returns the job's bytes from offset `since` onward.
//
// It reads the LIVE ring, not a snapshot taken at exit: an Attach to a
// running job must see output as it arrives, which is the entire reason
// handles exist.
func (r *jobRegistry) output(handle string, since int64) (data []byte, total int64, exited bool, exitCode int, found bool) {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return nil, 0, false, 0, false
	}
	data, total, dropped := j.out.since(since)
	if dropped {
		data = append([]byte("\n... [earlier output dropped: buffer limit reached] ...\n"), data...)
	}
	j.mu.Lock()
	exited, exitCode = j.exited, j.exitCode
	j.mu.Unlock()
	return data, total, exited, exitCode, true
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

// purge removes a job entry. Safe when the handle does not exist.
func (r *jobRegistry) purge(handle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, handle)
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

// killAll kills every registered job. Used when a workspace is released — a
// background job in a removed container is not a job, and reporting it as
// running is worse than reporting it gone.
func (r *jobRegistry) killAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for handle, j := range r.jobs {
		if j.cmd.Process != nil {
			_ = j.cmd.Process.Kill()
		}
		delete(r.jobs, handle)
	}
}
