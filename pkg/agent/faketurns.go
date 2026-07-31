package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// fakeSender replays pre-recorded assistant messages in order, ignoring the
// request. It is the test seam that lets engine and daemon integration tests
// drive a complete tool-use loop with no API key and no network.
type fakeSender struct {
	mu    sync.Mutex
	next  int
	turns []*anthropic.Message
}

// LoadFakeSender reads a file of newline-separated JSON anthropic.Message
// bodies and returns an llm.Sender that replays them in order. Blank lines are
// ignored; a malformed body is an error rather than a silently skipped turn.
// Once every scripted turn has been served, further calls return an error — an
// over-running loop is a test bug, not something to paper over. A file with
// zero messages (including an empty file, e.g. /dev/null) is not itself an
// error: it produces a sender that is immediately "exhausted", which is
// exactly right for driving a session that only ever needs get_state/no LLM
// call at all (see cmd/fundid agent --fake-turns's manual acceptance gate).
func LoadFakeSender(path string) (llm.Sender, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: read scripted turns: %w", err)
	}
	s := &fakeSender{}
	for i, line := range strings.Split(string(data), "\n") {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(text), &msg); err != nil {
			return nil, fmt.Errorf("agent: scripted turn %s:%d: %w", path, i+1, err)
		}
		s.turns = append(s.turns, &msg)
	}
	return s, nil
}

// New returns the next scripted message. Safe for concurrent use; the agent
// loop is sequential, but nothing in the Sender contract promises that.
//
// It honours ctx, which is not a formality. rafiki's agentloop.drive does not
// check ctx.Err() between iterations, so a sender that ignores its context
// answers one more iteration after an abort and the turn completes with a nil
// error. That made Engine.runTurn's abort branch — RepairOrphans included,
// whose whole job is to stop the NEXT API call being rejected for a dangling
// tool_use — unreachable from every fake-turns test, and it silently consumed a
// scripted message the aborted turn should never have seen. A real HTTP sender
// fails on a cancelled context; this one has to as well, or the harness is
// testing a loop that does not exist in production.
func (s *fakeSender) New(ctx context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.turns) {
		return nil, errors.New("agent: scripted turns exhausted")
	}
	msg := s.turns[s.next]
	s.next++
	return msg, nil
}
