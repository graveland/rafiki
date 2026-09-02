package main

import (
	"fmt"
	"sync"

	"go.graveland.dev/rafiki/pkg/fundi"
)

// turnOutcomeStore remembers the most recent fundi.TurnOutcome per child,
// between the moment Engine.OnTurnEnded fires and the moment
// handleStatusChange's idle transition asks "why did you stop". Guarded by
// its own mutex, like budgetBreaches: cheap, in-memory, and reset by take
// rather than get, so a stale outcome from an earlier turn is never
// reattached to a later, unrelated idle transition.
type turnOutcomeStore struct {
	mu sync.Mutex
	m  map[string]fundi.TurnOutcome
}

func (s *turnOutcomeStore) set(childID string, o fundi.TurnOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]fundi.TurnOutcome)
	}
	s.m[childID] = o
}

// take returns and clears the stored outcome, if any.
func (s *turnOutcomeStore) take(childID string) (fundi.TurnOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.m[childID]
	if ok {
		delete(s.m, childID)
	}
	return o, ok
}

// settleReason turns a stored TurnOutcome into the text notifySubagentSettled
// puts in front of the coordinator. A missing or clean outcome falls back to
// the generic "settled (idle)" — the only message this path has ever sent,
// preserved for the common case so existing callers/tests that pass their own
// reason (e.g. "exited") are unaffected.
func (c *Controller) settleReason(childID string) string {
	outcome, ok := c.turnOutcomes.take(childID)
	if !ok || outcome.Clean {
		return "settled (idle)"
	}
	if outcome.Err != nil {
		return fmt.Sprintf("settled after a turn error: %s", outcome.Err.Error())
	}
	return fmt.Sprintf("settled — %s", outcome.LimitReason)
}
