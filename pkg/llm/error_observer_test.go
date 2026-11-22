// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/errclass"
	"go.graveland.dev/rafiki/pkg/msg"
)

type report struct {
	provider string
	class    errclass.Class
}

type reports struct {
	mu  sync.Mutex
	got []report
}

func (r *reports) observe(provider string, class errclass.Class) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, report{provider, class})
}

func (r *reports) all() []report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]report(nil), r.got...)
}

// The failure an operator most needs to hear about is the one a failover hides:
// the primary is out of credit, the fallback answers, and every turn succeeds.
func TestErrorObserverReportsAMaskedPrimaryFailure(t *testing.T) {
	ck := assert.NewAborting(t)
	var rep reports
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){respondErr(creditExhaustedErr())}}
	fallback := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){respondText("from fallback")}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithErrorObserver(rep.observe),
	)
	ck.NoError(err, "NewClient")

	resp, err := c.send(context.Background(), SendMeta{Fallback: []string{"openrouter"}}, userTextRequest("claude-haiku-4-5"), nil)
	ck.NoError(err, "the failover answers")
	ck.Eq("from fallback", firstText(t, resp), "response")

	ck.EqDeep([]report{{"anthropic", errclass.UsageLimit}}, rep.all(),
		"the primary's credit exhaustion is reported even though the turn succeeded, and the successful fallback is not")
}

func TestErrorObserverReportsARefusedKeyAsAuth(t *testing.T) {
	ck := assert.NewAborting(t)
	var rep reports
	primary := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){respondErr(authErr())}}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithErrorObserver(rep.observe),
	)
	ck.NoError(err, "NewClient")
	_, err = c.send(context.Background(), SendMeta{}, userTextRequest("claude-haiku-4-5"), nil)
	ck.Error(err, "401 surfaces")
	ck.EqDeep([]report{{"anthropic", errclass.Auth}}, rep.all(), "auth")
}

func TestErrorObserverCountsEachAttemptOfARetriedCall(t *testing.T) {
	ck := assert.NewAborting(t)
	var rep reports
	s := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondErr(rateLimitErr()), respondErr(rateLimitErr()),
	}}
	o := &observedSender{inner: s, provider: "anthropic", observe: rep.observe}
	_, _ = o.Send(context.Background(), userTextRequest("m"), msg.EncodeOptions{})
	_, _ = o.Send(context.Background(), userTextRequest("m"), msg.EncodeOptions{})
	ck.Eq(2, len(rep.all()), "each failed upstream call is its own report")
}

type failingDecoder struct {
	events []msg.StreamEvent
	err    error
}

func (d *failingDecoder) Next() (msg.StreamEvent, error) {
	if len(d.events) > 0 {
		ev := d.events[0]
		d.events = d.events[1:]
		return ev, nil
	}
	return nil, d.err
}
func (d *failingDecoder) Close() error { return nil }

type streamSender struct {
	scriptedSender
	openErr error
	dec     msg.StreamDecoder
}

func (s *streamSender) Stream(context.Context, *msg.Request, msg.EncodeOptions) (msg.StreamDecoder, error) {
	return s.dec, s.openErr
}

func TestErrorObserverReportsStreamsOpenedFailedBrokenAndFinished(t *testing.T) {
	ctx := context.Background()
	req := userTextRequest("m")

	t.Run("failure to open", func(t *testing.T) {
		ck := assert.NewAborting(t)
		var rep reports
		o := &observedSender{inner: &streamSender{openErr: creditExhaustedErr()}, provider: "p", observe: rep.observe}
		_, err := o.Stream(ctx, req, msg.EncodeOptions{})
		ck.Error(err, "open error")
		ck.EqDeep([]report{{"p", errclass.UsageLimit}}, rep.all(), "open failure")
	})
	t.Run("broken mid-stream is reported once", func(t *testing.T) {
		ck := assert.NewAborting(t)
		var rep reports
		o := &observedSender{inner: &streamSender{dec: &failingDecoder{err: errors.New("connection reset")}}, provider: "p", observe: rep.observe}
		dec, err := o.Stream(ctx, req, msg.EncodeOptions{})
		ck.NoError(err, "open")
		_, e1 := dec.Next()
		_, e2 := dec.Next()
		ck.Error(e1, "first")
		ck.Error(e2, "second")
		ck.EqDeep([]report{{"p", errclass.Other}}, rep.all(), "one report for one broken stream")
	})
	t.Run("a clean end is not a failure", func(t *testing.T) {
		ck := assert.NewAborting(t)
		var rep reports
		o := &observedSender{inner: &streamSender{dec: &failingDecoder{err: io.EOF}}, provider: "p", observe: rep.observe}
		dec, err := o.Stream(ctx, req, msg.EncodeOptions{})
		ck.NoError(err, "open")
		_, err = dec.Next()
		ck.True(errors.Is(err, io.EOF), "EOF passes through")
		ck.Eq(0, len(rep.all()), "no report")
		ck.NoError(dec.Close(), "Close passes through")
	})
}

func TestNoObserverLeavesSendersUnwrapped(t *testing.T) {
	ck := assert.NewAborting(t)
	s := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){respondText("x")}}
	c, err := NewClient(WithProviderSender("anthropic", s), WithCatalog(seededCatalog(t)), WithLogger(testLogger(t)))
	ck.NoError(err, "NewClient")
	got, ok := c.senders["anthropic"].(*scriptedSender)
	ck.True(ok && got == s, "without an observer the sender is the one installed, not a wrapper")
}
