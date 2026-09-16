package main

import (
	"sync"

	"go.graveland.dev/rafiki/pkg/child"
)

// killMark records WHICH audience a self-initiated kill already has its
// answer for — the audience whose settlement notification would be pure
// duplication, because the killer's own tool call confirmed the kill
// synchronously (agent_kill blocks until cm.Remove). At most one audience is
// ever set per child: the first killer to mark wins, and a second agent_kill
// answers "child has already exited".
type killMark struct {
	// parent is set by controllerSpawner.Kill — a coordinator killing its own
	// subagent. The suppressed audience is the child's parent, whose event
	// buffer would otherwise receive the "exited" fragment the coordinator's
	// own agent_kill result already answered.
	parent bool

	// mcpUser is set by userSpawner.Kill — an MCP caller killing an agent. The
	// suppressed audience is that caller's own user: notifyMCPSettled excludes
	// this user's sessions from the fan-out. The parent fragment still fires —
	// an MCP caller can kill somebody else's worker, and that worker's
	// coordinator did not act.
	mcpUser string
}

// selfKillStore marks children whose kill was initiated by their own agent
// surface (controllerSpawner.Kill or userSpawner.Kill) — never by a human via
// CLI/Connect, which calls Controller.Kill directly and never touches this
// store. handleChildExit consults it to tell "I killed this myself" apart
// from a human kill or a crash.
//
// Guarded by its own mutex, like turnOutcomeStore, and reset by take rather
// than get: a stale marker from an earlier kill attempt must never suppress
// the notification for a later, unrelated exit.
type selfKillStore struct {
	mu sync.Mutex
	m  map[string]killMark
}

func (s *selfKillStore) set(childID string, mark killMark) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]killMark)
	}
	s.m[childID] = mark
}

// take reports whether childID was marked, clearing the mark.
func (s *selfKillStore) take(childID string) (killMark, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mark, ok := s.m[childID]
	if ok {
		delete(s.m, childID)
	}
	return mark, ok
}

// exitCausedByShutdown reports whether res is the death the shutdown sequence
// itself produced, rather than the child dying of something else while the
// sequence waited: either a rung beyond the passive stdin close ran (SIGTERM —
// including a runner whose own signal handler converts it to a nonzero exit,
// the way claude exits 143 instead of dying by signal — SIGKILL, or an
// abandoned wait), or the stdin closer was itself the stop request
// (darajapool's relay). A clean exit (code 0, no signal) on a genuinely
// passive stdin close needs no causality bit: the shape is unambiguous.
//
// This is what makes the claude case decidable at all. Our SIGTERM and an
// external one are wire-identical — (0, "terminated") from a dying process,
// (143, "") from claude's own handler — and the suppression must not answer
// "was this a clean death" but "did MY kill do this".
func exitCausedByShutdown(res child.ShutdownResult) bool {
	return res.ByShutdown || (res.Signal == "" && res.ExitCode == 0)
}

// selfKillDisposition is the exit-time meaning of a consumed self-kill mark:
// which audiences the settlement notification skips. Everything not listed
// still fires — the fragment, the residue check, the MCP fan-out to anyone
// but an excluded user.
type selfKillDisposition struct {
	// suppressParent silences the parent's settlement fragment entirely;
	// checkTaskResidue still runs. Set when the child's own coordinator did
	// the killing — that caller's agent_kill result already answered.
	suppressParent bool

	// excludeMCPUser removes this user's sessions from the settlement fan-out
	// while the fragment still fires elsewhere. Set when an MCP caller of
	// this user did the killing.
	excludeMCPUser string
}

// selfKillDispositionFor resolves the disposition. caused is the conjunction
// the caller computed: the mark existed AND the death was the kill's own
// doing (exitCausedByShutdown). When it is false — a crash or a foreign
// signal racing the kill, or no mark at all — nothing is suppressed: the
// death is news to every audience, and the fragment is the only channel that
// carries it.
func selfKillDispositionFor(mark killMark, caused bool) selfKillDisposition {
	if !caused {
		return selfKillDisposition{}
	}
	return selfKillDisposition{suppressParent: mark.parent, excludeMCPUser: mark.mcpUser}
}
