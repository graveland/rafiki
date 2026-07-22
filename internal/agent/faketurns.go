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

	"git.graveland.dev/brent/rafiki/llm"
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
// over-running loop is a test bug, not something to paper over.
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
	if len(s.turns) == 0 {
		return nil, fmt.Errorf("agent: scripted turns file %s has no messages", path)
	}
	return s, nil
}

// New returns the next scripted message. Safe for concurrent use; the agent
// loop is sequential, but nothing in the Sender contract promises that.
func (s *fakeSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.turns) {
		return nil, errors.New("agent: scripted turns exhausted")
	}
	msg := s.turns[s.next]
	s.next++
	return msg, nil
}
