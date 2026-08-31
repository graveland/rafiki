package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/routing"
)

// repairTimeout bounds the post-abort orphan-repair call (see runTurn) — it
// runs on its own context independent of the turn's cancelled context and
// independent of the engine's baseCtx, so it isn't cut short by whatever
// cancelled the turn in the first place.
const repairTimeout = 10 * time.Second

// fatalEmitTimeout bounds how long fatal() will wait for its parting
// agent_error frame to reach stdout before ending the child regardless. A
// reader that is consuming normally takes microseconds; this only ever expires
// when nothing is reading at all, in which case the frame was never going to
// arrive and ending the child is what matters. See fatal().
const fatalEmitTimeout = 2 * time.Second

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

	// AutoResume asks the engine to call agentloop.Resume on its
	// conversation before accepting any inbound prompts. The daemon sets
	// this on boot-time auto-recovery of fundi children whose persisted
	// LastStatus indicates they were alive when the daemon went down.
	// agentloop.Resume handles every recoverable state (clean end_turn,
	// dangling tool_use, truncated max_tokens) and reports failure only
	// when resume is impossible (cap exceeded, empty history).
	AutoResume bool

	// OnFatal is called at most once, from the turn worker, when the engine
	// has hit something it cannot continue past — today that means a panic
	// escaped a turn. The engine has already stopped accepting work and
	// released every outstanding Wait() count by the time it fires, so the
	// owner's job is only to END THE CHILD: record a non-zero exit and unblock
	// whatever is reading the child's stdin so its stdout closes and the
	// daemon sees an ordinary EOF (inproc.Runner does exactly this).
	//
	// A nil OnFatal is legal and means "log it and stop taking turns", which
	// is all a standalone `rafikid fundi` process can do; the in-process daemon
	// path always supplies one, because a silently-stopped queue there is a
	// wedged child that answers prompts forever and never runs one.
	OnFatal func(error)

	// NativeSink receives this engine's rafiki-native events. Optional.
	NativeSink NativeSink

	// OnConsumed reports that inbox frames have entered a turn and may be
	// retired. Nil is legal: a standalone agent has no inbox behind it.
	//
	// It is called SYNCHRONOUSLY, on the turn worker, immediately before the
	// message runs. On the daemon side that is one UPDATE; running it on a
	// goroutine would let the turn start before its message is confirmed,
	// reopening the very window this exists to close.
	OnConsumed func(ids []string)
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
	conv       *llm.Conversation
	client     *llm.Client
	tools      agentloop.ToolSet
	fe         *Frontend
	em         *Emitter
	state      StateData
	autoResume bool
	// baseCtx is the engine-lifetime root every turn's cancellable context
	// derives from — the single seam for wiring process shutdown (a
	// signal.NotifyContext parent) into in-flight turns.
	baseCtx context.Context
	onFatal func(error)
	// onConsumed reports that inbox frames have entered a turn and may be
	// retired. Nil is legal: a standalone agent has no inbox behind it.
	onConsumed func(ids []string)

	mu       sync.Mutex
	pending  []queued           // FIFO of queued prompts awaiting execution
	cancel   context.CancelFunc // non-nil while a turn is running
	steerBuf []queued           // steers accepted during the running turn
	dead     bool               // set by fatal(); the worker is gone, no more turns will run

	// wake carries at most one pending "queue is non-empty" signal. Sends are
	// non-blocking: a dropped send means a signal is already buffered, so the
	// worker is guaranteed at least one more pass over the queue.
	wake chan struct{}
	wg   sync.WaitGroup // one count per queued-but-unfinished turn
}

// queued is one entry in the turn queue: the text, and the inbox frame ids it
// discharges when it enters a turn. ids is a slice rather than a single id
// because orphaned steers are rejoined into ONE requeued prompt (see runTurn's
// tail), and every id in that join must be retired when the joined text
// finally runs.
type queued struct {
	ids  []string
	text string
}

// idSlice wraps a frame id for a queued entry. An empty id means "no inbox row
// behind this message" — a standalone agent, or a test — and must produce no
// ids at all rather than one empty string, so consume has nothing to report.
func idSlice(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

// toolSetWithConvID wraps an agentloop.ToolSet to inject the conversation ID
// into the context before every tool execution. Tools that need per-conversation
// persistence (the task_* tools) read it via tools.ConversationIDFromContext.
type toolSetWithConvID struct {
	agentloop.ToolSet
	convID string
}

func (t toolSetWithConvID) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	ctx = context.WithValue(ctx, tools.ConversationIDKey{}, t.convID)
	return t.ToolSet.Execute(ctx, name, input)
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

	// Wrap the tool set so every tool execution receives the conversation ID
	// in its context — tools that need per-conversation persistence (the task_* tools)
	// read it from there.
	tools := toolSetWithConvID{ToolSet: cfg.Tools, convID: conv.ID}

	e := &Engine{
		conv:       conv,
		client:     cfg.Client,
		tools:      tools,
		fe:         fe,
		em:         NewEmitter(fe, cfg.Provider, pricerFor(cfg.Client)),
		baseCtx:    baseCtx,
		onFatal:    cfg.OnFatal,
		onConsumed: cfg.OnConsumed,
		state: StateData{
			SessionID:   conv.ID,
			SessionName: cfg.Name,
			ModelID:     cfg.ModelID,
			Provider:    cfg.Provider,
		},
		wake: make(chan struct{}, 1),
	}
	if cfg.NativeSink != nil {
		e.em.SetNativeSink(cfg.NativeSink)
	}
	// fe's Handler is wired here rather than by the caller: Engine and
	// Frontend are the same package, but BuildEngine (cmd/rafikid's entry
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
	//
	// It runs on its own goroutine, which nothing else covers, and its body is
	// an HTTP GET plus a JSON decode of a third party's response — so it gets
	// its own recover. Swallowing the panic with a log is correct HERE and
	// nowhere else in this file: warming is already best-effort by
	// construction (a failure costs a turn's cost figure, nothing more), so
	// there is no child-ending decision to make.
	if cat := catalogOf(cfg.Client); cat != nil {
		go func() {
			defer func() {
				if v := recover(); v != nil {
					slog.Error("agent: model catalog warm panicked; pricing falls back to zero-cost",
						"panic", v, "stack", string(debug.Stack()))
				}
			}()
			cat.Warm()
		}()
	}
	slog.Info("agent: engine started", "conversation", conv.ID,
		"provider", upstreamLabel(cfg.Provider), "model", fullModel(cfg.Provider, cfg.ModelID))
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

// HandlePrompt queues text as a turn with no inbox id behind it.
func (e *Engine) HandlePrompt(text string) { e.HandlePromptID("", text) }

// HandleSteer buffers text for mid-turn injection with no inbox id behind it.
func (e *Engine) HandleSteer(text string) { e.HandleSteerID("", text) }

// HandlePromptID queues text as a turn and returns immediately — the Handler
// contract. Queued turns run in order, one at a time.
func (e *Engine) HandlePromptID(id, text string) {
	e.enqueue(queued{ids: idSlice(id), text: text})
}

// enqueue appends q to the turn queue and wakes the worker. It is the only
// writer to pending, shared by the inbound prompt path and runTurn's
// orphaned-steer requeue.
//
// An entry arriving after fatal() is dropped rather than queued. The
// alternative is worse than it looks: wg.Add on a dead worker is a count
// nothing will ever Done, so the next Wait() (the owner's shutdown path)
// would block forever. The Add is inside the lock for exactly that reason —
// it has to be atomic with the dead check. A dropped entry is deliberately
// NOT acked: it never entered a turn, and leaving its rows unconfirmed is what
// lets a restart deliver them to the replacement child.
func (e *Engine) enqueue(q queued) {
	e.mu.Lock()
	if e.dead {
		e.mu.Unlock()
		slog.Warn("agent: prompt dropped; the turn worker is no longer running",
			"conversation", e.conv.ID)
		return
	}
	e.wg.Add(1)
	e.pending = append(e.pending, q)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default: // a wakeup is already pending; the worker will see this entry
	}
}

// HandleSteerID buffers text for mid-turn injection when a turn is running,
// and otherwise treats it as a plain prompt.
func (e *Engine) HandleSteerID(id, text string) {
	e.mu.Lock()
	if e.cancel != nil {
		e.steerBuf = append(e.steerBuf, queued{ids: idSlice(id), text: text})
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	e.HandlePromptID(id, text)
}

// consume reports that ids have entered a turn and their inbox rows may be
// retired.
//
// Synchronous on purpose: it is one UPDATE, and running it on a goroutine
// would let a turn start before its message is confirmed, reopening the window
// this whole mechanism exists to close.
func (e *Engine) consume(ids []string) {
	if e.onConsumed == nil || len(ids) == 0 {
		return
	}
	e.onConsumed(ids)
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
// again — in cmd/rafikid's shutdown path that means: stop reading frames
// (Frontend.Run has already returned), then Wait(), then Close(). Sending on
// wake after Close (i.e. a HandlePrompt racing a Close) would panic on a
// closed channel; the ordering above is what rules that race out.
func (e *Engine) Close() { close(e.wake) }

// worker drains the prompt queue serially for the engine's lifetime, and
// stops for good the first time a turn panics. When AutoResume is set, it
// calls agentloop.Resume on startup to finalise any incomplete previous turn
// (dangling tool_use, truncated max_tokens) before accepting inbound prompts.
func (e *Engine) worker() {
	if e.autoResume {
		e.startupResume()
	}
	for range e.wake {
		for {
			e.mu.Lock()
			if len(e.pending) == 0 {
				e.mu.Unlock()
				break
			}
			q := e.pending[0]
			e.pending = e.pending[1:]
			e.mu.Unlock()

			// Ack HERE, not after the turn: a turn can run for minutes and
			// replaying a prompt the model already worked on duplicates the
			// work.
			//
			// Two residual exposures, both accepted, and neither of them
			// microseconds — do not repeat that claim:
			//
			// 1. A crash between this ack and agentloop.Run's AppendUser loses
			//    the prompt. The gap is NOT small. runTurn does two slow things
			//    first: e.em.UserMessage, a stdout write that Frontend.Emit
			//    holds a mutex across and which blocks indefinitely on a reader
			//    that has stopped consuming (see fatal()'s own comment on
			//    exactly that hazard); and e.events(), whose
			//    priorConversationCost is a DATABASE rollup with a 3-second
			//    timeout. Call it seconds. Still far better than the
			//    turn-length window acking-on-write leaves open, which is what
			//    this ack point buys.
			//
			// 2. agentloop.Run RETRACTS the user row it wrote when the first
			//    send fails llm.IsPromptTooLarge, so an oversized prompt ends
			//    up acked in the inbox AND erased from the conversation,
			//    leaving only an agent_error frame as its record. This is the
			//    one deterministic — not crash-window — path where an acked
			//    message keeps no durable copy of its content, and it is
			//    DELIBERATE. Do not "fix" it by treating IsPromptTooLarge as
			//    "never entered a turn": an oversized prompt is
			//    deterministically oversized, so leaving it unacked makes it a
			//    poison pill that redelivers on every restart, forever. One
			//    loud failure beats an infinite loop.
			e.consume(q.ids)

			if !e.runTurnGuarded(q.text) {
				return // fatal() has already been called; this child is ending
			}
		}
	}
}

// startupResume calls agentloop.Resume on the conversation once, before any
// inbound prompt arrives. Called by worker when EngineConfig.AutoResume is set.
//
// agentloop.Resume handles every recoverable state: a clean end_turn returns
// immediately; a dangling tool_use fabricates synthetic is_error results and
// Continues; a truncated max_tokens Continues. Failure (cap exceeded, empty
// history) emits an agent_error frame and ends the child via fatal.
func (e *Engine) startupResume() {
	e.em.AgentStart()
	events, streamOpt := e.events()
	result, err := agentloop.Resume(context.Background(), e.conv, e.tools, events, streamOpt)
	switch {
	case err != nil:
		slog.Error("agent: startup resume failed; ending this child",
			"conversation", e.conv.ID, "error", err)
		e.fe.Emit(map[string]any{"type": "agent_error", "error": err.Error()})
		e.fatal(err)
	case result.LimitReached:
		slog.Info("agent: startup resume wrapped up by guardrail",
			"conversation", e.conv.ID, "reason", result.LimitReason)
		e.em.AgentEnd()
	default:
		slog.Info("agent: startup resume completed",
			"conversation", e.conv.ID)
		e.em.AgentEnd()
	}
}

// runTurnGuarded runs one turn and reports whether the worker may continue.
//
// The recover lives here rather than in a top-level defer on the worker
// goroutine so the WaitGroup accounting stays exact: wg.Done() for THIS turn
// runs unconditionally, panic or not, and fatal() below then releases exactly
// the turns still queued. A recover on the goroutine as a whole would have to
// guess how many counts were outstanding, and guessing high trips "sync:
// negative WaitGroup counter" — a second, worse panic on the recovery path.
//
// A turn panic ends the child rather than being swallowed and retried. The
// panic can land anywhere in agentloop.Run — mid-stream inside
// acc.Accumulate/MapAssistantMessage, or between a tool_use being emitted and
// its tool_result being appended — so the conversation may now carry a
// dangling tool_use that the API rejects outright on the next request. A child
// that keeps accepting prompts and fails every one of them is worse than a
// child that exits and can be resumed.
func (e *Engine) runTurnGuarded(text string) (ok bool) {
	defer func() {
		// Unconditional, and deliberately before the recover: this turn is
		// over either way, and nothing else will ever call Done for it.
		e.wg.Done()
		if v := recover(); v != nil {
			slog.Error("agent: turn panicked; ending this conversation",
				"conversation", e.conv.ID, "panic", v, "stack", string(debug.Stack()))
			e.fatal(fmt.Errorf("agent turn panicked: %v", v))
			ok = false
		}
	}()
	e.runTurn(text)
	return true
}

// fatal marks the engine dead, releases every outstanding Wait() count, and
// hands the failure to the owner so the CHILD ends — not just the queue.
//
// Stopping the queue on its own would wedge the child: the Frontend keeps
// reading frames and answering get_state, so the daemon still sees a healthy
// idle child, and every prompt from that point on is accepted and never run.
// The owner's OnFatal is what turns this into an ordinary child exit.
func (e *Engine) fatal(err error) {
	e.mu.Lock()
	if e.dead { // fatal is once-only; the worker calls it and then returns
		e.mu.Unlock()
		return
	}
	e.dead = true
	// The panicked turn may have died before runTurn's own cancel(), leaving
	// its context registered on baseCtx forever. Release it here.
	cancel := e.cancel
	e.cancel = nil
	// Queued-but-unstarted turns each hold a wg count that no worker will ever
	// retire, and HandlePrompt refuses to add more now that dead is set — so
	// this is the complete outstanding set, and Wait() can return.
	// Named outstanding, not queued: queued is a package type (the turn-queue
	// entry) and shadowing it here is a trap for the next edit.
	outstanding := len(e.pending)
	e.pending = nil
	e.steerBuf = nil
	onFatal := e.onFatal
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for range outstanding {
		e.wg.Done()
	}

	// Emit the parting agent_error on a BOUNDED wait, then end the child.
	//
	// The hazard: Frontend.Emit holds its mutex across the stdout write, so if
	// the reader has stopped consuming — the daemon hit a frame-too-large error,
	// or is simply gone — it blocks with no error and no timeout. A plain
	// `e.fe.Emit(...)` here therefore blocked inside the very path whose only
	// job is to END this child, and nothing would unblock it short of an
	// external Kill. Narrow in practice (64 KB of kernel buffer plus a
	// continuously-reading daemon) but exactly the wrong way round.
	//
	// Why this shape and not a plain reversal (onFatal first, emit after): the
	// frame would then usually be LOST, not merely delayed. OnFatal unblocks
	// Frontend.Run, and run()'s remaining path — Wait, Close, shutdown, and the
	// stdout close — is microseconds, so it routinely wins the race against this
	// write. Measured, not assumed: reversing alone fails
	// TestRunnerPanicInTurnWorkerEndsTheChild, which requires the daemon to
	// receive the one frame that explains why the child died.
	//
	// So: emit off this goroutine, WAIT for it (which keeps delivery
	// deterministic and the frame strictly ahead of the child's stdout close),
	// but give up after fatalEmitTimeout and end the child anyway. The
	// abandoned emit goroutine cannot outlive the child: OnFatal's teardown
	// closes stdout, and the blocked write then fails. Preferred over a write
	// deadline, which is not reachable through the io.Writer a Frontend holds.
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		e.fe.Emit(map[string]any{"type": "agent_error", "error": err.Error()})
	}()
	select {
	case <-emitted:
	case <-time.After(fatalEmitTimeout):
		slog.Error("agent: could not emit agent_error before ending the child; "+
			"nothing is reading this child's stdout",
			"conversation", e.conv.ID, "waited", fatalEmitTimeout, "error", err)
	}

	if onFatal == nil {
		slog.Error("agent: engine is no longer running turns and has no OnFatal owner to end the child",
			"conversation", e.conv.ID, "error", err)
		return
	}
	onFatal(err)
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
	result, err := agentloop.Run(ctx, e.conv, e.tools, events, llm.UserText(text), streamOpt)
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
	case result.LimitReached:
		// Not a failure: the model got a forced-text wrap-up call and
		// answered (see agentloop.wrapUp) instead of being cut off
		// mid-loop. Surfaced at Info so an operator (or a future
		// coordinator watching this child) can tell a budget-limited turn
		// apart from an ordinary clean completion.
		slog.Info("agent: turn hit its guardrail and was wrapped up", "conversation", e.conv.ID, "reason", result.LimitReason)
	}
	e.em.AgentEnd()

	// Mirror drainSteers: join orphaned steers into ONE requeued prompt so a
	// multi-line steer batch that missed the last PendingUser poll becomes
	// one extra turn, not one turn per buffered line.
	if len(orphanedSteers) > 0 {
		var texts, ids []string
		for _, q := range orphanedSteers {
			texts = append(texts, q.text)
			ids = append(ids, q.ids...)
		}
		// Requeued UNACKED, carrying every id from the join: this text has not
		// entered a turn yet, and dropping the ids here would strand those
		// inbox rows forever — nothing downstream can retire them once the
		// text has moved on without them. Enqueued directly rather than
		// through HandlePromptID, which carries only one id.
		e.enqueue(queued{ids: ids, text: strings.Join(texts, "\n")})
	}
}

// priorConversationCost fetches the list-price cost of every completed turn
// that precedes the current one, priced per model. It runs detached from the
// turn's cancellable context on a short timeout, so a client that hung up
// mid-stream (cancelling the turn) cannot also take the cost rollup down with
// it. Returns 0 when the client has no capture store (no persistent
// conversation) or the rollup fails — best-effort, exactly like the proxy's
// costFields.
func (e *Engine) priorConversationCost() float64 {
	if e.client == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.baseCtx), 3*time.Second)
	defer cancel()
	return e.client.ConversationCost(ctx, e.conv.ID)
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
	// Fetched once per turn: prior completed turns don't change while THIS
	// turn is in flight, so the conversation-lifetime rollup is constant across
	// the turn's iterations and only the current turn's running cost (tracked
	// in e.em.usage) moves iteration to iteration.
	priorCost := e.priorConversationCost()

	var acc anthropic.Message
	var streamed bool
	var lastFlush time.Time

	handler := func(ev anthropic.MessageStreamEventUnion) {
		llm.FixEmptyToolInput(&ev)
		llm.SanitizeInvalidAccumulatedInput(&acc, ev)
		if err := acc.Accumulate(ev); err != nil {
			slog.Warn("agent: accumulate stream event", "error", err)
			return
		}
		llm.FixAccumulatedEmptyToolInput(&acc, ev)
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
		OnTurn: func(iteration int, resp *anthropic.Message, dur time.Duration, err error) {
			// The fundi-side equivalent of the proxy's per-turn "llm turn"
			// log line (pkg/server/proxy.go) — without it, a fundi child's
			// turns are invisible in the daemon's own logs even though they
			// are captured identically to a proxied turn in the DB. Fields
			// are fundi-flavored (child name, loop iteration) rather than an
			// exact mirror, since a fundi turn is one iteration of a
			// multi-call tool-use loop, not a single proxied request.
			if err != nil {
				slog.Warn("agent: turn", "conversation", e.conv.ID, "name", e.state.SessionName,
					"provider", upstreamLabel(e.state.Provider), "model", fullModel(e.state.Provider, e.state.ModelID), "iteration", iteration,
					"latency", dur.Round(100*time.Millisecond), "cost_total", fmt.Sprintf("%.2f", priorCost+e.em.usage.Cost.Total), "error", err)
			} else {
				// cost_total is the conversation-lifetime spend: every completed
				// prior turn (priorCost, rolled up over the capture store) plus this
				// turn's running cost (prior iterations already folded into
				// e.em.usage, this iteration added here). It mirrors the proxy's
				// "llm turn" cost_total so the two log lines agree, and it does NOT
				// reset at a turn boundary the way a per-turn running total would.
				iterCost := costOf(e.em.pricer, string(resp.Model), resp.Usage)
				conversationTotal := priorCost + e.em.usage.Cost.Total + iterCost.Total
				cachePct := cacheHitPct(resp.Usage)
				slog.Info("agent: turn", "conversation", e.conv.ID, "name", e.state.SessionName,
					"provider", upstreamLabel(e.state.Provider), "model", fullModel(e.state.Provider, e.state.ModelID), "iteration", iteration,
					"upstream_provider", llm.ProviderOf(resp),
					"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
					"cache_read_tokens", resp.Usage.CacheReadInputTokens, "cache_creation_tokens", resp.Usage.CacheCreationInputTokens,
					"cache_pct", cachePct,
					"stop_reason", resp.StopReason, "latency", dur.Round(100*time.Millisecond),
					"cost_total", fmt.Sprintf("%.2f", conversationTotal))
			}
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
			// The pi frames first, then ONE durable native publication for
			// both paths. It lives here rather than inside StreamEnd and
			// AssistantTurn because only one of those two is handed the
			// provider response, and because a completed turn is exactly the
			// boundary the durable event describes — how it was transported
			// is not the event's business. The early return above is why a
			// failed call publishes nothing: there is no assistant message.
			if wasStreamed {
				e.em.StreamEnd(e.em.mapMessage(resp))
			} else {
				e.em.AssistantTurn(resp)
			}
			e.em.publishAssistant(resp)
		},
		// OnToolStart/OnToolEnd are the only callbacks here that do NOT run on
		// the turn goroutine: agentloop invokes them from inside its per-tool
		// errgroup goroutine (one g.Go per tool_use block), which nothing
		// recovers — runTurnGuarded's recover cannot see them. Each therefore
		// gets its own. Recovering INSIDE the closure also keeps agentloop's
		// own emitMu discipline intact: the callback returns normally, so the
		// Unlock on the far side of it still runs.
		//
		// Swallow-with-log is right for these two specifically: they are pure
		// emission (a frame the TUI renders), the tool result itself is
		// unaffected, and the turn is still perfectly able to finish.
		OnToolStart: func(id, name string, input json.RawMessage) {
			defer recoverEmit("OnToolStart", name)
			e.em.ToolStart(id, name, input)
		},
		OnToolEnd: func(id, name, result string, err error) {
			defer recoverEmit("OnToolEnd", name)
			e.em.ToolEnd(id, name, result, err != nil)
		},
		PendingUser: e.drainSteers,
	}
	return ev, llm.WithStreamHandler(handler)
}

// recoverEmit contains a panic raised inside one of the observation callbacks
// agentloop runs on a goroutine of its own (see events). Call it as
// `defer recoverEmit(...)`, never as a bare call.
func recoverEmit(callback, tool string) {
	if v := recover(); v != nil {
		slog.Error("agent: emit callback panicked; the tool result is unaffected",
			"callback", callback, "tool", tool, "panic", v, "stack", string(debug.Stack()))
	}
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
		case b.Type == "thinking" && b.Thinking != "":
			return true
		}
	}
	return false
}

// upstreamLabel maps a model prefix (from splitModel) to the upstream provider
// name used in log lines. The label is the provider name as-is: the old
// hardcoded mapping (anthropic→anthropic, everything else→openrouter) predates
// the provider registry and stopped being correct once any provider could be
// configured.
func upstreamLabel(modelPrefix string) string {
	return modelPrefix
}

// fullModel reconstructs the provider-qualified model id from the split
// components stored in StateData (e.g. "deepseek" + "deepseek-v4-pro" →
// "deepseek/deepseek-v4-pro"). A bare model with no prefix returns as-is.
func fullModel(prefix, bareID string) string {
	if prefix == "" {
		return bareID
	}
	return prefix + "/" + bareID
}

// cacheHitPct returns the cache-read hit rate for an input as a percentage
// rounded to one decimal place (e.g. 75.3). Returns 0 when there are no
// input tokens.
func cacheHitPct(u anthropic.Usage) float64 {
	total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if total == 0 {
		return 0
	}
	return math.Round(float64(u.CacheReadInputTokens)/float64(total)*1000) / 10
}

// drainSteers is the loop's mid-turn steer seam: it takes everything buffered
// since the last poll, echoes each entry so the TUI renders the user bubble,
// and returns them as one user message.
//
// This is the steer half of the ack point. A steer is retired HERE, as it is
// handed to the running turn — not when its frame arrived, which may be a
// whole tool call earlier, and not at turn end.
func (e *Engine) drainSteers() []anthropic.ContentBlockParamUnion {
	e.mu.Lock()
	entries := e.steerBuf
	e.steerBuf = nil
	e.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	var texts, ids []string
	for _, q := range entries {
		texts = append(texts, q.text)
		ids = append(ids, q.ids...)
	}
	e.consume(ids)
	for _, t := range texts {
		e.em.UserMessage(t)
	}
	return llm.UserText(strings.Join(texts, "\n"))
}
