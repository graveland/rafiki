package fundi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// stallWriter is an io.Writer whose Write blocks forever once stall() is
// called — the daemon that has stopped reading a child's stdout. Writes before
// that are discarded.
type stallWriter struct {
	mu          sync.Mutex
	stalling    bool
	released    chan struct{}
	releaseOnce sync.Once
}

func newStallWriter() *stallWriter {
	return &stallWriter{released: make(chan struct{})}
}

func (w *stallWriter) stall() {
	w.mu.Lock()
	w.stalling = true
	w.mu.Unlock()
}

// release is idempotent so a test can both defer it and call it explicitly.
func (w *stallWriter) release() { w.releaseOnce.Do(func() { close(w.released) }) }

func (w *stallWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	stalling := w.stalling
	w.mu.Unlock()
	if stalling {
		<-w.released
	}
	return len(p), nil
}

// panickingSender stalls out the Frontend's writer and then panics, on the
// engine's turn worker goroutine (conv.Continue calls the Sender inline, and
// agentloop does not recover). That is the one path that reaches Engine.fatal.
type panickingSender struct{ w *stallWriter }

func (s panickingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.w.stall()
	panic("scripted panic on the turn worker")
}

// TestFatalDoesNotBlockOnAStalledReader guards the ordering inside
// Engine.fatal.
//
// fatal()'s entire job is to END this child. It used to do an unbounded
// e.fe.Emit of the parting agent_error first — and Frontend.Emit holds its
// mutex across the stdout write, so a reader that has stopped consuming (the
// daemon hit a frame-too-large error, or is gone) blocks it with no error and
// no timeout. The child then wedged inside its own death path until something
// external killed it, which is the reverse of safe.
//
// The emit is now bounded (fatalEmitTimeout) and OnFatal is called regardless.
// Note what this does NOT do: it does not reorder them unconditionally. A plain
// reversal loses the frame in the ordinary case, because OnFatal's teardown
// closes stdout in microseconds and wins the race — see
// TestRunnerPanicInTurnWorkerEndsTheChild in internal/inproc, which requires
// the daemon to actually receive it, and which a plain reversal fails.
func TestFatalDoesNotBlockOnAStalledReader(t *testing.T) {
	silenceSlog(t)
	w := newStallWriter()
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, panickingSender{w: w}),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}

	fatalCalled := make(chan error, 1)
	fe := NewFrontend(nil, w, nil)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		Tools:    fakeToolSet{},
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
		OnFatal:  func(err error) { fatalCalled <- err },
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.release)

	eng.HandlePrompt("go")

	// fatalEmitTimeout (2s) plus generous slack. Without the bound this never
	// fires at all: fatal() is parked in Emit and OnFatal is never reached.
	select {
	case ferr := <-fatalCalled:
		if ferr == nil {
			t.Error("OnFatal was called with a nil error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("OnFatal was never called: fatal() is blocked emitting agent_error to a reader " +
			"that stopped consuming, inside the path whose only job is to end this child")
	}

	// The engine really is dead, not merely slow: further prompts are rejected
	// rather than queued, and Wait() returns (fatal released every outstanding
	// count before it got as far as the emit).
	eng.HandlePrompt("ignored")
	done := make(chan struct{})
	go func() { defer close(done); eng.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait() did not return after a fatal; a wg count was left outstanding")
	}

	// Let the abandoned emit goroutine finish rather than leaving it wedged for
	// the rest of the test binary's life — the daemon's equivalent is OnFatal's
	// teardown closing stdout, which makes that blocked write fail. Nothing to
	// assert beyond -race staying quiet, which is why this is a release and not
	// an assertion. release is idempotent, so the deferred one above is fine.
	w.release()
}

// TestFatalEmitsTheAgentErrorWhenTheReaderIsAlive is the other half: bounding
// the emit must not turn the parting diagnostic into a coin flip. When stdout
// is being consumed normally, the frame goes out — and it goes out BEFORE
// OnFatal runs, so an owner that tears the child down on that callback cannot
// truncate it.
func TestFatalEmitsTheAgentErrorWhenTheReaderIsAlive(t *testing.T) {
	silenceSlog(t)
	out := &syncBuffer{}
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, panickingSender{w: newStallWriter()}),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	// The sender above stalls its OWN throwaway writer, not this one, so the
	// Frontend's writes here never block.
	fe := NewFrontend(nil, out, nil)

	seenAtFatal := make(chan string, 1)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		Tools:    fakeToolSet{},
		Provider: "anthropic",
		ModelID:  "claude-x",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
		// Snapshot what has been emitted at the moment the owner is told to end
		// the child: the agent_error must already be there.
		OnFatal: func(error) { seenAtFatal <- out.String() },
	}, fe)
	if err != nil {
		t.Fatal(err)
	}

	eng.HandlePrompt("go")
	var snapshot string
	select {
	case snapshot = <-seenAtFatal:
	case <-time.After(20 * time.Second):
		t.Fatal("OnFatal was never called")
	}
	eng.Wait()

	types := frameTypes(t, snapshot)
	if countFrames(types, "agent_error") != 1 {
		t.Errorf("no agent_error frame on the wire by the time OnFatal ran; the owner's teardown "+
			"can now truncate the only explanation of why the child died: %v", types)
	}
}
