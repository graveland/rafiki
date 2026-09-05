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
}

type fanEvent struct {
	resp *darajapb.RelayResponse
	err  error
}

// Response returns the relay response payload, or nil if this event carries an error.
func (e *fanEvent) Response() *darajapb.RelayResponse { return e.resp }

// Err returns the stream error, or nil on success.
func (e *fanEvent) Err() error { return e.err }

func newRelayHolder(childID string, cli darajapbconnect.DarajaServiceClient) *relayHolder {
	ctx, cancel := context.WithCancel(context.Background())
	return &relayHolder{
		childID: childID,
		client:  cli,
		ctx:     ctx,
		cancel:  cancel,
		fanOut:  make(map[chan *fanEvent]struct{}),
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
) *relayHolder {
	return &relayHolder{
		childID: childID,
		client:  cli,
		ctx:     ctx,
		cancel:  cancel,
		fanOut:  make(map[chan *fanEvent]struct{}),
		onDone:  onDone,
		closed:  false,
	}
}

// subscribe returns a channel that receives every RelayResponse as it arrives,
// plus an unsubscribe func. Must be called while the caller already holds a
// valid ClientFor (the holder validates client identity at every call site).
func (h *relayHolder) subscribe() (<-chan *fanEvent, func()) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, func() {}
	}
	ch := make(chan *fanEvent, 64)
	h.fanOut[ch] = struct{}{}
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
func (h *relayHolder) broadcast(ev fanEvent) {
	h.mu.Lock()
	if h.closed {
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

	// Shut down any stale holder whose client doesn't match.
	if holder != nil && holder.client != cli {
		holder.stop()
	}

	newHolder := newRelayHolder(childID, cli)
	if p.relayHolders == nil {
		p.relayHolders = make(map[string]*relayHolder)
	}
	p.relayHolders[childID] = newHolder
	p.mu.Unlock()

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
// concurrent watchers are allowed; if nobody is watching responses are dropped
// (the holder still runs, so stdin keeps flowing — daraja's stash keeps
// backpressure honest across reconnects).
func (p *Pool) Watch(childID string) (<-chan *fanEvent, func(), error) {
	holder, err := p.RelayFor(childID)
	if err != nil {
		return nil, nil, err
	}
	subCh, unsub := holder.subscribe()
	return subCh, unsub, nil
}
