// SPDX-License-Identifier: Apache-2.0

package fundi

// TestStartupResumeSurvivesAFailedResume pins the survive-on-error policy:
// a startup resume whose turn fails leaves the child ALIVE — the error is
// published and the child keeps accepting prompts. It used to fatal, which
// meant a daemon restart during a provider outage permanently exited every
// recovered child (handleChildExit persisted status=exited, which every later
// boot reads as terminal), orphaning the fleet exactly once and silently.
//
// The resume turn here fails on the FIRST send ("scripted turns exhausted" —
// non-retryable, so continueWithRetry returns immediately); the wrapper sender
// answers every LATER call with a clean end_turn, which is how the test proves
// the child went on living: its next prompt runs and completes.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// failFirstSender errors on its first call, then delegates.
type failFirstSender struct {
	inner llm.Sender
	once  sync.Once
	err   error
}

func (s *failFirstSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	if err := s.failOnce(); err != nil {
		return nil, err
	}
	return s.inner.Send(ctx, req, opts)
}

func (s *failFirstSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	if err := s.failOnce(); err != nil {
		return nil, err
	}
	return s.inner.Stream(ctx, req, opts)
}

func (s *failFirstSender) failOnce() error {
	var e error
	s.once.Do(func() { e = s.err })
	return e
}

func TestStartupResumeSurvivesAFailedResume(t *testing.T) {
	c := assert.NewAborting(t)
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	const ref = "startup-resume-survive"

	// History to resume: one user row, no assistant — a turn that was
	// interrupted before its first LLM call answered.
	seed, err := llm.NewClient(
		llm.WithProviderSender("anthropic", scriptedSender(t, sampleEndTurn)),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	conv1, err := seed.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	c.NoError(err)
	_, appendErr := conv1.AppendUser(ctx, llm.UserText("interrupted work"))
	c.NoError(appendErr)

	// The recovered engine: its FIRST send (the re-issued turn) fails; every
	// later send succeeds.
	sender := &failFirstSender{
		inner: scriptedSender(t, sampleEndTurn),
		err:   errors.New("agent: scripted turns exhausted"),
	}
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	eng, err := NewEngine(EngineConfig{
		Client:     client,
		Tools:      fakeToolSet{},
		Provider:   "anthropic",
		ModelID:    "claude-x",
		Name:       "w1",
		AutoResume: true,
		ConvOpts:   []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
	}, fe)
	c.NoError(err)
	eng.Start()
	fe.handler = eng

	// The failed startup resume publishes its error and closes the turn.
	deadline := time.Now().Add(10 * time.Second)
	failed := false
	for !failed && !time.Now().After(deadline) {
		failed = frameSeqHasAfter(frameTypes(t, out.String()), "agent_error", "agent_end")
		time.Sleep(5 * time.Millisecond)
	}
	c.Require().True(failed, "no failed-startup-resume frames; out:\n%s", out.String())

	// The child is alive: its next prompt enters a turn and completes.
	eng.HandlePrompt("still there?")
	eng.Wait()

	frames := frameTypes(t, out.String())
	c.True(frameSeqHasAfter(frames, "agent_error", "agent_start"),
		"a prompt after the failed startup resume must still run a turn (frames: %v)", frames)
	c.True(frameSeqHas(frames, "agent_settled"),
		"the post-failure prompt's turn must complete (frames: %v)", frames)
	eng.Close()
}

// blockFirstSender parks its first call until the request's context is
// cancelled, then delegates every later call.
type blockFirstSender struct {
	inner   llm.Sender
	once    sync.Once
	started chan struct{}
}

func (s *blockFirstSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	if err := s.blockOnce(ctx); err != nil {
		return nil, err
	}
	return s.inner.Send(ctx, req, opts)
}

func (s *blockFirstSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	if err := s.blockOnce(ctx); err != nil {
		return nil, err
	}
	return s.inner.Stream(ctx, req, opts)
}

func (s *blockFirstSender) blockOnce(ctx context.Context) error {
	block := false
	s.once.Do(func() { block = true })
	if block {
		close(s.started)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// TestStartupResumeIsAbortable pins that a turn re-issued by startup resume
// registers its cancel like any other turn: an abort that lands while it is in
// flight must stop it rather than be logged as "no turn running".
func TestStartupResumeIsAbortable(t *testing.T) {
	c := assert.NewAborting(t)
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	const ref = "startup-resume-abortable"

	seed, err := llm.NewClient(
		llm.WithProviderSender("anthropic", scriptedSender(t, sampleEndTurn)),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	conv1, err := seed.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	c.NoError(err)
	_, appendErr := conv1.AppendUser(ctx, llm.UserText("interrupted work"))
	c.NoError(appendErr)

	sender := &blockFirstSender{inner: scriptedSender(t, sampleEndTurn), started: make(chan struct{})}
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	eng, err := NewEngine(EngineConfig{
		Client:     client,
		Tools:      fakeToolSet{},
		Provider:   "anthropic",
		ModelID:    "claude-x",
		Name:       "w1",
		AutoResume: true,
		ConvOpts:   []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
	}, fe)
	c.NoError(err)
	eng.Start()
	fe.handler = eng

	select {
	case <-sender.started:
	case <-time.After(10 * time.Second):
		t.Fatalf("startup resume never reached the model; out:\n%s", out.String())
	}

	eng.HandleAbort()

	deadline := time.Now().Add(10 * time.Second)
	ended := false
	for !ended && !time.Now().After(deadline) {
		ended = frameSeqHasAfter(frameTypes(t, out.String()), "agent_start", "agent_end")
		time.Sleep(5 * time.Millisecond)
	}
	c.Require().True(ended, "an abort during startup resume must end the resumed turn; out:\n%s", out.String())

	eng.HandlePrompt("still there?")
	eng.Wait()
	c.True(frameSeqHas(frameTypes(t, out.String()), "agent_settled"),
		"the child must keep working after an aborted startup resume")
	eng.Close()
}

func frameSeqHas(seq []string, typ string) bool {
	for _, t := range seq {
		if t == typ {
			return true
		}
	}
	return false
}

func frameSeqHasAfter(seq []string, last, typ string) bool {
	seen := false
	for _, t := range seq {
		if seen && t == typ {
			return true
		}
		if t == last {
			seen = true
		}
	}
	return false
}
