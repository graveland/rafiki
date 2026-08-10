package executor

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"time"
)

// job holds a single background process and its output ring buffer.
type job struct {
	cmd    *exec.Cmd
	handle string

	mu      sync.Mutex
	buf     bytes.Buffer
	exited  bool
	exitCode int
	done    chan struct{}
}

// maxJobOutput is the byte budget for a single job's output buffer. When
// exceeded, head and tail are kept and the middle is elided — the same
// pattern bash.go uses for its clips.
const maxJobOutput = 100_000

// jobRegistry tracks running background jobs.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*job
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: make(map[string]*job)}
}

// start launches cmd as a background job, captures its combined output, and
// returns a handle. The job runs until completion or Kill.
func (r *jobRegistry) start(cmd *exec.Cmd, handle string) {
	j := &job{
		cmd:    cmd,
		handle: handle,
		done:   make(chan struct{}),
	}

	// Use a syncWriter so Stdout and Stderr can share a single buffer without
	// racing. This avoids the trap documented in bash.go: teeing two pipes
	// into one buffer without a mutex is a data race.
	sw := &syncWriter{}
	cmd.Stdout = sw
	cmd.Stderr = sw

	r.mu.Lock()
	r.jobs[handle] = j
	r.mu.Unlock()

	go func() {
		err := cmd.Run()
		sw.mu.Lock()
		j.mu.Lock()
		j.exited = true
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				j.exitCode = ee.ExitCode()
			} else {
				j.exitCode = -1
			}
		}
		j.buf = clipBuffer(sw.buf)
		j.mu.Unlock()
		sw.mu.Unlock()
		close(j.done)
	}()
}

// output returns the current combined output of job handle. The second return
// value is true when the job has exited.
func (r *jobRegistry) output(handle string) ([]byte, bool) {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return nil, true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]byte, j.buf.Len())
	copy(out, j.buf.Bytes())
	return out, j.exited
}

// kill terminates job handle. It returns nil when the job was found and
// signalled; the caller must still wait for the process to reap.
func (r *jobRegistry) kill(handle string) error {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return nil // already gone
	}
	return j.cmd.Process.Kill()
}

// wait blocks until job handle exits, then returns its exit code.
func (r *jobRegistry) wait(handle string) (int, bool) {
	r.mu.Lock()
	j, ok := r.jobs[handle]
	r.mu.Unlock()
	if !ok {
		return -1, false
	}
	select {
	case <-j.done:
	case <-time.After(10 * time.Minute):
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exitCode, true
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

// purge removes an exited job entry. Safe to call when the handle does not
// exist — it is a no-op.
func (r *jobRegistry) purge(handle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, handle)
}

// syncWriter is a mutex-guarded bytes.Buffer. See bash.go for the race this
// avoids.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

// clipBuffer keeps head and tail, elides the middle, exactly as bash.go does
// for oversized tool results.
func clipBuffer(buf bytes.Buffer) bytes.Buffer {
	if buf.Len() <= maxJobOutput {
		return buf
	}
	raw := buf.Bytes()
	headBytes := maxJobOutput * 20 / 100
	tailBytes := maxJobOutput - headBytes

	var out bytes.Buffer
	out.Write(raw[:headBytes])
	elided := len(raw) - headBytes - tailBytes
	out.WriteString("\n\n... [" + itoa(elided) + " bytes elided] ...\n\n")
	out.Write(raw[len(raw)-tailBytes:])
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// waitFor blocks until the job's done channel is closed or ctx is cancelled.
// Returns (exitCode, true) on completion, (0, false) on cancel/timeout.
func (j *job) waitFor(ctx context.Context) (int, bool) {
	select {
	case <-j.done:
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.exitCode, true
	case <-ctx.Done():
		return 0, false
	}
}
