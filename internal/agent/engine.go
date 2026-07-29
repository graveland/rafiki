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
	"git.graveland.dev/brent/rafiki/routing"
)

// repairTimeout bounds the post-abort orphan-repair call (see runTurn) — it
// runs on its own context independent of the turn's cancelled context and
// independent of the engine's baseCtx, so it isn't cut short by whatever
// cancelled the turn in the first place.
const repairTimeout = 10 * time.Second

// streamFlushInterval is the coalescing cadence for streamed message_update
// frames: at most one flush per interval, regardless of how many SDK stream
// events arrive. Each frame carries the WHOLE accumulated message (see
// events' doc), so emitting one per SDK event makes wire volume O(response²)
// — a 16 KB reply at realistic 15-50 char chunks is 330-1100 frames
// averaging ~8 KB, 2.6-8.8 MB for a single LLM call, and agentloop allows 20
// iterations per turn. 250ms is ~4 frames/second: imperceptible for a
// reading surface, a ~25x reduction in both frame count and bytes. Chosen
// deliberately over a smaller interval — this is not a game that needs
// realtime updates.
const streamFlushInterval = 250 * time.Millisecond

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
	// BaseCtx is the engine-lifetime root every turn's cancellable context
	// derives from — the seam for wiring process shutdown (a
	// signal.NotifyContext parent) into in-flight turns. Nil defaults to
	// context.Background(), matching every pre-Task-14 caller.
	BaseCtx context.Context
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
	baseCtx := cfg.BaseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	conv, err := cfg.Client.Conversation(baseCtx, cfg.ConvOpts...)
	if err != nil {
		return nil, fmt.Errorf("agent: create conversation: %w", err)
	}
	e := &Engine{
		conv:    conv,
		tools:   cfg.Tools,
		fe:      fe,
		em:      NewEmitter(fe, cfg.Provider, pricerFor(cfg.Client)),
		baseCtx: baseCtx,
		state: StateData{
			SessionID:   conv.ID,
			SessionName: cfg.Name,
			ModelID:     cfg.ModelID,
			Provider:    cfg.Provider,
		},
		wake: make(chan struct{}, 1),
	}
	// fe's Handler is wired here rather than by the caller: Engine and
	// Frontend are the same package, but BuildEngine (cmd/fundid's entry
	// point, per the import-direction constraint) constructs fe before the
	// Engine exists and cannot reach Frontend's unexported handler field
	// itself.
	fe.handler = e
	go e.worker()
	// Warm the model catalog off the hot path. Pricing a turn resolves the
	// served model through the catalog, and a cold catalog resolves it with a
	// SYNCHRONOUS OpenRouter fetch — that would land inside the first
	// AssistantTurn's emit, delaying the frame. Best-effort: a failed fetch is
	// logged by the catalog and simply leaves that turn's cost at 0.
	if cat := catalogOf(cfg.Client); cat != nil {
		go cat.Warm()
	}
	slog.Info("agent: engine started", "conversation", conv.ID, "provider", cfg.Provider, "model", cfg.ModelID)
	return e, nil
}

// catalogOf returns c's model catalog, or nil when there is no client. Split
// from pricerFor so the caller can both warm the catalog and price through it
// without reaching into the client twice.
func catalogOf(c *llm.Client) *routing.ModelCatalog {
	if c == nil {
		return nil
	}
	return c.Catalog()
}

// pricerFor derives the turn pricer from the client's model catalog.
// llm.NewClient always defaults a catalog in when the caller supplies none, so
// this is non-nil in practice; it stays nil-tolerant because tests construct
// Engines with hand-built Configs.
func pricerFor(c *llm.Client) Pricer {
	cat := catalogOf(c)
	if cat == nil {
		return nil
	}
	return cat.Pricing
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

// HandleAbort cancels the running turn's context, if any. An abort arriving
// with nothing in flight (queued-but-unstarted prompts, or the narrow window
// in runTurn before cancel is published) is logged rather than silently
// dropped — in-band abort is this project's entire reason for existing, so a
// no-op abort must still leave a trace.
func (e *Engine) HandleAbort() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel == nil {
		slog.Info("agent: abort requested with no turn running", "conversation", e.conv.ID)
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

// Close stops the engine's worker goroutine. Call it only after Wait() has
// returned and after nothing can call HandlePrompt/HandleSteer/HandleAbort
// again — in cmd/fundid's shutdown path that means: stop reading frames
// (Frontend.Run has already returned), then Wait(), then Close(). Sending on
// wake after Close (i.e. a HandlePrompt racing a Close) would panic on a
// closed channel; the ordering above is what rules that race out.
func (e *Engine) Close() { close(e.wake) }

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
	// Publish cancel BEFORE the Emit calls below: those write JSON to stdout
	// and can block on a slow reader, and an abort landing in that window
	// used to find cancel still nil and vanish silently (see HandleAbort).
	// Frame order on the wire is unchanged — user echo still precedes
	// agent_start — only the cancel publication moved earlier.
	ctx, cancel := context.WithCancel(e.baseCtx)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()

	e.em.UserMessage(text)
	e.em.AgentStart()

	events, streamOpt := e.events()
	_, err := agentloop.Run(ctx, e.conv, e.tools, events, llm.UserText(text), streamOpt)
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
		// A cancelled turn can leave the trailing assistant message's tool_use
		// blocks unresolved (no tool_result yet appended); the next turn's
		// request would then carry that dangling tool_use and the API would
		// reject it outright. Repair on its own short-lived context, NOT
		// e.baseCtx and not the turn's own (already cancelled) ctx — the
		// repair itself must not be aborted by the very cancellation it
		// exists to clean up after. This matters even more now that baseCtx
		// can be a signal.NotifyContext: a SIGINT-triggered abort would
		// otherwise cancel baseCtx and the repair together, leaving the
		// orphan behind for the process's next start.
		repairCtx, repairCancel := context.WithTimeout(context.Background(), repairTimeout)
		repaired, rErr := RepairOrphans(repairCtx, e.conv)
		repairCancel()
		if rErr != nil {
			slog.Error("agent: orphan repair failed after abort", "conversation", e.conv.ID, "error", rErr)
		}
		slog.Info("agent: turn cancelled", "conversation", e.conv.ID, "error", err, "orphans_repaired", repaired)
	case err != nil:
		slog.Error("agent: turn failed", "conversation", e.conv.ID, "error", err)
		e.fe.Emit(map[string]any{"type": "agent_error", "error": err.Error()})
	}
	e.em.AgentEnd()

	// Mirror drainSteers: join orphaned steers into ONE requeued prompt so a
	// multi-line steer batch that missed the last PendingUser poll becomes
	// one extra turn, not one turn per buffered line.
	if len(orphanedSteers) > 0 {
		e.HandlePrompt(strings.Join(orphanedSteers, "\n"))
	}
}

// events wires the agent loop's observation callbacks to the pi emitter, and
// returns the llm.SendOption that must ride along on every conv.Continue call
// of THIS turn so the loop actually streams.
//
// The stream handler and OnTurn are built together, sharing one accumulator,
// because of how agentloop.drive is structured: it builds its sendOpts ONCE
// (llm.WithTools(defs) plus whatever this method returns) and reuses that
// same slice — and therefore this same handler closure — for every iteration
// of the turn's tool-use loop (drive calls conv.Continue once per iteration,
// stopping only at end_turn). Both stream events and OnTurn fire on the
// loop's own goroutine, and the handler for iteration N is always fully done
// (conv.Continue has returned) before OnTurn(N) runs and before
// conv.Continue(N+1) can start — so it's safe for OnTurn to consume and reset
// the closure's accumulator with no locking, but the reset is mandatory:
// without it, iteration N+1's deltas would accumulate onto iteration N's
// stale content.
//
// OnTurn also has to decide, per iteration, whether anything actually
// streamed: a Sender that doesn't implement llm.StreamingSender (every
// fake-turn child, and any pre-delivery failover handoff) makes the handler
// never fire at all. Unconditionally calling StreamEnd there would emit a
// message_end with no preceding message_start — so streamed tracks whether
// the handler ran (past the hasContent gate) THIS iteration, and OnTurn
// falls back to the plain AssistantTurn triple when it didn't.
func (e *Engine) events() (*agentloop.Events, llm.SendOption) {
	var acc anthropic.Message
	var streamed bool
	var lastFlush time.Time

	handler := func(ev anthropic.MessageStreamEventUnion) {
		if err := acc.Accumulate(ev); err != nil {
			slog.Warn("agent: accumulate stream event", "error", err)
			return
		}
		// Emit only once content actually exists: a trim-retry fails before
		// any content event, so this keeps message_start off the wire until
		// the attempt is real. See hasContent's doc.
		if !hasContent(&acc) {
			return
		}
		streamed = true
		// Coalesce: one frame per streamFlushInterval, not one per SDK event.
		// Each frame carries the whole accumulated message, so emitting per
		// event makes wire volume O(response^2) — thousands of multi-KB
		// frames per turn, which saturates the per-subscriber bus buffer and
		// evicts the ring (see streamFlushInterval's doc). The handler is
		// synchronous on the send goroutine (rafiki guarantees it is never
		// called concurrently and never after Send returns), so comparing
		// against lastFlush inline needs no lock, timer, or goroutine.
		// lastFlush is zero at the start of every iteration (reset in OnTurn
		// below), so the first content-bearing event of a turn always
		// flushes immediately regardless of the interval — otherwise a short
		// turn could emit nothing before StreamEnd. The final partial is
		// never lost to this gate: StreamEnd (from OnTurn) always emits the
		// fully accumulated message regardless of when the last flush here
		// landed, so there is no need (and no correctness reason) to force a
		// flush at end-of-stream from inside this handler.
		now := time.Now()
		if !lastFlush.IsZero() && now.Sub(lastFlush) < streamFlushInterval {
			return
		}
		lastFlush = now
		msg := e.em.mapMessage(&acc)
		e.em.StreamStart(msg)
		e.em.StreamDelta(msg)
	}

	ev := &agentloop.Events{
		OnTurn: func(resp *anthropic.Message, _ time.Duration, err error) {
			// Reset for the next iteration regardless of outcome, BEFORE any
			// early return below, so a failed or non-streamed iteration never
			// leaks state into the next conv.Continue call. lastFlush resets
			// alongside acc/streamed for the same reason: agentloop runs
			// Continue once per tool-calling iteration, reusing this same
			// closure (see this method's doc), so each iteration must start
			// its own coalescing window rather than inheriting the previous
			// iteration's lastFlush and suppressing its own first flush.
			wasStreamed := streamed
			acc, streamed, lastFlush = anthropic.Message{}, false, time.Time{}
			if err != nil {
				return // a failed call has no assistant message to render
			}
			if wasStreamed {
				e.em.StreamEnd(e.em.mapMessage(resp))
				return
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
	return ev, llm.WithStreamHandler(handler)
}

// hasContent reports whether any content block has arrived yet. Guards the
// first emission: sendWithTrim can retry a prompt-too-large failure, and that
// failure lands before any content event, so gating on this keeps an
// abandoned attempt from putting a message_start (and therefore text) into an
// attached TUI. See spec §0.2.
func hasContent(m *anthropic.Message) bool {
	for _, b := range m.Content {
		switch {
		case b.Type == "text" && b.Text != "":
			return true
		case b.Type == "tool_use":
			return true
		}
	}
	return false
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
