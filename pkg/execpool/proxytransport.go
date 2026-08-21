// SPDX-License-Identifier: Apache-2.0

package execpool

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// proxyTransport is an http.RoundTripper that carries a request over an
// executor's existing connection instead of dialing it. A provider with
// via_executor gets one of these as its sender's transport, so nothing above
// the transport layer knows the difference between a direct dial and a
// relayed one.
type proxyTransport struct {
	pool       *Pool
	executorID string
	proxyName  string
}

// NewProxyTransport builds the relay transport for one provider on one
// executor. It turns one http.Request into a ProxyStart plus body chunks, and
// one ProxyHead plus body chunks back into an http.Response.
func NewProxyTransport(p *Pool, executorID, proxyName string) http.RoundTripper {
	return &proxyTransport{pool: p, executorID: executorID, proxyName: proxyName}
}

func (t *proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	client, err := t.pool.connectClientFor(t.executorID)
	if err != nil {
		return nil, fmt.Errorf("execpool: proxy %q: %w", t.proxyName, err)
	}
	stream := client.Proxy(req.Context())

	headers := make(map[string]string, len(req.Header))
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}
	if err := stream.Send(&executorpb.ProxyRequest{
		Msg: &executorpb.ProxyRequest_Start{Start: &executorpb.ProxyStart{
			ProxyName: t.proxyName,
			Method:    req.Method,
			Path:      req.URL.RequestURI(),
			Headers:   headers,
		}},
	}); err != nil {
		return nil, fmt.Errorf("execpool: proxy %q: send start: %w", t.proxyName, err)
	}

	if req.Body != nil {
		buf := make([]byte, 32<<10)
		for {
			n, rerr := req.Body.Read(buf)
			if n > 0 {
				if err := stream.Send(&executorpb.ProxyRequest{
					Msg: &executorpb.ProxyRequest_Body{Body: append([]byte(nil), buf[:n]...)},
				}); err != nil {
					return nil, fmt.Errorf("execpool: proxy %q: send body: %w", t.proxyName, err)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return nil, fmt.Errorf("execpool: proxy %q: read request body: %w", t.proxyName, rerr)
			}
		}
	}
	// CloseRequest is the half-close that tells the executor the body is
	// complete. Without it the executor's io.Pipe never reaches EOF and the
	// upstream request never completes.
	if err := stream.CloseRequest(); err != nil {
		return nil, fmt.Errorf("execpool: proxy %q: close request: %w", t.proxyName, err)
	}

	first, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("execpool: proxy %q: %w", t.proxyName, err)
	}
	head := first.GetHead()
	if head == nil {
		return nil, fmt.Errorf("execpool: proxy %q: first response message was not a head", t.proxyName)
	}
	resp := &http.Response{
		Status:     http.StatusText(int(head.GetStatus())),
		StatusCode: int(head.GetStatus()),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header, len(head.GetHeaders())),
		Request:    req,
	}
	for k, v := range head.GetHeaders() {
		resp.Header.Set(k, v)
	}
	// The body is a reader over the remaining stream, NOT a buffer: an SSE
	// response must be readable as it arrives, or a streaming turn stalls
	// until the upstream finishes.
	resp.Body = &proxyBody{stream: stream}
	return resp, nil
}

// proxyBody is an io.ReadCloser that pulls ProxyResponse messages from the
// stream and hands back their body bytes, one chunk at a time.
type proxyBody struct {
	stream *connect.BidiStreamForClient[executorpb.ProxyRequest, executorpb.ProxyResponse]
	// remainder holds bytes from a chunk that did not fit in the caller's
	// buffer on a previous Read.
	remainder []byte
	err       error // sticky: io.EOF or the terminal stream error
}

func (b *proxyBody) Read(p []byte) (int, error) {
	for len(b.remainder) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		msg, err := b.stream.Receive()
		if err != nil {
			// connect signals a clean stream end with an error that WRAPS
			// io.EOF (via errors.Is) rather than being io.EOF itself — the
			// end-of-stream envelope carries protocol-specific flags connect
			// reports as CodeUnknown. io.Copy/io.ReadAll require the exact
			// sentinel to treat it as a clean end rather than a read failure,
			// so it must be normalized here, not left wrapped.
			if errors.Is(err, io.EOF) {
				err = io.EOF
			}
			b.err = err
			continue
		}
		if chunk := msg.GetBody(); len(chunk) > 0 {
			b.remainder = chunk
		}
		// A head-only message this far into the stream would be a protocol
		// error on the executor's part; there is nothing to hand back, so we
		// loop for the next message rather than surface a spurious success.
	}
	n := copy(p, b.remainder)
	b.remainder = b.remainder[n:]
	return n, nil
}

func (b *proxyBody) Close() error {
	return b.stream.CloseResponse()
}
