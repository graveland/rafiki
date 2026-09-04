package main

import "sync"

// selfKillStore marks children whose kill was initiated by their own
// coordinator's agent_kill tool call (controllerSpawner.Kill) — never by a
// human via CLI/Connect, which calls Controller.Kill directly and never
// touches this store. handleChildExit consults it, via suppressExitNotice,
// to tell "I killed this myself" apart from a human kill or a crash.
//
// Guarded by its own mutex, like turnOutcomeStore, and reset by take rather
// than get: a stale marker from an earlier kill attempt must never suppress
// the notification for a later, unrelated exit.
type selfKillStore struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func (s *selfKillStore) set(childID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]struct{})
	}
	s.m[childID] = struct{}{}
}

// take reports whether childID was marked, clearing the mark.
func (s *selfKillStore) take(childID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[childID]
	if ok {
		delete(s.m, childID)
	}
	return ok
}

// suppressExitNotice reports whether the "exited" notification for childID
// should be suppressed. It consumes the self-kill marker exactly once as a
// side effect, regardless of the outcome — callers must not call take
// separately.
//
// The kill must have been self-initiated AND clean (no signal, ExitCode 0).
// agent_kill already blocks until cm.Remove, so its tool result confirms
// termination synchronously — a subsequent clean "exited" notification is
// pure duplication. Anything else — a panic's ExitCode=2 sentinel, a nonzero
// subprocess exit, or an escalated/forced SIGKILL (for a subprocess child,
// wire-identical to an EXTERNAL kill, e.g. an OOM killer targeting just that
// process) — still notifies: it is new information even when a kill was in
// flight when it happened.
func (c *Controller) suppressExitNotice(childID string, exitCode int, signal string) bool {
	return c.selfKilled.take(childID) && signal == "" && exitCode == 0
}
