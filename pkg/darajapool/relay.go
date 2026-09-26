// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapb/darajapbconnect"
)

// ErrNoRelayStream means the pool cannot serve the child yet — no daraja
// connection exists for childID.
var ErrNoRelayStream = errors.New("darajapool: no active daraja connection")

// subscriberBuffer is a live subscriber's channel capacity. Beyond it the
// fan drops events for that subscriber (select-default below) rather than
// stall the receive loop — the pre-existing slow-subscriber contract, which
// the replay belt does not change: replay fills a subscriber's channel up
// front and everything after is live, subject to the same bound.
const subscriberBuffer = 64

// relayReplayMax bounds the pool's pre-subscribe replay belt, in events per
// child. One event is at most daraja's stdout chunk (32 KiB, stdoutChunk in
// pkg/daraja), so the worst case is ~8 MiB transiently per child — and the
// belt only holds events broadcast while nobody was subscribed, which in a
// healthy lifecycle is the microseconds between a holder opening and
// Runner.pump's Watch (or, for a script that outran the pump, the whole
// script: a handful of frames). Overflow drops the OLDEST events, never the
// newest: the belt's whole job is to carry the terminal Exited event a
// script's settle needs, and a terminal event is by definition the last one.
const relayReplayMax = 256

// ─── relay holder ──────────────────────────────────────────────────────────────

// relayHolder owns the ONE Relay stream for a child. It runs a receive loop
// and fans outgoing events to any number of subscribers (Watch RPCs).
//
// Send serialises its write direction through stdinMu; the receive loop drives
// all fan-outs from a single goroutine. Invalidating against client identity
// survives executor reconnects: when the pool swaps in a new connection,
// ClientFor returns a new DarajaServiceClient, so the handler compares
// holder.client != pool.ClientFor(childID), tears down the stale holder,
// and opens a fresh one on the current client.
type relayHolder struct {
	childID string
	client  darajapbconnect.DarajaServiceClient
	stream  *connect.BidiStreamForClient[darajapb.RelayRequest, darajapb.RelayResponse] // nil until start() succeeds
	ctx     context.Context
	cancel  context.CancelFunc

	// onDone fires exactly once when the recvLoop exits (either from stream
	// error or context cancellation). Used by the pool to signal that the
	// underlying connection lifecycle has ended.
	onDone func()

	mu     sync.Mutex // protects closing and fanOut
	closed bool       // true after shutdown; no new ops
	fanOut map[chan *fanEvent]struct{}

	// pool back-reference for the replay belt. Nil on standalone test
	// holders, which then have no belt: a subscriber-less broadcast is
	// dropped, the pre-belt behaviour.
	pool *Pool
}

type fanEvent struct {
	resp *darajapb.RelayResponse
	err  error
}

// Response returns the relay response payload, or nil if this event carries an error.
func (e *fanEvent) Response() *darajapb.RelayResponse { return e.resp }

// Err returns the stream error, or nil on success.
func (e *fanEvent) Err() error { return e.err }

func newRelayHolder(childID string, cli darajapbconnect.DarajaServiceClient, pool *Pool) *relayHolder {
	ctx, cancel := context.WithCancel(context.Background())
	return &relayHolder{
		childID: childID,
		client:  cli,
		ctx:     ctx,
		cancel:  cancel,
		fanOut:  make(map[chan *fanEvent]struct{}),
		pool:    pool,
		closed:  false,
	}
}

// newRelayHolderWithCtx creates a holder whose ctx/cancel are supplied by the
// caller. Used by pool.handleConn so teardown cancels the holder's context.
// onDone can be nil.
func newRelayHolderWithCtx(
	childID string,
	cli darajapbconnect.DarajaServiceClient,
	ctx context.Context,
	cancel context.CancelFunc,
	onDone func(),
	pool *Pool,
) *relayHolder {
	return &relayHolder{
		childID: childID,
		client:  cli,
		ctx:     ctx,
		cancel:  cancel,
		fanOut:  make(map[chan *fanEvent]struct{}),
		onDone:  onDone,
		pool:    pool,
		closed:  false,
	}
}

// subscribe attaches a belt-consuming subscriber: events broadcast before
// this subscribe are replayed from the pool's belt into the channel first,
// and the belt is drained by taking it (one-shot). This is the runner's
// pump's path — belt consumption is pump-ONLY; see Pool.WatchLive for the
// admin surface's live-only variant. Must be called while the caller already
// holds a valid ClientFor (the holder validates client identity at every
// call site).
//
// The replay is one-shot — see the Pool belt's doc comment for why that is
// safe for claude. The channel is sized to hold the whole replay plus the
// live subscriberBuffer,
// so the replay sends cannot block; they run under h.mu because a broadcast
// racing between the unlock and the replay sends would otherwise land a live
// event in the channel AHEAD of the buffered ones and invert the order the
// runner (and TakeResetPending's ordering argument) depends on.
//
// A CLOSED holder returns a closed channel (not nil): the caller's drain
// treats it as holder teardown and re-Watches, which is what lets the pump
// recover onto the belt's stashed events or the next connection instead of
// parking forever on an unreadable channel.
func (h *relayHolder) subscribe() (<-chan *fanEvent, func()) {
	return h.subscribeBelt(true)
}

// subscribeLive is subscribe WITHOUT the belt: the subscriber gets live
// events from here on and never anything that was buffered before it
// attached. This is the admin surface's path (Pool.WatchLive → the
// DarajaWatch RPC): an admin watch must never drain the belt, because the
// belt exists to carry a script's terminal Exited to the runner's settle —
// consumed by an admin, the pump re-strands the child exactly as before the
// belt existed.
func (h *relayHolder) subscribeLive() (<-chan *fanEvent, func()) {
	return h.subscribeBelt(false)
}

func (h *relayHolder) subscribeBelt(takeBelt bool) (<-chan *fanEvent, func()) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		closedCh := make(chan *fanEvent)
		close(closedCh)
		return closedCh, func() {}
	}
	var replay []fanEvent
	if takeBelt && h.pool != nil {
		replay = h.pool.takeReplay(h.childID)
	}
	ch := make(chan *fanEvent, subscriberBuffer+len(replay))
	h.fanOut[ch] = struct{}{}
	for i := range replay {
		ch <- &replay[i]
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.fanOut, ch)
		h.mu.Unlock()
	}
}

// start launches the receive loop on the holder's OWN bidi stream. Returns
// an error if the initial attach (stream open) fails.
func (h *relayHolder) start() error {
	return h.startIn(h.ctx)
}

// startIn is like start but accepts an explicit context bounding how long the
// initial open may take. The stream itself runs on h.ctx, not this one — this
// one gets cancelled by the caller as soon as startIn returns, and a bidi
// stream's context is its whole lifetime, not just its opening.
//
// connect-go's CallBidiStream does not send request headers until the first
// Send; Send(nil) is the same idiom its own CallServerStream and
// CallBidiStreamSimple use to open a stream with no initial payload.
func (h *relayHolder) startIn(ctx context.Context) error {
	stream := h.client.Relay(h.ctx)
	sendErr := make(chan error, 1)
	go func() { sendErr <- stream.Send(nil) }()
	select {
	case err := <-sendErr:
		if err != nil {
			return fmt.Errorf("open relay stream: %w", err)
		}
	case <-ctx.Done():
		return fmt.Errorf("open relay stream: %w", ctx.Err())
	}
	h.mu.Lock()
	h.stream = stream
	h.mu.Unlock()
	go h.recvLoop(stream)
	return nil
}

// recvLoop reads from the daemon→daraja RelayResponse side and fans out.
// Runs on a dedicated goroutine started by start().
func (h *relayHolder) recvLoop(stream *connect.BidiStreamForClient[darajapb.RelayRequest, darajapb.RelayResponse]) {
	defer func() {
		h.shutdown()
		if h.onDone != nil {
			h.onDone()
		}
	}()

	for {
		resp, err := stream.Receive()
		if err != nil {
			h.broadcast(fanEvent{err: err})
			return
		}
		h.broadcast(fanEvent{resp: resp})
	}
}

// broadcast delivers ev to every subscriber. If a channel blocks (subscriber
// not reading), the fan skips that subscriber rather than stalling the loop.
// With no subscriber attached, the event goes to the pool's replay belt
// instead of being dropped — see the belt's doc comment on Pool.
func (h *relayHolder) broadcast(ev fanEvent) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if len(h.fanOut) == 0 {
		if h.pool != nil {
			h.pool.stashReplay(h.childID, ev)
		}
		h.mu.Unlock()
		return
	}
	for ch := range h.fanOut {
		select {
		case ch <- &ev:
		default:
			// Subscriber is slow — drop this event for them. The holder still
			// runs, so stdin keeps flowing and daraja's stash preserves order.
		}
	}
	h.mu.Unlock()
}

// shutdown closes the holder's ctx, marks it closed, cancels the stream, and
// closes all subscriber channels. Idempotent.
func (h *relayHolder) shutdown() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for ch := range h.fanOut {
		close(ch)
		delete(h.fanOut, ch)
	}
	stream := h.stream // capture under lock
	h.mu.Unlock()

	if stream != nil {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}
	h.cancel()
}

// writeStdin sends data to daraja's stdin on the shared stream. Serialised
// against other writes and against shutdown. Callers must hold a valid holder
// (RelayFor validated client identity).
func (h *relayHolder) writeStdin(data []byte) error {
	if h.stream == nil {
		return fmt.Errorf("relay stream not opened")
	}
	req := &darajapb.RelayRequest{Stdin: data}
	if err := h.stream.Send(req); err != nil {
		return fmt.Errorf("relay send: %w", err)
	}
	return nil
}

// stop tears down the holder entirely (used on client mismatch / reconnect).
func (h *relayHolder) stop() {
	h.shutdown()
}

// ─── Pool extensions ──────────────────────────────────────────────────────────

// The replay belt. pending holds, per child, the events broadcast while NO
// subscriber was attached — the drop window the fan used to have: a broadcast
// to a zero-subscriber set vanished, so a script whose driver set its result
// and exited before the daemon-side pump's first Watch could lose its Exited
// event and strand the child row as `streaming` forever. The belt lives on
// the POOL, not the holder, because the dangerous window is exactly the one
// where the holder DIES before any subscribe: daraja exits with a fast script
// (one Exited, done closes, process exit), the connection goes down, the
// holder is torn down — and a holder-owned buffer would be dropped with it.
// The first subscribe after a gap replays the belt in order and clears it.
//
// The replay is one-shot, NOT kind-aware, and NOT drain-on-consume, and that
// is what makes it safe for claude:
//
//   - Events enter the belt only from a holder with zero subscribers, and the
//     runner's pump (the production subscriber) subscribes exactly once per
//     holder — drain only returns on holder teardown or stream error, and a
//     re-Watch then lands on a FRESH holder or the belt itself. An event is
//     therefore either delivered live to a subscriber or buffered for the
//     next one, never both: no frame can reach the claude translator twice.
//     A reconnect's replay carries only post-disconnect events for the same
//     reason. The safety argument is ORDER + ONE-SHOT, never a claim about
//     which frames the belt can hold: across a disconnect gap the belt CAN
//     hold post-prompt frames (claude may have been mid-turn when the relay
//     dropped), but they are delivered in their original broadcast order
//     ahead of the live events that followed them, on the same single
//     ordered stream pkg/child reads — the translator sees the exact
//     sequence an unbroken subscription would have shown, so
//     TakeResetPending's ordering argument is unchanged.
//   - Belt consumption is PUMP-ONLY. The pump's Watch (below) is the only
//     path that drains the belt — the belt exists to carry a script's
//     terminal Exited to the runner's settle, and anything else that took it
//     would re-strand the child. The DarajaWatch admin RPC goes through
//     WatchLive, which never serves the belt: an admin watch landing in the
//     pump's re-Watch backoff (holder dead, belt non-empty) sees no buffered
//     events, gets live events only, and leaves the belt for the pump. What
//     the admin misses while unsubscribed is dropped FOR it, never taken
//     from the pump.
//   - Overflow (relayReplayMax) drops the OLDEST events, keeping the
//     terminal one; a belt handed to a Watch that has no live connection
//     delivers its events exactly once (the channel closes after) — there
//     is no replay target left afterward, matching the pre-belt semantics of
//     a torn-down holder.

// stashReplay appends ev to the child's belt, bounded by relayReplayMax with
// drop-oldest overflow. Called with the holder's mutex held (h.mu → p.mu is
// the only nesting order here; no pool path may call into a holder while
// holding p.mu).
func (p *Pool) stashReplay(childID string, ev fanEvent) {
	p.mu.Lock()
	p.replay[childID] = append(p.replay[childID], ev)
	if over := len(p.replay[childID]) - relayReplayMax; over > 0 {
		// Drop-oldest: copy the surviving tail over the head in place
		// (overlapping copy is safe). Keeps the newest relayReplayMax
		// events, the terminal one included.
		copy(p.replay[childID], p.replay[childID][over:])
		p.replay[childID] = p.replay[childID][:relayReplayMax]
	}
	p.mu.Unlock()
}

// takeReplay drains and returns the child's belt for replay into a fresh
// subscriber's channel. The caller must hold the holder's mutex, and must
// replay everything returned into a channel sized subscriberBuffer+len —
// both guarantee no belt event can be skipped past or re-buffered.
func (p *Pool) takeReplay(childID string) []fanEvent {
	p.mu.Lock()
	evts := p.replay[childID]
	delete(p.replay, childID)
	p.mu.Unlock()
	return evts
}

// takeReplayChan hands the child's belt over when there is no live
// connection to subscribe to: a channel pre-loaded with the belt (in order)
// and then closed — the Watch consumer reads the events, and the close is
// the usual "holder torn down" signal that makes Runner.pump re-Watch. Only
// events that were never delivered to anyone are here, so this is the last
// copy that will ever exist for them.
func (p *Pool) takeReplayChan(childID string) (<-chan *fanEvent, bool) {
	p.mu.Lock()
	evts := p.replay[childID]
	delete(p.replay, childID)
	p.mu.Unlock()
	if len(evts) == 0 {
		return nil, false
	}
	ch := make(chan *fanEvent, len(evts))
	for i := range evts {
		ch <- &evts[i]
	}
	close(ch)
	return ch, true
}

// dropReplay discards the child's belt — called only where the child is being
// torn down deliberately (Evict), where nobody will ever come for the events.
func (p *Pool) dropReplay(childID string) {
	p.mu.Lock()
	delete(p.replay, childID)
	p.mu.Unlock()
}

// RelayFor returns a relay holder for childID, creating one lazily on the
// current client for that child.
//
// It invalidates and rebuilds on client mismatch: when the pool reconnects
// an executor (swapping in a new http.Client), ClientFor returns a new
// DarajaServiceClient, so the comparison fails and a fresh holder is built.
func (p *Pool) RelayFor(childID string) (*relayHolder, error) {
	cli, err := p.ClientFor(childID)
	if err != nil {
		return nil, err
	}

	p.mu.RLock()
	holder := p.relayHolders[childID]
	p.mu.RUnlock()

	// Fast path: existing holder is still valid.
	if holder != nil && holder.client == cli {
		return holder, nil
	}

	// Slow path: need to create (or recreate) the holder.
	p.mu.Lock()
	// Double-check under write lock — another goroutine may have created one.
	if holder = p.relayHolders[childID]; holder != nil && holder.client == cli {
		p.mu.Unlock()
		return holder, nil
	}

	stale := holder // holder.client != cli when non-nil here
	newHolder := newRelayHolder(childID, cli, p)
	if p.relayHolders == nil {
		p.relayHolders = make(map[string]*relayHolder)
	}
	p.relayHolders[childID] = newHolder
	p.mu.Unlock()

	// Stopping the stale holder takes ITS mutex; it must not run under p.mu
	// (holder.mu → p.mu is the belt's nesting order — broadcast/subscribe
	// hold the holder lock and then take p.mu — so any p.mu → holder.mu
	// nesting here could deadlock against a subscriber-less broadcast).
	if stale != nil {
		stale.stop()
	}

	if err := newHolder.start(); err != nil {
		p.mu.Lock()
		delete(p.relayHolders, childID)
		p.mu.Unlock()
		slog.Warn("relay start failed, removing holder", "childId", childID, "error", err)
		return nil, fmt.Errorf("relay for %s: %w", childID, err)
	}

	return newHolder, nil
}

// Send writes data into the child's stdin via the holder. The stream stays
// open for further sends — Restart and repeated turns depend on this being
// callable many times across the child's life; closing the request side here
// was a leftover from an earlier one-shot design and made a second Send fail.
func (p *Pool) Send(childID string, data []byte) error {
	holder, err := p.RelayFor(childID)
	if err != nil {
		return err
	}
	return holder.writeStdin(data)
}

// Watch returns a fan-out channel for the child's relay events. Multiple
// concurrent watchers are allowed; if nobody is watching, responses go to the
// pool's replay belt (see the belt's doc comment) instead of being dropped.
// This is the PUMP's path — and the only belt-consuming one.
//
// With no live connection the belt is served FIRST: if it holds undelivered
// events — typically a fast script's whole life, its Exited included, whose
// daraja has already exited — they are handed over once and the channel then
// closes, so the pump learns the outcome instead of retrying into a
// permanently-gone connection. Otherwise the error propagates and the
// pump's retry loop keeps trying (the reconnect contract).
func (p *Pool) Watch(childID string) (<-chan *fanEvent, func(), error) {
	holder, err := p.RelayFor(childID)
	if err == nil {
		subCh, unsub := holder.subscribe()
		return subCh, unsub, nil
	}
	if ch, ok := p.takeReplayChan(childID); ok {
		return ch, func() {}, nil
	}
	return nil, nil, err
}

// WatchLive is the admin surface's Watch (the DarajaWatch RPC): subscribe to
// the child's live relay events and NEVER serve the replay belt. Belt
// consumption is pump-only — the belt's whole job is carrying a script's
// terminal Exited to the runner's settle, and an admin watch landing in the
// pump's re-Watch backoff (holder dead, belt non-empty) that drained it would
// re-strand the child exactly as before the belt existed. With no live
// connection the error propagates to the caller (an admin surface reports it,
// the pump retries); the belt stays where it is.
func (p *Pool) WatchLive(childID string) (<-chan *fanEvent, func(), error) {
	holder, err := p.RelayFor(childID)
	if err != nil {
		return nil, nil, err
	}
	subCh, unsub := holder.subscribeLive()
	return subCh, unsub, nil
}

// Restart asks the connected daraja to replace its child process: signal,
// wait, relaunch. spec nil means daraja reuses the spec it already holds.
func (p *Pool) Restart(ctx context.Context, childID string, spec *darajapb.ChildSpec, graceMs int32) (pid int32, err error) {
	cli, err := p.ClientFor(childID)
	if err != nil {
		return 0, err
	}
	resp, err := cli.Restart(ctx, connect.NewRequest(&darajapb.RestartRequest{
		Spec:    spec,
		GraceMs: graceMs,
	}))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetPid(), nil
}

// Shutdown ends the child gracefully (or immediately, at graceMs=0) and takes
// daraja down with it. The exit info in the response is authoritative — the
// relay stream may simply be torn down afterward rather than emitting a
// separate ProcessExited event, so callers should not wait on the Watch
// channel for this outcome.
func (p *Pool) Shutdown(ctx context.Context, childID string, graceMs int32) (exitCode int32, signal string, err error) {
	cli, err := p.ClientFor(childID)
	if err != nil {
		return 0, "", err
	}
	resp, err := cli.Shutdown(ctx, connect.NewRequest(&darajapb.ShutdownRequest{GraceMs: graceMs}))
	if err != nil {
		return 0, "", err
	}
	return resp.Msg.GetExitCode(), resp.Msg.GetSignal(), nil
}
