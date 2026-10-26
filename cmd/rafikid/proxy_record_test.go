// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
	"github.com/spf13/cobra"
)

// recordFixedDate pins the upstream's Date header, which net/http would
// otherwise stamp with the current time, so a recorded response file can be
// compared byte for byte.
const recordFixedDate = "Mon, 01 Jan 2024 00:00:00 GMT"

// recordTest bundles the T and its collecting assertions so a helper can fail
// the calling test without threading two arguments through every call.
type recordTest struct {
	t *testing.T
	c *assert.C
}

func newRecordTest(t *testing.T) recordTest {
	t.Helper()
	return recordTest{t: t, c: assert.NewCollecting(t)}
}

// syncBuffer is a bytes.Buffer that can be read while runProxyRecord writes to
// it from its own goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// proxyRecordHarness is one recorder under test in front of an httptest
// upstream: the URL a client posts to, and the directory it records into.
type proxyRecordHarness struct {
	url    string
	dir    string
	stderr *syncBuffer
}

// startProxyRecord starts the recorder in front of upstream, recording into a
// fresh directory it creates itself.
func startProxyRecord(r recordTest, upstream string) proxyRecordHarness {
	r.t.Helper()
	return startProxyRecordFrom(r, filepath.Join(r.t.TempDir(), "recordings"), upstream)
}

// startProxyRecordFrom is startProxyRecord for a caller-supplied directory,
// which is how the directory contract is exercised.
func startProxyRecordFrom(r recordTest, dir, upstream string) proxyRecordHarness {
	r.t.Helper()
	cfg, err := resolveProxyRecord(proxyRecordOptions{
		Listen:    "127.0.0.1:0",
		Upstream:  upstream,
		RecordDir: dir,
	})
	r.c.Require().NoError(err, "resolveProxyRecord(%s)", upstream)
	buf := &syncBuffer{}
	srv := httptest.NewServer(newRecordHandler(cfg.Upstream, newExchangeRecorder(cfg.RecordDir), buf))
	r.t.Cleanup(srv.Close)
	return proxyRecordHarness{url: srv.URL, dir: cfg.RecordDir, stderr: buf}
}

// recorded is one file the recorder wrote, read back verbatim.
//
// The pair is committed after the response has been relayed to the client, so
// the file is awaited rather than assumed to exist the moment the client reads
// its last byte. Waiting on a -response.http file also means the whole pair is
// on disk: it is renamed second, so its presence implies its request.
func (r recordTest) recorded(dir, name string) []byte {
	r.t.Helper()
	path := filepath.Join(dir, name)
	r.c.EventuallyTrue(5*time.Second, 5*time.Millisecond, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, "%s was never recorded", name)
	body, err := os.ReadFile(path)
	r.c.Require().NoError(err, "read %s", name)
	return body
}

// post drives one exchange through the recorder the way a provider client
// would. The response value is returned for its status and headers only; its
// body has already been read and closed.
func (r recordTest) post(url, body string) (*http.Response, []byte) {
	r.t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	r.c.Require().NoError(err, "POST %s", url)
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	r.c.Require().NoError(err, "read %s", url)
	return resp, got
}

// postRaw speaks HTTP/1.1 by hand, so the recorded request holds exactly the
// headers the test wrote and nothing the Go client would have added.
func (r recordTest) postRaw(addr, raw string) (status int, body []byte) {
	r.t.Helper()
	conn, err := net.Dial("tcp", addr)
	r.c.Require().NoError(err, "dial recorder")
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, raw)
	r.c.Require().NoError(err, "write raw request")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	r.c.Require().NoError(err, "read raw response")
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	r.c.Require().NoError(err, "read raw body")
	return resp.StatusCode, body
}

// TestProxyRecordJSONExchangeIsRecordedVerbatim pins both files of an ordinary
// JSON exchange byte for byte: the request as it arrived (credential included)
// and the response as the client received it.
func TestProxyRecordJSONExchangeIsRecordedVerbatim(t *testing.T) {
	r := newRecordTest(t)
	requestBody := `{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	responseBody := `{"id":"msg_01","type":"message","content":[{"type":"text","text":"hello"}]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", recordFixedDate)
		w.Header().Set("Request-Id", "req-abc")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	host := strings.TrimPrefix(harness.url, "http://")

	raw := "POST /v1/messages?beta=1 HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: Bearer sk-test-secret\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(requestBody)) + "\r\n" +
		"\r\n" + requestBody
	status, got := r.postRaw(host, raw)
	r.c.Eq(http.StatusOK, status, "client status")
	r.c.EqDeep([]byte(responseBody), got, "the client must receive the upstream body unchanged")

	wantRequest := "POST /v1/messages?beta=1 HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: Bearer sk-test-secret\r\n" +
		"Content-Length: " + strconv.Itoa(len(requestBody)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"\r\n" + requestBody
	r.c.EqDeep([]byte(wantRequest), r.recorded(harness.dir, "000001-request.http"), "recorded request")

	wantResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Length: " + strconv.Itoa(len(responseBody)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Date: " + recordFixedDate + "\r\n" +
		"Request-Id: req-abc\r\n" +
		"\r\n" + responseBody
	r.c.EqDeep([]byte(wantResponse), r.recorded(harness.dir, "000001-response.http"), "recorded response")
}

// TestProxyRecordSSEExchangeIsRecordedVerbatim records a multi-event stream: the
// response file must hold exactly the bytes relayed to the client, event framing
// included.
func TestProxyRecordSSEExchangeIsRecordedVerbatim(t *testing.T) {
	r := newRecordTest(t)
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hel\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"lo\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Date", recordFixedDate)
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	resp, got := r.post(harness.url+"/v1/messages", `{"stream":true}`)
	r.c.Eq(http.StatusOK, resp.StatusCode, "client status")
	r.c.Eq("text/event-stream", resp.Header.Get("Content-Type"), "client content type")
	stream := strings.Join(events, "")
	r.c.EqDeep([]byte(stream), got, "the client must receive every SSE event")

	wantResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/event-stream\r\n" +
		"Date: " + recordFixedDate + "\r\n" +
		"\r\n" + stream
	r.c.EqDeep([]byte(wantResponse), r.recorded(harness.dir, "000001-response.http"), "recorded SSE response")
	r.c.FileExists(filepath.Join(harness.dir, "000001-request.http"), "an SSE exchange is still a pair")
}

// TestProxyRecordProvider429IsRecorded keeps the provider's refusal exactly as
// it came back: a 429 is a response worth reading later, not something to
// translate.
func TestProxyRecordProvider429IsRecorded(t *testing.T) {
	r := newRecordTest(t)
	errorBody := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", recordFixedDate)
		w.Header().Set("Request-Id", "req-429")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, errorBody)
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	resp, got := r.post(harness.url+"/v1/messages", `{"model":"x"}`)
	r.c.Eq(http.StatusTooManyRequests, resp.StatusCode, "client status")
	r.c.Eq("7", resp.Header.Get("Retry-After"), "client Retry-After")
	r.c.EqDeep([]byte(errorBody), got, "the client must receive the provider's error body")

	wantResponse := "HTTP/1.1 429 Too Many Requests\r\n" +
		"Content-Length: " + strconv.Itoa(len(errorBody)) + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Date: " + recordFixedDate + "\r\n" +
		"Request-Id: req-429\r\n" +
		"Retry-After: 7\r\n" +
		"\r\n" + errorBody
	r.c.EqDeep([]byte(wantResponse), r.recorded(harness.dir, "000001-response.http"), "recorded 429")
}

// TestProxyRecordUpstreamDialFailureRecords502 records the recorder's own
// client-visible 502 in the normal response file — there is no sidecar format
// for failures.
func TestProxyRecordUpstreamDialFailureRecords502(t *testing.T) {
	r := newRecordTest(t)
	harness := startProxyRecord(r, "http://"+closedLoopbackAddr(r))
	requestBody := `{"model":"x"}`

	resp, got := r.post(harness.url+"/v1/messages", requestBody)
	r.c.Eq(http.StatusBadGateway, resp.StatusCode, "client status")
	r.c.Eq(recordBadGatewayBody, string(got), "client body")

	wantResponse := "HTTP/1.1 502 Bad Gateway\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + recordBadGatewayBody
	r.c.EqDeep([]byte(wantResponse), r.recorded(harness.dir, "000001-response.http"), "recorded 502")
	r.strContains(string(r.recorded(harness.dir, "000001-request.http")), requestBody, "the failed exchange is still a pair")
	r.strContains(harness.stderr.String(), "rafikid proxy record: upstream ", "the operator must be told why the upstream failed")
}

// TestProxyRecordRejectedRequestsWriteNoFiles pins the one accepted route: a
// wrong method or path is a 404 and leaves the directory untouched.
func TestProxyRecordRejectedRequestsWriteNoFiles(t *testing.T) {
	r := newRecordTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "should never be reached")
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/messages"},
		{http.MethodPut, "/v1/messages"},
		{http.MethodPost, "/v1/other"},
		{http.MethodPost, "/"},
		{http.MethodPost, "/v1/messages/"},
	} {
		req, err := http.NewRequest(tc.method, harness.url+tc.path, strings.NewReader(`{}`))
		r.c.Require().NoError(err, "%s %s", tc.method, tc.path)
		resp, err := http.DefaultClient.Do(req)
		r.c.Require().NoError(err, "%s %s", tc.method, tc.path)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		r.c.Eq(http.StatusNotFound, resp.StatusCode, "%s %s must be 404", tc.method, tc.path)
	}
	entries, err := os.ReadDir(harness.dir)
	r.c.Require().NoError(err, "read record dir")
	r.c.Len(entries, 0, "a rejected request must create no recording files")
}

// TestProxyRecordSequenceOrderUnderConcurrentRequests fills a barrier inside the
// upstream, so every request is in flight at once: a recorder that serialised
// proxying could never fill it. Each pair must then line up, one to one, in
// contiguous arrival order.
func TestProxyRecordSequenceOrderUnderConcurrentRequests(t *testing.T) {
	r := newRecordTest(t)
	const n = 8
	arrived := make(chan struct{}, n)
	gate := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		arrived <- struct{}{}
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		_, _ = w.Write(append([]byte("echo:"), body...))
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(harness.url+"/v1/messages", "application/json",
				strings.NewReader(`{"n":`+strconv.Itoa(i)+`}`))
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			bodies[i], errs[i] = io.ReadAll(resp.Body)
		}(i)
	}
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			r.c.Fail("only %d of %d requests reached the upstream concurrently; the recorder is serialising exchanges", i, n)
			close(gate)
			wg.Wait()
			return
		}
	}
	close(gate)
	wg.Wait()

	for i := 0; i < n; i++ {
		r.c.Require().NoError(errs[i], "request %d", i)
		r.c.EqDeep([]byte(`echo:{"n":`+strconv.Itoa(i)+`}`), bodies[i], "client %d body", i)
	}
	for seq := 1; seq <= n; seq++ {
		r.recorded(harness.dir, fmt.Sprintf("%06d-response.http", seq))
	}
	entries, err := os.ReadDir(harness.dir)
	r.c.Require().NoError(err, "read record dir")
	r.c.Len(entries, 2*n, "one request and one response file per exchange: %v", entries)

	seen := map[string]bool{}
	for seq := 1; seq <= n; seq++ {
		name := fmt.Sprintf("%06d", seq)
		request := r.recorded(harness.dir, name+"-request.http")
		token := requestToken(request)
		r.c.NotEmpty(token, "%s-request.http must carry a request body", name)
		r.strContains(string(r.recorded(harness.dir, name+"-response.http")), "echo:"+token,
			"%s-response.http must answer %s-request.http", name, name)
		r.c.False(seen[token], "%s was recorded twice", token)
		seen[token] = true
	}
	r.c.Len(seen, n, "every request must be recorded exactly once")
}

// TestProxyRecordOutputIsAtomicAndPrivate pins the on-disk modes and that a
// completed exchange leaves exactly its two files — no staging leftovers.
func TestProxyRecordOutputIsAtomicAndPrivate(t *testing.T) {
	r := newRecordTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	harness := startProxyRecord(r, upstream.URL)
	resp, _ := r.post(harness.url+"/v1/messages", `{"model":"x"}`)
	r.c.Eq(http.StatusOK, resp.StatusCode, "client status")

	r.recorded(harness.dir, "000001-response.http") // await the completed pair

	dirInfo, err := os.Stat(harness.dir)
	r.c.Require().NoError(err, "stat record dir")
	r.c.Eq(fs.FileMode(0o700), dirInfo.Mode().Perm(), "the record directory must be private")

	entries, err := os.ReadDir(harness.dir)
	r.c.Require().NoError(err, "read record dir")
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	r.c.EqDeep([]string{"000001-request.http", "000001-response.http"}, names, "exactly one pair, no temp files")
	for _, name := range names {
		info, err := os.Stat(filepath.Join(harness.dir, name))
		r.c.Require().NoError(err, "stat %s", name)
		r.c.Eq(fs.FileMode(0o600), info.Mode().Perm(), "%s must be 0600", name)
	}
}

// TestProxyRecordCreatesRecordDirPrivate covers the absent-directory half of the
// contract: created on demand, and created private.
func TestProxyRecordCreatesRecordDirPrivate(t *testing.T) {
	r := newRecordTest(t)
	dir := filepath.Join(t.TempDir(), "fresh", "recordings")
	cfg, err := resolveProxyRecord(proxyRecordOptions{
		Listen:    "127.0.0.1:0",
		Upstream:  "http://127.0.0.1:9",
		RecordDir: dir,
	})
	r.c.Require().NoError(err, "resolveProxyRecord")
	r.c.Eq(dir, cfg.RecordDir, "the record dir resolves to an absolute path")
	info, err := os.Stat(dir)
	r.c.Require().NoError(err, "stat created record dir")
	r.c.Eq(fs.FileMode(0o700), info.Mode().Perm(), "a created record directory must be 0700")
	r.c.Eq("127.0.0.1:0", cfg.Listen, "listen")
}

// TestProxyRecordRefusesNonemptyRecordDir refuses to write into a directory that
// already holds anything, so a previous capture is never overwritten or mixed
// with a new one.
func TestProxyRecordRefusesNonemptyRecordDir(t *testing.T) {
	r := newRecordTest(t)
	dir := t.TempDir()
	existing := filepath.Join(dir, "000001-request.http")
	r.c.Require().NoError(os.WriteFile(existing, []byte("an earlier capture"), 0o600))

	_, err := resolveProxyRecord(proxyRecordOptions{
		Listen:    "127.0.0.1:0",
		Upstream:  "http://127.0.0.1:9",
		RecordDir: dir,
	})
	r.c.ErrorIs(err, errProxyRecordDir, "a nonempty record dir must be refused")
	r.c.ErrorContains(err, "not empty", "the refusal must say why")

	got, err := os.ReadFile(existing)
	r.c.Require().NoError(err, "read the pre-existing file")
	r.c.Eq("an earlier capture", string(got), "the recorder must not touch what is already there")
}

// TestProxyRecordRefusesGroupAccessibleRecordDir refuses an unsafe directory
// instead of quietly tightening it: the operator's permissions are theirs.
func TestProxyRecordRefusesGroupAccessibleRecordDir(t *testing.T) {
	r := newRecordTest(t)
	dir := filepath.Join(t.TempDir(), "wide")
	r.c.Require().NoError(os.Mkdir(dir, 0o755), "mkdir")
	r.c.Require().NoError(os.Chmod(dir, 0o755), "chmod")

	_, err := resolveProxyRecord(proxyRecordOptions{
		Listen:    "127.0.0.1:0",
		Upstream:  "http://127.0.0.1:9",
		RecordDir: dir,
	})
	r.c.ErrorIs(err, errProxyRecordDir, "a group/world accessible record dir must be refused")
	r.c.ErrorContains(err, "0755", "the refusal must name the mode it found")

	info, statErr := os.Stat(dir)
	r.c.Require().NoError(statErr, "stat record dir")
	r.c.Eq(fs.FileMode(0o755), info.Mode().Perm(), "the recorder must never relax or tighten existing permissions")
}

func TestProxyRecordAcceptsLoopbackListen(t *testing.T) {
	r := newRecordTest(t)
	for _, listen := range []string{"127.0.0.1:0", "127.0.0.1:8080", "localhost:0", "[::1]:9000"} {
		got, err := validateRecordListen(listen)
		r.c.NoError(err, "%s must be accepted", listen)
		r.c.NotEmpty(got, "%s must resolve to an address", listen)
	}
}

func TestProxyRecordRefusesNonLoopbackListen(t *testing.T) {
	r := newRecordTest(t)
	for _, listen := range []string{"", ":0", "0.0.0.0:0", "[::]:0", "10.1.2.3:80", "example.com:80", "localhost", "127.0.0.1:http", "127.0.0.1:70000"} {
		_, err := validateRecordListen(listen)
		r.c.ErrorIs(err, errProxyRecordListen, "%q must be refused", listen)
	}
}

func TestProxyRecordRefusesInvalidUpstream(t *testing.T) {
	r := newRecordTest(t)
	for _, upstream := range []string{"", "example.com", "example.com/v1", "ftp://example.com", "http://user:pass@example.com", "http:///nope", "://x"} {
		_, err := validateRecordUpstream(upstream)
		r.c.ErrorIs(err, errProxyRecordUpstream, "%q must be refused", upstream)
	}
	for _, upstream := range []string{"http://127.0.0.1:9000", "https://api.anthropic.com"} {
		got, err := validateRecordUpstream(upstream)
		r.c.Require().NoError(err, "%q must be accepted", upstream)
		r.c.Eq(upstream, got.String(), "%q must round-trip unchanged", upstream)
	}
}

// TestProxyRecordCommandBuildsWithoutDatabase pins the isolation: the command is
// reachable from the root with no RAFIKI_DB, and with all three flags set it
// fails in the recorder's own validation rather than in any daemon setup.
func TestProxyRecordCommandBuildsWithoutDatabase(t *testing.T) {
	r := newRecordTest(t)
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")

	root := newRootCmd()
	proxy, _, err := root.Find([]string{"proxy"})
	r.c.Require().NoError(err, "rafikid must have a proxy command")
	r.c.Eq("proxy", proxy.Name(), "parent command name")

	record, _, err := root.Find([]string{"proxy", "record"})
	r.c.Require().NoError(err, "rafikid must have a proxy record command")
	r.c.Eq("record", record.Name(), "child command name")
	for _, name := range []string{"listen", "upstream", "record-dir"} {
		flag := record.Flags().Lookup(name)
		r.c.Require().NotNil(flag, "--%s must exist", name)
		r.c.Eq("string", flag.Value.Type(), "--%s must be a string", name)
		r.c.NotNil(flag.Annotations[cobra.BashCompOneRequiredFlag], "--%s must be required", name)
	}

	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{
		"proxy", "record",
		"--listen", "127.0.0.1:0",
		"--upstream", "example.com",
		"--record-dir", filepath.Join(t.TempDir(), "recordings"),
	})
	r.c.ErrorIs(root.Execute(), errProxyRecordUpstream,
		"an invalid upstream must be refused by the recorder itself, with no database configured")
}

// TestProxyRecordAnnouncesAndDrainsOnShutdown runs the real listener: it parses
// the announced URL, then proves a cancelled context waits for an in-flight
// exchange to finish its paired write.
func TestProxyRecordAnnouncesAndDrainsOnShutdown(t *testing.T) {
	r := newRecordTest(t)
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		close(started)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write(append([]byte("echo:"), body...))
	}))
	t.Cleanup(upstream.Close)

	dir := filepath.Join(t.TempDir(), "recordings")
	stderr := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- runProxyRecord(ctx, proxyRecordOptions{
			Listen:    "127.0.0.1:0",
			Upstream:  upstream.URL,
			RecordDir: dir,
		}, stderr)
	}()

	url := r.awaitListener(stderr)

	clientBody := make(chan []byte, 1)
	go func() {
		resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(`{"n":1}`))
		if err != nil {
			clientBody <- nil
			return
		}
		defer func() { _ = resp.Body.Close() }()
		got, _ := io.ReadAll(resp.Body)
		clientBody <- got
	}()
	<-started
	cancel()

	// Shutdown must wait for the exchange in flight: the process may not return
	// while a paired write is still pending.
	select {
	case err := <-runErr:
		r.c.Fail("runProxyRecord returned (%v) before the in-flight exchange finished its write", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	select {
	case got := <-clientBody:
		r.c.EqDeep([]byte(`echo:{"n":1}`), got, "the in-flight exchange must be allowed to complete")
	case <-time.After(10 * time.Second):
		r.c.Fail("the in-flight exchange never answered")
	}
	select {
	case err := <-runErr:
		r.c.NoError(err, "shutdown must be clean")
	case <-time.After(10 * time.Second):
		r.c.Fail("runProxyRecord did not return after shutdown")
	}

	r.strContains(string(r.recorded(dir, "000001-response.http")), `echo:{"n":1}`,
		"the drained exchange must still be recorded")

	announced := stderr.String()
	r.strContains(announced, "rafikid proxy record: listening on "+url, "the actual listener URL must be announced")
	r.strContains(announced, "rafikid proxy record: recording to "+dir, "the record directory must be announced")
	r.strContains(announced, "WARNING", "the recording must be announced as containing secrets")
	r.strContains(announced, "never commit", "the warning must say what not to do with it")
}

// awaitListener waits for the announced listener line and returns its URL.
func (r recordTest) awaitListener(stderr *syncBuffer) string {
	r.t.Helper()
	const marker = "rafikid proxy record: listening on "
	r.c.EventuallyTrue(5*time.Second, 5*time.Millisecond, func() bool {
		return strings.Contains(stderr.String(), marker)
	}, "runProxyRecord must announce its listener")
	for _, line := range strings.Split(stderr.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, marker); ok {
			return strings.TrimSpace(rest)
		}
	}
	r.c.Fail("no listener line in %q", stderr.String())
	return ""
}

// closedLoopbackAddr returns a loopback address nothing is listening on.
func closedLoopbackAddr(r recordTest) string {
	r.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	r.c.Require().NoError(err, "reserve a loopback port")
	addr := ln.Addr().String()
	r.c.Require().NoError(ln.Close(), "release the reserved port")
	return addr
}

// strContains is c.Contains for strings: the assertion vocabulary is generic
// over slices, and a substring check reads better named than inline.
func (r recordTest) strContains(haystack, needle, format string, args ...any) {
	r.t.Helper()
	r.c.True(strings.Contains(haystack, needle), "%s: %q not in %q", fmt.Sprintf(format, args...), needle, haystack)
}

// requestToken pulls the `{"n":K}` marker out of a recorded request.
func requestToken(message []byte) string {
	start := bytes.Index(message, []byte(`{"n":`))
	if start < 0 {
		return ""
	}
	end := bytes.IndexByte(message[start:], '}')
	if end < 0 {
		return ""
	}
	return string(message[start : start+end+1])
}
