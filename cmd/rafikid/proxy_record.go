// SPDX-License-Identifier: Apache-2.0

// The isolated proxy recorder: `rafikid proxy record` forwards POST
// /v1/messages to one upstream and writes every exchange to disk byte for
// byte, so a provider's real response can later be turned into a fixture.
//
// Nothing in this file may reach the daemon: no config file, no profile, no
// DSN, no socket, no provider routing, no telemetry. This is the one rafikid
// command path that runs with no database at all, which is what makes it
// usable against a broken or absent daemon — a loadConfig, runDaemon, paths,
// pgxpool or server.MessagesProxy call here would re-couple the diagnostic to
// the thing being diagnosed.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// The recorder's refusal vocabulary. Every check runs before anything is bound
// or written, so a refused flag leaves neither a listener nor a file behind.
var (
	errProxyRecordListen   = errors.New("invalid --listen")
	errProxyRecordUpstream = errors.New("invalid --upstream")
	errProxyRecordDir      = errors.New("invalid --record-dir")
)

const (
	// recordMessagesPath is the only route the recorder serves: one provider
	// endpoint, captured verbatim. Every other path and method is a 404 and
	// leaves no files.
	recordMessagesPath = "/v1/messages"

	// recordSeqWidth zero-pads the sequence number to six digits, so a lexical
	// sort of the directory is the arrival order.
	recordSeqWidth = 6

	// recordShutdownGrace bounds how long a clean shutdown waits for in-flight
	// exchanges to finish their paired writes.
	recordShutdownGrace = 30 * time.Second

	// The generated 502 for an upstream transport failure is both the
	// client-visible response and what lands in that exchange's response file:
	// one format, no error sidecar.
	recordBadGatewayStatus = "502 Bad Gateway"
	recordBadGatewayBody   = "rafikid proxy record: upstream request failed\n"

	// recordSecretsWarning is printed on every start, because the directory is
	// about to hold credentials in clear text.
	recordSecretsWarning = "rafikid proxy record: WARNING: the record directory holds raw " +
		"HTTP headers and bodies, including credentials. Treat it as secret: " +
		"never commit it and never share it."
)

// proxyRecordOptions is the recorder's entire flag contract. It carries no
// daemon configuration on purpose.
type proxyRecordOptions struct {
	Listen    string
	Upstream  string
	RecordDir string
}

// proxyRecordConfig is proxyRecordOptions validated and resolved: ready to bind.
type proxyRecordConfig struct {
	Listen    string
	Upstream  *url.URL
	RecordDir string
}

// newProxyRecordCmd is the `proxy` parent command. It is attached directly from
// newRootCmd, next to fundi/agent/migrate/user, and everything under it runs as
// a standalone diagnostic process — see the file comment.
func newProxyRecordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Record raw provider HTTP exchanges, without a daemon or a database",
		Long: `Standalone HTTP recorders. They forward one route to one upstream and write
what crossed the boundary to disk, with no daemon, no database, no config file
and no profile involved.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newProxyRecordRunCmd())
	return cmd
}

func newProxyRecordRunCmd() *cobra.Command {
	var opts proxyRecordOptions

	cmd := &cobra.Command{
		Use:   "record",
		Short: "Record POST /v1/messages exchanges with one upstream to disk",
		Long: `Records what a provider actually sends, byte for byte.

Forwards POST /v1/messages to --upstream and writes each exchange to
--record-dir as 000001-request.http and 000001-response.http, numbered in
arrival order. Each file is an HTTP message: start line, headers, blank line,
then the exact body — an SSE stream included. Nothing is redacted, so the
directory holds credentials.

This process is deliberately isolated: it consults no config file, no profile
and no database, and starts no other listener. (rafikid's shared pre-cobra
environment-file load still runs, as it does for every subcommand, but the
recorder reads nothing from it.) Its only purpose is capturing a real response
so it can later be sanitized into a fixture.

--record-dir must be absent (created 0700) or empty; the recorder never
overwrites a recording. Every file is 0600 and appears only once its pair is
complete.`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// SIGINT/SIGTERM cancel this context instead of killing the
			// process, so the http.Server can drain in-flight exchanges and
			// each one finishes its paired write.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runProxyRecord(ctx, opts, cmd.ErrOrStderr())
		},
	}

	cmd.Flags().StringVar(&opts.Listen, "listen", "", "loopback host:port to serve on (port 0 picks a free one)")
	cmd.Flags().StringVar(&opts.Upstream, "upstream", "", "absolute http:// or https:// URL to forward to")
	cmd.Flags().StringVar(&opts.RecordDir, "record-dir", "", "absent or empty directory for the raw recordings")
	for _, name := range []string{"listen", "upstream", "record-dir"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// resolveProxyRecord validates all three flags before anything is bound or
// written, so a refusal never leaves a listener or a file behind.
func resolveProxyRecord(opts proxyRecordOptions) (proxyRecordConfig, error) {
	var cfg proxyRecordConfig
	listen, err := validateRecordListen(opts.Listen)
	if err != nil {
		return cfg, err
	}
	upstream, err := validateRecordUpstream(opts.Upstream)
	if err != nil {
		return cfg, err
	}
	dir, err := prepareRecordDir(opts.RecordDir)
	if err != nil {
		return cfg, err
	}
	cfg.Listen, cfg.Upstream, cfg.RecordDir = listen, upstream, dir
	return cfg, nil
}

// validateRecordListen refuses anything but a loopback listener: a recorder
// captures credentials in clear text, so it must never be reachable from
// another host. Port 0 is allowed and means "pick a free port".
func validateRecordListen(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not host:port: %v", errProxyRecordListen, listen, err)
	}
	if port == "" {
		return "", fmt.Errorf("%w: %q has no port (use 0 for an ephemeral one)", errProxyRecordListen, listen)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("%w: port %q is not a number in 0..65535", errProxyRecordListen, port)
	}
	if host == "localhost" {
		return net.JoinHostPort(host, strconv.Itoa(n)), nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: host %q is not a loopback IP or localhost; the recorder never listens on a routable interface", errProxyRecordListen, host)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// validateRecordUpstream accepts an absolute http(s) URL with a host and no
// userinfo — a credential in the upstream URL would end up in every log line
// and in nothing the recorder is meant to capture.
func validateRecordUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: an absolute http:// or https:// URL is required", errProxyRecordUpstream)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", errProxyRecordUpstream, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q is not an http or https URL", errProxyRecordUpstream, raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q has no host", errProxyRecordUpstream, raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: %q carries userinfo; the recorder does not take an upstream credential in the URL", errProxyRecordUpstream, raw)
	}
	return u, nil
}

// prepareRecordDir enforces the directory contract: absent means "create it
// 0700", present means "empty and not group/world accessible". It never
// relaxes permissions it finds, and never writes into a directory that already
// holds anything.
func prepareRecordDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("%w: a directory is required", errProxyRecordDir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errProxyRecordDir, err)
	}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", fmt.Errorf("%w: create %s: %v", errProxyRecordDir, abs, err)
		}
		if info, err = os.Stat(abs); err != nil {
			return "", fmt.Errorf("%w: %v", errProxyRecordDir, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("%w: %v", errProxyRecordDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", errProxyRecordDir, abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errProxyRecordDir, err)
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("%w: %s is not empty (%d entries); the recorder never overwrites a recording", errProxyRecordDir, abs, len(entries))
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("%w: %s is mode %04o, which group or others can access; chmod 700 it first — the recorder never relaxes permissions", errProxyRecordDir, abs, perm)
	}
	return abs, nil
}

// runProxyRecord binds, announces and serves until ctx is cancelled, then
// shuts down cleanly. It is the whole command, minus flag parsing, and touches
// nothing outside the listener and the record directory.
func runProxyRecord(ctx context.Context, opts proxyRecordOptions, stderr io.Writer) error {
	cfg, err := resolveProxyRecord(opts)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: newRecordHandler(cfg.Upstream, newExchangeRecorder(cfg.RecordDir), stderr),
		// No WriteTimeout: an SSE stream legitimately runs for minutes.
		ReadHeaderTimeout: 30 * time.Second,
	}

	// The URL announced is the one actually bound, so --listen with port 0 is
	// usable: the caller reads the port out of this line.
	_, _ = fmt.Fprint(stderr, recordAnnouncement(recordListenerURL(ln), cfg.RecordDir))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Shutdown waits for in-flight handlers, which is what lets an exchange
	// finish its paired write before the process exits.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), recordShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// recordAnnouncement is the startup block: the URL actually bound, the
// directory the recordings land in, and the secrets warning.
func recordAnnouncement(listenURL, dir string) string {
	return fmt.Sprintf("rafikid proxy record: listening on %s\nrafikid proxy record: recording to %s\n%s\n",
		listenURL, dir, recordSecretsWarning)
}

// recordListenerURL renders the bound address as a URL a client can use.
func recordListenerURL(ln net.Listener) string {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return "http://" + ln.Addr().String()
	}
	return "http://" + net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
}

// recordProxy is the recorder's entire HTTP surface: one route, one method, no
// authentication, no rewriting.
type recordProxy struct {
	upstream  *url.URL
	rec       *exchangeRecorder
	transport http.RoundTripper
	stderr    io.Writer
}

func newRecordHandler(upstream *url.URL, rec *exchangeRecorder, stderr io.Writer) http.Handler {
	return &recordProxy{
		upstream:  upstream,
		rec:       rec,
		transport: newRecordTransport(),
		stderr:    stderr,
	}
}

// newRecordTransport forwards bytes and nothing else. No environment proxy:
// --upstream is the operator's own endpoint, and honoring HTTPS_PROXY would
// send the capture somewhere else. No transparent gzip: the transport must not
// rewrite a body the recorder exists to see exactly as it crossed the boundary.
func newRecordTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	return t
}

func (p *recordProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != recordMessagesPath {
		http.NotFound(w, r)
		return
	}

	// The raw body is read in full before anything is numbered: a request that
	// never arrives whole is a 400, not a half-recorded exchange.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "rafikid proxy record: could not read the request body", http.StatusBadRequest)
		return
	}

	// Numbering happens at acceptance, so the files follow arrival order even
	// though the exchanges finish in whatever order the upstream answers.
	seq := p.rec.nextSeq()
	recordedRequest := formatRecordedRequest(r, body)

	resp, err := p.forward(r, body)
	if err != nil {
		fmt.Fprintf(p.stderr, "rafikid proxy record: upstream %s: %v\n", p.upstream.Host, err)
		header := http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
		badGateway := []byte(recordBadGatewayBody)
		writeGeneratedResponse(w, http.StatusBadGateway, header, badGateway)
		p.writePair(seq, recordedRequest, formatRecordedResponse(recordBadGatewayStatus, header, badGateway))
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Recorded after the body is relayed, because the file must hold exactly
	// the bytes the client received — including an SSE stream flushed chunk by
	// chunk.
	written, streamErr := streamResponse(w, resp.Body)
	if streamErr != nil {
		fmt.Fprintf(p.stderr, "rafikid proxy record: %s (recorded %d response bytes)\n", streamErr, len(written))
	}
	p.writePair(seq, recordedRequest, formatRecordedResponse(upstreamStatusLine(resp), resp.Header, written))
}

// forward replays the accepted request against the upstream: same method, same
// path and query, headers as they arrived, raw body. Nothing is added, removed
// or redacted — the recorder is not a gateway and has no opinion about how a
// call is authenticated.
func (p *recordProxy) forward(r *http.Request, body []byte) (*http.Response, error) {
	target := *p.upstream
	// The request's own path and query are preserved; a path on --upstream is
	// not a prefix, so the recorded and forwarded targets stay identical.
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Host is left to the URL: the upstream names the authority the bytes are
	// for. The client's Host is still what the request file records.
	out.Header = r.Header.Clone()
	return p.transport.RoundTrip(out)
}

// writePair records one exchange; a pair that cannot be written is reported
// but never fails the exchange, which has already been served.
func (p *recordProxy) writePair(seq uint64, request, response []byte) {
	if err := p.rec.writePair(seq, request, response); err != nil {
		fmt.Fprintf(p.stderr, "rafikid proxy record: %v\n", err)
	}
}

// writeGeneratedResponse writes a response the recorder made up itself, from
// the same header map and body that go into the response file.
func writeGeneratedResponse(w http.ResponseWriter, status int, header http.Header, body []byte) {
	for name, values := range header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// formatRecordedRequest serialises the request as it arrived: start line,
// headers, blank line, exact body. Header values are kept verbatim, credentials
// included, because these files are forensic input for a later sanitizer.
func formatRecordedRequest(r *http.Request, body []byte) []byte {
	var b bytes.Buffer
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", r.Method, target)
	// net/http lifts the request's Host out of Header into r.Host, so writing
	// only Header would record an HTTP/1.1 message with no authority.
	if r.Host != "" {
		fmt.Fprintf(&b, "Host: %s\r\n", r.Host)
	}
	_ = r.Header.Write(&b) // keys sorted, one line per value
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

// formatRecordedResponse serialises a response the same way: the status line as
// the client saw it, the headers, a blank line, then the exact body bytes.
func formatRecordedResponse(statusLine string, header http.Header, body []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %s\r\n", statusLine)
	_ = header.Write(&b)
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

// upstreamStatusLine is the status line for a recorded upstream response, with
// the reason phrase filled in if the upstream somehow omitted it.
func upstreamStatusLine(resp *http.Response) string {
	if resp.Status != "" {
		return resp.Status
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

// streamResponse copies the upstream body to the client as it arrives — flushing
// every chunk so an SSE stream actually streams — and returns exactly the bytes
// it handed to the client, which is what gets recorded.
func streamResponse(w http.ResponseWriter, body io.Reader) ([]byte, error) {
	var captured bytes.Buffer
	ctrl := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			captured.Write(buf[:wn])
			if werr != nil {
				return captured.Bytes(), werr
			}
			if ferr := ctrl.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return captured.Bytes(), ferr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return captured.Bytes(), nil
			}
			return captured.Bytes(), rerr
		}
	}
}

// exchangeRecorder numbers exchanges and writes each one as a paired
// request/response file. Only numbering and file creation take the mutex:
// proxying, streaming and the body writes themselves run concurrently, so one
// slow exchange never delays another's bytes.
type exchangeRecorder struct {
	dir string
	mu  sync.Mutex
	seq uint64
}

func newExchangeRecorder(dir string) *exchangeRecorder {
	return &exchangeRecorder{dir: dir}
}

// recordFileName is the on-disk name of one half of an exchange, e.g.
// 000001-request.http.
func recordFileName(seq uint64, kind string) string {
	return fmt.Sprintf("%0*d-%s.http", recordSeqWidth, seq, kind)
}

// nextSeq reserves the next sequence number. It is called once per accepted
// request, so the numbers follow arrival order.
func (r *exchangeRecorder) nextSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

// writePair writes both halves of one exchange, and never one without the
// other: the files are staged as 0600 temp files and renamed only once both have
// been written in full.
func (r *exchangeRecorder) writePair(seq uint64, request, response []byte) error {
	staged, err := r.stagePair(seq, request, response)
	if err != nil {
		return err
	}
	return commitPair(staged)
}

// stagedRecord is one half of an exchange between creation and rename.
type stagedRecord struct {
	file  *os.File
	final string
	body  []byte
}

// stagePair reserves both file names and creates their temp files under the
// numbering mutex; the bodies are written and renamed outside it.
func (r *exchangeRecorder) stagePair(seq uint64, request, response []byte) ([]*stagedRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var staged []*stagedRecord
	for _, msg := range []struct {
		kind string
		body []byte
	}{
		{"request", request},
		{"response", response},
	} {
		final := filepath.Join(r.dir, recordFileName(seq, msg.kind))
		// Never overwrite: rename(2) would happily replace an existing file, and
		// a previous recording is not something to lose.
		if _, err := os.Lstat(final); err == nil {
			discardStaged(staged)
			return nil, fmt.Errorf("%w: %s already exists", errProxyRecordDir, final)
		} else if !errors.Is(err, fs.ErrNotExist) {
			discardStaged(staged)
			return nil, err
		}
		f, err := os.CreateTemp(r.dir, "."+recordFileName(seq, msg.kind)+".*")
		if err != nil {
			discardStaged(staged)
			return nil, err
		}
		staged = append(staged, &stagedRecord{file: f, final: final, body: msg.body})
	}
	return staged, nil
}

// commitPair writes both bodies, fixes each file at 0600, and only then renames
// them into place. Any failure removes everything it staged, so the directory
// never shows a request without its response.
func commitPair(staged []*stagedRecord) error {
	for _, s := range staged {
		if err := writeAndSync(s.file, s.body); err != nil {
			discardStaged(staged)
			return fmt.Errorf("write %s: %w", s.final, err)
		}
	}
	for i, s := range staged {
		if err := os.Rename(s.file.Name(), s.final); err != nil {
			discardStaged(staged[i:])
			for _, done := range staged[:i] {
				_ = os.Remove(done.final)
			}
			return fmt.Errorf("rename %s: %w", s.final, err)
		}
	}
	return nil
}

// discardStaged removes every temp file in the set, renamed or not.
func discardStaged(staged []*stagedRecord) {
	for _, s := range staged {
		_ = s.file.Close()
		_ = os.Remove(s.file.Name())
	}
}

// writeAndSync fills one staged file in full, pins it at 0600 whatever the
// operator's umask is, and closes it.
func writeAndSync(f *os.File, body []byte) error {
	if _, err := f.Write(body); err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
