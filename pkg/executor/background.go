package executor

import (
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// registryBackgrounder is the executor's tools.Backgrounder: it starts a
// bash command as a PROVISIONAL job in one workspace's name, so the bash
// tool's auto-background path can wait on it before deciding whether it is a
// job at all. The job registry is the only thing that can own a process here,
// so the tool's seam is an adapter over it rather than a second spawn path.
//
// workspaceID is the workspace whose jobs this backgrounder creates, "" for
// the executor's own root registry. It must match the workspace's id or a
// Release would not end the jobs the tool started.
type registryBackgrounder struct {
	r           *jobRegistry
	workspaceID string
}

func (b *registryBackgrounder) Start(argv, env []string, dir, command, owner string) (tools.Provisional, error) {
	j, err := b.r.startProvisional(argv, env, dir, b.workspaceID, owner, command)
	if err != nil {
		// Returned verbatim, never stringified: a missing binary must stay an
		// *exec.Error wrapping exec.ErrNotFound / os.ErrPermission so the bash
		// tool reports it as a tool error rather than folding it into result
		// text.
		return nil, err
	}
	return jobProvisional{r: b.r, j: j}, nil
}

// jobProvisional adapts a provisional *job to tools.Provisional.
type jobProvisional struct {
	r *jobRegistry
	j *job
}

func (p jobProvisional) Done() <-chan struct{} { return p.j.done }

// Result reads the finished job's retained output and the error exec.Cmd.Wait
// reported. waitErr is read without j.mu because Done being closed is the
// happens-before edge: wait() records it under the mutex and only then closes
// done, so a reader that saw done closed sees the write.
func (p jobProvisional) Result() (combined, stderrHead string, err error) {
	b, _ := p.j.out.all()
	return string(b), p.j.stderrHead.String(), p.j.waitErr
}

func (p jobProvisional) Snapshot() (string, time.Time) {
	b, _ := p.j.out.all()
	return string(b), p.j.out.lastWriteAt()
}

func (p jobProvisional) Promote() string { return p.r.promote(p.j) }

func (p jobProvisional) Discard() { p.r.discard(p.j) }
