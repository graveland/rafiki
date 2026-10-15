package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Backgrounder starts bash commands that may outlive their tool call. The
// executor provides one; the in-process executor does not, and its bash
// keeps kill-at-timeout.
type Backgrounder interface {
	// Start runs argv in dir under env as a provisional job: running, its
	// output recorded, but not yet visible as a job.
	Start(argv, env []string, dir, command string, owner string) (Provisional, error)
}

// Provisional is a running command that is not yet a job.
type Provisional interface {
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// Result is valid after Done: merged output, the first 4 KiB of stderr,
	// and the error exec.Cmd.Wait reported (nil on exit 0).
	Result() (combined, stderrHead string, err error)
	// Snapshot is the merged output so far and when it was last written
	// (zero if nothing has been).
	Snapshot() (combined string, lastOutput time.Time)
	// Promote makes it a visible job and returns its handle.
	Promote() string
	// Discard kills it if still running and removes every trace.
	Discard()
}

type autoBackgroundKey struct{}
type ownerKey struct{}

// WithAutoBackground marks ctx's bash call as eligible for auto-background,
// on behalf of owner.
func WithAutoBackground(ctx context.Context, owner string) context.Context {
	ctx = context.WithValue(ctx, autoBackgroundKey{}, true)
	return context.WithValue(ctx, ownerKey{}, owner)
}

func autoBackground(ctx context.Context) (owner string, ok bool) {
	flag, _ := ctx.Value(autoBackgroundKey{}).(bool)
	if !flag {
		return "", false
	}
	owner, _ = ctx.Value(ownerKey{}).(string)
	return owner, true
}

// autoRun is the outcome of one auto-background-eligible execution: either the
// command finished within its timeout (out/stderr/err exactly as runSubprocess
// would report), or it was promoted to a visible background job.
type autoRun struct {
	out      string
	stderr   string
	err      error
	promoted bool
	snap     string
	last     time.Time
	handle   string
}

// runAuto runs argv — the same argv run would exec (an rtk rewrite, or
// `bash -c <command>`) — as a provisional job under bt's env and cwd, then
// waits either for it to exit or for timeout to elapse. A clean finish is
// returned so the caller can run it through the ordinary exit-code and clip
// handling; a timeout promotes the still-running process to a visible job and
// returns its handle. ctx here is the CALLER's context, not the timed one: the
// timer owns the promote deadline, so a done ctx means genuine cancellation
// (a timed context would race the timer at exactly timeout).
func (bt *bashTool) runAuto(ctx context.Context, timeout time.Duration, argv []string, command, owner string) autoRun {
	p, err := bt.bg.Start(argv, bt.env, bt.cwd, command, owner)
	if err != nil {
		return autoRun{err: err}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.Done():
		combined, stderrHead, runErr := p.Result()
		p.Discard()
		return autoRun{out: combined, stderr: stderrHead, err: runErr}
	case <-timer.C:
		snap, last := p.Snapshot()
		handle := p.Promote()
		return autoRun{promoted: true, snap: snap, last: last, handle: handle}
	case <-ctx.Done():
		p.Discard()
		return autoRun{err: ctx.Err()}
	}
}

// backgroundHandoff renders the result the model sees when a bash call was
// moved to the background: what it printed so far, how fresh that is, and how
// to read more or stop it. rtkApplied adds the note that a rewritten command's
// output only appears once it exits.
func (bt *bashTool) backgroundHandoff(r autoRun, timeout time.Duration, rtkApplied bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[still running after %s; moved to the background as job %s]\n", timeout.Round(time.Second), r.handle)
	b.WriteString(bt.p.Clip(r.snap, "bash-"+r.handle))
	b.WriteString("\n")
	if r.last.IsZero() {
		b.WriteString("[no output yet]\n")
	} else {
		fmt.Fprintf(&b, "[last output %s ago]\n", time.Since(r.last).Round(time.Second))
	}
	if rtkApplied {
		b.WriteString("(output is compacted by rtk and appears when it exits)\n")
	}
	fmt.Fprintf(&b, "You will get a message when it exits. Read more with bash_output {\"handle\":\"%s\"}; stop it with bash_kill.\n", r.handle)
	return b.String()
}
