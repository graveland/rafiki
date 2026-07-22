package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/rafiki/agentloop"
	"git.graveland.dev/brent/rafiki/llm"
)

// EngineConfig is the wiring for one agent child: the rafiki client and the
// conversation options that identify its conversation, the tools it may call,
// and the identity it reports through get_state.
type EngineConfig struct {
	Client   *llm.Client
	ConvOpts []llm.ConvOption
	Tools    agentloop.ToolSet
	Provider string
	ModelID  string
	Name     string
}

// Engine is the agent runtime: it turns inbound prompt/steer/abort frames into
// a driven rafiki agent loop and emits pi protocol frames throughout. It
// implements Handler.
//
// Turns are serialized: prompts are queued FIFO and executed one at a time on
// a dedicated worker goroutine, so the Frontend's reader loop is never blocked
// (see Handler — a blocking HandlePrompt would make in-band abort impossible).
// A steer arriving during a turn is buffered and injected mid-turn through the
// loop's PendingUser seam; a steer arriving while idle is just a prompt. An
// abort cancels the running turn's context.
type Engine struct {
	conv  *llm.Conversation
	tools agentloop.ToolSet
	fe    *Frontend
	em    *Emitter
	state StateData
	// baseCtx is the engine-lifetime root every turn's cancellable context
	// derives from — the single seam for wiring process shutdown (a
	// signal.NotifyContext parent) into in-flight turns.
	baseCtx context.Context

	mu       sync.Mutex
	pending  []string           // FIFO of queued prompts awaiting execution
	cancel   context.CancelFunc // non-nil while a turn is running
	steerBuf []string           // steers accepted during the running turn

	// wake carries at most one pending "queue is non-empty" signal. Sends are
	// non-blocking: a dropped send means a signal is already buffered, so the
	// worker is guaranteed at least one more pass over the queue.
	wake chan struct{}
	wg   sync.WaitGroup // one count per queued-but-unfinished turn
}

// NewEngine creates the engine's conversation and starts its turn worker.
func NewEngine(cfg EngineConfig, fe *Frontend) (*Engine, error) {
	if cfg.Client == nil {
		return nil, errors.New("agent: EngineConfig.Client is required")
	}
	if cfg.Tools == nil {
		return nil, errors.New("agent: EngineConfig.Tools is required")
	}
	if fe == nil {
		return nil, errors.New("agent: a Frontend is required")
	}
	baseCtx := context.Background()
	conv, err := cfg.Client.Conversation(baseCtx, cfg.ConvOpts...)
	if err != nil {
		return nil, fmt.Errorf("agent: create conversation: %w", err)
	}
	e := &Engine{
		conv:    conv,
		tools:   cfg.Tools,
		fe:      fe,
		em:      NewEmitter(fe, cfg.Provider, cfg.ModelID),
		baseCtx: baseCtx,
		state: StateData{
			SessionID:   conv.ID,
			SessionName: cfg.Name,
			ModelID:     cfg.ModelID,
			Provider:    cfg.Provider,
		},
		wake: make(chan struct{}, 1),
	}
	go e.worker()
	slog.Info("agent: engine started", "conversation", conv.ID, "provider", cfg.Provider, "model", cfg.ModelID)
	return e, nil
}

// HandlePrompt queues text as a turn and returns immediately — the Handler
// contract. Queued turns run in order, one at a time.
func (e *Engine) HandlePrompt(text string) {
	e.wg.Add(1)
	e.mu.Lock()
	e.pending = append(e.pending, text)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default: // a wakeup is already pending; the worker will see this entry
	}
}

// HandleSteer buffers text for mid-turn injection when a turn is running, and
// otherwise treats it as a plain prompt.
func (e *Engine) HandleSteer(text string) {
	e.mu.Lock()
	if e.cancel != nil {
		e.steerBuf = append(e.steerBuf, text)
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.HandlePrompt(text)
}

// HandleAbort cancels the running turn's context, if any.
func (e *Engine) HandleAbort() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	slog.Info("agent: turn abort requested", "conversation", e.conv.ID)
	cancel()
}

// State reports the identity the daemon sniffs out of the get_state response.
func (e *Engine) State() StateData { return e.state }

// Wait blocks until every queued turn has finished. Intended for tests and
// shutdown; it does not stop new work from being queued.
func (e *Engine) Wait() { e.wg.Wait() }

// worker drains the prompt queue serially for the engine's lifetime.
func (e *Engine) worker() {
	for range e.wake {
		for {
			e.mu.Lock()
			if len(e.pending) == 0 {
				e.mu.Unlock()
				break
			}
			text := e.pending[0]
			e.pending = e.pending[1:]
			e.mu.Unlock()

			e.runTurn(text)
			e.wg.Done()
		}
	}
}

// runTurn drives one prompt through the agent loop, emitting the pi frames
// that bracket it. The user echo precedes agent_start: a pi child echoes the
// user message itself, and the attach TUI renders the bubble before the
// agent's activity indicator.
func (e *Engine) runTurn(text string) {
	e.em.UserMessage(text)
	e.em.AgentStart()

	ctx, cancel := context.WithCancel(e.baseCtx)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()

	_, err := agentloop.Run(ctx, e.conv, e.tools, e.events(), llm.UserText(text))
	// Read the abort signal BEFORE releasing the context: our own cancel() would
	// otherwise make every failure look like an abort.
	aborted := errors.Is(ctx.Err(), context.Canceled)

	e.mu.Lock()
	e.cancel = nil
	// A steer that landed after the loop's last PendingUser poll has nowhere to
	// go in this turn; requeue it as the next prompt rather than dropping it.
	orphanedSteers := e.steerBuf
	e.steerBuf = nil
	e.mu.Unlock()
	cancel()

	switch {
	case err != nil && aborted:
		// TODO(task 10): repair the aborted turn's orphaned tool_use blocks via
		// RepairOrphans so the next turn's request is API-valid.
		slog.Info("agent: turn cancelled", "conversation", e.conv.ID, "error", err)
	case err != nil:
		slog.Error("agent: turn failed", "conversation", e.conv.ID, "error", err)
		e.fe.Emit(map[string]any{"type": "agent_error", "error": err.Error()})
	}
	e.em.AgentEnd()

	for _, s := range orphanedSteers {
		e.HandlePrompt(s)
	}
}

// events wires the agent loop's observation callbacks to the pi emitter. Every
// callback fires on the loop's own goroutine or under its callback mutex, so
// the Emitter (single-goroutine by contract) is never driven concurrently.
func (e *Engine) events() *agentloop.Events {
	return &agentloop.Events{
		OnTurn: func(resp *anthropic.Message, _ time.Duration, err error) {
			if err != nil {
				return // a failed call has no assistant message to render
			}
			e.em.AssistantTurn(resp)
		},
		OnToolStart: func(id, name string, input json.RawMessage) {
			e.em.ToolStart(id, name, input)
		},
		OnToolEnd: func(id, name, result string, err error) {
			e.em.ToolEnd(id, name, result, err != nil)
		},
		PendingUser: e.drainSteers,
	}
}

// drainSteers is the loop's mid-turn steer seam: it takes everything buffered
// since the last poll, echoes each entry so the TUI renders the user bubble,
// and returns them as one user message.
func (e *Engine) drainSteers() []anthropic.ContentBlockParamUnion {
	e.mu.Lock()
	texts := e.steerBuf
	e.steerBuf = nil
	e.mu.Unlock()
	if len(texts) == 0 {
		return nil
	}
	for _, t := range texts {
		e.em.UserMessage(t)
	}
	return llm.UserText(strings.Join(texts, "\n"))
}
