// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/multigres/testkit/assert"
)

// A child that is already halted must not buy a turn: the loop's own wrap-up
// after a guardrail stop is a paid LLM call. The prompt is still appended to
// the conversation, so what woke the child is in its context once the halt
// lifts, and the turn reports the halt reason as its limit.
func TestHaltedChildRunsNoLLMCallButKeepsThePrompt(t *testing.T) {
	ck := assert.NewAborting(t)
	var halted atomic.Bool
	halted.Store(true)
	var got []TurnOutcome
	sender := newCapturingSender(t, sampleEndTurn)
	eng, out := newTestEngineWithConfig(t, fakeToolSet{}, sender, func(cfg *EngineConfig) {
		cfg.Halted = func() (bool, string) { return halted.Load(), "budget held by c_root" }
		cfg.OnTurnEnded = func(o TurnOutcome) { got = append(got, o) }
	})

	eng.HandlePrompt("a subagent finished")
	eng.Wait()

	sender.mu.Lock()
	calls := len(sender.captured)
	sender.mu.Unlock()
	ck.Eq(0, calls, "a halted child made an LLM call")
	ck.Len(got, 1, "OnTurnEnded fired %d times, want 1", len(got))
	ck.Eq("budget held by c_root", got[0].LimitReason, "turn outcome limit reason")
	ck.Contains(userMessageTexts(t, out.String()), "a subagent finished", "the halted turn did not echo the prompt")
	hist, err := eng.conv.History(context.Background())
	ck.NoError(err, "History")
	ck.Len(hist, 1, "the prompt must land in the conversation, got %d messages", len(hist))

	halted.Store(false)
	eng.HandlePrompt("go on")
	eng.Wait()

	sender.mu.Lock()
	calls = len(sender.captured)
	sender.mu.Unlock()
	ck.Eq(1, calls, "once the halt lifts the next prompt runs a normal turn")
	ck.Len(got, 2, "OnTurnEnded fired %d times, want 2", len(got))
	ck.True(got[1].Clean, "outcome after the halt lifted = %+v, want clean", got[1])
}
