// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"io"
	"sync"

	"go.graveland.dev/rafiki/pkg/errclass"
	"go.graveland.dev/rafiki/pkg/msg"
)

// ErrorObserver is told about one failed upstream call: the provider name the
// call went to and why it failed. It runs on the calling goroutine and must not
// block.
type ErrorObserver func(provider string, class errclass.Class)

// WithErrorObserver reports every failed upstream call. The report is made at
// the sender, so it sees each attempt: a primary that is out of credit is
// reported even when the failover to a fallback then succeeds, and each retry
// of a failing call is its own report. That is the point, since a masked
// failure is the one an operator most needs to hear about.
func WithErrorObserver(fn ErrorObserver) ClientOption {
	return func(c *Client) { c.errObserver = fn }
}

// observeSenders wraps every sender the client holds. It runs once, at the end
// of NewClient, so senders installed by the registry and by WithProviderSender
// are both covered, and no code path can reach a sender around it.
func (c *Client) observeSenders() {
	if c.errObserver == nil {
		return
	}
	for name, s := range c.senders {
		c.senders[name] = &observedSender{inner: s, provider: name, observe: c.errObserver}
	}
}

type observedSender struct {
	inner    Sender
	provider string
	observe  ErrorObserver
}

func (o *observedSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	resp, err := o.inner.Send(ctx, req, opts)
	if err != nil {
		o.observe(o.provider, errclass.FromError(err))
	}
	return resp, err
}

func (o *observedSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	dec, err := o.inner.Stream(ctx, req, opts)
	if err != nil {
		o.observe(o.provider, errclass.FromError(err))
		return dec, err
	}
	return &observedDecoder{inner: dec, provider: o.provider, observe: o.observe}, nil
}

// observedDecoder reports a stream that ends in an error, once. io.EOF is the
// clean end of a stream and is not a failure.
type observedDecoder struct {
	inner    msg.StreamDecoder
	provider string
	observe  ErrorObserver
	once     sync.Once
}

func (d *observedDecoder) Next() (msg.StreamEvent, error) {
	ev, err := d.inner.Next()
	if err != nil && !errors.Is(err, io.EOF) {
		d.once.Do(func() { d.observe(d.provider, errclass.FromError(err)) })
	}
	return ev, err
}

func (d *observedDecoder) Close() error { return d.inner.Close() }
